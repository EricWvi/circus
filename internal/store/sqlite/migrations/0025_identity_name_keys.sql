-- Device and worker names are unique per user and kind ignoring ASCII case:
-- name_key holds the compared form (store.IdentityNameKey, which equals
-- lower() for every name), and the unique indexes move to it. A database
-- whose live identities already hold two names differing only in case
-- fails here; revoke one of them with an older binary first.
--
-- A column added to a table with rows cannot carry a CHECK that refers to
-- another column's value (the default would fail it), so the triggers keep
-- name_key equal to lower(name) instead.
ALTER TABLE devices ADD COLUMN name_key TEXT NOT NULL DEFAULT '';
UPDATE devices SET name_key = lower(name);
DROP INDEX devices_user_name;
CREATE UNIQUE INDEX devices_user_name ON devices (user_id, name_key) WHERE deleted_at IS NULL;
CREATE TRIGGER devices_name_key_insert BEFORE INSERT ON devices
    WHEN NEW.name_key IS NOT lower(NEW.name)
    BEGIN SELECT RAISE(ABORT, 'devices.name_key must be lower(name)'); END;
CREATE TRIGGER devices_name_key_update BEFORE UPDATE OF name, name_key ON devices
    WHEN NEW.name_key IS NOT lower(NEW.name)
    BEGIN SELECT RAISE(ABORT, 'devices.name_key must be lower(name)'); END;

ALTER TABLE workers ADD COLUMN name_key TEXT NOT NULL DEFAULT '';
UPDATE workers SET name_key = lower(name);
DROP INDEX workers_user_name;
CREATE UNIQUE INDEX workers_user_name ON workers (user_id, name_key) WHERE deleted_at IS NULL;
CREATE TRIGGER workers_name_key_insert BEFORE INSERT ON workers
    WHEN NEW.name_key IS NOT lower(NEW.name)
    BEGIN SELECT RAISE(ABORT, 'workers.name_key must be lower(name)'); END;
CREATE TRIGGER workers_name_key_update BEFORE UPDATE OF name, name_key ON workers
    WHEN NEW.name_key IS NOT lower(NEW.name)
    BEGIN SELECT RAISE(ABORT, 'workers.name_key must be lower(name)'); END;
