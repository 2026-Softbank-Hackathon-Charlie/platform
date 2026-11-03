-- Apps registered from GitHub, their deployments and the deployments' events,
-- and the GitHub webhook deliveries already handled.
-- No foreign keys, like the other tables: CreateDeployment and
-- AppendDeploymentEvent lock the parent row themselves, and DeleteApp removes
-- an app's deployments and events with it.

CREATE TABLE apps (
    name         text PRIMARY KEY,
    repo         text NOT NULL DEFAULT '',
    branch       text NOT NULL DEFAULT '',
    environments text[] NOT NULL DEFAULT '{}',
    auto_deploy  boolean NOT NULL DEFAULT true,
    -- appspec.Spec JSON of the newest platform.yaml that passed the check.
    spec         jsonb,
    spec_commit  text NOT NULL DEFAULT '',
    created_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
    id           text PRIMARY KEY,
    app          text NOT NULL,
    ref          text NOT NULL DEFAULT '',
    commit_sha   text NOT NULL DEFAULT '',
    trigger      text NOT NULL DEFAULT '',
    triggered_by text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT '',
    images       jsonb NOT NULL DEFAULT '[]',
    environments jsonb NOT NULL DEFAULT '[]',
    error        text NOT NULL DEFAULT '',
    build_id     text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz
);

CREATE INDEX deployments_app_created_at_idx ON deployments (app, created_at DESC);

CREATE TABLE deployment_events (
    seq           bigserial PRIMARY KEY,
    deployment_id text NOT NULL,
    time          timestamptz NOT NULL DEFAULT now(),
    type          text NOT NULL DEFAULT '',
    phase         text NOT NULL DEFAULT '',
    env           text NOT NULL DEFAULT '',
    message       text NOT NULL DEFAULT ''
);

CREATE INDEX deployment_events_deployment_id_seq_idx ON deployment_events (deployment_id, seq);

CREATE TABLE webhook_deliveries (
    id          text PRIMARY KEY,
    received_at timestamptz NOT NULL DEFAULT now()
);
