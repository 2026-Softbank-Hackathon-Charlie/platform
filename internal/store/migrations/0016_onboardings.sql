-- Onboarding apps that ran outside the platform with docker compose on one
-- VM (ADR 0042).
--
-- onboardings holds one row per onboarding. state is its state
-- (apiv1.OnboardingState); doc is the full apiv1.Onboarding JSON object,
-- whose id, app, repo and state every write sets from the columns, which
-- are the source of truth; facts is the discovery's pkg/onboard/facts.Facts
-- JSON, which never holds a secret value. revision is 1 at creation and
-- grows with every update, which applies only to the revision it read. At
-- most one onboarding per app is not done (onboardings_app_active), so a
-- failed one is resumed or deleted before the app is onboarded again.
--
-- The values discovery and connecting got from the VM are only stored as
-- AES-256-GCM ciphertext under the platform's secrets key, like app_secrets
-- (ADR 0019): key_version and key_check are those of the key (0 and '' while
-- nothing is sealed), and the additional data is ("onboardings", id, field,
-- "") with field "secrets" (a JSON object of the containers' environment
-- values, {"<container>": {"<NAME>": "<value>"}}), "superuser.password" (the
-- fence user's) or "replication.password" (hopcharlie_replicator's). Every
-- pair is NULL until it is known.
--
-- onboarding_events is an onboarding's step log (GET
-- /v1/onboardings/{id}/events); seq increases across all onboardings, and
-- only an onboarding's newest 2000 are kept.
-- DeleteOnboarding removes an onboarding's events with it.
--
-- app_databases gets what an onboarded app's database needs:
--   source_kind   'peer' (ADR 0030) or 'external': born a replica of the
--                 source outside the platform, until the cutover succeeded
--   image         the CNPG image, pinned by digest, of every database of the
--                 app; '' is the operator's default
--   database,     the database name and its owner; '' is app
--   owner
--   storage_size  the PVC size ('2Gi'); '' is the chart's default
--   external_host the tailnet hostname of the source (hop-db-legacy-<app>)
--   replication_password_nonce, replication_password_ciphertext
--                 hopcharlie_replicator's password, sealed like the app
--                 account's: additional data ("app_databases", app,
--                 "replication.password", ""); NULL without a source
--   reindex       the "schema.index" names rebuilt after the promotion
-- external_host, the password and reindex stay after the cutover: the
-- target's externalClusters entry legacy still names the password's Secret.
-- No foreign keys, like the other tables.

CREATE TABLE onboardings (
    id                              text PRIMARY KEY,
    app                             text NOT NULL,
    repo                            text NOT NULL,
    state                           text NOT NULL,
    revision                        bigint NOT NULL DEFAULT 1,
    doc                             jsonb NOT NULL,
    facts                           jsonb,
    key_version                     integer NOT NULL DEFAULT 0,
    key_check                       text NOT NULL DEFAULT '',
    secrets_nonce                   bytea,
    secrets_ciphertext              bytea,
    superuser_password_nonce        bytea,
    superuser_password_ciphertext   bytea,
    replication_password_nonce      bytea,
    replication_password_ciphertext bytea,
    created_at                      timestamptz NOT NULL,
    updated_at                      timestamptz NOT NULL,
    CHECK (id <> '' AND app <> '' AND repo <> '' AND state <> '')
);

CREATE UNIQUE INDEX onboardings_app_active ON onboardings (app) WHERE state <> 'done';

CREATE TABLE onboarding_events (
    seq           bigserial PRIMARY KEY,
    onboarding_id text NOT NULL,
    time          timestamptz NOT NULL,
    state         text NOT NULL,
    kind          text NOT NULL,
    message       text NOT NULL DEFAULT '',
    data          jsonb
);

CREATE INDEX onboarding_events_onboarding_idx ON onboarding_events (onboarding_id, seq);

ALTER TABLE app_databases
    ADD COLUMN source_kind text NOT NULL DEFAULT 'peer' CHECK (source_kind IN ('peer', 'external')),
    ADD COLUMN image text NOT NULL DEFAULT '',
    ADD COLUMN database text NOT NULL DEFAULT '',
    ADD COLUMN owner text NOT NULL DEFAULT '',
    ADD COLUMN storage_size text NOT NULL DEFAULT '',
    ADD COLUMN external_host text NOT NULL DEFAULT '',
    ADD COLUMN replication_password_nonce bytea,
    ADD COLUMN replication_password_ciphertext bytea,
    ADD COLUMN reindex text[] NOT NULL DEFAULT '{}';
