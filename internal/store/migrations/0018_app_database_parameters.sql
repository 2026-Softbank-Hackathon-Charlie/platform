-- ADR 0042: the PostgreSQL parameters an onboarded app's databases are
-- drawn with (values.postgres.parameters): the source's standby-relevant
-- settings, raised to the CNPG floor, so a replica never starts below its
-- source. Empty for every other app.
ALTER TABLE app_databases ADD COLUMN parameters jsonb NOT NULL DEFAULT '{}'::jsonb;
