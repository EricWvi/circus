-- Local mode: the session runs on its pinned worker, which shares the
-- default device's machine. pin_derived marks a pin the control plane
-- chose (the creating request named no worker). Existing sessions are
-- not local.
ALTER TABLE sessions ADD COLUMN local INTEGER NOT NULL DEFAULT 0 CHECK (local IN (0, 1));
ALTER TABLE sessions ADD COLUMN pin_derived INTEGER NOT NULL DEFAULT 0 CHECK (pin_derived IN (0, 1));
