-- 0018 is reserved by the independent on-prem registration change. This
-- migration has no dependency on its schema; migrations are tracked by file name.
CREATE TABLE written_record_collectors (
    stream text PRIMARY KEY,
    generation text NOT NULL,
    scope text NOT NULL CHECK (scope = 'application_commits'),
    coverage text NOT NULL CHECK (coverage = 'reservation_inserts'),
    last_received_at timestamptz NOT NULL
);
-- Deliberately do not backfill activity from old receipts or event timestamps.
