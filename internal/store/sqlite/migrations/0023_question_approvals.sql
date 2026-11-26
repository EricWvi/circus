-- Approvals gain a kind. A tool approval is as before. A question approval
-- carries the model's AskUserQuestion input while pending; it has no device,
-- match key or expiry, and ends answered, dismissed, superseded or, when its session
-- stops, expired. At most one question is pending per session, and a
-- tool_use_id asks at most one question.
CREATE TABLE session_approvals_new (
    approval_id TEXT    PRIMARY KEY,
    session_id  TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    user_id     TEXT    NOT NULL, -- the session's, for the per-user list
    kind        TEXT    NOT NULL CHECK (kind IN ('tool', 'question')),
    tool_use_id TEXT,
    agent_id    TEXT,
    tool        TEXT    NOT NULL,
    device_id   TEXT,
    -- SHA-256 of the command or path, 64 lowercase hex digits.
    match_key   TEXT    CHECK (length(match_key) = 64 AND match_key NOT GLOB '*[^0-9a-f]*'),
    display     TEXT    CHECK (length(CAST(display AS BLOB)) <= 1024),
    reason      TEXT    CHECK (length(CAST(reason AS BLOB)) <= 1024),
    input_json  TEXT    CHECK (length(CAST(input_json AS BLOB)) <= 32768),
    state       TEXT    NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'answered', 'dismissed', 'superseded')),
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER,
    resolved_at INTEGER,
    CHECK ((state = 'pending') = (resolved_at IS NULL)),
    CHECK ((kind = 'tool') = (device_id IS NOT NULL)),
    CHECK ((kind = 'tool') = (match_key IS NOT NULL)),
    CHECK ((kind = 'tool') = (expires_at IS NOT NULL)),
    -- The input is dropped once the question is resolved.
    CHECK ((kind = 'question' AND state = 'pending') = (input_json IS NOT NULL)),
    CHECK (kind = 'tool' OR tool_use_id IS NOT NULL),
    CHECK (kind = 'question' OR state IN ('pending', 'approved', 'denied', 'expired')),
    CHECK (kind = 'tool' OR state IN ('pending', 'answered', 'dismissed', 'superseded', 'expired'))
) STRICT;
INSERT INTO session_approvals_new (approval_id, session_id, user_id, kind, tool_use_id, agent_id, tool, device_id,
    match_key, display, reason, state, created_at, expires_at, resolved_at)
SELECT approval_id, session_id, user_id, 'tool', tool_use_id, agent_id, tool, device_id,
    match_key, display, reason, state, created_at, expires_at, resolved_at FROM session_approvals;
DROP TABLE session_approvals;
ALTER TABLE session_approvals_new RENAME TO session_approvals;
-- One pending tool approval per key.
CREATE UNIQUE INDEX session_approvals_pending_key ON session_approvals (session_id, tool, device_id, match_key)
    WHERE state = 'pending' AND kind = 'tool';
-- One pending question per session; one question per tool use.
CREATE UNIQUE INDEX session_approvals_pending_question ON session_approvals (session_id)
    WHERE state = 'pending' AND kind = 'question';
CREATE UNIQUE INDEX session_approvals_question_use ON session_approvals (session_id, tool_use_id)
    WHERE kind = 'question';
CREATE INDEX session_approvals_session_keyset ON session_approvals (session_id, created_at, approval_id);
CREATE INDEX session_approvals_user_keyset ON session_approvals (user_id, created_at, approval_id);
CREATE INDEX session_approvals_expiry ON session_approvals (expires_at) WHERE state = 'pending';
-- Trimming resolved approvals to the newest ones.
CREATE INDEX session_approvals_resolved ON session_approvals (session_id, resolved_at, approval_id) WHERE state <> 'pending';

-- Inputs gain a fourth kind: the user's answers to a question approval.
CREATE TABLE session_inputs_new (
    input_id      TEXT    PRIMARY KEY,
    session_id    TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    client_msg_id TEXT    NOT NULL,
    kind          TEXT    NOT NULL CHECK (kind IN ('user', 'approval', 'note', 'answer')),
    text          TEXT    CHECK (length(CAST(text AS BLOB)) <= 262144),
    priority      TEXT    NOT NULL CHECK (priority IN ('normal', 'now')),
    state         TEXT    NOT NULL CHECK (state IN ('pending', 'taken', 'persisted')),
    taken_epoch   INTEGER,
    created_at    INTEGER NOT NULL,
    taken_at      INTEGER,
    persisted_at  INTEGER,
    event_seq     INTEGER NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    note          TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(note AS BLOB)) <= 64),
    deferred      INTEGER NOT NULL DEFAULT 0 CHECK (deferred IN (0, 1)),
    UNIQUE (session_id, client_msg_id),
    CHECK (state <> 'taken' OR taken_epoch IS NOT NULL),
    CHECK (state <> 'pending' OR taken_epoch IS NULL),
    CHECK ((state = 'persisted') = (persisted_at IS NOT NULL)),
    -- The text is dropped once the transcript holds it.
    CHECK ((state = 'persisted') = (text IS NULL)),
    CHECK ((kind = 'note') = (note <> ''))
) STRICT;
INSERT INTO session_inputs_new (input_id, session_id, client_msg_id, kind, text, priority, state, taken_epoch,
    created_at, taken_at, persisted_at, event_seq, note, deferred)
SELECT input_id, session_id, client_msg_id, kind, text, priority, state, taken_epoch,
    created_at, taken_at, persisted_at, event_seq, note, deferred FROM session_inputs;
DROP TABLE session_inputs;
ALTER TABLE session_inputs_new RENAME TO session_inputs;
CREATE INDEX session_inputs_open ON session_inputs (session_id, created_at, input_id) WHERE state <> 'persisted';
CREATE INDEX session_inputs_persisted ON session_inputs (session_id, persisted_at, input_id) WHERE state = 'persisted';
