-- Sessions: leases with a monotonic epoch, the transcript, inputs, turns,
-- the conversation view, runtime records, approvals and grants, and the
-- per-call persistence markers. Every table below except sessions and
-- users hangs off a session and is deleted with it (ON DELETE CASCADE);
-- calls rows are deleted by DeleteSession explicitly (no foreign key: M2
-- calls name sessions that never had a row). Bounds are enforced by the
-- store (store.SessionOps); the CHECKs here hold only what a single row can
-- prove.

-- Per-user transcript total, kept in step with sessions.transcript_bytes.
ALTER TABLE users ADD COLUMN transcript_bytes INTEGER NOT NULL DEFAULT 0 CHECK (transcript_bytes >= 0);

-- Target of the (worker, user) key of sessions.pinned_worker_id.
CREATE UNIQUE INDEX workers_id_user ON workers (id, user_id);

CREATE TABLE sessions (
    session_id          TEXT    PRIMARY KEY,
    user_id             TEXT    NOT NULL REFERENCES users (id),
    claude_session_id   TEXT    NOT NULL UNIQUE,
    state               TEXT    NOT NULL CHECK (state IN ('idle', 'queued', 'running', 'stopped', 'broken')),
    default_device_id   TEXT    NOT NULL,
    model               TEXT,
    permission_mode     TEXT,
    pinned_worker_id    TEXT,
    turn_count          INTEGER NOT NULL DEFAULT 0 CHECK (turn_count >= 0),
    last_turn_outcome   TEXT    CHECK (last_turn_outcome IN ('completed', 'interrupted', 'error')),
    -- The highest turn number the user asked to interrupt; 0 if never.
    interrupt_turn_no   INTEGER NOT NULL DEFAULT 0,
    -- The most recent turn was closed without its ENDED report (lease loss,
    -- or a successor's STARTED) and no turn has started since.
    previous_turn_interrupted INTEGER NOT NULL DEFAULT 0 CHECK (previous_turn_interrupted IN (0, 1)),
    -- The turn whose incomplete transcript made the session broken; 0 when
    -- not broken.
    broken_turn_no      INTEGER NOT NULL DEFAULT 0,
    -- A gap the user accepted (RecoverSession) and the model has not been
    -- told of: the turn that broke; 0 if none.
    transcript_gap_turn_no INTEGER NOT NULL DEFAULT 0,
    -- Usage totals over every ended turn; old turn rows are deleted.
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_create_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micro_usd      INTEGER NOT NULL DEFAULT 0,
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    transcript_bytes    INTEGER NOT NULL DEFAULT 0 CHECK (transcript_bytes >= 0),
    view_bytes          INTEGER NOT NULL DEFAULT 0 CHECK (view_bytes >= 0),
    view_truncated      INTEGER NOT NULL DEFAULT 0 CHECK (view_truncated IN (0, 1)),
    last_event_seq      INTEGER NOT NULL DEFAULT 0,
    event_count         INTEGER NOT NULL DEFAULT 0 CHECK (event_count >= 0),
    -- PublishEvents batches: the last sequence consumed in event_batch_epoch,
    -- and the batches dropped for an invalid event.
    event_batch_epoch   INTEGER NOT NULL DEFAULT 0,
    event_batch_seq     INTEGER NOT NULL DEFAULT 0,
    dropped_event_batches INTEGER NOT NULL DEFAULT 0,
    reason              TEXT    CHECK (length(CAST(reason AS BLOB)) <= 256),
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    stopped_at          INTEGER,
    CHECK ((state = 'stopped') = (stopped_at IS NOT NULL)),
    FOREIGN KEY (default_device_id, user_id) REFERENCES devices (id, user_id),
    FOREIGN KEY (pinned_worker_id, user_id) REFERENCES workers (id, user_id)
) STRICT;
CREATE INDEX sessions_keyset ON sessions (created_at, session_id);
CREATE INDEX sessions_user_keyset ON sessions (user_id, created_at, session_id);
-- The scheduler's walk over sessions waiting for a worker.
CREATE INDEX sessions_queued ON sessions (created_at, session_id) WHERE state = 'queued';

-- One row per session, created with it. The epoch only grows; a lease
-- without a holder keeps its epoch for the next acquisition to bump.
CREATE TABLE session_leases (
    session_id       TEXT    PRIMARY KEY REFERENCES sessions (session_id) ON DELETE CASCADE,
    epoch            INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    holder_worker_id TEXT    REFERENCES workers (id),
    holder_boot_id   TEXT,
    -- The connection the lease is bound to; opaque, chosen by the control
    -- plane. Fences name it, so a lease answers one connection at a time.
    holder_conn_id   TEXT,
    acquired_at      INTEGER,
    expires_at       INTEGER,
    -- The holder that last lost the lease at this epoch, so its resent
    -- ReleaseLease is recognized.
    released_worker_id TEXT,
    released_boot_id   TEXT,
    CHECK ((holder_worker_id IS NULL) = (holder_boot_id IS NULL)),
    CHECK ((holder_worker_id IS NULL) = (holder_conn_id IS NULL)),
    CHECK ((holder_worker_id IS NULL) = (expires_at IS NULL)),
    CHECK ((holder_worker_id IS NULL) = (acquired_at IS NULL))
) STRICT;
CREATE INDEX session_leases_holder ON session_leases (holder_worker_id, holder_boot_id, acquired_at)
    WHERE holder_worker_id IS NOT NULL;
CREATE INDEX session_leases_expiry ON session_leases (expires_at) WHERE holder_worker_id IS NOT NULL;

CREATE TRIGGER session_leases_epoch_monotonic
BEFORE UPDATE OF epoch ON session_leases
WHEN NEW.epoch < OLD.epoch
BEGIN
    SELECT RAISE(ABORT, 'session_leases.epoch only grows');
END;

-- Per (session, subpath): the next seq and the batch dedup cursor.
-- last_batch_seq applies to batch_epoch only; a newer epoch starts over.
CREATE TABLE transcript_subpaths (
    session_id     TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    subpath        TEXT    NOT NULL CHECK (length(CAST(subpath AS BLOB)) <= 1024),
    last_seq       INTEGER NOT NULL DEFAULT 0,
    bytes          INTEGER NOT NULL DEFAULT 0,
    batch_epoch    INTEGER NOT NULL DEFAULT 0,
    last_batch_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, subpath)
) STRICT, WITHOUT ROWID;

