-- Existing sessions have no client request key or recorded location index.
ALTER TABLE sessions ADD COLUMN client_request_id TEXT;
ALTER TABLE sessions ADD COLUMN location_index INTEGER NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX sessions_project_client_request_id
    ON sessions (project_id, client_request_id)
    WHERE client_request_id IS NOT NULL;
