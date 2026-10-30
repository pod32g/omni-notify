# The on-call plugin

**Status:** on `main` since 2026-10-29, off by default; on at chizu with an owner-only starter
directory. Sign-in with Omni Identity added 2026-10-30.
**Client:** the Omni On-Call iOS app (`omni-notify-ios`), whose contract is its
`docs/oncall/API.md`.

The plugin adds incidents, on-call schedules and escalation to Omni-Notify for the Omni On-Call
app. It is an add-on, not a change to what Notify is: the core stays a router and delivery engine,
and with the plugin off (the default) nothing about Notify changes.

## Where it meets the core

```
omni-metrics ─event─▶ Notify core ─route─▶ provider "oncall-jellyfin" (kind oncall) ─▶ plugin
                         ▲                                                            │ opens, adds to,
                         │ Deliver(page event, the person's providers)               │ resolves, escalates
                         └────────────────────────────────────────────────────────────┘
iPhone ─/oncall/api/v1 (Omni Identity token, or a personal token)─▶ plugin
```

The plugin talks to the core in two ways only, both through `internal/plugin.Host`:

1. **Alerts come in as a provider.** The plugin registers the provider kind `oncall`. A provider
   of that kind names one plugin service (`config.service`), and Notify's ordinary routes send
   events to it, the same way they send them to PagerDuty or Opsgenie. Dedup, routing and retries
   are Notify's, as for any provider.
2. **Pages go out as deliveries.** To page a person, the plugin asks the core to deliver an event
   (`type: oncall.page`, `source: omni-oncall`) through that person's contacts, which are Notify
   provider names. `Notifier.Deliver` skips routing and dedup and uses the normal queue, retries
   and delivery records, so pages show in Notify's UI like everything else. Routes are never
   consulted, so a page can't loop back into the plugin (and an `omni-oncall` event that reaches
   it anyway is ignored).

Everything else belongs to the plugin:
- its own SQLite file (`plugins.oncall.storage.path`, default `oncall.db`);
- its routes under `/oncall/`, which authenticate people rather than Notify's admin tokens;
- its escalation clock.

The core never imports it. `cmd/omni-notify/plugins.go` lists the plugins compiled in, and a
`plugins:` section for an unknown plugin fails at start.

## Configuration

```yaml
plugins:
  oncall:
    enabled: true
    storage: {path: /data/oncall.db}
    escalation_delay: 5m          # a level's default wait before the next level
    identity:                     # sign-in with Omni Identity (optional)
      issuer: https://identity.omni.pod32g.dev
      audiences: [omni_vy47oxmf]  # the Omni Access gateway's client id
      # jwks_url: …               # by default from the issuer's discovery document
    users:
      - id: u_david
        name: David
        identity: david           # Omni Identity username (or subject)
        timezone: America/Mexico_City
        token_sha256: <hex>       # optional personal token: printf %s "$TOKEN" | shasum -a 256
        contacts: [telegram-david] # Notify providers a page goes through
        # capabilities: [create_incident, reassign, escalate, resolve, manage_overrides]
    teams:
      - {id: t_media, name: Media, members: [u_david]}
    schedules:
      - id: sch_media
        name: Media primary
        team: t_media
        timezone: America/Mexico_City
        rotation: weekly          # or daily
        handoff: "09:00"          # wall clock in the schedule's zone
        start: "2026-11-02"       # the first user's turn starts that day
        users: [u_david]
    escalation_policies:
      - id: ep_media
        name: Media escalation
        team: t_media
        repeat: 1                 # start again from level 1 once
        levels:
          - {delay: 10m, schedules: [sch_media]}
          - {delay: 10m, users: [u_david]}
    services:
      - {id: svc_jellyfin, name: jellyfin, escalation_policy: ep_media}

providers:
  - {name: oncall-jellyfin, kind: oncall, config: {service: svc_jellyfin}}

routes:
  - {name: jellyfin-to-oncall, match: {labels.service: jellyfin}, providers: [oncall-jellyfin]}
```

The directory (people, teams, schedules, policies and services) is read from config at every
start and checked: an unknown reference, time zone, rotation or malformed token hash refuses to
start. Incidents, overrides, the timeline and the feed live in the plugin's database.

## How it behaves

