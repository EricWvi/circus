-- Existing sessions cannot be assigned a project. Delete all existing session
-- data and every dependent row, including session calls and device jobs, and
-- reset transcript accounting. Users, devices, workers and projects survive.
-- No historical outcome is reclassified; the widened checks also accept stopped.
-- Delete before rebuilding the roots: foreign keys remain enabled throughout
-- the migrator's transaction, and dependent tables keep their original targets.
DELETE FROM calls WHERE session_id IN (SELECT session_id FROM sessions);
DELETE FROM jobs WHERE session_id IN (SELECT session_id FROM sessions);
DELETE FROM sessions;
UPDATE users SET transcript_bytes = 0;
DROP TABLE session_turns;
DROP TABLE sessions;

CREATE TABLE sessions (
    session_id          TEXT    PRIMARY KEY,
    user_id             TEXT    NOT NULL REFERENCES users (id),
    project_id          TEXT    NOT NULL REFERENCES projects (id),
    title               TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(title AS BLOB)) <= 1024),
    last_activity_at    INTEGER NOT NULL,
    claude_session_id   TEXT    NOT NULL UNIQUE,
    state               TEXT    NOT NULL CHECK (state IN ('idle', 'queued', 'running', 'stopped', 'broken')),
    default_device_id   TEXT    NOT NULL,
    model               TEXT,
    permission_mode     TEXT,
    pinned_worker_id    TEXT,
    turn_count          INTEGER NOT NULL DEFAULT 0 CHECK (turn_count >= 0),
    last_turn_outcome   TEXT    CHECK (last_turn_outcome IN ('completed', 'interrupted', 'error', 'stopped')),
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
CREATE INDEX sessions_keyset ON sessions (last_activity_at DESC, session_id DESC);
CREATE INDEX sessions_user_keyset ON sessions (user_id, last_activity_at DESC, session_id DESC);
CREATE INDEX sessions_project_keyset ON sessions (project_id, last_activity_at DESC, session_id DESC);
-- The scheduler's walk over sessions waiting for a worker.
CREATE INDEX sessions_queued ON sessions (created_at, session_id) WHERE state = 'queued';

CREATE TABLE session_turns (
    session_id          TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    turn_no             INTEGER NOT NULL CHECK (turn_no >= 1),
    epoch               INTEGER NOT NULL,
    local_seq           INTEGER NOT NULL CHECK (local_seq >= 1),
    origin              TEXT    NOT NULL CHECK (origin IN ('input', 'notification', 'cli')),
    started_at          INTEGER,
    ended_at            INTEGER,
    outcome             TEXT    CHECK (outcome IN ('completed', 'interrupted', 'error', 'stopped')),
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

