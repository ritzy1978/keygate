-- The plan a trial licence moves to when its trial ends, instead of
-- expiring (issue #56). Only trial plans carry one, and only a free,
-- non-trial plan of the same product can be it; the admin API checks
-- that, and the expiry sweep checks it again before moving anything.
-- Deleting the plan it points at clears it, so those trials simply
-- expire again as they did before.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS trial_fallback_plan_id TEXT
    REFERENCES plans(id) ON DELETE SET NULL;
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_trial_fallback_not_self;
ALTER TABLE plans ADD CONSTRAINT plans_trial_fallback_not_self
    CHECK (trial_fallback_plan_id IS NULL OR trial_fallback_plan_id <> id);
-- Only a trial plan has trials to end. The admin API clears the field
-- when a plan leaves the trial type; this holds it when two edits race
-- (one setting a fallback, one changing the type).
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_trial_fallback_trial_only;
ALTER TABLE plans ADD CONSTRAINT plans_trial_fallback_trial_only
    CHECK (trial_fallback_plan_id IS NULL OR license_type = 'trial');
