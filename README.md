# Omni-Notify

A generic event notification service. External systems decide *when* something
happened; Omni-Notify receives those events, **deduplicates**, **routes**, and
**delivers** notifications through pluggable providers — with delivery tracking,
retries, and Prometheus metrics.

Omni-Notify does **not** evaluate alert rules, query metrics, run health checks,
or contain business logic. It is a router and delivery engine.

## Features

- Generic event ingestion with required/optional fields and strict validation
- **Lifecycle (`status`) and `severity` are separate axes** (see below)
- Stateful (`firing`/`resolved`) and stateless event handling
- **Priority-ordered routing** with optional `stop_processing`, exact-match on
  `type`, `source`, `severity`, `status`, and dotted `labels.<k>` /
  `annotations.<k>`, plus a default-route fallback
- **One delivery per provider** even when multiple routes select it
- Per-route deduplication windows and repeat intervals
- Pluggable providers: **Discord**, **Slack**, **generic webhook**, **SMTP**
- Async, durable delivery queue (SQLite-backed) with exponential-backoff retry
  and crash recovery
- Bearer-token auth, request-size limits, **configurable SSRF protection** for
  webhook targets, **secrets encrypted at rest** (AES-256-GCM) and masked on the API
- Prometheus metrics and a `/healthz` endpoint
- REST API with `POST` (create) / `PUT` (replace) / `PATCH` (partial) semantics
- **Embedded web UI** (served at `/`) for events, states, deliveries, and full
  provider/route management
- Hybrid config: a YAML file seeds providers/routes at boot; the REST API does
  runtime CRUD; **SQLite is the source of truth**

## Lifecycle vs severity

`status` describes the **lifecycle** and is limited to `firing` and `resolved`.
`severity` is an independent axis: `critical`, `error`, `warning`, `info`,
`debug`. Stateless events omit `status` entirely.

```json
{ "status": "firing", "severity": "critical" }   // an alert firing
{ "severity": "info" }                            // a deployment finished
{ "severity": "warning" }                         // a failed login
```

**Backward compatibility:** a legacy `status` of `info`/`warning`/`error` is
auto-migrated to `severity` on ingest (when `severity` is empty) and the status
cleared, so older producers keep working.

## Quick start

```sh
# 1. Build
make build

# 2. Generate an encryption key for provider secrets
export OMNI_NOTIFY_ENCRYPTION_KEY="$(./omni-notify genkey)"

# 3. Set an API token and any provider secrets referenced by the config
export OMNI_NOTIFY_API_TOKEN="$(openssl rand -hex 24)"
export DISCORD_HOME_WEBHOOK="https://discord.com/api/webhooks/…"

# 4. Copy and edit the example config
cp config.example.yaml config.yaml

# 5. Run
./omni-notify -config config.yaml
```

## Sending an event

```sh
curl -X POST http://localhost:8080/api/v1/events \
  -H "Authorization: Bearer $OMNI_NOTIFY_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "event_id": "pihole-down",
    "type": "alert",
    "source": "homelab",
    "status": "firing",
    "severity": "critical",
    "title": "Pi-hole Down",
    "summary": "Pi-hole is not responding",
    "labels": {"service": "pihole", "host": "raspberrypi"},
    "timestamp": "2026-06-28T20:00:00Z"
  }'
```

Response:

```json
{
  "fingerprint": "…",
  "event_id": 1,
  "deduplicated": false,
  "routes_matched": ["critical-events", "pihole"],
  "deliveries_enqueued": 2
}
```

### Event fields

| Field         | Required | Notes                                            |
|---------------|----------|--------------------------------------------------|
| `event_id`    | yes      | producer's identifier for the event              |
| `type`        | yes      | e.g. `alert`, `deploy`, `security`               |
| `source`      | yes      | producing system                                 |
| `title`       | yes      | short human title                                |
| `timestamp`   | yes      | RFC3339                                          |
| `status`      | no       | lifecycle: `firing`\|`resolved` (omit if stateless) |
| `severity`    | no       | `critical`\|`error`\|`warning`\|`info`\|`debug`   |
| `summary`     | no       | one-line summary                                 |
| `description` | no       | longer description                               |
| `labels`      | no       | string→string map (matchable, in fingerprint)   |
| `annotations` | no       | string→string map (matchable)                    |
| `fingerprint` | no       | dedup key; derived if omitted                    |

## Deduplication

The dedup key is `fingerprint` if supplied, else
`sha256(type | source | event_id | sorted labels)`.

