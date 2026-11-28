-- The update of a user's machine (its device and worker identities share
-- machine_id) to a circus release, started from the Web: at most one per
-- machine. A completed update is deleted; a failed one stays, with its
-- error, until it is retried or the machine's last identity is deleted.
-- update_id names the attempt (a retry replaces the row), so a late
-- outcome of an older attempt matches nothing.
CREATE TABLE machine_updates (
    user_id        TEXT    NOT NULL REFERENCES users (id),
    machine_id     TEXT    NOT NULL CHECK (length(CAST(machine_id AS BLOB)) BETWEEN 1 AND 255),
    update_id      TEXT    NOT NULL UNIQUE CHECK (length(CAST(update_id AS BLOB)) BETWEEN 1 AND 128),
    target_version TEXT    NOT NULL CHECK (length(CAST(target_version AS BLOB)) BETWEEN 1 AND 64),
    state          TEXT    NOT NULL CHECK (state IN ('running', 'failed')),
    -- Why a failed update failed; '' while running.
    failure        TEXT    NOT NULL DEFAULT '' CHECK (failure IN ('', 'command', 'timed_out', 'unreachable')),
    error          TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(error AS BLOB)) <= 2048),
    started_at     INTEGER NOT NULL,
    deadline_at    INTEGER NOT NULL,
    CHECK ((state = 'failed') = (failure != '')),
    CHECK (state = 'failed' OR error = ''),
    PRIMARY KEY (user_id, machine_id)
) STRICT;
