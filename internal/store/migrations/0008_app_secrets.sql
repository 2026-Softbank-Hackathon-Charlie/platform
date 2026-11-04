-- App secrets (ADR 0019) and the buckets and keys made for instances
-- (ADR 0016).
--
-- app_secrets holds one row per app, scope and name: instance is '' for an
-- app-wide secret, or the instance whose override it is (the app's name for
-- the default instance). The value is only stored as AES-256-GCM ciphertext
-- under the platform's secrets key, whose version is key_version; the
-- additional data is "<app>/<instance>/<name>". DeleteApp removes an app's
-- rows and DeleteAppBranch the overrides of the branch's instance.
--
-- instance_storage holds, per instance and environment, the bucket and the
-- key the environment's storage driver made. Only the key sealed with the
-- cluster's sealed-secrets certificate is kept, and the key id to delete it.
-- Rows outlive the app and the instance until the driver has deleted the
-- bucket and the key.
-- No foreign keys, like the other tables.

CREATE TABLE app_secrets (
    app         text NOT NULL,
    instance    text NOT NULL DEFAULT '',
    name        text NOT NULL,
    key_version integer NOT NULL,
    nonce       bytea NOT NULL,
    ciphertext  bytea NOT NULL,
    updated_at  timestamptz NOT NULL,
    updated_by  text NOT NULL DEFAULT '',
    PRIMARY KEY (app, instance, name)
);

CREATE TABLE instance_storage (
    instance          text NOT NULL,
    env               text NOT NULL,
    app               text NOT NULL,
    driver            text NOT NULL,
    bucket            text NOT NULL,
    key_id            text NOT NULL DEFAULT '',
    sealed_access_key text NOT NULL DEFAULT '',
    sealed_secret_key text NOT NULL DEFAULT '',
    cert_fingerprint  text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL,
    PRIMARY KEY (instance, env)
);

CREATE INDEX instance_storage_app_idx ON instance_storage (app);
