-- Written-records streams (writes a load generator saw succeed, checked by
-- verification) and plan attachments (files a plan refers to, such as the
-- target CNPG Cluster manifest).

CREATE TABLE written_records (
    seq         bigserial PRIMARY KEY,
    stream      text NOT NULL,
    tbl         text NOT NULL,
    pk          text NOT NULL,
    at          timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX written_records_stream_seq_idx ON written_records (stream, seq);

CREATE TABLE plan_attachments (
    ref        text NOT NULL,
    name       text NOT NULL,
    content    bytea NOT NULL,
    sha256     text NOT NULL,
    size       bigint NOT NULL,
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ref, name)
);
