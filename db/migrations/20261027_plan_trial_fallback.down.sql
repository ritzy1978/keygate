ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_trial_fallback_trial_only;
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_trial_fallback_not_self;
ALTER TABLE plans DROP COLUMN IF EXISTS trial_fallback_plan_id;
