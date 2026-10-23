-- Upgrades of a licence Stripe does not bill (a free plan's) to a paid
-- plan through checkout, one row per checkout session. The licence keeps
-- its key; this row is what lets the upgrade be undone.
--
-- state:
--   pending   the checkout is open; nothing has changed yet
--   applied   paid and in effect; the from_* columns hold the licence
--             as the upgrade found it
--   reverted  undone (end_reason says why: a full refund, or the
--             subscription ending), the licence put back as it was
--   expired   the checkout was replaced by a newer one, or ran out,
--             before it was paid
--   refunded  refunded before it could be applied: never applies
--   void      paid, but there was nothing left to apply it to (the
--             subscription had already ended): never applies
--
-- A full refund or the end of the subscription puts the licence back on
-- its free plan instead of revoking or cancelling it: the customer had
-- that licence before paying anything.
CREATE TABLE IF NOT EXISTS license_upgrades (
    id                         TEXT PRIMARY KEY,
    license_id                 TEXT NOT NULL REFERENCES licenses(id) ON DELETE CASCADE,
    -- Nullable so a plan can still be deleted. An upgrade whose free
    -- plan is gone cannot be put back; it is closed and the licence
    -- handled as it was before upgrades existed (see
    -- RevertLicenseUpgradeIn).
    to_plan_id                 TEXT REFERENCES plans(id) ON DELETE SET NULL,
    state                      TEXT NOT NULL DEFAULT 'pending',
    stripe_checkout_session_id TEXT NOT NULL,
    stripe_payment_intent_id   TEXT NOT NULL DEFAULT '',
    stripe_subscription_id     TEXT NOT NULL DEFAULT '',
    from_plan_id               TEXT REFERENCES plans(id) ON DELETE SET NULL,
    from_status                TEXT NOT NULL DEFAULT '',
    from_valid_until           TIMESTAMPTZ,
    from_updates_until         TIMESTAMPTZ,
    from_updates_terms_set     BOOLEAN NOT NULL DEFAULT false,
    from_payment_provider      TEXT NOT NULL DEFAULT '',
    from_stripe_customer_id    TEXT NOT NULL DEFAULT '',
    from_canceled_at           TIMESTAMPTZ,
    end_reason                 TEXT NOT NULL DEFAULT '',
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at                 TIMESTAMPTZ,
    ended_at                   TIMESTAMPTZ,
    CONSTRAINT license_upgrades_state CHECK (state IN ('pending', 'applied', 'reverted', 'expired', 'refunded', 'void'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_session
    ON license_upgrades (stripe_checkout_session_id);
CREATE INDEX IF NOT EXISTS idx_license_upgrades_payment_intent
    ON license_upgrades (stripe_payment_intent_id) WHERE stripe_payment_intent_id <> '';
CREATE INDEX IF NOT EXISTS idx_license_upgrades_subscription
    ON license_upgrades (stripe_subscription_id) WHERE stripe_subscription_id <> '';
CREATE INDEX IF NOT EXISTS idx_license_upgrades_license
    ON license_upgrades (license_id);
-- A licence is upgraded by one payment at a time.
CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_one_applied
    ON license_upgrades (license_id) WHERE state = 'applied';
