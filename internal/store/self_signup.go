package store

import (
	"context"
	"errors"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/tabloy/keygate/internal/model"
)

// ErrSelfSignupClosed means the plan stopped taking self sign-ups (or
// gained a price) after the caller read it.
var ErrSelfSignupClosed = errors.New("the plan no longer takes self sign-ups")

// ErrSelfSignupExists means the address already holds a self sign-up
// licence for the product.
var ErrSelfSignupExists = errors.New("this address already has a self sign-up licence for this product")

// ErrSignupCapReached means the plan took as many self sign-ups today
// (UTC) as its signup_daily_cap allows.
var ErrSignupCapReached = errors.New("the plan's daily sign-up limit is reached")

// FindSelfSignupLicense returns the self sign-up licence email holds
// for the product, on whatever plan it is now, or sql.ErrNoRows.
func (s *Store) FindSelfSignupLicense(ctx context.Context, productID, email string) (*model.License, error) {
	l := new(model.License)
	err := s.DB.NewSelect().Model(l).
		Where("license.product_id = ?", productID).
		Where("lower(license.email) = lower(?)", email).
		Where("license.signup_source = ?", model.SignupSourceSelf).
		Limit(1).
		Scan(ctx)
	return l, err
}

// CreateSelfSignupLicense writes a licence a customer claimed through
// self sign-up. Under a per-product lock it re-reads the plan, so a
// plan switched off or given a price since the caller read it takes
// no more claims, then refuses a second self sign-up licence for the
// same address on the product (ErrSelfSignupExists) and a claim over
// the plan's daily cap (ErrSignupCapReached). The partial unique index
// on (product_id, lower(email)) backs the first check up.
func (s *Store) CreateSelfSignupLicense(ctx context.Context, l *model.License, plan *model.Plan) error {
	l.SignupSource = model.SignupSourceSelf
	return s.createLicenseWithSubscription(ctx, l, plan, func(ctx context.Context, tx bun.Tx) error {
		// Claims on one product queue here, so the counts below see
		// every claim that committed before this one.
		if _, err := tx.NewRaw("SELECT pg_advisory_xact_lock(hashtext(?))", "self_signup:"+l.ProductID).Exec(ctx); err != nil {
			return err
		}
		var open bool
		var price string
		var dailyCap int
		if err := tx.NewRaw("SELECT self_signup, coalesce(stripe_price_id, ''), signup_daily_cap FROM plans WHERE id = ?", l.PlanID).
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
			if err := tx.NewRaw(
				"SELECT count(*) FROM licenses WHERE plan_id = ? AND signup_source = ? AND created_at >= date_trunc('day', now(), 'UTC')",
				l.PlanID, model.SignupSourceSelf).Scan(ctx, &n); err != nil {
				return err
			}
			if n >= dailyCap {
				return ErrSignupCapReached
			}
		}
		return nil
	})
}

// IsSelfSignupConflict reports whether err is the unique index on
// self sign-up licences refusing a second one for the same address.
func IsSelfSignupConflict(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "23505" &&
		pgErr.Field('n') == "licenses_self_signup_product_email"
}
