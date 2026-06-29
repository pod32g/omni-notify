# Omni-Notify MVP — Design Spec

**Date:** 2026-06-28
**Status:** Approved (build)

## Goal

Omni-Notify is a **generic event notification service**. External systems decide
when something happened; Omni-Notify only **receives events, routes them,
deduplicates them, stores delivery history, and sends notifications** through
pluggable providers. It does **not** evaluate alert rules, query metrics, run
health checks, or contain business logic.

## Resolved architectural decisions

1. **Config model — hybrid.** A YAML file seeds providers and routes at startup;
   the REST API performs runtime CRUD. **SQLite is the source of truth.** Seeded
   rows are marked `managed_by=config` and re-synced on boot; API-created rows
   (`managed_by=api`) are left untouched.
2. **Secrets — encrypted at rest** in SQLite using AES-256-GCM with a key from
   `OMNI_NOTIFY_ENCRYPTION_KEY` (base64, 32 bytes). Secrets are masked on the API
   and never logged. Startup fails fast if encrypted secrets exist but no/invalid key.
3. **Delivery — async.** `POST /events` validates, dedupes, persists, and returns
   `202`. A DB-backed worker pool sends notifications and retries failures with
   exponential backoff. The `deliveries` table doubles as a durable queue
   (at-least-once delivery, crash recovery).
4. **Scope — full MVP** in one pass.

## Tech choices

- **Go 1.23+** (built on 1.26), module `github.com/pod32g/omni-notify`.
- **Router:** stdlib `net/http` with Go 1.22+ `ServeMux` (method + `{id}` patterns).
- **SQLite:** `modernc.org/sqlite` (pure Go, CGO-free).
- **Deps:** `prometheus/client_golang`, `gopkg.in/yaml.v3`, stdlib `log/slog`.
- **Migrations:** embedded SQL run idempotently on startup.
- **Clock interface** injected wherever time matters (testable windows/backoff).

## Packages

```
cmd/omni-notify      entrypoint + wiring
internal/models      Event, Status, Severity, State, ProviderConfig, Route, Delivery, NotificationMessage
internal/config      YAML load, env-var resolution, validation
internal/storage     SQLite migrations, repositories, secret crypto
internal/dedupe      fingerprint generation + notify/suppress decision engine
internal/router      event -> route matching
internal/notifier    routing orchestration, dispatcher, worker pool, retry/backoff
internal/providers   Provider interface + registry + discord/slack/webhook/smtp
internal/api         HTTP handlers, middleware (auth, size limit, logging), server
internal/metrics     Prometheus collectors
```

## Event model

Required: `event_id`, `type`, `source`, `title`, `timestamp`.
Optional: `status`, `severity`, `summary`, `description`, `labels`,
`annotations`, `fingerprint`.

Statuses: `firing`, `resolved`, `info`, `warning`, `error`.
`firing`/`resolved` are the stateful lifecycle; the rest (and no-status) are stateless.

## Dedup & state engine

Two concepts:
- **Event state** (per `fingerprint`) — drives `GET /states`; latest known status.
- **Notification dedup state** (per `fingerprint` + `route`) — drives notify/suppress
  honoring each route's own `dedup_window` / `repeat_interval`.

**Fingerprint:** use provided `fingerprint`; else `sha256(type | source | event_id | sorted(labels))`.

**Decision rules (per matched route):**
- `firing`: notify if not currently active; if active, suppress unless the route's
  `repeat_interval` has elapsed since `last_notified_at`.
- `resolved`: notify once if it was active, then mark inactive; suppress a resolve
  for something that never fired.
- stateless (`info`/`warning`/`error`/none): notify if the route's `dedup_window`
  has elapsed since last notify (or never notified); else suppress.

## Routing

`match` is a map; supported keys: `type`, `source`, `severity`, `status`, and dotted
`labels.<k>` / `annotations.<k>`. Exact match; a route matches when **all** conditions
hold. Disabled routes skipped. Multiple routes may match; matched routes are evaluated
in `priority` order (descending, then name), each independently for dedup. `is_default`
routes fire only when no non-default route matched, and `stop_processing` halts
evaluation of strictly lower-priority routes.

