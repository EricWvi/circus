-- A note input may be deferred: it starts no turn and waits, pending, for
-- the next input that starts one, which takes it along (store.Input).
ALTER TABLE session_inputs ADD COLUMN deferred INTEGER NOT NULL DEFAULT 0 CHECK (deferred IN (0, 1));