CREATE TABLE transcript_entries (
    session_id TEXT    NOT NULL,
    subpath    TEXT    NOT NULL,
    seq        INTEGER NOT NULL CHECK (seq >= 1),
    uuid       TEXT    CHECK (length(CAST(uuid AS BLOB)) <= 128),
    entry      BLOB    NOT NULL, -- the SDK's JSON, never parsed
    -- The entry's charge against the caps: JSON, uuid and a fixed 64 B
    -- for the row's keys and index entries.
    bytes      INTEGER NOT NULL CHECK (bytes = length(entry) + COALESCE(length(CAST(uuid AS BLOB)), 0) + 64),
    epoch      INTEGER NOT NULL,
    PRIMARY KEY (session_id, subpath, seq),
    FOREIGN KEY (session_id, subpath) REFERENCES transcript_subpaths (session_id, subpath) ON DELETE CASCADE
) STRICT;
-- Entries are idempotent by uuid within a subpath.
CREATE UNIQUE INDEX transcript_entries_uuid ON transcript_entries (session_id, subpath, uuid) WHERE uuid IS NOT NULL;
-- "Is this input id in the transcript", across subpaths.
CREATE INDEX transcript_entries_session_uuid ON transcript_entries (session_id, uuid) WHERE uuid IS NOT NULL;

