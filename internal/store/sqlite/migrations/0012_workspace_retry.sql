ALTER TABLE workspaces ADD COLUMN backoff_streak INTEGER NOT NULL DEFAULT 0
    CHECK (backoff_streak BETWEEN 0 AND 7);

ALTER TABLE workspaces ADD COLUMN expired_job_resumed INTEGER NOT NULL DEFAULT 0
    CHECK (expired_job_resumed IN (0, 1));
