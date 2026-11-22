-- Providers: a Web user's model providers. The key and the custom variable
-- values are one sealed blob (secrets) the store never opens; the variable
-- names, their order and the last four characters of each secret are kept
-- in the clear as JSON arrays. Names are unique per user ignoring case;
-- name_key holds the compared form (store.ProjectNameKey). Counts and the
-- other bounds are enforced by the store (store.ProviderOps).
--
-- A provider is deleted outright. Sessions, projects and users keep the id
-- they named (no foreign key), which then reads as a deleted provider.
CREATE TABLE providers (
    id           TEXT    PRIMARY KEY,
    user_id      TEXT    NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    name_key     TEXT    NOT NULL CHECK (length(name_key) = length(name)),
    kind         TEXT    NOT NULL CHECK (kind IN ('claude_code')),
    base_url     TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(base_url AS BLOB)) <= 2048),
    scheme       TEXT    NOT NULL CHECK (scheme IN ('x_api_key', 'bearer')),
    opus_model   TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(opus_model AS BLOB)) <= 256),
    sonnet_model TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(sonnet_model AS BLOB)) <= 256),
    haiku_model  TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(haiku_model AS BLOB)) <= 256),
    max_sessions INTEGER NOT NULL DEFAULT 0 CHECK (max_sessions BETWEEN 0 AND 1024),
    -- Nonce, ciphertext and tag of the key and every variable value.
    secrets      BLOB    NOT NULL CHECK (length(secrets) BETWEEN 1 AND 421888),
    key_last4    TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(key_last4 AS BLOB)) <= 16),
    env_names    TEXT    NOT NULL DEFAULT '[]',
    env_last4    TEXT    NOT NULL DEFAULT '[]',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (user_id, name_key)
) STRICT;

-- The workers a provider may run on, in the order written. Rows of a
-- worker go when the worker is deleted (DeleteIdentity), rows of a
-- provider with it.
CREATE TABLE provider_workers (
    provider_id TEXT    NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL CHECK (position BETWEEN 0 AND 255),
    worker_id   TEXT    NOT NULL REFERENCES workers (id),
    PRIMARY KEY (provider_id, position),
    UNIQUE (provider_id, worker_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX provider_workers_worker ON provider_workers (worker_id);

-- The user's default provider; may name a deleted provider.
ALTER TABLE users ADD COLUMN default_provider_id TEXT;
-- The project's default provider for new sessions; may name a deleted one.
ALTER TABLE projects ADD COLUMN default_provider_id TEXT;
-- The provider the session's CLI starts with; NULL is local login.
ALTER TABLE sessions ADD COLUMN provider_id TEXT;
CREATE INDEX sessions_provider ON sessions (provider_id, state) WHERE provider_id IS NOT NULL;
-- The provider the lease was acquired under, while it has a holder.
ALTER TABLE session_leases ADD COLUMN provider_id TEXT;
CREATE INDEX session_leases_provider ON session_leases (provider_id, expires_at)
    WHERE provider_id IS NOT NULL;
