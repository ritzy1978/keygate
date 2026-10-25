package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// perSeatLicense is a licence on a hybrid product's plan sold per seat
// (two devices a seat), paid for quantity seats.
func perSeatLicense(t *testing.T, s *store.Store, ctx context.Context, quantity int, floating bool) (*model.Product, *model.Plan, *model.License) {
	t.Helper()
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Per seat", Slug: "per-seat-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	licenseModel := "standard"
	if floating {
		licenseModel = "floating"
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "team-" + suffix, LicenseType: "subscription",
		LicenseModel: licenseModel, FloatingTimeout: 30, MaxActivations: 2, MaxSeats: 1, PerSeat: true, MinQuantity: 1, MaxQuantity: 50}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-" + suffix + "@example.com",
		LicenseKey: "KEY-PS-" + suffix, Status: model.StatusActive, Quantity: quantity}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	return prod, plan, lic
}

func setQuantity(t *testing.T, s *store.Store, ctx context.Context, licenseID string, q int) {
	t.Helper()
	if _, err := s.DB.NewRaw("UPDATE licenses SET quantity = ? WHERE id = ?", q, licenseID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
}

func invite(s *store.Store, ctx context.Context, lic *model.License, n, max int) error {
	_, _, err := s.AddSeatWithinLimit(ctx, &model.Seat{LicenseID: lic.ID, Email: fmt.Sprintf("m%d-%s", n, lic.Email), Role: "member"}, max)
	return err
}

// The seats bought are the people a licence on a plan sold per seat
// holds, and the limit is the one the licence has under the lock: a
// caller that read a higher one before the seats went down cannot use
// it.
func TestPerSeat_SeatsReadUnderTheLock(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 3, false)

	// Three seats: the owner and two members.
	for i := range 2 {
		if err := invite(s, ctx, lic, i, 3); err != nil {
			t.Fatalf("invite %d: %v", i, err)
		}
	}
	if err := invite(s, ctx, lic, 2, 3); !errors.Is(err, store.ErrSeatLimitReached) {
		t.Fatalf("a fourth person on three seats = %v, want the seat limit", err)
	}
	// The seats went up to four: one more fits for a caller that read
	// four. (A caller that still read three stays at three; the licence's
	// limit only ever makes a check stricter.)
	setQuantity(t, s, ctx, lic.ID, 4)
	if err := invite(s, ctx, lic, 2, 4); err != nil {
		t.Fatalf("a fourth person on four seats: %v", err)
	}
	// Down to two, with a caller that read ten: refused, though ten
	// would leave room for the four people already on it.
	setQuantity(t, s, ctx, lic.ID, 2)
	if err := invite(s, ctx, lic, 3, 10); !errors.Is(err, store.ErrSeatLimitReached) {
		t.Fatalf("an invite on a stale limit of 10 after the seats went to 2 = %v, want the seat limit", err)
	}
}

// Devices are per seat, and also read under the lock.
func TestPerSeat_ActivationsReadUnderTheLock(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 2, false) // 2 seats × 2 devices
	activate := func(n, max int) error {
		return s.ActivateWithinLimit(ctx, &model.Activation{LicenseID: lic.ID, Identifier: fmt.Sprintf("dev-%d", n), IdentifierType: "device"}, max)
	}
	for i := range 4 {
		if err := activate(i, 4); err != nil {
			t.Fatalf("device %d of 4: %v", i, err)
		}
	}
	if err := activate(4, 4); err == nil {
		t.Fatal("a fifth device on 2 seats × 2 was let in")
	}
	// A caller that read a higher limit from before the seats went down
	// is held to the licence's.
	if err := activate(4, 100); err == nil {
		t.Fatal("a stale limit of 100 let a fifth device in")
	}
	setQuantity(t, s, ctx, lic.ID, 3)
	if err := activate(4, 6); err != nil {
		t.Fatalf("a fifth device on 3 seats × 2: %v", err)
	}
}

