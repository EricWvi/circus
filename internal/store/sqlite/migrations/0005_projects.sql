-- Projects: a Web user's named set of locations, each a directory on one
-- device, and an optional repository URL. Names are unique per user
-- ignoring case; name_key holds the compared form (store.ProjectNameKey),
-- computed by the store. Bounds on counts and on last_used_location
-- against the locations are enforced by the store (store.ProjectOps).
CREATE TABLE projects (
    id                 TEXT    PRIMARY KEY,
    user_id            TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name               TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    name_key           TEXT    NOT NULL CHECK (length(name_key) = length(name)),
    -- The git remote; NULL when none. Its form is checked by the caller.
    repo_url           TEXT    CHECK (length(CAST(repo_url AS BLOB)) BETWEEN 1 AND 2048),
    -- Index into the project's locations by position: the default for new
    -- sessions; 0 when the project has none.
    last_used_location INTEGER NOT NULL DEFAULT 0 CHECK (last_used_location BETWEEN 0 AND 15),
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    UNIQUE (user_id, name_key)
) STRICT;

-- A location references the device by id, not by name: a name is unique
-- only among live devices, so after a revocation it could be taken by
-- another machine, and the location must not silently move there. The
-- device's owner may differ from the project's user (Web users bind the
-- legacy machine owner's devices), so there is no (device_id, user_id) key.
--
-- An empty path is the device's default directory for the project. The
-- unique (project_id, device_id, path) key therefore also allows at most
-- one default location per device in a project.
CREATE TABLE project_locations (
    project_id TEXT    NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    position   INTEGER NOT NULL CHECK (position BETWEEN 0 AND 15),
    device_id  TEXT    NOT NULL REFERENCES devices (id),
    path       TEXT    NOT NULL CHECK (length(CAST(path AS BLOB)) <= 4096),
    PRIMARY KEY (project_id, position),
    UNIQUE (project_id, device_id, path)
) STRICT, WITHOUT ROWID;
