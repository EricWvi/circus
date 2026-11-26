-- Agents a worker installs on request. Per (worker, agent): the worker's
-- last report (state NULL until it reports the agent) and the user's wish
-- (wanted NULL until one is recorded; wanted_version set exactly when
-- wanted = 1). A worker's report replaces every reported state it held.
CREATE TABLE worker_agents (
    worker_id         TEXT    NOT NULL REFERENCES workers (id),
    agent             TEXT    NOT NULL CHECK (agent IN ('claude_code')),
    state             TEXT    CHECK (state IN ('not_installed', 'installing', 'installed', 'outdated', 'failed', 'uninstalling')),
    installed_version TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(installed_version AS BLOB)) <= 64),
    target_version    TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(target_version AS BLOB)) <= 64),
    error             TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(error AS BLOB)) <= 1024),
    downloaded_bytes  INTEGER NOT NULL DEFAULT 0 CHECK (downloaded_bytes >= 0),
    total_bytes       INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    reported_at       INTEGER,
    wanted            INTEGER CHECK (wanted IN (0, 1)),
    wanted_version    TEXT    CHECK (length(CAST(wanted_version AS BLOB)) <= 64),
    -- Counts the user's install requests for the agent (SetWorkerAgent),
    -- so the worker can tell a new request from the same wish sent again.
    wanted_attempt    INTEGER NOT NULL DEFAULT 0 CHECK (wanted_attempt >= 0),
    CHECK ((state IS NULL) = (reported_at IS NULL)),
    CHECK ((wanted IS 1) = (wanted_version IS NOT NULL)),
    PRIMARY KEY (worker_id, agent)
) STRICT;

-- When the worker last reported its agents (Attach.agents or
-- ReportAgents); NULL until it has.
ALTER TABLE workers ADD COLUMN agents_reported_at INTEGER;
-- Counts the writes of the worker's agents (reports and wishes), so a
-- reader can tell which of two reads is newer.
ALTER TABLE workers ADD COLUMN agents_rev INTEGER NOT NULL DEFAULT 0;
-- The connection that claimed the reports with its Attach: only it may
-- send later reports (an older connection of the same boot may still
-- deliver one). Connections are ordered by when the control plane made
-- them: agents_conn_start names the control plane's start that made the
-- connection, agents_conn_seq its number within that start; an older
-- connection's late Attach does not take the claim over.
ALTER TABLE workers ADD COLUMN agents_conn_id TEXT;
ALTER TABLE workers ADD COLUMN agents_conn_start TEXT;
ALTER TABLE workers ADD COLUMN agents_conn_seq INTEGER;

-- The agent a session runs; it never changes.
ALTER TABLE sessions ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude_code' CHECK (agent IN ('claude_code'));
