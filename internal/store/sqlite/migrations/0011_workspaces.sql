CREATE TABLE workspaces (
    workspace_id       TEXT    PRIMARY KEY,
    user_id            TEXT    NOT NULL,
    project_id         TEXT    NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    device_id          TEXT    NOT NULL,
    state              TEXT    NOT NULL CHECK (state IN ('pending', 'preparing', 'ready', 'failed')),
    waiting            TEXT    CHECK (waiting IN ('device_offline', 'daemon_outdated', 'backoff', 'limit')),
    path               TEXT    CHECK (length(CAST(path AS BLOB)) BETWEEN 1 AND 4096),
    action             TEXT    CHECK (action IN ('created', 'used_empty', 'cloned', 'used_repository')),
    repo_url           TEXT    CHECK (length(CAST(repo_url AS BLOB)) BETWEEN 1 AND 2048),
    attempt            INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    lost_streak        INTEGER NOT NULL DEFAULT 0 CHECK (lost_streak BETWEEN 0 AND 3),
    call_id            TEXT,
    timeout_ms         INTEGER,
    resume_job_id      TEXT,
    job_id             TEXT,
    job_boot_id        TEXT,
    error_code         INTEGER,
    error_detail       TEXT    CHECK (length(CAST(error_detail AS BLOB)) <= 1024),
    retry_at           INTEGER,
    attempt_started_at INTEGER,
    ready_at           INTEGER,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    UNIQUE (project_id, device_id),
    FOREIGN KEY (device_id, user_id) REFERENCES devices (id, user_id),
    CHECK ((state = 'ready') = (path IS NOT NULL) AND (path IS NULL) = (action IS NULL)),
    CHECK ((state = 'preparing') = (call_id IS NOT NULL) AND (call_id IS NULL) = (timeout_ms IS NULL)),
    CHECK (state = 'preparing' OR (resume_job_id IS NULL AND job_id IS NULL AND job_boot_id IS NULL)),
    CHECK ((job_id IS NULL) = (job_boot_id IS NULL)),
    CHECK ((state = 'failed') = (error_code IS NOT NULL)),
    CHECK (state = 'failed' OR error_detail IS NULL),
    CHECK (state = 'pending' OR (waiting IS NULL AND retry_at IS NULL))
) STRICT;

CREATE INDEX workspaces_active ON workspaces (state, updated_at) WHERE state IN ('pending', 'preparing');
CREATE INDEX workspaces_active_device ON workspaces (device_id, state, updated_at, workspace_id)
WHERE state IN ('pending', 'preparing');

ALTER TABLE sessions ADD COLUMN workspace INTEGER NOT NULL DEFAULT 0 CHECK (workspace IN (0, 1));

UPDATE sessions SET workspace = 1 WHERE EXISTS (
    SELECT 1 FROM session_cwds AS c
    WHERE c.session_id = sessions.session_id AND c.device_id = sessions.default_device_id
        AND c.agent_id = '' AND c.cwd = '~/circus/projects/' || sessions.project_id
);

INSERT INTO workspaces (workspace_id, user_id, project_id, device_id, state, created_at, updated_at)
SELECT lower(hex(randomblob(16))), p.user_id, s.project_id, s.default_device_id, 'pending', MIN(s.created_at), MAX(s.updated_at)
FROM sessions s JOIN projects p ON p.id = s.project_id AND p.user_id = s.user_id
WHERE s.workspace = 1
GROUP BY s.project_id, s.default_device_id;

-- Synthetic calls have no session row to delete them on project deletion.
CREATE INDEX calls_workspace_terminal ON calls (session_id, call_id)
WHERE session_id GLOB 'workspace-*' AND call_id GLOB 'ws-*'
    AND tool IN ('prepare_workspace', 'remove_workspace')
    AND state IN ('completed', 'offline', 'cancelled', 'rejected', 'lost');
