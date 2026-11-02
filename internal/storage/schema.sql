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

-- Voice calls (VOICE_CALL channel). The table is also the voice scheduler: a
-- 'scheduled' row whose scheduled_at has passed is claimed by its transition to
-- 'ringing'. Optional timestamps are '' when unset.
CREATE TABLE IF NOT EXISTS voice_calls (
    id               TEXT PRIMARY KEY,
    event_ref        INTEGER NOT NULL DEFAULT 0,
    agent            TEXT NOT NULL,
    caller_name      TEXT NOT NULL,
    simulated        INTEGER NOT NULL DEFAULT 0,
    test             INTEGER NOT NULL DEFAULT 0,
    recipient        TEXT NOT NULL,
    type             TEXT NOT NULL,
    severity         TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL DEFAULT '',
    message          TEXT NOT NULL,
    details          TEXT NOT NULL DEFAULT '',
    interactive      INTEGER NOT NULL DEFAULT 1,
    user_requested   INTEGER NOT NULL DEFAULT 0,
    idempotency_key  TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL,
    outcome          TEXT NOT NULL DEFAULT '',
    reason           TEXT NOT NULL DEFAULT '',
    attempt          INTEGER NOT NULL DEFAULT 1,
    parent_call_id   TEXT NOT NULL DEFAULT '',
    origin           TEXT NOT NULL DEFAULT 'api',
    scheduled_at     TEXT NOT NULL,
    expires_at       TEXT NOT NULL,
    ring_deadline    TEXT NOT NULL DEFAULT '',
    rang_at          TEXT NOT NULL DEFAULT '',
    answered_at      TEXT NOT NULL DEFAULT '',
    answered_by      TEXT NOT NULL DEFAULT '',
    ended_at         TEXT NOT NULL DEFAULT '',
    last_activity_at TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    incident         TEXT NOT NULL DEFAULT '' -- JSON models.CallIncident, '' for none
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_voice_calls_idem
    ON voice_calls(agent, idempotency_key) WHERE idempotency_key <> '';
CREATE INDEX IF NOT EXISTS idx_voice_calls_due       ON voice_calls(state, scheduled_at);
CREATE INDEX IF NOT EXISTS idx_voice_calls_recipient ON voice_calls(recipient, state);

CREATE TABLE IF NOT EXISTS voice_call_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    call_id    TEXT NOT NULL,
    at         TEXT NOT NULL,
    kind       TEXT NOT NULL,
    from_state TEXT NOT NULL DEFAULT '',
    to_state   TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_voice_call_log_call ON voice_call_log(call_id);

-- Which devices a call rang; a device may only see and act on calls it was rung for.
CREATE TABLE IF NOT EXISTS voice_call_rings (
    call_id   TEXT NOT NULL,
    device_id TEXT NOT NULL,
    at        TEXT NOT NULL,
    status    TEXT NOT NULL,
    error     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (call_id, device_id)
);

-- One row per spoken turn, keyed by the device's sequence number so a retried
-- request replays the stored reply instead of acting twice. The transcript is
-- not stored unless voice.store_transcripts is set.
CREATE TABLE IF NOT EXISTS voice_turns (
    call_id    TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    device_id  TEXT NOT NULL,
    at         TEXT NOT NULL,
    intent     TEXT NOT NULL,
    transcript TEXT NOT NULL DEFAULT '',
    response   TEXT NOT NULL,
    PRIMARY KEY (call_id, seq)
);

CREATE TABLE IF NOT EXISTS voice_devices (
    id               TEXT PRIMARY KEY,
    user             TEXT NOT NULL,
    name             TEXT NOT NULL,
    platform         TEXT NOT NULL DEFAULT 'ios',
    bundle_id        TEXT NOT NULL DEFAULT '',
    apns_environment TEXT NOT NULL DEFAULT 'development',
    voip_token       TEXT NOT NULL DEFAULT '',
    secret_hash      TEXT NOT NULL UNIQUE,
    disabled         INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_voice_devices_user ON voice_devices(user);

CREATE TABLE IF NOT EXISTS voice_enrollments (
    code_hash  TEXT PRIMARY KEY,
    user       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT NOT NULL DEFAULT '',
    device_id  TEXT NOT NULL DEFAULT ''
);