- **firing** → notify once; suppressed while still firing unless the route's
  `repeat_interval` elapses.
- **resolved** → notify once if it was firing, then mark inactive; a resolve for
  something that never fired is suppressed.
- **stateless** (no `status`) → notify if the route's `dedup_window` has elapsed
  since the last notification.

Windows and intervals are per-route, with config defaults as the fallback. A
route value of `0` (or unset) inherits the configured default; a **negative**
value (e.g. `dedup_window: -1s`) explicitly **disables** that behaviour for the
route (notify every time / never auto-repeat).

## Routing & provider resolution

Routes carry an optional `priority` (default `0`) and `stop_processing` flag.
The resolution algorithm is deterministic:

```
collect non-disabled routes whose match conditions all hold
  ↓  (if none match, use matching default routes)
sort by priority (desc), then name (asc)
  ↓
if a matched route sets stop_processing, drop strictly lower-priority routes
  ↓
for each route in order, evaluate per-route deduplication
  ↓
collect providers from notifying routes, deduplicate by provider
  ↓  (first/highest-priority route that names a provider owns its delivery)
create one delivery record per unique provider, enqueue
  ↓
workers send, with retry + delivery tracking
```

Match keys: `type`, `source`, `severity`, `status`, and dotted `labels.<k>` /
`annotations.<k>`; all conditions must match exactly. A provider reached through
several matching routes receives exactly **one** delivery per event.

## Web UI

A self-contained admin UI (no build step, embedded in the binary) is served at
`http://<host>:<port>/`:

- **Events / States / Deliveries** — filterable tables with auto-refresh; click a
  row for the full JSON.
- **Providers / Routes** — create, replace, and partially edit from the browser,
  plus a per-provider **Send test** button.
- **Send event** — post a sample event to exercise routing and dedup.

