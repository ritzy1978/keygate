package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// max_seats is how many people a license holds, the owner included:
// with 3 the owner invites two members, and with 1 nobody. The server
// used to let the owner add max_seats members on top, while the portal
// and the docs counted the owner as one of them.
func TestSeatLimitCountsTheOwner(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	defer s.Close()
	if err := s.RunMigrations("../../db/migrations"); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "Seats", Slug: "seats-owner-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	svc := NewSeatService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "https://keygate.example")

	licence := func(name string, maxSeats int) *model.License {
		t.Helper()
		plan := &model.Plan{ProductID: prod.ID, Name: name, Slug: "seats-" + name + "-" + suffix, LicenseType: "perpetual",
			LicenseModel: "standard", MaxSeats: maxSeats, Active: true}
		if err := s.CreatePlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
		l := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-" + name + "-" + suffix + "@example.com",
			LicenseKey: "KG-SEATS-" + name + "-" + suffix, Status: model.StatusActive}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	invite := func(l *model.License, who string) error {
		_, err := svc.AddSeat(ctx, AddSeatInput{LicenseKey: l.LicenseKey, Email: who + "-" + suffix + "@example.com", Role: "member", ActorEmail: l.Email})
		return err
	}
	seatLimit := func(err error, current, max int) bool {
		var ae *apperr.AppError
		if !errors.As(err, &ae) || ae.Code != "SEAT_LIMIT" {
			return false
		}
		d, _ := ae.Details.(map[string]any)
		return d["current"] == current && d["max"] == max
	}

	three := licence("three", 3)
	for _, who := range []string{"a", "b"} {
		if err := invite(three, who); err != nil {
			t.Fatalf("invite %s on a 3 seat license: %v", who, err)
		}
	}
	if err := invite(three, "c"); !seatLimit(err, 3, 3) {
		t.Fatalf("third member on a 3 seat license = %v, want SEAT_LIMIT with current 3, max 3", err)
	}

	// At the limit: re-inviting a member still pending only renews the
	// invite and takes no new place; removing a member frees one; and
	// bringing a removed member back is a new place, refused when full.
	if err := invite(three, "a"); err != nil {
		t.Fatalf("re-invite pending member a at the limit: %v", err)
	}
	seatID := func(l *model.License, who string) string {
		t.Helper()
		seat, err := s.FindSeatByEmail(ctx, l.ID, who+"-"+suffix+"@example.com")
		if err != nil {
			t.Fatalf("seat of %s: %v", who, err)
		}
		return seat.ID
	}
	if err := svc.RemoveSeat(ctx, three.LicenseKey, seatID(three, "b"), "", "", three.Email); err != nil {
		t.Fatalf("remove b: %v", err)
	}
	if err := invite(three, "c"); err != nil {
		t.Fatalf("invite c into the place b left: %v", err)
	}
	if err := invite(three, "b"); !seatLimit(err, 3, 3) {
		t.Fatalf("bring b back on a full license = %v, want SEAT_LIMIT with current 3, max 3", err)
	}
	if err := svc.RemoveSeat(ctx, three.LicenseKey, seatID(three, "c"), "", "", three.Email); err != nil {
		t.Fatalf("remove c: %v", err)
	}
	if err := invite(three, "b"); err != nil {
		t.Fatalf("bring b back once there is room: %v", err)
	}

	single := licence("single", 1)
	if err := invite(single, "d"); !seatLimit(err, 1, 1) {
		t.Fatalf("member on a single seat license = %v, want SEAT_LIMIT with current 1, max 1", err)
	}

	unlimited := licence("unlimited", 0)
	for _, who := range []string{"e", "f", "g"} {
		if err := invite(unlimited, who); err != nil {
			t.Fatalf("invite %s with no limit: %v", who, err)
		}
	}

	// The license email moves to member a's address (a Stripe customer
	// changing theirs). a is the owner now and counts once: b is the one
	// member, so one more fits and the next does not.
	if _, err := s.DB.NewUpdate().Model((*model.License)(nil)).Set("email = ?", "A-"+suffix+"@example.com").
		Where("id = ?", three.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	three.Email = "A-" + suffix + "@example.com" // the owner who invites now
	if n, _ := s.CountActiveSeats(ctx, three.ID); n != 1 {
		t.Fatalf("members after the owner took a member's address = %d, want 1", n)
	}
	if err := invite(three, "c"); err != nil {
		t.Fatalf("invite c once the owner holds a's address: %v", err)
	}
	if err := invite(three, "e2"); !seatLimit(err, 3, 3) {
		t.Fatalf("fourth person = %v, want SEAT_LIMIT with current 3, max 3", err)
	}
}
