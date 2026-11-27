-- The model id Claude Code's fable alias resolves to
-- (ANTHROPIC_DEFAULT_FABLE_MODEL), like the other alias models.
ALTER TABLE providers ADD COLUMN fable_model TEXT NOT NULL DEFAULT ''
    CHECK (length(CAST(fable_model AS BLOB)) <= 256);
