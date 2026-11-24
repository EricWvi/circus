-- The integrator grant of a worker: its credential may merge ledger
-- changesets that do not fast-forward and discard ones it did not open.
-- Deleting the worker or revoking its credentials clears it.
ALTER TABLE workers ADD COLUMN integrator INTEGER NOT NULL DEFAULT 0 CHECK (integrator IN (0, 1));

-- A changeset of a project's ledger. Its content lives on the ledger
-- repository's changesets/<id> branch; this row holds its state. A
-- project's history of changesets has no bound, so deleting the project
-- does not cascade to them: its rows are deleted in bounded transactions
-- after the project, and rows of a deleted project are never read.
CREATE TABLE ledger_changesets (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('open', 'merged', 'discarded')),
    -- Main-line commit it started from; empty for an empty ledger.
    base_commit      TEXT NOT NULL,
    opener_worker_id TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    -- The merge commit and its main-line parent, the code commit and the
    -- assigned numbers ("a=12,b=13"). On an open changeset they describe a
    -- merge in progress: the commit is on the main line only if the merge
    -- published it before it was interrupted.
    merge_commit     TEXT NOT NULL DEFAULT '',
    merge_parent     TEXT NOT NULL DEFAULT '',
    code_commit      TEXT NOT NULL DEFAULT '',
    numbers          TEXT NOT NULL DEFAULT '',
    discard_reason   TEXT NOT NULL DEFAULT '',
    -- 0 while a discarded changeset's branch still has to be archived.
    archived         INTEGER NOT NULL DEFAULT 1 CHECK (archived IN (0, 1)),
    CHECK (state <> 'merged' OR merge_commit <> ''),
    CHECK (state <> 'discarded' OR merge_commit = ''),
    CHECK (state = 'discarded' OR (discard_reason = '' AND archived = 1))
) STRICT;

CREATE INDEX ledger_changesets_list ON ledger_changesets (project_id, created_at, id);
CREATE INDEX ledger_changesets_state ON ledger_changesets (project_id, state, created_at, id);
CREATE INDEX ledger_changesets_repair ON ledger_changesets (project_id, id)
    WHERE (state = 'open' AND merge_commit <> '') OR archived = 0;
