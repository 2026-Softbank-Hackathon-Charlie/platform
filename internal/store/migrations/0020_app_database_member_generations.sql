-- ADR 0064: the backup path generation of every member of an app's
-- database group, by environment, counted up each time that environment is
-- removed from the app. An environment not in it has generation 0, and
-- peer_env at least peer_generation (ADR 0043), which the platform keeps
-- equal to the generation of peer_env. explicit_members is set once a
-- member left a group of three: the files then keep dbTopology.members
-- with two environments too (infra ADR 0098).
--
-- A binary from before this migration reads neither column. Rolling back
-- to one is safe only before any member was removed from a group of three:
-- it would draw the remaining pair without members (and a peer moved by
-- the removal with peer_generation, not its own generation).
ALTER TABLE app_databases ADD COLUMN member_generations jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE app_databases ADD COLUMN explicit_members boolean NOT NULL DEFAULT false;
