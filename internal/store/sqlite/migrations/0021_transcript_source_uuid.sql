-- An input Claude Code absorbs into a running turn is recorded as a
-- queued_command attachment entry with a uuid of its own; the input's id
-- is the attachment's source_uuid (store.QueuedCommandSource), kept here
-- so a lost lease finds that the CLI got the input.
ALTER TABLE transcript_entries ADD COLUMN source_uuid TEXT CHECK (length(CAST(source_uuid AS BLOB)) <= 128);
CREATE INDEX transcript_entries_source_uuid ON transcript_entries (session_id, source_uuid) WHERE source_uuid IS NOT NULL;
