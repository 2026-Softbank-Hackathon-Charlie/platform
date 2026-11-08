-- ADR 0063: the backup path generation of every member of an app's
-- database group, by environment, counted up each time that environment is
-- removed from the app. An environment not in it has generation 0, but
-- peer_env, whose generation stays peer_generation (ADR 0043); the platform
-- keeps peer_generation equal to the generation of peer_env, so a binary
-- from before this migration draws the same paths.
ALTER TABLE app_databases ADD COLUMN member_generations jsonb NOT NULL DEFAULT '{}'::jsonb;
