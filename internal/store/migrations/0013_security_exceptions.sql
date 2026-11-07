-- Security exceptions (SEC-08): an operator accepts the risk of one finding or
-- vulnerability until expires_at, unless revoked first.
--
-- An exception is active while revoked_at is NULL and expires_at is in the
-- future. Whether a row is active depends on the clock, so no partial unique
-- index can express "one active exception per target"; the store's single
-- INSERT ... WHERE NOT EXISTS statement does. Rows stay after expiry or revoke
-- for the record. No foreign keys, like the other tables.

CREATE TABLE security_exceptions (
    id          text PRIMARY KEY,
    target_type text NOT NULL,
    target_id   text NOT NULL,
    reason      text NOT NULL,
    created_by  text NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    revoked_by  text,
    CHECK (id <> '' AND target_type IN ('finding', 'vulnerability') AND target_id <> '' AND reason <> ''),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

CREATE INDEX security_exceptions_target ON security_exceptions (target_type, target_id);