CREATE TABLE session_inputs (
    input_id      TEXT    PRIMARY KEY,
    session_id    TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    client_msg_id TEXT    NOT NULL,
    kind          TEXT    NOT NULL CHECK (kind IN ('user', 'approval')),
    text          TEXT    CHECK (length(CAST(text AS BLOB)) <= 262144),
    priority      TEXT    NOT NULL CHECK (priority IN ('normal', 'now')),
    state         TEXT    NOT NULL CHECK (state IN ('pending', 'taken', 'persisted')),
    taken_epoch   INTEGER,
    created_at    INTEGER NOT NULL,
    taken_at      INTEGER,
    persisted_at  INTEGER,
    UNIQUE (session_id, client_msg_id),
    CHECK (state <> 'taken' OR taken_epoch IS NOT NULL),
    CHECK (state <> 'pending' OR taken_epoch IS NULL),
    CHECK ((state = 'persisted') = (persisted_at IS NOT NULL)),
    -- The text is dropped once the transcript holds it.
    CHECK ((state = 'persisted') = (text IS NULL))
) STRICT;
-- Pending and taken inputs in delivery order.
CREATE INDEX session_inputs_open ON session_inputs (session_id, created_at, input_id) WHERE state <> 'persisted';
-- Trimming persisted inputs to the newest ones.
CREATE INDEX session_inputs_persisted ON session_inputs (session_id, persisted_at, input_id) WHERE state = 'persisted';

CREATE TABLE session_turns (
    session_id          TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    turn_no             INTEGER NOT NULL CHECK (turn_no >= 1),
    epoch               INTEGER NOT NULL,
    local_seq           INTEGER NOT NULL CHECK (local_seq >= 1),
    origin              TEXT    NOT NULL CHECK (origin IN ('input', 'notification', 'cli')),
    started_at          INTEGER,
    ended_at            INTEGER,
    outcome             TEXT    CHECK (outcome IN ('completed', 'interrupted', 'error')),
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_create_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micro_usd      INTEGER NOT NULL DEFAULT 0,
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    -- The turn's ENDED report arrived (a turn closed by its successor's
    -- STARTED has an outcome but no report yet).
    ended_reported      INTEGER NOT NULL DEFAULT 0 CHECK (ended_reported IN (0, 1)),
    PRIMARY KEY (session_id, turn_no),
    -- ReportTurn is idempotent by (epoch, local_seq).
    UNIQUE (session_id, epoch, local_seq),
    CHECK ((ended_at IS NULL) = (outcome IS NULL))
) STRICT, WITHOUT ROWID;

CREATE TABLE session_events (
    session_id    TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL CHECK (seq >= 1),
    at            INTEGER NOT NULL,
    kind          TEXT    NOT NULL CHECK (length(CAST(kind AS BLOB)) <= 256),
    format        TEXT,
    payload       BLOB    CHECK (length(payload) <= 262144),
    payload_bytes INTEGER NOT NULL,
    truncated     INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
    PRIMARY KEY (session_id, seq),
    CHECK (truncated = 0 OR payload IS NULL)
) STRICT;

CREATE TABLE session_cwds (
    session_id TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    agent_id   TEXT    NOT NULL, -- '' for the main thread
    device_id  TEXT    NOT NULL,
    cwd        TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, agent_id, device_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE session_files (
    session_id TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    device_id  TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    sha256     BLOB    NOT NULL CHECK (length(sha256) = 32),
    mtime_ms   INTEGER NOT NULL,
    touched_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, device_id, path)
) STRICT;
-- Eviction of the least recently touched record.
CREATE INDEX session_files_touched ON session_files (session_id, touched_at, device_id, path);

-- Device background jobs started by the session, keyed by the starting
-- call. The job's state is read from jobs (device_id, boot_id, job_id).
CREATE TABLE session_jobs (
    session_id  TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    call_id     TEXT    NOT NULL,
    device_id   TEXT    NOT NULL,
    boot_id     TEXT    NOT NULL,
    job_id      TEXT    NOT NULL,
    tool_use_id TEXT,
    description TEXT,
    output_path TEXT,
    notice      TEXT    NOT NULL DEFAULT 'no' CHECK (notice IN ('no', 'handed', 'persisted')),
    notice_at   INTEGER,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (session_id, call_id),
    UNIQUE (device_id, boot_id, job_id)
) STRICT, WITHOUT ROWID;
-- Ended jobs whose notification is not yet handed, and the purge walk.
CREATE INDEX session_jobs_notice ON session_jobs (session_id, notice);
CREATE INDEX session_jobs_persisted ON session_jobs (notice_at) WHERE notice = 'persisted';

