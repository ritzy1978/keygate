-- Self sign-up for free plans. A plan with no Stripe price can let a
-- visitor claim a licence by confirming an email address, instead of
-- an operator creating each one by hand.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS self_signup BOOLEAN NOT NULL DEFAULT false;
-- Most self sign-ups a plan accepts per UTC day; 0 = no cap.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS signup_daily_cap INTEGER NOT NULL DEFAULT 0;
-- Label of the optional marketing-consent checkbox on the sign-up
-- form; empty = the form shows no checkbox.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS signup_consent_text TEXT NOT NULL DEFAULT '';

-- A self sign-up plan is free and sells no update period. The admin
-- handlers check this before writing, but against the plan as they
-- read it: two edits racing (one turning sign-up on, one adding a
-- price) would each pass and leave the pair. The constraint judges
-- the row as it is written.
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_self_signup_free;
ALTER TABLE plans ADD CONSTRAINT plans_self_signup_free CHECK (
    NOT self_signup OR (
        coalesce(stripe_price_id, '') = '' AND updates_days = 0 AND
        renewal_days = 0 AND stripe_renewal_price_id = ''
    )
);

-- How a licence came to exist when that is not an operator or a
-- payment: 'self_signup' for now. Empty for every other licence.
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS signup_source TEXT NOT NULL DEFAULT '';
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS marketing_consent BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS marketing_consent_at TIMESTAMPTZ;
-- The plan a self sign-up licence was claimed on. plan_id follows the
-- licence when it moves; the daily cap counts claims, and a claim
-- moved to another plan the same day still took its place.
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS signup_plan_id TEXT;
CREATE INDEX IF NOT EXISTS licenses_self_signup_plan_day
    ON licenses (signup_plan_id, created_at)
    WHERE signup_source = 'self_signup';

-- One self sign-up licence per address per product, wherever that
-- licence has moved since: one upgraded to a paid plan keeps its
-- source, so the address cannot come back for a second free one. The
-- handler checks first and answers with the existing licence; this is
-- the backstop for two confirmations racing each other.
CREATE UNIQUE INDEX IF NOT EXISTS licenses_self_signup_product_email
    ON licenses (product_id, lower(email))
    WHERE signup_source = 'self_signup';
