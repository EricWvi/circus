-- The seq of the USER view event the control plane records with a new user
-- input, so a repeated SendMessage answers the same event_seq. 0 when none.
ALTER TABLE session_inputs ADD COLUMN event_seq INTEGER NOT NULL DEFAULT 0 CHECK (event_seq >= 0);
