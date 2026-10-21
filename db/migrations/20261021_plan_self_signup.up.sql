-- Self sign-up for free plans. A plan with no Stripe price can let a
-- visitor claim a licence by confirming an email address, instead of
-- an operator creating each one by hand.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS self_signup BOOLEAN NOT NULL DEFAULT false;
-- Most self sign-ups a plan accepts per UTC day; 0 = no cap.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS signup_daily_cap INTEGER NOT NULL DEFAULT 0;
-- Label of the optional marketing-consent checkbox on the sign-up
-- form; empty = the form shows no checkbox.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS signup_consent_text TEXT NOT NULL DEFAULT '';

-- How a licence came to exist when that is not an operator or a
-- payment: 'self_signup' for now. Empty for every other licence.
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS signup_source TEXT NOT NULL DEFAULT '';
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS marketing_consent BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS marketing_consent_at TIMESTAMPTZ;

-- One self sign-up licence per address per product, wherever that
-- licence has moved since: one upgraded to a paid plan keeps its
-- source, so the address cannot come back for a second free one. The
-- handler checks first and answers with the existing licence; this is
-- the backstop for two confirmations racing each other.
CREATE UNIQUE INDEX IF NOT EXISTS licenses_self_signup_product_email
    ON licenses (product_id, lower(email))
    WHERE signup_source = 'self_signup';
