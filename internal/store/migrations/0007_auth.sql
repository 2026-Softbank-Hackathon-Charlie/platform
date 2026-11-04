-- Administrator login (ADR 0018): console sessions and CLI tokens, and the
-- device logins a CLI waits on until an administrator approves it in the
-- console. Only the SHA-256 of a token or device code is stored.
-- Expired rows are removed when the next session or device login is created;
-- reads compare expires_at with the current time, so they never see one.

CREATE TABLE auth_sessions (
    token_hash bytea PRIMARY KEY,
    name       text NOT NULL,
    role       text NOT NULL,
    -- console or cli
    kind       text NOT NULL,
    -- the CLI's client string; empty for the console
    client     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE INDEX auth_sessions_expires_at_idx ON auth_sessions (expires_at);

CREATE TABLE device_logins (
    device_code_hash bytea PRIMARY KEY,
    -- eight letters without the dash; unique while the row exists
    user_code        text NOT NULL UNIQUE,
    client           text NOT NULL,
    -- pending, approved or denied
    status           text NOT NULL,
    created_at       timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    reviewed_by      text NOT NULL DEFAULT '',
    reviewed_at      timestamptz,
    -- the role the approver picked for the CLI token; empty unless approved
    token_role       text NOT NULL DEFAULT ''
);

CREATE INDEX device_logins_expires_at_idx ON device_logins (expires_at);
