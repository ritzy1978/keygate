package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

// NewLicenseUpgradeID returns an id for an upgrade row. The checkout
// carries it in its metadata, so it exists before the row does.
func NewLicenseUpgradeID() string { return newID() }

// CreateLicenseUpgradeIn records a pending upgrade for a checkout
// session just created.
func CreateLicenseUpgradeIn(ctx context.Context, db bun.IDB, u *model.LicenseUpgrade) error {
	if u.ID == "" {
		u.ID = newID()
	}
	if u.State == "" {
		u.State = model.UpgradePending
	}
	q := db.NewInsert().Model(u)
	if u.CreatedAt.IsZero() {
		// When it is written, not when its transaction began: invites
		// are held up from here (see ErrUpgradeCheckoutOpen), and the
		// checkout was opened just before.
		q = q.Value("created_at", "clock_timestamp()")
	}
	_, err := q.Returning("created_at").Exec(ctx)
	return err
}

// FindLicenseUpgradeBySession returns the upgrade a checkout session
// belongs to, or sql.ErrNoRows.
func (s *Store) FindLicenseUpgradeBySession(ctx context.Context, sessionID string) (*model.LicenseUpgrade, error) {
	return findLicenseUpgrade(ctx, s.DB, false, "stripe_checkout_session_id = ?", sessionID)
}

// LockLicenseUpgradeIn reads an upgrade by id and locks its row.
func LockLicenseUpgradeIn(ctx context.Context, db bun.IDB, id string) (*model.LicenseUpgrade, error) {
	return findLicenseUpgrade(ctx, db, true, "id = ?", id)
}

// LockLicenseUpgradeBySessionIn reads the upgrade a session belongs to
// and locks its row.
func LockLicenseUpgradeBySessionIn(ctx context.Context, db bun.IDB, sessionID string) (*model.LicenseUpgrade, error) {
	return findLicenseUpgrade(ctx, db, true, "stripe_checkout_session_id = ?", sessionID)
}

// FindAppliedUpgrade returns the upgrade in effect on a licence, or
// sql.ErrNoRows.
func (s *Store) FindAppliedUpgrade(ctx context.Context, licenseID string) (*model.LicenseUpgrade, error) {
	return findLicenseUpgrade(ctx, s.DB, false, "license_id = ? AND state = '"+model.UpgradeApplied+"'", licenseID)
}

// FindLicenseUpgradeByPaymentIntent returns the upgrade a one-time
// payment paid for, or sql.ErrNoRows.
func (s *Store) FindLicenseUpgradeByPaymentIntent(ctx context.Context, paymentIntentID string) (*model.LicenseUpgrade, error) {
	return findLicenseUpgrade(ctx, s.DB, false, "stripe_payment_intent_id = ?", paymentIntentID)
}

// FindLicenseUpgradeBySubscription returns the newest upgrade that
// started a subscription, or sql.ErrNoRows.
func (s *Store) FindLicenseUpgradeBySubscription(ctx context.Context, subscriptionID string) (*model.LicenseUpgrade, error) {
	return findLicenseUpgrade(ctx, s.DB, false, "stripe_subscription_id = ?", subscriptionID)
}

// FindLicenseUpgradeByID returns an upgrade by id, or sql.ErrNoRows.
func (s *Store) FindLicenseUpgradeByID(ctx context.Context, id string) (*model.LicenseUpgrade, error) {
	return findLicenseUpgrade(ctx, s.DB, false, "id = ?", id)
}

func findLicenseUpgrade(ctx context.Context, db bun.IDB, lock bool, where string, arg string) (*model.LicenseUpgrade, error) {
	u := new(model.LicenseUpgrade)
	q := db.NewSelect().Model(u).Where(where, arg).OrderExpr("created_at DESC").Limit(1)
	if lock {
		q = q.For("UPDATE")
	}
	return u, q.Scan(ctx)
}

