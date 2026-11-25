-- Session ledger snapshots: a copy of the project's ledger on a session's
-- device, collected into the session's changeset after every turn.

-- The session whose snapshot a changeset collects; empty for one opened
-- by a worker. A session has at most one open changeset.
ALTER TABLE ledger_changesets ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX ledger_changesets_session_open ON ledger_changesets (session_id)
    WHERE session_id <> '' AND state = 'open';
CREATE INDEX ledger_changesets_session ON ledger_changesets (session_id, created_at, id)
    WHERE session_id <> '';

-- A session's snapshot. The row hangs off its session; deleting the
-- session turns it into a removal (ledger_snapshot_removals) in the same
-- transaction.
CREATE TABLE session_ledgers (
    session_id        TEXT    PRIMARY KEY REFERENCES sessions (session_id) ON DELETE CASCADE,
    user_id           TEXT    NOT NULL,
    project_id        TEXT    NOT NULL,
    device_id         TEXT    NOT NULL,
    state             TEXT    NOT NULL CHECK (state IN ('pending', 'preparing', 'ready', 'failed')),
    path              TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(path AS BLOB)) <= 4096),
    error_code        INTEGER NOT NULL DEFAULT 0 CHECK (error_code >= 0),
    error_detail      TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(error_detail AS BLOB)) <= 1024),
    -- The commit the snapshot's manifest holds ('' for an empty ledger),
    -- and the base of the session's next changeset. They differ only when
    -- a rebase kept changes the agent made after the merge it applied.
    base_commit       TEXT    NOT NULL DEFAULT '',
    view_commit       TEXT    NOT NULL DEFAULT '',
    collected_at      INTEGER NOT NULL DEFAULT 0,
    -- The latest turn end that no successful collection has seen yet; 0
    -- when none.
    due_at            INTEGER NOT NULL DEFAULT 0 CHECK (due_at >= 0),
    -- The last collection's complete change set relative to base_commit
    -- (JSON: path, sha256 or deleted), its problems and warnings (JSON,
    -- bounded) and counts.
    known             TEXT    NOT NULL DEFAULT '[]',
    problems          TEXT    NOT NULL DEFAULT '[]',
    warnings          TEXT    NOT NULL DEFAULT '[]',
    change_count      INTEGER NOT NULL DEFAULT 0 CHECK (change_count >= 0),
    problem_count     INTEGER NOT NULL DEFAULT 0 CHECK (problem_count >= 0),
    -- Hash of the last problem set the agent was told of; '' when none.
    noted_hash        TEXT    NOT NULL DEFAULT '',
    -- Work the snapshot owes after a merge ('rebase') or a discard
    -- ('reset') of the changeset named, with the discard's reason.
    pending           TEXT    NOT NULL DEFAULT '' CHECK (pending IN ('', 'rebase', 'reset')),
    pending_changeset TEXT    NOT NULL DEFAULT '',
    pending_reason    TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(pending_reason AS BLOB)) <= 1024),
    -- The base commit of the last rebase sent whose outcome is not known:
    -- the device's manifest may record it.
    rebase_target     TEXT    NOT NULL DEFAULT '',
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    retry_at          INTEGER NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    CHECK ((pending = '') = (pending_changeset = '')),
    CHECK (state = 'ready' OR path = ''),
    FOREIGN KEY (device_id, user_id) REFERENCES devices (id, user_id)
) STRICT;

-- Rows with work: preparation, a due collection, a rebase or a reset.
CREATE INDEX session_ledgers_work ON session_ledgers (device_id, session_id)
    WHERE state IN ('pending', 'preparing') OR (state = 'ready' AND (due_at > 0 OR pending <> ''));

-- Snapshots of deleted sessions still to remove from their devices.
CREATE TABLE ledger_snapshot_removals (
    session_id TEXT    PRIMARY KEY,
    user_id    TEXT    NOT NULL,
    device_id  TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX ledger_snapshot_removals_device ON ledger_snapshot_removals (device_id, session_id);
CREATE INDEX ledger_snapshot_removals_age ON ledger_snapshot_removals (created_at);

-- Terminal snapshot calls are pruned; they have no session row.
CREATE INDEX calls_ledger_terminal ON calls (session_id, call_id)
WHERE session_id GLOB 'ledger-*' AND call_id GLOB 'lg-*'
    AND state IN ('completed', 'offline', 'cancelled', 'rejected', 'lost');

-- Inputs gain a third kind: a note the control plane writes for the
-- model, with its category.
CREATE TABLE session_inputs_new (
    input_id      TEXT    PRIMARY KEY,
    session_id    TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    client_msg_id TEXT    NOT NULL,
    kind          TEXT    NOT NULL CHECK (kind IN ('user', 'approval', 'note')),
    text          TEXT    CHECK (length(CAST(text AS BLOB)) <= 262144),
    priority      TEXT    NOT NULL CHECK (priority IN ('normal', 'now')),
    state         TEXT    NOT NULL CHECK (state IN ('pending', 'taken', 'persisted')),
    taken_epoch   INTEGER,
    created_at    INTEGER NOT NULL,
    taken_at      INTEGER,
    persisted_at  INTEGER,
    event_seq     INTEGER NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    note          TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(note AS BLOB)) <= 64),
    UNIQUE (session_id, client_msg_id),
    CHECK (state <> 'taken' OR taken_epoch IS NOT NULL),
    CHECK (state <> 'pending' OR taken_epoch IS NULL),
    CHECK ((state = 'persisted') = (persisted_at IS NOT NULL)),
    -- The text is dropped once the transcript holds it.
    CHECK ((state = 'persisted') = (text IS NULL)),
    CHECK ((kind = 'note') = (note <> ''))
) STRICT;
INSERT INTO session_inputs_new (input_id, session_id, client_msg_id, kind, text, priority, state, taken_epoch,
    created_at, taken_at, persisted_at, event_seq)
SELECT input_id, session_id, client_msg_id, kind, text, priority, state, taken_epoch,
    created_at, taken_at, persisted_at, event_seq FROM session_inputs;
DROP TABLE session_inputs;
ALTER TABLE session_inputs_new RENAME TO session_inputs;
CREATE INDEX session_inputs_open ON session_inputs (session_id, created_at, input_id) WHERE state <> 'persisted';
CREATE INDEX session_inputs_persisted ON session_inputs (session_id, persisted_at, input_id) WHERE state = 'persisted';
