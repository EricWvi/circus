-- Keep existing machine ownership and names intact. Legacy owner rows have
-- no email or human role and cannot authenticate to the Web UI.
ALTER TABLE users ADD COLUMN email TEXT CHECK (email IS NULL OR (email = lower(trim(email)) AND length(email) BETWEEN 3 AND 254));
ALTER TABLE users ADD COLUMN role TEXT CHECK (role IN ('user', 'super'));
ALTER TABLE users ADD COLUMN password_hash TEXT;
CREATE UNIQUE INDEX users_email ON users (email) WHERE email IS NOT NULL;
CREATE UNIQUE INDEX users_single_super ON users (role) WHERE role = 'super';

CREATE TABLE user_identities (
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (issuer, subject)
) STRICT;
CREATE INDEX user_identities_user ON user_identities (user_id);

CREATE TABLE web_sessions (
    token_hash BLOB PRIMARY KEY CHECK (length(token_hash) = 32),
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    CHECK (last_used_at >= created_at AND expires_at > last_used_at)
) STRICT;
CREATE INDEX web_sessions_user ON web_sessions (user_id);
CREATE INDEX web_sessions_expiry ON web_sessions (expires_at);
