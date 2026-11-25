-- A background task keeps the kind the SDK reports for it (its task_type:
-- local_agent, local_bash, local_workflow, mcp_task, ...), an open set,
-- bounded like an id.
CREATE TABLE session_bgtasks_new (
    session_id  TEXT    NOT NULL REFERENCES sessions (session_id) ON DELETE CASCADE,
    task_id     TEXT    NOT NULL,
    tool_use_id TEXT,
    kind        TEXT    NOT NULL CHECK (kind <> '' AND length(CAST(kind AS BLOB)) <= 128),
    description TEXT,
    epoch       INTEGER NOT NULL,
    started_at  INTEGER NOT NULL,
    PRIMARY KEY (session_id, task_id)
) STRICT, WITHOUT ROWID;
INSERT INTO session_bgtasks_new (session_id, task_id, tool_use_id, kind, description, epoch, started_at)
SELECT session_id, task_id, tool_use_id, kind, description, epoch, started_at FROM session_bgtasks;
DROP TABLE session_bgtasks;
ALTER TABLE session_bgtasks_new RENAME TO session_bgtasks;