// ListPendingUpgradesIn returns a licence's upgrades whose checkout may
// still be open, locked, oldest first.
func ListPendingUpgradesIn(ctx context.Context, db bun.IDB, licenseID string) ([]*model.LicenseUpgrade, error) {
	var out []*model.LicenseUpgrade
	err := db.NewSelect().Model(&out).
		Where("license_id = ? AND state = ?", licenseID, model.UpgradePending).
		OrderExpr("created_at").For("UPDATE").Scan(ctx)
	return out, err
}

// EndLicenseUpgradeIn closes an upgrade that never applied (expired,
// refunded or void), or records why one that applied ended. It changes
// nothing on the licence.
func EndLicenseUpgradeIn(ctx context.Context, db bun.IDB, u *model.LicenseUpgrade, state, reason string) error {
	now := time.Now()
	u.State, u.EndReason, u.EndedAt = state, reason, &now
	_, err := db.NewUpdate().Model(u).Column("state", "end_reason", "ended_at").WherePK().Exec(ctx)
	return err
}

// MarkLicenseUpgradeAppliedIn records that an upgrade took effect: the
// payment that made it, and the licence as it found it.
func MarkLicenseUpgradeAppliedIn(ctx context.Context, db bun.IDB, u *model.LicenseUpgrade) error {
	now := time.Now()
	u.State, u.AppliedAt = model.UpgradeApplied, &now
	_, err := db.NewUpdate().Model(u).Column(
		"state", "applied_at", "to_plan_id", "stripe_payment_intent_id", "stripe_subscription_id",
		"from_plan_id", "from_status", "from_valid_until", "from_updates_until",
		"from_updates_terms_set", "from_payment_provider", "from_stripe_customer_id", "from_canceled_at",
		"from_quantity",
	).WherePK().Exec(ctx)
	return err
}

// ResetSubscriptionCancelStateIn forgets whether a licence's subscription
// ends at its period end. The subscription row outlives the Stripe
// subscription an upgrade linked: once that is gone, or replaced by a
// new one, the recorded state belongs to nothing, and with it recorded
// the cancel-state sync would never ask about the new one.
func ResetSubscriptionCancelStateIn(ctx context.Context, db bun.IDB, licenseID string) error {
	_, err := db.NewUpdate().Model((*model.Subscription)(nil)).
		Set("cancel_at_period_end = false").Set("cancel_state_synced_at = NULL").
		Where("license_id = ?", licenseID).Exec(ctx)
	return err
}

// UpgradeRevert says what RevertLicenseUpgradeIn did.
type UpgradeRevert struct {
	// Reverted reports that the licence is back as the upgrade found it.
	Reverted bool
	// Unlinked: the licence is no longer billed by what the upgrade
	// linked (an operator unlinked it, or it was deleted). The upgrade
	// is closed and the licence left alone, since writing it would undo
	// what the operator did.
	Unlinked bool
	// FreePlanGone: the plan the licence came from was deleted, so there
	// is nothing to put it back on. The upgrade is closed and the caller
	// handles the licence as it would have before upgrades existed.
	FreePlanGone bool
	License      *model.License
	// Plan is the plan the licence is back on; PrevPlanID the one it
	// left, which is not the upgrade's target if the customer changed
	// plan since.
	Plan       *model.Plan
	PrevPlanID string
	// PrevQuantity is the seats the licence had on the upgrade's plan.
	PrevQuantity int
}