// Floating sessions are the per-seat value times the seats too.
func TestPerSeat_FloatingReadUnderTheLock(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 1, true) // 1 seat × 2 sessions
	checkOut := func(n, max int) error {
		_, err := s.CheckOutFloatingWithLimit(ctx, &model.FloatingSession{LicenseID: lic.ID, Identifier: fmt.Sprintf("m-%d", n),
			ExpiresAt: time.Now().Add(time.Hour)}, max)
		return err
	}
	for i := range 2 {
		if err := checkOut(i, 2); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if err := checkOut(2, 50); !errors.Is(err, store.ErrFloatingLimitReached) {
		t.Fatalf("a third session on 1 seat × 2 with a stale limit = %v, want the floating limit", err)
	}
}

// An admin sets the seats of a licence nothing bills; a Stripe
// subscription's are Stripe's, and a plan not sold per seat has none to
// set.
func TestPerSeat_SetLicenseQuantity(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 2, false)
	prev, err := s.SetLicenseQuantity(ctx, lic.ID, 9)
	if err != nil || prev != 2 {
		t.Fatalf("set 9 = %d %v, want 2 before", prev, err)
	}
	if got, _ := s.FindLicenseByID(ctx, lic.ID); got.Quantity != 9 {
		t.Fatalf("quantity = %d, want 9", got.Quantity)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", "sub_ps_"+lic.ID, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLicenseQuantity(ctx, lic.ID, 3); !errors.Is(err, store.ErrQuantityStripeManaged) {
		t.Fatalf("set on a Stripe-billed licence = %v, want ErrQuantityStripeManaged", err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = NULL WHERE id = ?", lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE plans SET per_seat = false WHERE id = ?", lic.PlanID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLicenseQuantity(ctx, lic.ID, 3); !errors.Is(err, store.ErrQuantityNotPerSeat) {
		t.Fatalf("set on a plan not sold per seat = %v, want ErrQuantityNotPerSeat", err)
	}
}

// A plan sold per seat keeps 1 in max_seats on a product with seats and
// 0 on one without; a product retyped takes its plans along, and is not
// refused over the 1 it keeps.
func TestPerSeat_ProductRetypeMovesMaxSeats(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, plan, _ := perSeatLicense(t, s, ctx, 2, false)
	if n, err := s.CountPlansIncompatibleWithType(ctx, prod.ID, "desktop"); err != nil || n != 0 {
		t.Fatalf("plans in the way of desktop = %d %v, want 0", n, err)
	}
	prod.Type = "desktop"
	if err := store.UpdateProductIn(ctx, s.DB, prod, "type"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.FindPlanByID(ctx, plan.ID); p.MaxSeats != 0 {
		t.Fatalf("max_seats on desktop = %d, want 0", p.MaxSeats)
	}
	prod.Type = "saas"
	if err := store.UpdateProductIn(ctx, s.DB, prod, "type"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.FindPlanByID(ctx, plan.ID); p.MaxSeats != 1 {
		t.Fatalf("max_seats on saas = %d, want 1", p.MaxSeats)
	}
}

// The database holds the rules a handler checks: only subscriptions are
// sold per seat, and the range makes sense.
func TestPerSeat_DatabaseConstraints(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, plan, lic := perSeatLicense(t, s, ctx, 2, false)
	for _, stmt := range []string{
		"UPDATE plans SET license_type = 'perpetual' WHERE id = ?",
		"UPDATE plans SET min_quantity = 0 WHERE id = ?",
		"UPDATE plans SET max_quantity = 0 WHERE id = ?",
		"UPDATE plans SET max_quantity = 10001 WHERE id = ?",
	} {
		if _, err := s.DB.NewRaw(stmt, plan.ID).Exec(ctx); err == nil {
			t.Errorf("%s was accepted", stmt)
		}
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET quantity = 0 WHERE id = ?", lic.ID).Exec(ctx); err == nil {
		t.Error("a licence with 0 seats was accepted")
	}
}
