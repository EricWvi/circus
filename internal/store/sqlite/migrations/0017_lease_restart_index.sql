-- Held leases under a provider in acquisition order, so the extension
-- after a restart walks each provider's expired leases from a cursor.
CREATE INDEX session_leases_provider_age ON session_leases (provider_id, acquired_at, session_id)
    WHERE holder_worker_id IS NOT NULL AND provider_id IS NOT NULL;
