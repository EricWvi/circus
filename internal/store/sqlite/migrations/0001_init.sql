-- Initial control plane schema (outline 4.1, identities per D43 item 8).
-- Times are Unix milliseconds; NULL means "not set". Token columns hold
-- SHA-256 hashes. No table holds request content or tool output.

CREATE TABLE users (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
) STRICT;

-- devices and workers have the same shape: one identity per machine × role.
CREATE TABLE devices (
    id                   TEXT    PRIMARY KEY,
    user_id              TEXT    NOT NULL REFERENCES users (id),
    name                 TEXT    NOT NULL,
    machine_id           TEXT    NOT NULL,
    os                   TEXT,
    arch                 TEXT,
    hostname             TEXT,
    client_version       TEXT,
    current_boot_id      TEXT,
    last_connected_at    INTEGER,
    last_disconnected_at INTEGER,
    created_at           INTEGER NOT NULL,
    deleted_at           INTEGER,
    -- Target of the (device_id, user_id) keys of calls and jobs: a row can
    -- only name a device of its own user (D14 item 3).
    UNIQUE (id, user_id)
) STRICT;
CREATE UNIQUE INDEX devices_user_name ON devices (user_id, name) WHERE deleted_at IS NULL;
CREATE INDEX devices_machine ON devices (machine_id);

CREATE TABLE workers (
    id                   TEXT    PRIMARY KEY,
    user_id              TEXT    NOT NULL REFERENCES users (id),
    name                 TEXT    NOT NULL,
    machine_id           TEXT    NOT NULL,
    os                   TEXT,
    arch                 TEXT,
    hostname             TEXT,
    client_version       TEXT,
    current_boot_id      TEXT,
    last_connected_at    INTEGER,
    last_disconnected_at INTEGER,
    created_at           INTEGER NOT NULL,
    deleted_at           INTEGER
) STRICT;
CREATE UNIQUE INDEX workers_user_name ON workers (user_id, name) WHERE deleted_at IS NULL;
CREATE INDEX workers_machine ON workers (machine_id);

-- principal_id points into devices or workers, by principal_kind; the store
-- checks it exists on insert (a foreign key cannot span two tables).
CREATE TABLE credentials (
    id             TEXT    PRIMARY KEY,
    principal_kind TEXT    NOT NULL CHECK (principal_kind IN ('device', 'worker')),
    principal_id   TEXT    NOT NULL,
    token_hash     BLOB    NOT NULL UNIQUE,
    created_at     INTEGER NOT NULL,
    last_used_at   INTEGER,
    revoked_at     INTEGER
) STRICT;
CREATE INDEX credentials_principal ON credentials (principal_kind, principal_id);

CREATE TABLE enroll_tokens (
    id         TEXT    PRIMARY KEY,
    user_id    TEXT    NOT NULL REFERENCES users (id),
    token_hash BLOB    NOT NULL UNIQUE,
    kinds      TEXT    NOT NULL CHECK (kinds IN ('device', 'worker', 'device,worker')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER,
    used_by    TEXT
) STRICT;
CREATE INDEX enroll_tokens_unused_expiry ON enroll_tokens (expires_at) WHERE used_at IS NULL;

CREATE TABLE calls (
    call_id             TEXT    PRIMARY KEY,
    user_id             TEXT    NOT NULL REFERENCES users (id),
    device_id           TEXT    NOT NULL,
    session_id          TEXT    NOT NULL,
    agent_id            TEXT,
    tool                TEXT    NOT NULL,
    request_hash        BLOB    NOT NULL,
    request_bytes       INTEGER NOT NULL,
    state               TEXT    NOT NULL CHECK (state IN ('accepted', 'dispatched', 'running', 'unknown',
                                    'completed', 'offline', 'cancelled', 'rejected', 'lost')),
    dispatched_boot_id  TEXT,
    submitted_at        INTEGER NOT NULL,
    dispatched_at       INTEGER,
    acked_at            INTEGER,
    finished_at         INTEGER,
    cancel_requested_at INTEGER,
    exit_code           INTEGER,
    is_error            INTEGER NOT NULL DEFAULT 0 CHECK (is_error IN (0, 1)),
    fault_code          TEXT,
    job_id              TEXT,
    outcome_reason      TEXT,
    -- A call that may be executing on a device always knows which boot.
    CHECK (state NOT IN ('dispatched', 'running', 'unknown') OR dispatched_boot_id IS NOT NULL),
    CHECK (length(CAST(outcome_reason AS BLOB)) <= 256 AND length(CAST(fault_code AS BLOB)) <= 256),
    FOREIGN KEY (device_id, user_id) REFERENCES devices (id, user_id)
) STRICT;
CREATE INDEX calls_device_state ON calls (device_id, state);
CREATE INDEX calls_boot_state ON calls (dispatched_boot_id, state);

-- The boot a call was dispatched to is written once and never changes: a
-- call sent to one boot must never be sent to another (outline 4.4).
CREATE TRIGGER calls_boot_write_once
BEFORE UPDATE OF dispatched_boot_id ON calls
WHEN OLD.dispatched_boot_id IS NOT NULL AND NEW.dispatched_boot_id IS NOT OLD.dispatched_boot_id
BEGIN
    SELECT RAISE(ABORT, 'calls.dispatched_boot_id is write-once');
END;

-- The daemon's short job id is unique within one boot only (D43 item 6).
CREATE TABLE jobs (
    device_id  TEXT    NOT NULL,
    boot_id    TEXT    NOT NULL,
    job_id     TEXT    NOT NULL,
    user_id    TEXT    NOT NULL REFERENCES users (id),
    session_id TEXT,
    call_id    TEXT,
    state      TEXT    NOT NULL CHECK (state IN ('running', 'exited', 'killed', 'lost')),
    exit_code  INTEGER,
    last_seq   INTEGER NOT NULL,
    started_at INTEGER,
    ended_at   INTEGER,
    updated_at INTEGER,
    PRIMARY KEY (device_id, boot_id, job_id),
    FOREIGN KEY (device_id, user_id) REFERENCES devices (id, user_id)
) STRICT;
CREATE INDEX jobs_session ON jobs (session_id);

CREATE TABLE timers (
    kind        TEXT    NOT NULL,
    subject_id  TEXT    NOT NULL,
    due_at      INTEGER NOT NULL,
    claimed_by  TEXT,
    claim_until INTEGER,
    attempts    INTEGER NOT NULL DEFAULT 0,
    claim_token TEXT,
    PRIMARY KEY (kind, subject_id)
) STRICT;
CREATE INDEX timers_due ON timers (due_at);
