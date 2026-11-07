ALTER TABLE written_records ADD COLUMN event_id text NOT NULL DEFAULT '';
ALTER TABLE written_records ADD COLUMN generation text NOT NULL DEFAULT '';
ALTER TABLE written_records ADD COLUMN scope text NOT NULL DEFAULT 'unspecified';
ALTER TABLE written_records ADD COLUMN coverage text NOT NULL DEFAULT 'reported_records';
CREATE UNIQUE INDEX written_records_event_idx ON written_records(stream,event_id) WHERE event_id <> '';