-- Background tasks of the CLI itself (asynchronous subagents).
CREATE TABLE session_bgtasks (
    session_id  TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    task_id     TEXT    NOT NULL,
    tool_use_id TEXT,
    kind        TEXT    NOT NULL CHECK (kind IN ('agent')),
    description TEXT,
    epoch       INTEGER NOT NULL,
    started_at  INTEGER NOT NULL,
    PRIMARY KEY (session_id, task_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE session_approvals (
    approval_id TEXT    PRIMARY KEY,
    session_id  TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    user_id     TEXT    NOT NULL, -- the session's, for the per-user list
    tool_use_id TEXT,
    agent_id    TEXT,
    tool        TEXT    NOT NULL,
    device_id   TEXT    NOT NULL,
    -- SHA-256 of the command or path, 64 lowercase hex digits.
    match_key   TEXT    NOT NULL CHECK (length(match_key) = 64 AND match_key NOT GLOB '*[^0-9a-f]*'),
    display     TEXT    CHECK (length(CAST(display AS BLOB)) <= 1024),
    reason      TEXT    CHECK (length(CAST(reason AS BLOB)) <= 1024),
    state       TEXT    NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired')),
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    resolved_at INTEGER,
    CHECK ((state = 'pending') = (resolved_at IS NULL))
) STRICT;
-- One pending approval per key.
CREATE UNIQUE INDEX session_approvals_pending_key ON session_approvals (session_id, tool, device_id, match_key)
    WHERE state = 'pending';
CREATE INDEX session_approvals_session_keyset ON session_approvals (session_id, created_at, approval_id);
CREATE INDEX session_approvals_user_keyset ON session_approvals (user_id, created_at, approval_id);
CREATE INDEX session_approvals_expiry ON session_approvals (expires_at) WHERE state = 'pending';
-- Trimming resolved approvals to the newest ones.
CREATE INDEX session_approvals_resolved ON session_approvals (session_id, resolved_at, approval_id) WHERE state <> 'pending';

CREATE TABLE session_grants (
    grant_id   TEXT    PRIMARY KEY, -- the approval's id
    session_id TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    tool       TEXT    NOT NULL,
    device_id  TEXT    NOT NULL,
    match_key  TEXT    NOT NULL CHECK (length(match_key) = 64 AND match_key NOT GLOB '*[^0-9a-f]*'),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    -- The tool call that consumed the grant; kept until expiry so a resent
    -- ConsumeGrant of that call is answered the same.
    consumed_tool_use_id TEXT,
    consumed_at          INTEGER,
    CHECK ((consumed_tool_use_id IS NULL) = (consumed_at IS NULL))
) STRICT;
CREATE UNIQUE INDEX session_grants_consumer ON session_grants (session_id, consumed_tool_use_id)
    WHERE consumed_tool_use_id IS NOT NULL;
CREATE INDEX session_grants_key ON session_grants (session_id, tool, device_id, match_key, expires_at);
CREATE INDEX session_grants_expiry ON session_grants (expires_at);
CREATE INDEX session_grants_session ON session_grants (session_id, grant_id);

-- Calls: the row is the record of an in-flight call of a session.
ALTER TABLE calls ADD COLUMN tool_use_id TEXT;
ALTER TABLE calls ADD COLUMN returned_at INTEGER;
ALTER TABLE calls ADD COLUMN persisted_at INTEGER;
-- A completion notice for the call is still owed to the model.
ALTER TABLE calls ADD COLUMN notice_owed INTEGER NOT NULL DEFAULT 0 CHECK (notice_owed IN (0, 1));
CREATE INDEX calls_session_keyset ON calls (session_id, submitted_at, call_id);
-- A session's recovery candidates.
CREATE INDEX calls_session_unpersisted ON calls (session_id, submitted_at, call_id) WHERE persisted_at IS NULL;
CREATE INDEX calls_session_recovery ON calls (session_id, submitted_at, call_id) WHERE persisted_at IS NULL OR notice_owed = 1;
CREATE INDEX calls_session_owed ON calls (session_id) WHERE notice_owed = 1;
-- Admission's count of a session's active calls.
CREATE INDEX calls_session_state ON calls (session_id, state);
