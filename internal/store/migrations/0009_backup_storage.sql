-- The buckets and keys made for the apps' Postgres backups (ADR 0025).
--
-- backup_storage holds, per app and environment, the backup bucket and the
-- key the environment's storage driver made for the app's default instance,
-- like instance_storage: only the key sealed with the cluster's
-- sealed-secrets certificate is kept, and the key id. instance is the name
-- the driver made them under, <app>---pg.
-- The table is separate from instance_storage so that the cleanup of
-- deleted instances, in this and in earlier versions of the platform, never
-- sees these rows: a backup bucket outlives the app, and only an operator
-- deletes it (ADR 0025). No foreign keys, like the other tables.

CREATE TABLE backup_storage (
    app               text NOT NULL,
    env               text NOT NULL,
    instance          text NOT NULL,
    driver            text NOT NULL,
    bucket            text NOT NULL,
    key_id            text NOT NULL DEFAULT '',
    old_key_id        text NOT NULL DEFAULT '',
    sealed_access_key text NOT NULL DEFAULT '',
    sealed_secret_key text NOT NULL DEFAULT '',
    cert_fingerprint  text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL,
    PRIMARY KEY (app, env)
);
