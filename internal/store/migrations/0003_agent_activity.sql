-- The AI agent's activity log: one session per task, one event per tool call,
-- tool result, message, blocked action, decision or error.
-- No foreign key from events to sessions, like run_events: appends check the
-- session row themselves (see AppendAgentEvent).

CREATE TABLE agent_sessions (
    id         text PRIMARY KEY,
    kind       text NOT NULL DEFAULT '',
    title      text NOT NULL DEFAULT '',
    actor      text NOT NULL DEFAULT '',
    plan_ref   text NOT NULL DEFAULT '',
    run_id     text NOT NULL DEFAULT '',
    model      text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT '',
    summary    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    ended_at   timestamptz
);

CREATE INDEX agent_sessions_created_at_idx ON agent_sessions (created_at DESC);
CREATE INDEX agent_sessions_plan_ref_idx ON agent_sessions (plan_ref) WHERE plan_ref <> '';
CREATE INDEX agent_sessions_run_id_idx ON agent_sessions (run_id) WHERE run_id <> '';

CREATE TABLE agent_events (
    seq           bigserial PRIMARY KEY,
    session_id    text NOT NULL,
    time          timestamptz NOT NULL DEFAULT now(),
    type          text NOT NULL DEFAULT '',
    tool          text NOT NULL DEFAULT '',
    input         jsonb,
    output        jsonb,
    status        text NOT NULL DEFAULT '',
    duration_ms   bigint,
    input_tokens  bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0
);

CREATE INDEX agent_events_session_id_seq_idx ON agent_events (session_id, seq);