The UI shell and its `/assets/*` are served unauthenticated; the page calls the
authenticated API with a **bearer token you paste into the top bar** (kept in the
browser's `localStorage`). Enter your `OMNI_NOTIFY_API_TOKEN` to load data.

## REST API

All `/api/v1/*` endpoints require `Authorization: Bearer <token>`.
`/healthz` and the UI (`/`, `/assets/*`) are always open; `/metrics` is open
unless `metrics_require_auth: true`.

| Method | Path                              | Purpose                              |
|--------|-----------------------------------|--------------------------------------|
| POST   | `/api/v1/events`                  | ingest an event (returns `202`)      |
| GET    | `/api/v1/events`                  | list events (filters: source/type/severity/status/fingerprint/limit/offset) |
| GET    | `/api/v1/events/{id}`             | get one event                        |
| GET    | `/api/v1/states`                  | list current states (`?active=true`) |
| GET    | `/api/v1/states/{fingerprint}`    | get one state                        |
| GET    | `/api/v1/providers`               | list providers (secrets masked)      |
| POST   | `/api/v1/providers`               | create a provider (`409` if it exists) |
| GET    | `/api/v1/providers/{name}`        | get one provider (masked)            |
| PUT    | `/api/v1/providers/{name}`        | replace a provider                   |
| PATCH  | `/api/v1/providers/{name}`        | partially update a provider          |
| GET    | `/api/v1/routes`                  | list routes                          |
| POST   | `/api/v1/routes`                  | create a route (`409` if it exists)  |
| GET    | `/api/v1/routes/{name}`           | get one route                        |
| PUT    | `/api/v1/routes/{name}`           | replace a route                      |
| PATCH  | `/api/v1/routes/{name}`           | partially update a route             |
| GET    | `/api/v1/deliveries`              | delivery history (`?fingerprint=`/`?status=`) |
| POST   | `/api/v1/test`                    | send a test notification to a provider |
| GET    | `/healthz`                        | liveness/readiness                   |
| GET    | `/metrics`                        | Prometheus metrics                   |

## Providers

| Kind         | Secret            | Key config                                          |
|--------------|-------------------|-----------------------------------------------------|
| `discord`    | webhook URL       | `username`, `avatar_url`                            |
| `slack`      | webhook URL       | `username`, `icon_emoji`                            |
| `webhook`    | target URL¹       | `method`, `content_type`, `headers`, `template`     |
| `smtp`       | password          | `host`, `port`, `username`, `from`, `to[]`, `tls`, `subject_template` |
| `telegram`   | bot token         | `chat_id` (req), `parse_mode`                       |
| `ntfy`       | topic URL         | `priority`, `tags`, `token`                         |
| `gotify`     | app token         | `url` (req, server base), `priority`                |
| `pushover`   | API token         | `user` (req), `priority`, `device`                  |
| `teams`      | webhook URL       | —                                                   |
| `matrix`     | access token      | `homeserver` (req), `room_id` (req), `msgtype`      |
| `pagerduty`  | routing key       | `source` (firing→trigger, resolved→resolve)         |
| `opsgenie`   | API key           | — (firing→create, resolved→close, keyed by fingerprint) |
| `googlechat` | webhook URL       | —                                                   |
| `twilio`     | auth token        | `account_sid` (req), `from` (req), `to` (req)       |

Providers that talk to a fixed SaaS endpoint also accept an override config key
(`api_base` / `api_url` / `events_url`) — handy for self-hosted/proxied instances.

Add a provider via the API (secret is encrypted before storage):

```sh
# Telegram
curl -X POST http://localhost:8080/api/v1/providers \
  -H "Authorization: Bearer $OMNI_NOTIFY_API_TOKEN" \
  -d '{"name":"tg-home","kind":"telegram","secret":"<bot-token>","config":{"chat_id":"123456789"}}'

# Slack
curl -X POST http://localhost:8080/api/v1/providers \
  -H "Authorization: Bearer $OMNI_NOTIFY_API_TOKEN" \
  -d '{"name":"ops-slack","kind":"slack","secret":"https://hooks.slack.com/…"}'
```

¹ The webhook secret may be a bare URL, or a JSON object to carry an encrypted
auth header alongside the URL:
`{"url":"https://host/hook","auth_header":"Authorization","auth_value":"Bearer xyz"}`.
Provider URLs must be `http`/`https`. When
`security.allow_private_webhook_targets` is `false` (default), targets that are —
or resolve to — loopback/private/link-local/multicast addresses are rejected
(checked both at create time and at connection time).

On `POST`/`PUT`/`PATCH`, a provider's secret is only changed when a non-empty
`secret` is supplied; omit it to keep the stored one (secrets are never readable
back through the API).

Implementing a new provider: satisfy `providers.Provider` and register it in
`providers.NewDefault`.

## Voice calls (VOICE_CALL)

Omni-Notify can deliver a notification as a **real incoming call** on the owner's
iPhone: the phone rings like any call (PushKit + CallKit, locked or not), and when it
is answered, Gobbie's voice reads the message and the owner can answer back:
"repeat that", "tell me more", "okay", "never mind", "give me ten more minutes", "call
me back at five". Voice is a delivery channel, not a severity. Design and decisions:
[docs/superpowers/specs/2026-10-26-voice-call-channel-design.md](docs/superpowers/specs/2026-10-26-voice-call-channel-design.md).
The phone side is the Omni Notify iOS app (`omni-notify-ios`).

How it fits together:

- **Gobbie's voice runs on the phone.** It is the same Kokoro-82M model and sherpa-onnx
  engine as Gobbie (OmniWeather), and recognition is on-device too. No audio crosses the
  network.
- **The server owns the rest:**
  - who may call whom, and the policy (quiet hours, rate limits, retries);
  - the schedule;
  - the call's state machine;
  - the dialogue. Each utterance is a *turn*; the server interprets it, persists any
    action, and only then returns the reply to speak.
- **The calls table is the scheduler.** A scheduled call is claimed by its conditional
  transition to `ringing`, so a restart never rings a call twice, and nothing waits on an
  in-memory timer.

States: `scheduled → ringing → answered → active → completed`, or `declined`,
`missed`, `expired`, `failed`, `cancelled`. Every transition is validated and logged.
The `outcome` (`acknowledged`, `dismissed`, `snoozed`, `rescheduled`) is separate:
answering is not acknowledging. A snooze or a retry is a new call linked by
`parent_call_id`.

### Placing a call

```sh
curl -X POST http://localhost:8080/api/v1/calls \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -d '{
    "recipient": "me",
    "type": "reminder",
    "severity": "info",
    "message": "Wake up. You asked me to remind you to lend me your phone.",
    "user_requested": true,
    "schedule": {"in": "10m"},
    "idempotency_key": "lend-phone-1"
  }'
```

The agent is whoever the credential says. Two credentials work:

- an Omni-Identity `client_credentials` token whose client is listed in
  `voice.agents[].identity_clients`;
- a static token whose SHA-256 is in `token_sha256`.

A request's `agent` field is only a cross-check. With the admin token, `agent` names
who the call is for, and the call is marked **simulated**. `test: true` says "test
call" on screen and aloud. `schedule` takes `at` (RFC 3339 with offset) or `in`. If the
call is held back, the answer explains why:

- `429` with `Retry-After` for the rate limit or cooldown;
- `deferred: "quiet_hours"`, to ring when quiet hours end.

| Method | Path | Who | Purpose |
|---|---|---|---|
| POST | `/api/v1/calls` | agent, admin | call now or later |
| GET | `/api/v1/calls[?state=scheduled]` | agent (own), admin | list calls |
| GET | `/api/v1/calls/{id}` | agent (own), admin, the rung device | call, log / the phone's view |
| POST | `/api/v1/calls/{id}/cancel` | agent (own), admin | cancel a scheduled or ringing call |
| POST | `/api/v1/calls/{id}/state` | the rung device | answered, active, declined, missed, completed, failed |
| POST | `/api/v1/calls/{id}/turns` | the answering device | one utterance in, the reply out |
| POST | `/api/v1/devices/enrollments` | admin | one-time enrollment code for a user |
| POST | `/api/v1/devices/enroll` | the code | enroll a phone, get its credential |
| GET/PUT | `/api/v1/devices/self` | device | refresh the VoIP token; `calls` says what the phone's Calls tab can use |
| GET | `/api/v1/devices/self/ringing` | device | calls waiting for it (development poll transport) |
| GET | `/api/v1/devices/self/calls[?limit=&before=]` | device | the person's calls, newest first (the phone's history) |
| GET | `/api/v1/devices/self/calls/{id}` | device | one of them, with what was said |
| POST | `/api/v1/devices/self/calls` | device | call back about an earlier call (`Idempotency-Key`); answered by that phone |
| GET, PATCH | `/api/v1/devices[/{id}]` | admin | list, disable |

