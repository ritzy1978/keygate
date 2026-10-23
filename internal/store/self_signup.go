package store

import (
	"context"
	"errors"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/tabloy/keygate/internal/model"
)

// ErrSelfSignupClosed means the plan stopped taking self sign-ups (was
// deactivated, or gained a price) after the caller read it.
var ErrSelfSignupClosed = errors.New("the plan no longer takes self sign-ups")

// ErrSelfSignupExists means the address already holds a self sign-up
// licence for the product.
var ErrSelfSignupExists = errors.New("this address already has a self sign-up licence for this product")

// ErrSignupCapReached means the plan took as many self sign-ups today
// (UTC) as its signup_daily_cap allows.
var ErrSignupCapReached = errors.New("the plan's daily sign-up limit is reached")

// FindSelfSignupLicense returns the self sign-up licence email holds
// for the product, on whatever plan it is now, or sql.ErrNoRows. The
// plan comes with it: whether the licence is still usable depends on
// the plan's grace period.
func (s *Store) FindSelfSignupLicense(ctx context.Context, productID, email string) (*model.License, error) {
	l := new(model.License)
	err := s.DB.NewSelect().Model(l).
		Relation("Plan").
		Where("license.product_id = ?", productID).
		Where("lower(license.email) = lower(?)", email).
		Where("license.signup_source = ?", model.SignupSourceSelf).
		Limit(1).
		Scan(ctx)
	return l, err
}

// CreateSelfSignupLicense writes a licence a customer claimed through
// self sign-up. Under a per-product lock it re-reads the plan and holds
// it, so a plan switched off, deactivated or given a price since the
// caller read it takes no more claims, and an edit waits for a claim
// already past the check. It then refuses a second self sign-up licence for the
// same address on the product (ErrSelfSignupExists) and a claim over
// the plan's daily cap (ErrSignupCapReached). The partial unique index
// on (product_id, lower(email)) backs the first check up.
func (s *Store) CreateSelfSignupLicense(ctx context.Context, l *model.License, plan *model.Plan) error {
	l.SignupSource = model.SignupSourceSelf
	l.SignupPlanID = l.PlanID
	return s.createLicenseWithSubscription(ctx, l, plan, func(ctx context.Context, tx bun.Tx) error {
		// Claims on one product queue here, so the counts below see
		// every claim that committed before this one.
		if _, err := tx.NewRaw("SELECT pg_advisory_xact_lock(hashtext(?))", "self_signup:"+l.ProductID).Exec(ctx); err != nil {
			return err
		}
		var open bool
		var price string
		var dailyCap int
		if err := tx.NewRaw("SELECT self_signup AND active, coalesce(stripe_price_id, ''), signup_daily_cap FROM plans WHERE id = ? FOR SHARE", l.PlanID).
			Scan(ctx, &open, &price, &dailyCap); err != nil {
			return err
		}
		if !open || price != "" {
			return ErrSelfSignupClosed
		}
		var n int
		if err := tx.NewRaw(
			"SELECT count(*) FROM licenses WHERE product_id = ? AND lower(email) = lower(?) AND signup_source = ?",
			l.ProductID, l.Email, model.SignupSourceSelf).Scan(ctx, &n); err != nil {
			return err
		}
		if n > 0 {
			return ErrSelfSignupExists
		}
		if dailyCap > 0 {
			n, err := countSelfSignupsToday(ctx, tx, l.PlanID)
			if err != nil {
				return err
			}
			if n >= dailyCap {
				return ErrSignupCapReached
			}
		}
		return nil
	})
}

// CountSelfSignupsToday returns how many self sign-up licences were
// claimed on the plan today (UTC), wherever they have moved since. Read
// outside the claim lock it is only a hint: CreateSelfSignupLicense
// counts again under the lock.
func (s *Store) CountSelfSignupsToday(ctx context.Context, planID string) (int, error) {
	return countSelfSignupsToday(ctx, s.DB, planID)
}

func countSelfSignupsToday(ctx context.Context, db bun.IDB, planID string) (int, error) {
	var n int
	err := db.NewRaw(
		"SELECT count(*) FROM licenses WHERE signup_plan_id = ? AND signup_source = ? AND created_at >= date_trunc('day', now(), 'UTC')",
		planID, model.SignupSourceSelf).Scan(ctx, &n)
	return n, err
}

// IsSelfSignupPlanConflict reports whether err is the plans_self_signup_free
// constraint refusing a self sign-up plan with a price or an update
// period.
func IsSelfSignupPlanConflict(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "23514" &&
		pgErr.Field('n') == "plans_self_signup_free"
}

// IsSelfSignupConflict reports whether err is the unique index on
// self sign-up licences refusing a second one for the same address.
func IsSelfSignupConflict(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "23505" &&
		pgErr.Field('n') == "licenses_self_signup_product_email"
}
