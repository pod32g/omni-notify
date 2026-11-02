# Design: producer-written messages and rich Telegram

**Date:** 2026-10-29
**Status:** Built on branch `producer-messages` (worktree
`~/Documents/code/omni-notify-wt-messages`, from `main` at e9f9425). Not
committed, merged or deployed.

## Problem

Every provider lays an event out itself. For Telegram that is the subject line,
then `Source`, `Type`, `Severity`, `Event ID`, `Time` and every label as
`label:k: v`, e.g. fansly-api's failed recording:

```
[ERROR] ❌ Recording failed: @CatinaJacket

ffmpeg_error

Source: fansly-api
Type: recording
...
label:username: CatinaJacket
```

Producers know what a reader needs and should write it. Telegram also offers far
more than plain `sendMessage`: formatting, buttons of several kinds, media and
albums, rich messages, silent sends, topics, and editing, pinning and reacting
to earlier messages. Before this change Omni-Notify used none of that beyond the
photo and buttons added on 2026-10-28.

## Telegram Bot API, as researched (10.3, 2026-08-24)

Features relevant to a notifier:

- **Formatting:** `parse_mode` HTML / MarkdownV2 / Markdown, or `entities`. HTML
  has bold, italic, underline, strike, `<tg-spoiler>`, links, code/pre with a
  language, `<blockquote>` and `<blockquote expandable>`, custom emoji (needs
  Premium) and `<tg-time unix format>` (9.5) for local dates and times.
- **Rich messages** (10.1–10.3): `sendRichMessage` with an `InputRichMessage`
  (`markdown`, `html` or `blocks`) for headings, lists, task lists, tables,
  `<details>`, footnotes, formulas, maps, collages, inline buttons; 32,768
  characters; also `editMessageText(rich_message)`.
- **Inline buttons:** `url`, `callback_data` (64 bytes), `copy_text` (7.11),
  `style` danger/success/primary and `icon_custom_emoji_id` (9.4), `disabled`
  (10.3). Also `web_app`, `login_url` and `switch_inline_query*`, which need
  setup or are not for notifications.
- **Media:** `sendPhoto`, `sendVideo`, `sendAnimation`, `sendAudio`,
  `sendDocument`; `sendMediaGroup` for 2–10 items (photos and videos mix;
  audio and documents only with their own kind; no animations; **no
  reply_markup**). `has_spoiler`, `show_caption_above_media` (7.4). Captions are
  limited to 1,024 characters; text to 4,096. By URL Telegram fetches up to 5 MB
  for photos and 20 MB otherwise; an upload can be 10 MB (photo) or 50 MB.
- **Delivery:** `disable_notification`, `protect_content`, `message_effect_id`
  (private chats only, 7.4), `link_preview_options` (7.0), `message_thread_id`
  (forum topics, and topics in private chats since 9.3), `reply_parameters`.
- **Later changes:** `editMessageText`, `editMessageCaption`,
  `editMessageReplyMarkup`, `pinChatMessage`/`unpinChatMessage` (a group needs
  the pin right), `setMessageReaction` (one emoji from a fixed list),
  `deleteMessage` (bots: their messages, within 48 h in groups).
- Not used: ephemeral messages (they need a recent user action),
  `sendMessageDraft` (streaming), checklists and suggested posts (business and
  channel DMs only), paid broadcast (costs Stars), polls, location and venue.

## Decisions

1. **One optional `message` object on the event.** It has `title` and `text` for
   every channel, plus a key per provider kind for overrides. On the wire it is
   flat (`"message": {"text": …, "telegram": {…}}`); in Go it is
   `models.Message{Title, Text, Channels map[string]json.RawMessage}` with custom
   JSON. It is stored in a new `events.message` column (an idempotent `ALTER
   TABLE` migration) and is neither matched by routes nor fingerprinted.
2. **Producer text is sent as written** by every provider: no tag, no fields
   (`format.go`: `headline`, `bodyText`, `fullText`, `summaryText`;
   `detailFields` returns none). `title` fills title slots, defaulting to the
   untagged event title. On SMTP it wins over `subject_template`. Without
   `message`, nothing changes.
