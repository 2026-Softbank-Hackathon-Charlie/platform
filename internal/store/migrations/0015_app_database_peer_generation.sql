-- How many times the peer environment of a database pair was removed from
-- its app (ADR 0043). The peer's backups go under a path that has this
-- number, so a peer added back after a removal archives to a new, empty
-- path, where Barman's check that a new database's WAL archive is empty
-- passes. 0 for every pair made before, whose paths stay as they were.

ALTER TABLE app_databases ADD COLUMN peer_generation integer NOT NULL DEFAULT 0;
