-- The role a plan was submitted with (platform #24).
--
-- An agent submits AI plans; approving one needs a passing rehearsal report
-- that matches the plan as it is now (plans.requireRehearsal). The role comes
-- from the authenticated caller, never from the request body. Plans from
-- before this migration keep '' (unknown).

ALTER TABLE plans ADD COLUMN submitted_role text NOT NULL DEFAULT '';