Agent and device credentials reach only these endpoints.

Routed events can ring too: a `voice_call` provider (`config: {recipient, agent}`) in
any route turns the event into a call.

A call can name the on-call incident it is about (`incident: {id, number, title,
severity}` on `POST /api/v1/calls`; a routed page from the on-call plugin carries it from
its `oncall_incident` label). The phone shows it with Acknowledge, and follow-ups keep
it. The phone-side contract is `docs/calls/API.md` in the On-Call app's repository.

### For AI agents (MCP)

`omni-notify mcp` is a stdio MCP server with these tools:

- `omni_notify_call`
- `omni_notify_schedule_call`
- `omni_notify_cancel_call`
- `omni_notify_get_call_status`
- `omni_notify_list_scheduled_calls`

Configure it with `OMNI_NOTIFY_URL`, and either `OMNI_IDENTITY_TOKEN_URL` with
`OMNI_NOTIFY_CLIENT_ID` and `OMNI_NOTIFY_CLIENT_SECRET[_FILE]`, or
`OMNI_NOTIFY_AGENT_TOKEN[_FILE]`. For example, in Claude Code:

```sh
claude mcp add omni-notify -e OMNI_NOTIFY_URL=https://notify.example -e OMNI_NOTIFY_AGENT_TOKEN_FILE=~/.omni-notify-claude.token -- omni-notify mcp
```

`omni-notify call -message "…" [-in 10m | -at …] [-test] [-agent claude]` places one
call from the shell (test mode).

### Metrics

The new counters and histograms:

- `omni_notify_voice_calls_total{origin}`
- `omni_notify_voice_calls_{answered,declined,missed,failed}_total`
- `omni_notify_voice_calls_ended_total{state,outcome}`
- `omni_notify_voice_call_duration_seconds`
- `omni_notify_voice_call_setup_duration_seconds`
- `omni_notify_voice_time_to_answer_seconds`
- `omni_notify_voice_first_audio_latency_seconds`
- `omni_notify_voice_stt_latency_seconds`
- `omni_notify_voice_tts_latency_seconds`
- `omni_notify_voice_turn_latency_seconds`
- `omni_notify_voice_turns_total{intent}`
- `omni_notify_voice_scheduled_calls_total{kind}`
- `omni_notify_voice_scheduling_failures_total{reason}`
- `omni_notify_voice_push_total{result}`
- `omni_notify_voice_deferred_total{reason}`
- `omni_notify_voice_duplicates_suppressed_total`

Audio is never recorded, and transcripts are not stored unless
`voice.store_transcripts` is set.

## Metrics

`omni_notify_events_received_total`, `omni_notify_events_deduplicated_total`,
`omni_notify_notifications_sent_total`, `omni_notify_notifications_failed_total`,
`omni_notify_provider_errors_total`, `omni_notify_active_states`,
`omni_notify_delivery_duration_seconds`.