3. **Overrides are validated on ingest** (`providers.ValidateMessage`, called by
   `POST /api/v1/events`) with unknown fields rejected, so typos are a 400. An
   override for a kind that takes none yet is refused rather than ignored. At
   send time an invalid stored override is logged and ignored.
4. **`message.telegram` mirrors Bot API names** (`text`, `parse_mode`,
   `entities`, `disable_notification`, `protect_content`, `link_preview_options`,
   `message_thread_id`, `show_caption_above_media`), adds `rich`, `media`,
   `buttons` (rows), `message_effect` (a name or an ID), and the ref operations.
   Passing the raw Bot API objects through (rich messages, link previews,
   entities) keeps new API features usable without Omni-Notify changes.
5. **Message refs.** The new table `sent_messages(channel, account, chat, ref →
   message_id, kind)` remembers each sent message under `ref`, defaulting to
   the ref it edits, then to the event fingerprint. A message sent anew because
   the old one couldn't be edited takes over the ref. It is per bot (the token's public ID) and chat, kept
   30 days and pruned on write. The table is channel-neutral, so other
   providers can later remember Matrix event IDs or Discord message IDs.
   `edit`, `reply_to`, `pin`, `unpin`, `delete`, `react` and `send: false` work
   on refs, which gives firing → resolved lifecycles in one message. Providers
   get the store through `providers.NewDefault(…, WithMessageStore(store))`; a
   variadic option leaves other call sites unchanged.
6. **Only the message can fail a delivery.** Pins, unpins, reactions and
   deletions are best effort and logged, so a notifier retry never duplicates
   the message. With `send: false` they *are* the delivery: network errors,
   rate limits and outages are returned (retried); refusals are not.
7. **Degrade, don't drop.** On a 400, `callDegrading` tries again:
   - If the text can't be parsed, it is sent as plain text (HTML tags stripped,
     entities unescaped).
   - Otherwise the optional parameters (buttons, effect, preview, topic, reply,
     protect) are dropped.

   `sendMessage` is the last resort, so it drops those parameters on any 400.
   Media, album and rich sends drop them only when the refusal names one, and
   otherwise fall back to text. Media are not re-uploaded after Telegram
   rejected them. A failed edit (except "message is not modified") sends a new
   message.
8. **Lengths in UTF-16 units**, as Telegram counts. HTML is measured after
   stripping tags. Formatted text over the limit is sent as plain text before
   cutting, because cutting markup breaks it. When a producer's text is too
   long for a caption, it goes out as a reply after the media. The default
   layout's caption is still cut, as before.
9. **Media sources.** URLs on `media_hosts` (alias of the existing
   `image_hosts`) are fetched by Omni-Notify (guarded client, redirects only to
   allowed hosts, size caps, photos sniffed) and uploaded. Other URLs are given
   to Telegram, so Omni-Notify itself never fetches an unlisted host (no SSRF);
   a non-URL is a `file_id`. The `image_url` annotation keeps its upload-only
   behaviour.
10. **Albums can't carry buttons.** With buttons, a first item that has its own
    caption, or text too long for a caption, the text and buttons follow the
    album as a reply to it.
11. **Callback buttons in `message.telegram`** sign `c2:<event>:<index>:<mac>`.
    The index is among the override's `callback_url` buttons in row order; `c1`
    stays for the `callbacks` annotation. A press re-reads the stored event's
    override and checks the button's host against `callback_hosts` and
    `callback_user_ids`, as for `c1`.
12. **New provider config:** `media_hosts`, `media_max_bytes` (20 MiB, at most
    50 MiB), `message_thread_id`, and `silent_severities`, which sends the
    listed severities without sound unless the event sets
    `disable_notification`. The admin UI has fields for these and for
    `callback_hosts` and `callback_user_ids`.

## Verification

- Unit tests run against the fake Bot API and an in-memory ref store, and all
  pass under `-race`. They cover:
  - validation and each send path: text, rich, media by URL and by upload,
    long captions, albums with and without buttons
  - options, silent severities and degrading
  - refs: edit, reply, pin, unpin, react, delete, `send: false`, and edit
    fallbacks
  - `c2` callback presses
  - producer text on Discord, Slack, Teams, Google Chat and SMTP
  - storage round trips, ref expiry and pruning, migration of an old database
  - the API's 400s