- **Alerts → incidents.** A firing event opens an incident on the provider's service, with the
  first level of the service's policy assigned and paged. Another firing with the same fingerprint
  adds to the open incident (`alert_count`). A resolve closes it (`auto_resolve: false` only notes
  it). Stateless events open an incident each time Notify delivers one.
- **Severity:** label `oncall_severity` (sev1–sev4) if present, else critical → SEV1,
  error → SEV2, warning → SEV3, anything else → SEV4.
- **Order:** Notify delivers in parallel, so a resolve can arrive before its firing. The plugin
  remembers when an alert resolved (for a day) and ignores a firing stamped no later than that.
  A retried delivery (same Notify delivery id) is taken in once.
- **Escalation:** each level has `delay` to acknowledge before the next level is assigned and paged.
  After the last level, `repeat` starts again from level 1. Acknowledging stops it. Reassigning to
  a person keeps the incident's place in the policy; reassigning to a policy starts at its level 1.
- **Schedules:** daily or weekly turns handing off at a wall-clock time, so a turn across a
  daylight-saving change is 23 or 25 hours. Overrides are carved out of the rotation, later ones
  over earlier ones. A change of who's on call is recorded as a handoff in the feed.
- **Who may do what:**
  - acknowledge: the assignees and the incident's team;
  - resolve, reassign, escalate, open incidents, cover shifts: the matching capability;
  - notes: anyone.
- **The truth for the app:** every response carries `as_of`, incidents carry a `version`, actions
  take an `Idempotency-Key` (kept a day per person), a refused action returns 409 with the current
  incident, and `GET /changes?since=` gives a cursor. A timeline names "you" per viewer, and a page
  with no contact methods says "Not paged: no contact methods" instead of pretending.

## API

`/oncall/api/v1/…` as in the app's `docs/oncall/API.md`:
- `me`, `users`;
- `incidents` (list, create, get, `timeline`, and `acknowledge`, `resolve`, `reassign`,
  `escalate`, `notes`);
- `oncalls`, `schedules` (list, get, `overrides`);
- `escalation_policies`, `services`, `activity`, `changes`.

`/me` includes `server: {kind: "omni-notify"}` so the app can tell it from the development
stand-in.

**Auth:** `Authorization: Bearer <token>`, one of:
- **An Omni Identity access token** (when `identity` is set): signed by the issuer (EdDSA or RS256,
  keys from its JWKS, cached ten minutes), for one of `audiences`, unexpired. Its
  `preferred_username`, else its `sub`, is matched against the users' `identity`. A valid token
  for someone not in the directory gets 403 `not_in_directory`; Identity unreachable with no cached
  key gets 503 `identity_unavailable`.
- **A personal token** whose SHA-256 is a user's `token_sha256`, for scripts and development.

Notify's admin tokens are not accepted here.

**How the app signs in.** Notify is behind Omni Access, which admits only an enrolled device's
token. The Omni On-Call app shares the Omni Access app's sign-in on the iPhone (same team, its app
group `group.com.omni.access`) and mints the same short-lived token the Access tunnel uses
(RFC 8693, audience = the gateway's client id). It sends that token twice:
`Proxy-Authorization` for the gateway, which checks and strips only that header, and
`Authorization` for the plugin, which checks it again and maps the person. So `audiences` is
the gateway's client id (`GET <gateway>/api/v1/meta` → `client_id`).

**Reaching Identity from the container.** The plugin fetches Identity's keys over HTTPS. chizu's
containers resolve with a public resolver, so `docker-compose.yml` gives Notify the Pi-hole first
(`OMNI_NOTIFY_DNS`), as Omni Access does.

## Not yet

- Push to the app. An APNs alert provider in Notify is still missing; today pages go through
  whatever providers a person lists (Telegram, ntfy, Pushover, …).
- Shift swaps (`request_swaps` is never granted), service health from omni-metrics (`health` is
  empty), editing the directory through the API, and Telegram buttons that acknowledge.

## Running it locally

Build Notify, add a `plugins.oncall` section and `oncall` providers and routes as above, and post
events to `/api/v1/events` with the routed labels. The iOS repo's OnCallKit live test runs against
it with `ONCALL_MOCK_URL=http://127.0.0.1:<port>/oncall` and a user whose token is `dev`.
