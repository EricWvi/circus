-- Keyset walks avoid materializing or sorting entire audit tables.
CREATE INDEX calls_keyset ON calls (submitted_at, call_id);
CREATE INDEX calls_user_keyset ON calls (user_id, submitted_at, call_id);
CREATE INDEX timers_keyset ON timers (due_at, kind, subject_id);
