-- The database pairs the platform makes for apps registered in exactly two
-- environments (ADR 0030).
--
-- app_databases holds one row per app: the environment where its default
-- instance's database is born primary (the app's first environment) and the
-- other one, where it is born a replica of it; the password of the app
-- account and the CA both clusters' certificates are issued with. The
-- password and the CA key are only stored as AES-256-GCM ciphertext under
-- the platform's secrets key, like app_secrets (ADR 0019): key_version and
-- key_check are those of the key, and the additional data names the app
-- and the field. The CA certificate is public. CreateAppWithDatabase writes
-- the row in the transaction that registers the app; DeleteApp removes it.
-- No foreign keys, like the other tables.

CREATE TABLE app_databases (
    app                 text PRIMARY KEY,
    primary_env         text NOT NULL,
    peer_env            text NOT NULL,
    key_version         integer NOT NULL,
    key_check           text NOT NULL,
    password_nonce      bytea NOT NULL,
    password_ciphertext bytea NOT NULL,
    ca_cert             text NOT NULL,
    ca_key_nonce        bytea NOT NULL,
    ca_key_ciphertext   bytea NOT NULL,
    created_at          timestamptz NOT NULL,
    CHECK (primary_env <> '' AND peer_env <> '' AND primary_env <> peer_env)
);
