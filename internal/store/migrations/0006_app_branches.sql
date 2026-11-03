-- Branch instances (ADR 0015). An app deploys its own branch (apps.branch) as
-- the default instance, named after the app, and every branch an operator
-- adds as another instance; app_branches holds the added ones, one row per
-- branch with its instance name and the newest platform.yaml that passed the
-- check on it. Instance names are unique across all apps.
-- deployments records the branch and the instance each deployment deploys.
-- Existing deployments deployed their app's default instance.
-- No foreign keys, like the other tables: CreateDeployment locks the branch
-- row itself, and DeleteAppBranch and DeleteApp remove the deployments and
-- events with it.

CREATE TABLE app_branches (
    app          text NOT NULL,
    branch       text NOT NULL,
    instance     text NOT NULL UNIQUE,
    auto_deploy  boolean NOT NULL DEFAULT true,
    -- appspec.Spec JSON of the newest platform.yaml on the branch that passed the check.
    spec         jsonb,
    spec_commit  text NOT NULL DEFAULT '',
    created_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app, branch)
);

ALTER TABLE deployments
    ADD COLUMN branch text NOT NULL DEFAULT '',
    ADD COLUMN instance text NOT NULL DEFAULT '';

UPDATE deployments d SET instance = d.app, branch = a.branch FROM apps a WHERE a.name = d.app;
-- Deployments whose app is gone, if any.
UPDATE deployments SET instance = app WHERE instance = '';

CREATE INDEX deployments_instance_created_at_idx ON deployments (instance, created_at DESC);
