ALTER TABLE onprem_enrollments
    ADD COLUMN session_hash bytea,
    ADD COLUMN session_expires_at timestamptz,
    ADD COLUMN cluster_uid text NOT NULL DEFAULT '',
    ADD COLUMN stage text NOT NULL DEFAULT '',
    ADD COLUMN checks jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN registration_ciphertext bytea,
    ADD COLUMN version bigint NOT NULL DEFAULT 0,
    ADD COLUMN lease_id text NOT NULL DEFAULT '',
    ADD COLUMN lease_expires_at timestamptz,
    ADD CONSTRAINT onprem_registration_ciphertext_bounded CHECK (octet_length(registration_ciphertext) <= 131072),
    ADD CONSTRAINT onprem_registration_session_hash CHECK (session_hash IS NULL OR octet_length(session_hash) = 32),
    ADD CONSTRAINT onprem_registration_checks_bounded CHECK (jsonb_typeof(checks) = 'array' AND octet_length(checks::text) <= 16384);

-- Retained after revocation: revoking credentials never silently transfers an
-- existing cluster or environment name to another enrollment.
CREATE TABLE onprem_environment_bindings (
    environment text PRIMARY KEY,
    enrollment_id text NOT NULL UNIQUE REFERENCES onprem_enrollments(id),
    cluster_uid text NOT NULL,
    created_at timestamptz NOT NULL
);
