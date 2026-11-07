CREATE TABLE onprem_enrollments (
    id text PRIMARY KEY,
    actor text NOT NULL,
    environment text NOT NULL,
    install_k3s boolean NOT NULL,
    code_hash bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    status text NOT NULL CHECK (status IN ('pending','claimed','verifying','ready','failed','expired','revoked')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    UNIQUE (actor, request_hash)
);