> **Updated (refinement, 2026-06-28):** providers are de-duplicated **per unique
> provider across all notifying routes** — a provider reached by several matched
> routes receives exactly one delivery, owned by the highest-priority notifying
> route. (Supersedes the earlier per-(route, provider) behavior.)

## Providers

```go
type Provider interface {
    Send(ctx context.Context, msg NotificationMessage) error
}
```

Registry maps `kind -> constructor(config map[string]any, secret string) (Provider, error)`.
MVP kinds:
- **discord** — secret = webhook URL; config: `username`, `avatar_url`.
- **slack** — secret = webhook URL.
- **webhook** — secret = URL (+ optional auth header value); config: `method`, `headers`, `template`.
- **smtp** — secret = password; config: `host`, `port`, `username`, `from`, `to[]`, `tls`.

## Storage schema (SQLite)

- `events` — full event, `labels`/`annotations`/`raw_payload` as JSON; indexed on
  fingerprint, received_at, source, type.
- `states` — `fingerprint` PK: current status/severity/title/labels, first_seen,
  last_seen, active.
- `route_dedup` — (`fingerprint`,`route`) PK: last_notified_at, last_status, active, repeat_count.
- `providers` — name PK, kind, `config` JSON, encrypted secret blob, enabled, managed_by.
- `routes` — name PK, `match` JSON, `providers` JSON, is_default, disabled, dedup_window,
  repeat_interval, managed_by.
- `deliveries` — durable queue + history: id, fingerprint, event_ref, route, provider,
  status (pending|in_progress|success|failed|dead), attempt_count, max_attempts,
  next_attempt_at, last_error, last_duration_ms, timestamps.

## Delivery, retry, workers

Dispatcher polls `deliveries` for `status∈{pending,failed}` with `next_attempt_at ≤ now`,
claims them (`in_progress`), feeds a buffered channel of N workers. Worker calls `Send`
with a timeout. Success → `success` + duration. Error → `attempt_count++`, reschedule
with exponential backoff (`base·factor^n`, capped) until `max_attempts`, then `dead`.
On boot, stale `in_progress` rows reset to `pending`.

## REST API

All under bearer auth except `/healthz` and `/metrics` (metrics unauthenticated by default).

- `POST /api/v1/events` — ingest one event; returns `{fingerprint, deduplicated, routes_matched, deliveries_enqueued}`.
- `GET /api/v1/events` — filters: source/type/severity/status/fingerprint/limit/offset.
- `GET /api/v1/events/{id}`
- `GET /api/v1/states`, `GET /api/v1/states/{fingerprint}`
- `GET /api/v1/providers`, `POST /api/v1/providers` (secrets masked/encrypted)
- `GET /api/v1/routes`, `POST /api/v1/routes`
- `GET /api/v1/deliveries` — read-only delivery history (debugging aid).
- `POST /api/v1/test` — send a synthetic notification to a named provider.
- `GET /healthz`, `GET /metrics`

## Security

Bearer token(s) from env, constant-time compare. `http.MaxBytesReader` size cap
(default 1 MB). Strict payload validation (required fields, status/severity enums,
RFC3339 timestamp). AES-256-GCM secret encryption (fail-fast on key issues).
slog with secret redaction; never log credentials.

## Prometheus metrics

`omni_notify_events_received_total`, `omni_notify_notifications_sent_total`,
`omni_notify_notifications_failed_total`, `omni_notify_active_states` (gauge),
`omni_notify_delivery_duration_seconds` (histogram),
`omni_notify_provider_errors_total`, plus `omni_notify_events_deduplicated_total`.
Low label cardinality (kind/result, not unbounded source).

## Testing

TDD. Unit: dedupe transition table, router matching, fingerprint stability,
config+env resolution, crypto round-trip, provider formatting (httptest for webhooks,
fake SMTP). Integration: ingest→route→deliver via stub provider, auth, size limits,
resolve flow, retry/backoff with injected failures + fake clock.

## Out of scope (per brief)

Alert evaluation, metric querying, health/ping/synthetic checks, business logic,
AI summaries. Future: push/SMS/PagerDuty/Teams/Telegram, maintenance windows,
grouping, escalation, clustering, web UI, RBAC, audit log, replay, dead-letter queue.
