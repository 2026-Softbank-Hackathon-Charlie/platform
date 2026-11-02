-- Plan revisions and rollback links.
--
-- revision is 1 at submit and grows by one with every attachment upload, so
-- an approval can name the content it reviewed. rollback_of is the run a
-- rollback plan reverses: a run has at most one rollback plan that is not
-- rejected, so a failed rollback start is retried with the same plan.

ALTER TABLE plans ADD COLUMN revision bigint NOT NULL DEFAULT 1;
ALTER TABLE plans ADD COLUMN rollback_of text NOT NULL DEFAULT '';

-- Plans from before this migration: at least one revision per attachment.
UPDATE plans SET revision = 1 + (SELECT count(*) FROM plan_attachments a WHERE a.ref = plans.ref);

CREATE UNIQUE INDEX plans_rollback_of_idx ON plans (rollback_of) WHERE rollback_of <> '' AND status <> 'rejected';
