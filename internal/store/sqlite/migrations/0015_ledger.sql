CREATE TABLE ledger_heads (
    project_id  TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    head        TEXT NOT NULL,
    next_number INTEGER NOT NULL CHECK (next_number BETWEEN 0 AND 4294967295),
    -- Readers see only the entries and versions of generation. A full
    -- replacement stages its rows under a newer reserved generation.
    generation  INTEGER NOT NULL DEFAULT 0,
    reserved    INTEGER NOT NULL DEFAULT 0,
    -- One more than the highest position among the live versions.
    next_position INTEGER NOT NULL DEFAULT 0,
    CHECK (reserved >= generation)
) STRICT, WITHOUT ROWID;

CREATE TABLE ledger_entries (
    project_id   TEXT NOT NULL REFERENCES ledger_heads(project_id) ON DELETE CASCADE,
    generation   INTEGER NOT NULL,
    number       INTEGER NOT NULL CHECK (number BETWEEN 0 AND 4294967294),
    dir          TEXT NOT NULL,
    title        TEXT NOT NULL,
    path         TEXT NOT NULL,
    last_commit  TEXT NOT NULL,
    committed_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, generation, number)
) STRICT, WITHOUT ROWID;

CREATE INDEX ledger_entries_path ON ledger_entries (project_id, generation, dir, path);

CREATE TABLE ledger_versions (
    project_id      TEXT NOT NULL REFERENCES ledger_heads(project_id) ON DELETE CASCADE,
    generation      INTEGER NOT NULL,
    number          INTEGER NOT NULL CHECK (number BETWEEN 0 AND 4294967294),
    position        INTEGER NOT NULL,
    git_commit      TEXT NOT NULL,
    committed_at    INTEGER NOT NULL,
    author          TEXT NOT NULL,
    summary         TEXT NOT NULL,
    dir             TEXT NOT NULL,
    path            TEXT NOT NULL,
    title           TEXT NOT NULL,
    code_commit     TEXT NOT NULL,
    added           INTEGER NOT NULL,
    content_changed INTEGER NOT NULL,
    moved           INTEGER NOT NULL,
    retitled        INTEGER NOT NULL,
    PRIMARY KEY (project_id, generation, number, position),
    UNIQUE (project_id, generation, number, git_commit)
) STRICT, WITHOUT ROWID;
