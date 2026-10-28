# Design: Telegram photos, link buttons and callback buttons

**Date:** 2026-10-28
**Status:** Built on branch `telegram-media`; not deployed, live provider config unchanged.

## Problem

Omni-Media (live since 2026-10-28) sends richer `omni-media.episode` events
(its DESIGN §26): annotations `image_url` (the series poster), `actions` (a JSON
list of `{"label","url"}` link buttons: Watch, Open in Omni-Media) and links to
episode pages. The Telegram provider sent plain text only, so all of that showed
up as `k: v` lines. The owner asked for posters and buttons in Telegram
(Omni-Media backlog #33), and for buttons that act — Retry, Try another release
— without opening the dashboard (#36).

## What Omni-Notify's container can reach (checked on chizu, 2026-10-28)

From a throwaway container on `omni-notify_default` (the network Omni-Media
joins):

- `media.omni.pod32g.dev` resolves (to chizu, 192.168.68.34), its certificate
  verifies, and `GET /api/series/91/poster` returns the 158 KB PNG.
- `http://omni-media:8080/api/series/91/poster` returns the same PNG.
- `POST /api/episodes/0/retry` (rejected as "bad id" before doing anything) with
  `X-Media-Request: 1` reaches Omni-Media's handler both ways; without the
  header it is refused as cross-site. Omni-Media's Access route is public
  (`auth.public: true`), so no sign-in is involved.

So the public URLs Omni-Media already sends work as they are: no rewriting to
`omni-media:8080` is needed (it would only avoid the Access hop).

## Decisions

1. **Rendering hints are annotations**, not new event fields: `image_url`,
   `actions`, `callbacks`. Telegram renders them and leaves them out of its text;
   every other provider is unchanged (they still list them as text).
2. **Everything is opt-in per provider.** `image_hosts` and `callback_hosts` are
   allowlists of hostnames (case-insensitive, any port, http/https only, no
   userinfo). Empty means text only / no callback buttons — today's behaviour.
   Omni-Notify never fetches or calls a host the provider config did not name
   (no SSRF through an event), and the guarded dialer still applies
   `security.allow_private_webhook_targets` after DNS resolution.
3. **Photos are fetched by Omni-Notify and uploaded** (multipart `sendPhoto`),
   because Telegram's servers cannot resolve `media.omni.pod32g.dev`. Limits:
   5 s, `image_max_bytes` (5 MiB default, 10 MiB max = Telegram's photo limit),
   redirects only to allowed hosts (at most 3), and the bytes must sniff as
   JPEG, PNG or WebP (headers are not trusted). The caption is the usual text cut
   to 1,024 characters.
4. **No failure costs the message.** Any photo failure (fetch, size, type,
   Telegram refusing it) falls back to `sendMessage` with the same buttons; a
   message Telegram refuses with 400 while it has buttons is sent again without
   them. Other errors stay delivery errors, retried by the notifier as before.
5. **Callback buttons carry no URL.** `callback_data` is limited to 64 bytes, so
   a button holds `c1:<event row id>:<index>:<mac>` (≤ 50 bytes): the stored
   event's ID and the entry's index in its `callbacks` annotation, with a
   truncated HMAC-SHA256 keyed by a value derived from the bot token and bound to
   the configured chat. A modified Telegram client cannot invent buttons or move
   one to another event or chat. No new table: the action is looked up again in
   the stored event when pressed. `NotificationMessage` gained `EventRef` (and
   `DeliveryID`, matching the parked voice-call branch so the two merge cleanly);
   test sends have no event, so they never get callback buttons.
6. **getUpdates long polling, not a webhook.** Telegram cannot reach Omni-Notify
   (home network, double NAT, `notify.omni.pod32g.dev` behind Access and
   LAN-only DNS), so a webhook would need a public endpoint we do not have.
   `providers.TelegramCallbacks` (started from `main`) re-reads the providers
   every 30 s and runs one poller per bot that has an enabled Telegram provider
   with `callback_hosts`, asking for `callback_query` updates only (25 s long
   polls; backoff 2 s → 1 min on errors, e.g. a 409 when the bot has a webhook or
   another consumer). On shutdown it confirms the last offset so a restart does
   not see the same presses again.
7. **A press is checked against the current config, then answered at once,
   then run.** Checks: the message's chat is the provider's `chat_id`; the user
   is in `callback_user_ids` when set; the MAC; the event exists and arrived
   within `callback_ttl` (7 days); the entry exists; its URL is still on an
   allowed host. Refusals are alerts. A valid press is answered with a toast
   ("Retry…") *before* the request, because Omni-Media may take up to 3 minutes
   (a retry can search); if Telegram no longer accepts the answer (the press was
   made while Omni-Notify was down) the action is not run. Then `POST <url>`,
   no body, `callback_headers`, 3 min 15 s timeout, redirects not followed.
   2xx: the pressed button becomes "✓ <label>" (other buttons kept; pressing it
   again only says "Already done."). Otherwise: a reply to the message,
   "⚠️ <label> didn't work: <reason>", quoting an `{"error": "…"}` answer.
8. **Bot API errors never contain the token.** The new client unwraps transport
   errors (`*url.Error` carries the token-bearing URL); before, a network error
   put the token into the delivery's `last_error`.

## Contract for producers (Omni-Media)

```json
"annotations": {
  "image_url": "https://media.omni.pod32g.dev/api/series/91/poster",
  "actions":   "[{\"label\":\"Watch\",\"url\":\"https://jellyfin.omni.pod32g.dev/item/abc\"},{\"label\":\"Open in Omni-Media\",\"url\":\"https://media.omni.pod32g.dev/#/episode/25\"}]",
  "callbacks": "[{\"label\":\"Retry\",\"url\":\"https://media.omni.pod32g.dev/api/episodes/25/retry\"},{\"label\":\"Try another release\",\"url\":\"https://media.omni.pod32g.dev/api/episodes/25/another\"}]"
}
```

- `callbacks` has the same shape as `actions`: a JSON **string** (annotations are
  string maps) holding a list of `{"label","url"}`. Each URL is called with
  `POST`, no body; a 2xx answer is success, an `{"error": "…"}` body is shown on
  failure. Order matters only for layout: Telegram shows callback buttons first,
  then link buttons, two per row, at most 8 of each; entries with an empty label
  or a URL not on `callback_hosts` are skipped.
- A button stays valid for `callback_ttl` after the event arrived and keeps
  pointing at that event's entry, so producers must not reuse event IDs for a
  different meaning within that time (Omni-Notify stores each post as its own
  row, so this holds by construction).
- Every press reaches the producer as an anonymous request (Omni-Media records
  it as "someone on the network", as for any public-mode request).

### What Omni-Media must add for #36 (not built there)

In `internal/engine/notices.go`:

1. Give `message` the episode it acts on (for example `episodeID int64` and
   `actsOn bool`), set by `episodeMessage` for the per-episode problem kinds
   (download failed, needs you, gave up, bad file) and, in grouped "needs you"
   messages, for a message holding exactly one problem.
2. In `send`, when set and `public_url` is known, add the annotation
   `callbacks` = `[{"label":"Retry","url":"<public_url>/api/episodes/<id>/retry"},
   {"label":"Try another release","url":"<public_url>/api/episodes/<id>/another"}]`
   (`json.Marshal` of `[]msgAction`, like `actions`). Keep `actions` link-only.
3. Update DESIGN §26 "Buttons in Telegram (#36)" and the backlog: the buttons
   now act through Omni-Notify, which POSTs from the home network with
   `X-Media-Request: 1`; opening a link still never changes anything.
4. Optional, later: a grouped message with several problems could carry one
   pair per problem ("Retry Frieren S02E05"), up to Omni-Notify's 8 callback
   buttons.

### What the owner switches on (Omni-Notify, after deploying)

PATCH the Telegram provider used by the `omni-media` route:

```json
{"config": {
  "image_hosts": ["media.omni.pod32g.dev"],
  "callback_hosts": ["media.omni.pod32g.dev"],
  "callback_headers": {"X-Media-Request": "1"},
  "callback_user_ids": [<the owner's Telegram user ID>]
}}
```

`callback_user_ids` matters if the chat is shared (a family group): without
it anyone in the chat can press Retry. Polling uses that bot's `getUpdates`, so
the bot must not have a webhook or another program reading its updates.
