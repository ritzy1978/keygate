-- Plans sold per seat (issue #41). On such a plan the buyer picks how
-- many seats to pay for, between min_quantity and max_quantity, and a
-- license holds that many people; its devices and floating sessions
-- are the plan's per-seat values times the seats. Only subscriptions
-- are sold this way. Every existing plan keeps per_seat off and
-- behaves as before.
ALTER TABLE plans ADD COLUMN IF NOT EXISTS per_seat BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS min_quantity INT NOT NULL DEFAULT 1;
ALTER TABLE plans ADD COLUMN IF NOT EXISTS max_quantity INT NOT NULL DEFAULT 100;
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_quantity_range_check;
ALTER TABLE plans ADD CONSTRAINT plans_quantity_range_check
    CHECK (min_quantity >= 1 AND max_quantity >= min_quantity AND max_quantity <= 10000);
ALTER TABLE plans DROP CONSTRAINT IF EXISTS plans_per_seat_subscription_check;
ALTER TABLE plans ADD CONSTRAINT plans_per_seat_subscription_check
    CHECK (NOT per_seat OR license_type = 'subscription');

-- How many seats a license is paid for. For a Stripe subscription it
-- is the quantity Stripe bills; every license so far is one.
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS quantity INT NOT NULL DEFAULT 1;
ALTER TABLE licenses DROP CONSTRAINT IF EXISTS licenses_quantity_check;
-- NOT VALID: every row already holds the default 1, and checking them
-- would scan the table under the lock ADD CONSTRAINT takes.
ALTER TABLE licenses ADD CONSTRAINT licenses_quantity_check CHECK (quantity >= 1) NOT VALID;

-- When Stripe holds a change to the subscription until its invoice is
-- paid, the moment that offer runs out; NULL when nothing waits. A copy
-- of Stripe's pending_update, so the portal can say a payment is due.
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS stripe_pending_update_expires_at TIMESTAMPTZ;

-- While a downgrade the portal made may still be with Stripe, the
-- moment until which the license keeps the lower plan and seats it was
-- given: a read of the subscription that still shows the old ones does
-- not put them back. Cleared by a read that shows the change, or by
-- putting the old ones back when Stripe did not make it; NULL otherwise.
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS terms_held_until TIMESTAMPTZ;
-- When a hold that ran out unsettled is next checked against Stripe,
-- after a check that could not finish; NULL to check at once.
ALTER TABLE licenses ADD COLUMN IF NOT EXISTS terms_check_after TIMESTAMPTZ;
-- The few licences with a hold, found without reading the rest.
CREATE INDEX IF NOT EXISTS licenses_terms_held_idx ON licenses (terms_held_until) WHERE terms_held_until IS NOT NULL;

-- The seats a license had before an upgrade, so undoing the upgrade
-- gives them back.
ALTER TABLE license_upgrades ADD COLUMN IF NOT EXISTS from_quantity INT NOT NULL DEFAULT 1;