// RevertLicenseUpgradeIn puts the licence an applied upgrade changed
// back the way the upgrade found it: its plan, status, dates, payment
// provider and Stripe customer, with the Stripe links removed. The key
// and the activations were never touched. The caller has locked the
// upgrade row; this locks the licence.
//
// A revoked licence, or one an operator suspended, keeps that status:
// only its plan and billing go back. Whatever the outcome, the upgrade
// is closed (see UpgradeRevert), so a retry finds nothing left to do.
func RevertLicenseUpgradeIn(ctx context.Context, tx bun.IDB, u *model.LicenseUpgrade, reason string) (UpgradeRevert, error) {
	lic, err := LockLicenseIn(ctx, tx, u.LicenseID)
	if errors.Is(err, sql.ErrNoRows) {
		return UpgradeRevert{Unlinked: true}, EndLicenseUpgradeIn(ctx, tx, u, model.UpgradeReverted, reason+":licence_gone")
	}
	if err != nil {
		return UpgradeRevert{}, err
	}
	linked := lic.PaymentProvider == "stripe" &&
		((u.StripeSubscriptionID != "" && lic.StripeSubscriptionID == u.StripeSubscriptionID) ||
			(u.StripeSubscriptionID == "" && lic.StripeCheckoutSessionID == u.StripeCheckoutSessionID))
	if !linked {
		return UpgradeRevert{Unlinked: true, License: lic}, EndLicenseUpgradeIn(ctx, tx, u, model.UpgradeReverted, reason+":unlinked")
	}
	var plan *model.Plan
	if u.FromPlanID != "" {
		plan, err = FindPlanByIDIn(ctx, tx, u.FromPlanID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return UpgradeRevert{}, err
		}
		if err != nil {
			plan = nil
		}
	}
	if plan == nil {
		return UpgradeRevert{FreePlanGone: true, License: lic}, EndLicenseUpgradeIn(ctx, tx, u, model.UpgradeReverted, reason+":free_plan_gone")
	}

	l := *lic
	l.PlanID = plan.ID
	cols := []string{
		"plan_id", "valid_until", "canceled_at", "past_due_at", "updates_until", "updates_terms_set",
		"payment_provider", "stripe_customer_id", "stripe_subscription_id", "stripe_payment_intent_id",
		"stripe_checkout_session_id", "quantity", "stripe_pending_update_expires_at",
	}
	operatorHeld := lic.Status == model.StatusRevoked ||
		(lic.Status == model.StatusSuspended && lic.SuspendedBy != model.SuspendedByStripe)
	if !operatorHeld {
		l.Status = u.FromStatus
		l.SuspendedAt, l.SuspendedBy = nil, ""
		cols = append(cols, "status", "suspended_at", "suspended_by")
	}
	l.ValidUntil, l.CanceledAt, l.PastDueAt = u.FromValidUntil, u.FromCanceledAt, nil
	l.UpdatesUntil, l.UpdatesTermsSet = u.FromUpdatesUntil, u.FromUpdatesTermsSet
	l.PaymentProvider, l.StripeCustomerID = u.FromPaymentProvider, u.FromStripeCustomerID
	l.StripeSubscriptionID, l.StripePaymentIntentID, l.StripeCheckoutSessionID = "", "", ""
	// The seats the subscription paid for go with it, and any change
	// it held for payment.
	l.Quantity = max(u.FromQuantity, 1)
	l.StripePendingUpdateExpiresAt = nil
	if err := UpdateLicenseIn(ctx, tx, &l, cols...); err != nil {
		return UpgradeRevert{}, err
	}
	if err := SyncLicenseSubscriptionIn(ctx, tx, l.ID, plan, l.Status, l.ValidUntil); err != nil {
		return UpgradeRevert{}, err
	}
	if err := ResetSubscriptionCancelStateIn(ctx, tx, l.ID); err != nil {
		return UpgradeRevert{}, err
	}
	if err := EndLicenseUpgradeIn(ctx, tx, u, model.UpgradeReverted, reason); err != nil {
		return UpgradeRevert{}, err
	}
	return UpgradeRevert{Reverted: true, License: &l, Plan: plan, PrevPlanID: lic.PlanID, PrevQuantity: lic.Quantity}, nil
}

// ListUpgradeTargetPlans returns the active, priced plans of the given
// products, for working out which licences can be upgraded.
func (s *Store) ListUpgradeTargetPlans(ctx context.Context, productIDs []string) ([]*model.Plan, error) {
	var out []*model.Plan
	if len(productIDs) == 0 {
		return out, nil
	}
	err := s.DB.NewSelect().Model(&out).
		Where("product_id IN (?)", bun.In(productIDs)).
		Where("active = true AND COALESCE(stripe_price_id, '') <> ''").
		Scan(ctx)
	return out, err
}
