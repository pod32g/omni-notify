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

## REST API

All `/api/v1/*` endpoints require `Authorization: Bearer <token>`.
`/healthz` is always open; `/metrics` is open unless `metrics_require_auth: true`.

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

| Kind      | Secret           | Key config                                          |
|-----------|------------------|-----------------------------------------------------|
| `discord` | webhook URL      | `username`, `avatar_url`                             |
| `slack`   | webhook URL      | `username`, `icon_emoji`                             |
| `webhook` | target URL¹      | `method`, `content_type`, `headers`, `template`     |
| `smtp`    | password         | `host`, `port`, `username`, `from`, `to[]`, `tls`, `subject_template` |

Add a provider via the API (secret is encrypted before storage):

```sh
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

## Metrics

`omni_notify_events_received_total`, `omni_notify_events_deduplicated_total`,
`omni_notify_notifications_sent_total`, `omni_notify_notifications_failed_total`,
`omni_notify_provider_errors_total`, `omni_notify_active_states`,
`omni_notify_delivery_duration_seconds`.

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

```sh
make docker   # builds omni-notify:latest from deploy/Dockerfile (CGO-free, distroless)
```

## License

MIT
