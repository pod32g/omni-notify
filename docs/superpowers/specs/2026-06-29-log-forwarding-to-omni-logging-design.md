# Design: Forward omni-notify logs to omni-logging

**Date:** 2026-06-29
**Status:** Approved (pending spec review)

## Problem

omni-notify and [omni-logging](https://github.com/pod32g/omni-logging) both run via
Docker on the same host (`192.168.68.34`). omni-notify currently logs via Go's
`log/slog` to **stdout only** (text or JSON, see `cmd/omni-notify/main.go:newLogger`).
We want omni-notify's logs to also land in omni-logging so they are centrally
searchable, without losing local `docker logs` visibility and without letting the
logging path ever block, slow, or crash the service.

## omni-logging ingest contract (external, fixed)

- **Endpoint:** `POST /api/v1/ingest`, HTTP only, default port `:8080`.
- **Auth:** `X-Api-Key: <key>` header. Keys are configured **server-side at
  omni-logging startup** via `OMNILOG_INGEST_KEYS` / `--ingest-key` — they are not
  created per-producer through an API.
- **Body:** `Content-Type: application/x-ndjson` — newline-delimited JSON objects.
- **Recognized fields (with aliases):**
  - message: `message` | `msg`
  - level: `level` | `severity` | `lvl` — normalized to `debug/info/warn/error/fatal`
    (also accepts `warning`, `err`, `critical`, syslog 0–7)
  - service: `service` | `logger`
  - source: `source` | `host` | `hostname`
  - timestamp: `timestamp` | `time` | `ts` | `@timestamp` — RFC3339 or unix
    seconds/millis/nanos; **optional**, defaults to receipt time if omitted.
  - Any unrecognized key folds into a searchable `attributes` object
    (filterable as `attr.key=value`).

This maps cleanly onto `slog.Record`: `Message`, `Level`, `Time`, plus attributes.

## Decisions (from brainstorming)

1. **Where:** app-native — a custom `slog.Handler` inside omni-notify (not a sidecar).
2. **Output:** **tee** — keep the existing stdout handler AND fan out to omni-logging,
   so `docker logs omni-notify` still works and a omni-logging outage doesn't blind us.
3. **Networking:** omni-notify reaches omni-logging by **host IP**; the endpoint URL is
   a config value defaulting to `http://192.168.68.34:8080/api/v1/ingest`.

## Architecture

New package **`internal/logship`** with three pieces:

### `Handler` (implements `slog.Handler`)
- `Enabled` defers to the configured min level (same level as stdout).
- `Handle(ctx, rec)` builds a JSON map from the record:
  - `message` ← `rec.Message`
  - `level` ← `rec.Level.String()` lowercased (`debug`/`info`/`warn`/`error`)
  - `timestamp` ← `rec.Time` formatted RFC3339Nano
  - `service` ← configured service name (default `"omni-notify"`)
  - each attr → its own key; **groups flatten to dotted keys** (group `http`, attr
    `status` → `"http.status"`). `WithAttrs`/`WithGroup` accumulate as slog requires.
  - Reserved keys (`message`/`level`/`timestamp`/`service`) take precedence; a
    user attr colliding with one is namespaced (e.g. `attr.level`) so the canonical
    field is never clobbered.
- After building the map, it does a **non-blocking** send onto a buffered channel:
  `select { case ch <- payload: default: atomic drop++ }`. It never blocks the caller.

### `shipper` (background goroutine)
- Drains the channel into a batch slice.
- Flushes when **either** `batch_size` records are buffered **or** `flush_interval`
  elapses (via a ticker), whichever comes first.
- Flush = marshal each payload to a JSON line, join into one NDJSON body, `POST` to
  `endpoint` with `X-Api-Key` and `Content-Type: application/x-ndjson`, using the
  configured `timeout`.
- **Best-effort delivery:** on transport error or non-2xx, retry a small fixed number
  of times (e.g. 2) with short backoff; then **drop the batch** and write one line to
  `os.Stderr`. Delivery must never back-pressure the app.
- **No feedback loop:** the shipper logs its own failures to `os.Stderr` directly,
  never through `slog`.
- **Plain HTTP client:** uses its own `http.Client` (NOT
  `providers.NewGuardedClient`, whose SSRF guard would block the private
  `192.168.68.34` target).
- **Shutdown:** `Shutdown(ctx)` stops accepting, drains the channel, flushes a final
  batch, and returns when done or when `ctx` expires.

### `multiHandler` (tee)
- Small `slog.Handler` holding `[]slog.Handler`. `Enabled` returns true if any child
  is enabled; `Handle` forwards to each enabled child; `WithAttrs`/`WithGroup` return a
  new `multiHandler` whose children each got the call. ~40 lines.

## Config

New `forward` block under the existing `log` section (`internal/config/config.go`):

```yaml
log:
  level: info
  format: json
  forward:
    enabled: true
    endpoint: "http://192.168.68.34:8080/api/v1/ingest"
    api_key: "${OMNI_LOGGING_API_KEY}"   # env-expanded by os.Expand like tokens
    service: "omni-notify"               # default applied if empty
    batch_size: 100
    flush_interval: 2s
    buffer_size: 10000
    timeout: 5s
```

- New struct `ForwardConfig` on `LogConfig`. `endpoint` is a string; durations use the
  existing `models.Duration` type.
- `api_key` is env-expanded automatically — `config.Parse` already runs `os.Expand`
  over the whole file before YAML unmarshal.
- **Defaults** (applied in config when enabled and unset): `service="omni-notify"`,
  `batch_size=100`, `flush_interval=2s`, `buffer_size=10000`, `timeout=5s`.
- **Validation** (when `enabled: true`): `endpoint` and `api_key` must be non-empty;
  `batch_size > 0`, `buffer_size > 0`, `flush_interval > 0`, `timeout > 0`.
- `enabled: false` or block omitted → behavior is byte-for-byte identical to today.

## Wiring (`cmd/omni-notify/main.go`)

- `newLogger` builds the stdout handler as now. When `cfg.Log.Forward.Enabled`, it
  constructs a `logship.Handler` + shipper, wraps `[stdoutHandler, logshipHandler]`
  in `multiHandler`, and returns the `*slog.Logger` plus a shutdown hook.
- Signature changes to return the logger and an optional `func(context.Context) error`
  (nil when forwarding is disabled). `run()` calls the hook in the graceful-shutdown
  block (after HTTP shutdown / delivery drain), bounded by the existing shutdown
  timeout context.

## Deployment touch-ups

- `docker-compose.yml`: add `OMNI_LOGGING_API_KEY: ${OMNI_LOGGING_API_KEY:-}` to the
  service `environment` (mirrors the existing token/key passthrough).
- `config.docker.yml` and `config.example.yaml`: add the documented `forward` block
  (disabled by default in the example, enabled with `${OMNI_LOGGING_API_KEY}` in the
  docker config).
- `README.md`: add a "Forwarding logs to omni-logging" section covering the config,
  the host-IP endpoint, and the **prerequisite** that omni-logging must be started
  with a matching `OMNILOG_INGEST_KEYS` value.

## Testing

`internal/logship` unit tests using `net/http/httptest`:
- field mapping: `message`/`level`/`timestamp`/`service` correct; attrs present;
  groups flattened to dotted keys; reserved-key collision namespaced.
- batching by **size**: N≥batch_size records trigger a POST without waiting.
- batching by **interval**: a partial batch flushes after `flush_interval`.
- request shape: `X-Api-Key` header set, `Content-Type: application/x-ndjson`, body is
  valid NDJSON (one JSON object per line, trailing newline).
- **non-blocking drop**: with `buffer_size` tiny and the server blocked, `Handle`
  returns immediately and excess records are dropped (drop counter increments).
- **graceful drain**: `Shutdown` flushes buffered records before returning.
- `multiHandler` fan-out: a record reaches both a stdout-capturing handler and the
  logship handler.

Config tests: defaults applied when enabled; validation errors when `enabled` but
`endpoint`/`api_key` missing; disabled block leaves logger behavior unchanged.

## Out of scope (YAGNI)

- Exposing the dropped-record count as a Prometheus metric (could be a later
  follow-up; for now stderr is enough).
- Shipping logs from *other* containers on the box (that's the sidecar approach we
  deliberately did not take).
- TLS/compression to the ingest endpoint (plain HTTP on the LAN is sufficient).

## Prerequisite for the user (operational, not code)

Add an ingest key to the omni-logging deployment (e.g.
`OMNILOG_INGEST_KEYS=<key>`), then set the same value as `OMNI_LOGGING_API_KEY` in
omni-notify's host env/`.env` before enabling `forward`.
