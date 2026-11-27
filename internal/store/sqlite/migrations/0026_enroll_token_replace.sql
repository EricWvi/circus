-- A one-time token may be issued to rejoin as an existing identity: the
-- enrollment replaces that identity (revokes it and takes its name).
-- replace_id names it in the table of the token's single kind; the store
-- does not reference it with a foreign key (devices and workers are two
-- tables), and the enrollment checks it is still live and the token user's.
ALTER TABLE enroll_tokens ADD COLUMN replace_id TEXT
    CHECK (replace_id IS NULL OR (kinds IN ('device', 'worker') AND length(replace_id) BETWEEN 1 AND 128));
