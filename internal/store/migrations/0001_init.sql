-- Plans, runs, run events, the audit log and idempotency keys.
-- No foreign keys: events and audit entries may refer to rows that are written
-- later or never (an engine that fails to start still emits events).

CREATE TABLE plans (
    ref            text PRIMARY KEY,
    plan_id        text NOT NULL DEFAULT '',
    status         text NOT NULL,
    summary        jsonb,
    -- bytea, not jsonb: the plan is kept byte for byte as submitted.
    raw            bytea,
    submitted_by   text NOT NULL DEFAULT '',
    submitted_at   timestamptz NOT NULL DEFAULT now(),
    reviewed_by    text NOT NULL DEFAULT '',
    reviewed_at    timestamptz,
    review_comment text NOT NULL DEFAULT '',
    run_id         text NOT NULL DEFAULT ''
);

CREATE TABLE runs (
    run_id     text PRIMARY KEY,
    plan_ref   text NOT NULL DEFAULT '',
    source_env text NOT NULL DEFAULT '',
    target_env text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT '',
    doc        jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX runs_created_at_idx ON runs (created_at DESC);
CREATE INDEX runs_status_idx ON runs (status);
-- Latest run per source/target pair, for the result matrix.
CREATE INDEX runs_envs_updated_at_idx ON runs (source_env, target_env, updated_at DESC);

CREATE TABLE run_events (
    seq     bigserial PRIMARY KEY,
    run_id  text NOT NULL,
    time    timestamptz NOT NULL DEFAULT now(),
    kind    text NOT NULL DEFAULT '',
    step    text NOT NULL DEFAULT '',
    message text NOT NULL DEFAULT '',
    data    jsonb
);

CREATE INDEX run_events_run_id_seq_idx ON run_events (run_id, seq);

CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    time       timestamptz NOT NULL DEFAULT now(),
    actor      text NOT NULL DEFAULT '',
    role       text NOT NULL DEFAULT '',
    action     text NOT NULL DEFAULT '',
    target     text NOT NULL DEFAULT '',
    status     integer NOT NULL DEFAULT 0,
    request_id text NOT NULL DEFAULT '',
    detail     jsonb
);

CREATE TABLE idempotency_keys (
    key        text PRIMARY KEY,
    plan_ref   text NOT NULL DEFAULT '',
    run_id     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