## Forwarding logs to omni-logging

Omni-Notify can **tee** its own logs to an
[omni-logging](https://github.com/pod32g/omni-logging) ingest endpoint while still
writing them to stdout (so `docker logs` keeps working). Each `slog` record is
shipped as one NDJSON object to `POST /api/v1/ingest`, with `message`, `level`,
`timestamp`, and `service` fields plus any structured attributes (slog groups
flatten to dotted keys, e.g. `http.status`).

Forwarding is **asynchronous and best-effort**: records are buffered and shipped in
batches by a background goroutine. If omni-logging is slow or down, records are
**dropped rather than blocking** request handling, and the shipper's own errors go
to stderr (never back through the log, avoiding a feedback loop).

Enable it under `log.forward`:

```yaml
log:
  level: info
  format: json
  forward:
    enabled: true
    endpoint: "http://192.168.68.34:8080/api/v1/ingest"
    api_key: "${OMNI_LOGGING_API_KEY}"   # X-Api-Key header
    service: "omni-notify"
    # batch_size: 100  buffer_size: 10000  flush_interval: 2s  timeout: 5s
```

**Prerequisite (omni-logging side):** omni-logging only accepts ingest keys that
were configured at *its* startup. Start it with a matching key — e.g.
`OMNILOG_INGEST_KEYS=<key>` (or `--ingest-key <key>`) — and provide the same value
to omni-notify as `OMNI_LOGGING_API_KEY`. The bundled
[`config.docker.yml`](config.docker.yml) and [`docker-compose.yml`](docker-compose.yml)
already wire this env through; just set `OMNI_LOGGING_API_KEY` in your host `.env`.

> The endpoint defaults to the host LAN IP because omni-notify's webhook SSRF guard
> does not apply to the log shipper — it uses its own plain HTTP client so private
> targets like `192.168.68.34` are reachable.

## Configuration

See [`config.example.yaml`](config.example.yaml). `${ENV}` references are resolved
at load time. The `OMNI_NOTIFY_ENCRYPTION_KEY` (base64, 32 bytes) is required
whenever any provider has a secret; the service refuses to start otherwise.

## Development

```sh
make test     # run tests
make vet      # go vet
make cover    # coverage summary
make all      # fmt + vet + test + build
```

## Deployment

The image is a CGO-free, distroless static build. The container self-probes via
the `healthcheck` subcommand (no shell/curl needed).

```sh
make docker        # build omni-notify:latest from the root Dockerfile
make compose-up    # build + run via docker compose (publishes host :8088 -> 8080)
make compose-down
```

Compose mounts [`config.docker.yml`](config.docker.yml) and reads two secrets
from the environment (or a gitignored `.env`):

```sh
export OMNI_NOTIFY_API_TOKEN="$(openssl rand -hex 24)"
export OMNI_NOTIFY_ENCRYPTION_KEY="$(go run ./cmd/omni-notify genkey)"
docker compose up --build -d
curl -fsS http://localhost:8088/healthz
```

Compose maps host port **8088 → container 8080** (the app listens on 8080 inside
the container; the host port differs because 8080 is already taken on the deploy
box). The SQLite database lives in the named volume `omni-notify-data` (mounted at
`/data`), so it survives container recreates.

## CI/CD

[`.github/workflows/cicd.yml`](.github/workflows/cicd.yml) runs on a **self-hosted
runner** and mirrors the omni stack's pipeline:

- **build** (every push / same-repo PR) — `go test ./...` then `docker compose
  build` as a gate. Fork PRs are never built on the self-hosted runner.
- **deploy** (push to `main` only, serialized) — the runner sits on the target
  host: it rsyncs the checkout into `~/omni-notify`, snapshots the data volume to
  `backups/` (keeping the latest 10), recreates the container stop-first (so two
  processes never hold the SQLite WAL at once), waits for `healthcheck` readiness,
  and runs an external smoke test on the published host port (`:8088`).

**Self-hosted runner setup:** register a GitHub Actions runner on the deploy host
with the labels **`self-hosted`** and **`omni-notify`** (matching `runs-on`), with
Docker available to the runner user. Then add two repository secrets:

| Secret | Purpose |
|--------|---------|
| `OMNI_NOTIFY_API_TOKEN`      | bearer token the deployed API accepts |
| `OMNI_NOTIFY_ENCRYPTION_KEY` | base64 32-byte key (`omni-notify genkey`) for secret encryption |

## License

MIT
