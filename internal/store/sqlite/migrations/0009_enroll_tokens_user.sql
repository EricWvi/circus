CREATE INDEX enroll_tokens_unused_user_expiry ON enroll_tokens (user_id, expires_at) WHERE used_at IS NULL;

-- Retire machine identities from before enrollment had a Web user.
UPDATE devices
SET deleted_at = CAST(unixepoch('subsec') * 1000 AS INTEGER)
WHERE deleted_at IS NULL AND user_id IN (SELECT id FROM users WHERE email IS NULL);

UPDATE workers
SET deleted_at = CAST(unixepoch('subsec') * 1000 AS INTEGER)
WHERE deleted_at IS NULL AND user_id IN (SELECT id FROM users WHERE email IS NULL);

UPDATE credentials
SET revoked_at = CAST(unixepoch('subsec') * 1000 AS INTEGER)
WHERE revoked_at IS NULL AND (
    (principal_kind = 'device' AND principal_id IN (SELECT d.id FROM devices d JOIN users u ON u.id = d.user_id WHERE u.email IS NULL))
    OR (principal_kind = 'worker' AND principal_id IN (SELECT w.id FROM workers w JOIN users u ON u.id = w.user_id WHERE u.email IS NULL))
);

DELETE FROM enroll_tokens
WHERE used_at IS NULL AND user_id IN (SELECT id FROM users WHERE email IS NULL);