- End to end, the built binary ran against a local fake Telegram. fansly's
  failed-recording event went out unchanged without `message`; with
  `message.text` it went out as written. A started → failed pair was sent,
  pinned, edited in place, unpinned and reacted to. A rich digest went out via
  `sendRichMessage`, and a misspelt option was refused with a 400.
- **Not verified against the real Telegram:** the effect IDs behind the names
  are community-known values, not documented by Telegram (a refused effect is
  dropped). `style`, `copy_text`, `disabled` and `sendRichMessage` follow the
  documentation but haven't been seen in a real chat.

## The other channels (same day, same branch)

The same pattern now covers every provider. `overrides.go` holds the registry
(`overrideValidators`), strict decoding and shared checks. `refs.go` holds the
ref book that Telegram, Discord and Matrix share. `doRequest`/`sendJSON` return
response bodies, and `isPayloadRefused` (400, 413 or 422) triggers the plain
fallback.

| Kind | What it adds | Refs |
|------|--------------|------|
| discord | content, ≤10 embeds, components (`with_components=true`), poll, allowed_mentions, username/avatar, tts, flags by name, thread_id/thread_name/applied_tags | sends with `?wait=true` to get the ID; `edit` = PATCH (clearing content, embeds and components the edit doesn't set), `delete` = DELETE; account = hash of the webhook URL, chat = thread_id |
| slack | text, Block Kit blocks, attachments, mrkdwn/unfurl, thread_ts, legacy username/icons | none: incoming webhooks return no `ts` and can't edit or delete |
| teams | an Adaptive Card in the Workflows webhook shape (`type: message`, `attachments[].contentType` adaptive, `contentUrl: null`), or a legacy MessageCard | none |
| googlechat | text, cardsV2, threadKey (+ `messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD`) | the thread key is its own grouping |
| ntfy | JSON publishing to the root URL: title, message, markdown, priority, tags, click, attach, filename, icon, ≤3 actions (view/broadcast/http/copy), delay, email | ntfy sequence IDs need no storage: `ref`/`edit` publish under one (an invalid ref is hashed to `^[-_A-Za-z0-9]{1,64}$`), `clear` = PUT `/topic/sid/clear`, `delete` = DELETE `/topic/sid` |
| gotify | priority, extras passthrough + shorthands (markdown, click_url, big_image_url, intent_url) | none |
| pushover | html/monospace, url/url_title, priority with emergency retry/expire, sound, device, ttl, tags | none |
| matrix | HTML formatted_body, msgtype, m.mentions | event IDs: `reply_to` (m.in_reply_to), `thread` (m.thread with fallback), `edit` (m.replace + m.new_content; the ref keeps naming the original), `react` (m.reaction annotation), `pin`/`unpin` (read-modify-write of m.room.pinned_events), `delete` (redact) |
| smtp | an HTML part (multipart/alternative, quoted-printable, so long HTML lines stay under SMTP's 998-byte limit) | none |
| pagerduty | payload component/group/class/custom_details, links, images, client, dedup_key, event_action (acknowledge, resolve) | the dedup key |
| opsgenie | priority, tags, details, actions, entity, note (also on close), user, responders, visibleTo, alias | the alias |
| twilio | body, media_url (MMS) | none |

The rules are the same as Telegram's:
- Checked on ingest, with unknown fields refused.
- Rich content is passed through for the channel to check.
- A refused rich message falls back to the plain one.
- Actions on earlier messages are best effort, except under `send: false`,
  where errors worth retrying are returned.

Also fixed: SMTP subjects that aren't ASCII (an emoji) are now RFC 2047-encoded.

Not done:
- Media uploads for Discord, Matrix (mxc upload) and Pushover attachments.
  They need the fetch-and-upload allowlist Telegram has; image URLs that
  Discord embeds and ntfy `attach` fetch themselves already work.
- Slack editing, which needs a bot token and `chat.postMessage`/`chat.update`
  rather than a webhook.

## Next

- Producers adopting `message`: fansly-api (`src/utils/omni_notify.py`) and
  Omni-Media. For fansly: `ref` = `rec:<stream_id>`, then `edit` on
  failed/finished.
