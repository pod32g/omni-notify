-- Omni-Notify schema. Applied idempotently on startup.

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id    TEXT NOT NULL,
    type        TEXT NOT NULL,
    source      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL,
    summary     TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    labels      TEXT NOT NULL DEFAULT '{}',
    annotations TEXT NOT NULL DEFAULT '{}',
    timestamp   TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    raw_payload TEXT NOT NULL DEFAULT '{}',
    received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_fingerprint ON events(fingerprint);
CREATE INDEX IF NOT EXISTS idx_events_received_at ON events(received_at);
CREATE INDEX IF NOT EXISTS idx_events_source      ON events(source);
CREATE INDEX IF NOT EXISTS idx_events_type        ON events(type);

CREATE TABLE IF NOT EXISTS states (
    fingerprint TEXT PRIMARY KEY,
    event_id    TEXT NOT NULL,
    type        TEXT NOT NULL,
    source      TEXT NOT NULL,
    status      TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT '',
    title       TEXT NOT NULL,
    labels      TEXT NOT NULL DEFAULT '{}',
    active      INTEGER NOT NULL DEFAULT 0,
    first_seen  TEXT NOT NULL,
    last_seen   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_states_active ON states(active);

CREATE TABLE IF NOT EXISTS route_dedup (
    fingerprint      TEXT NOT NULL,
    route            TEXT NOT NULL,
    last_status      TEXT NOT NULL DEFAULT '',
    active           INTEGER NOT NULL DEFAULT 0,
    repeat_count     INTEGER NOT NULL DEFAULT 0,
    last_notified_at TEXT NOT NULL,
    PRIMARY KEY (fingerprint, route)
);

CREATE TABLE IF NOT EXISTS providers (
    name       TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    config     TEXT NOT NULL DEFAULT '{}',
    secret     BLOB,
    enabled    INTEGER NOT NULL DEFAULT 1,
    managed_by TEXT NOT NULL DEFAULT 'api',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS routes (
    name            TEXT PRIMARY KEY,
    match           TEXT NOT NULL DEFAULT '{}',
    providers       TEXT NOT NULL DEFAULT '[]',
    is_default      INTEGER NOT NULL DEFAULT 0,
    disabled        INTEGER NOT NULL DEFAULT 0,
    priority        INTEGER NOT NULL DEFAULT 0,
    stop_processing INTEGER NOT NULL DEFAULT 0,
    dedup_window    INTEGER NOT NULL DEFAULT 0,
    repeat_interval INTEGER NOT NULL DEFAULT 0,
    managed_by      TEXT NOT NULL DEFAULT 'api',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS deliveries (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint      TEXT NOT NULL,
    event_ref        INTEGER NOT NULL,
    route            TEXT NOT NULL,
    provider         TEXT NOT NULL,
    status           TEXT NOT NULL,
    attempt_count    INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL,
    next_attempt_at  TEXT NOT NULL,
    last_error       TEXT NOT NULL DEFAULT '',
    last_duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deliveries_queue       ON deliveries(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_deliveries_fingerprint ON deliveries(fingerprint);
