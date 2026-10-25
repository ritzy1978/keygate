package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/apperr"
)

// The license owner manages the team: invites and removes. A member,
// including one who was a team admin before teams lost that role, and
// anyone else get what an unknown licence gets; everyone joins as a
// member.
func TestSeatManagementIsTheOwners(t *testing.T) {
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
	prod := &model.Product{Name: "Team", Slug: "team-owner-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "team-owner-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 10, MaxSeats: 10, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "Owner-Team-" + suffix + "@example.com",
		LicenseKey: "KG-TEAM-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	svc := NewSeatService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "")
	email := func(who string) string { return who + "-team-" + suffix + "@example.com" }
	invite := func(actor, who, role string) error {
		_, err := svc.AddSeat(ctx, AddSeatInput{LicenseKey: lic.LicenseKey, Email: email(who), Role: role, ActorEmail: actor})
		return err
	}
	accept := func(who string) *model.Seat {
		t.Helper()
		seat, err := s.FindSeatByEmail(ctx, lic.ID, email(who))
		if err != nil {
			t.Fatal(err)
		}
		plain, hash := store.GenerateSeatInviteToken()
		if _, err := s.DB.NewRaw("UPDATE seats SET invite_token_hash = ?, invite_expires_at = now() + interval '1 hour' WHERE id = ?",
			hash, seat.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
			t.Fatal(err)
		}
		seat, _ = s.FindSeatByID(ctx, seat.ID)
		return seat
	}
	remove := func(actor string, seat *model.Seat) error {
		return svc.RemoveSeat(ctx, lic.LicenseKey, seat.ID, "", "", actor)
	}
	code := func(err error) (int, string) {
		var ae *apperr.AppError
		if errors.As(err, &ae) {
			return ae.Status, ae.Code
		}
		return 0, ""
	}
	notFound := func(err error) bool {
		st, c := code(err)
		return st == 404 && c == "LICENSE_NOT_FOUND"
	}

	// The owner invites, with the role left out or given as member,
	// matched whatever the case of the address.
	if err := invite("owner-team-"+suffix+"@example.com", "alice", ""); err != nil {
		t.Fatalf("owner invites alice: %v", err)
	}
	if err := invite(lic.Email, "bob", "member"); err != nil {
		t.Fatalf("owner invites bob: %v", err)
	}
	alice, bob := accept("alice"), accept("bob")
	if alice.Role != "member" {
		t.Fatalf("alice joined as %q", alice.Role)
	}
	// Nobody is invited as an admin any more.
	if err := invite(lic.Email, "carol", "admin"); func() bool { st, _ := code(err); return st != 400 }() {
		t.Fatalf("invite as admin = %v, want 400", err)
	}

	// A member neither invites nor removes.
	if err := invite(alice.Email, "dave", "member"); !notFound(err) {
		t.Fatalf("member invites = %v, want LICENSE_NOT_FOUND", err)
	}
	if err := remove(alice.Email, bob); !notFound(err) {
		t.Fatalf("member removes another = %v, want LICENSE_NOT_FOUND", err)
	}
	if err := remove(alice.Email, alice); !notFound(err) {
		t.Fatalf("member removes themselves = %v, want LICENSE_NOT_FOUND", err)
	}
	// The team as each sees it: the owner sees a pending invitation
	// too, matched whatever the case of their address; a member sees
	// who has joined.
	if err := invite(lic.Email, "hank", "member"); err != nil {
		t.Fatal(err)
	}
	emails := func(seats []*model.Seat) string {
		var out []string
		for _, st := range seats {
			out = append(out, strings.SplitN(st.Email, "-", 2)[0])
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	for actor, want := range map[string]string{
		strings.ToUpper(lic.Email): "alice,bob,hank",
		alice.Email:                "alice,bob",
	} {
		seats, err := svc.ListSeats(ctx, lic.LicenseKey, "", actor)
		if err != nil || emails(seats) != want {
			t.Fatalf("team as %s sees it = %s, %v; want %s", actor, emails(seats), err, want)
		}
	}
	hank, err := s.FindSeatByEmail(ctx, lic.ID, email("hank"))
	if err != nil {
		t.Fatal(err)
	}
	if err := remove(lic.Email, hank); err != nil {
		t.Fatal(err)
	}

	// Nor does anyone else.
	if err := invite("stranger-"+suffix+"@example.com", "erin", "member"); !notFound(err) {
		t.Fatalf("stranger invites = %v, want LICENSE_NOT_FOUND", err)
	}

	// A seat left with the old admin role carries no say either.
	if _, err := s.DB.NewRaw("UPDATE seats SET role = 'admin' WHERE id = ?", bob.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := invite(bob.Email, "frank", "member"); !notFound(err) {
		t.Fatalf("old admin invites = %v, want LICENSE_NOT_FOUND", err)
	}
	if err := remove(bob.Email, alice); !notFound(err) {
		t.Fatalf("old admin removes = %v, want LICENSE_NOT_FOUND", err)
	}

	// Who asks is settled first: a member naming the owner's address
	// meets the same 404, not the 400 an owner would.
	_, err = svc.AddSeat(ctx, AddSeatInput{LicenseKey: lic.LicenseKey, Email: lic.Email, ActorEmail: alice.Email})
	if !notFound(err) {
		t.Fatalf("member inviting the owner's address = %v, want LICENSE_NOT_FOUND", err)
	}
	for _, in := range []AddSeatInput{
		{Email: "not an address"}, {Email: email("kai"), Role: "admin"},
	} {
		in.LicenseKey, in.ActorEmail = lic.LicenseKey, alice.Email
		if _, err := svc.AddSeat(ctx, in); !notFound(err) {
			t.Fatalf("member's invite %+v = %v, want LICENSE_NOT_FOUND before any 400", in, err)
		}
	}

	// Suspended, the licence takes no one new, but its owner still sees
	// the team and takes people off it, so a renewal does not bring
	// them back.
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = ? WHERE id = ?", model.StatusSuspended, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := invite(lic.Email, "gina", "member"); !notFound(err) {
		t.Fatalf("invite on a suspended licence = %v, want LICENSE_NOT_FOUND", err)
	}
	if seats, err := svc.ListSeats(ctx, lic.LicenseKey, "", lic.Email); err != nil || len(seats) != 2 {
		t.Fatalf("list on a suspended licence = %d seats, %v; want 2", len(seats), err)
	}
	if err := remove(lic.Email, alice); err != nil {
		t.Fatalf("owner removes alice from a suspended licence: %v", err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = ? WHERE id = ?", model.StatusActive, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// Removing again is a quiet no-op: one removal on record.
	if err := remove(lic.Email, alice); err != nil {
		t.Fatalf("removing alice again: %v", err)
	}
	var removals int
	for range 40 {
		if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'seat_removed'", alice.ID).Scan(ctx, &removals); err != nil {
			t.Fatal(err)
		}
		if removals > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if err := s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'seat_removed'", alice.ID).Scan(ctx, &removals); err != nil || removals != 1 {
		t.Fatalf("%d removals on record (%v), want 1", removals, err)
	}
	if err := remove(lic.Email, bob); err != nil {
		t.Fatalf("owner removes bob: %v", err)
	}

	// Accepting is held to the rule inviting is: a canceled license still
	// inside its paid period takes the people it invited, and one past it
	// does not.
	acceptOn := func(who string, validUntil time.Time) error {
		t.Helper()
		if _, err := s.DB.NewRaw("UPDATE licenses SET status = ?, valid_until = NULL WHERE id = ?", model.StatusActive, lic.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if err := invite(lic.Email, who, "member"); err != nil {
			t.Fatalf("invite %s: %v", who, err)
		}
		if _, err := s.DB.NewRaw("UPDATE licenses SET status = ?, valid_until = ? WHERE id = ?", model.StatusCanceled, validUntil, lic.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		seat, _ := s.FindSeatByEmail(ctx, lic.ID, email(who))
		plain, hash := store.GenerateSeatInviteToken()
		if _, err := s.DB.NewRaw("UPDATE seats SET invite_token_hash = ?, invite_expires_at = now() + interval '1 hour' WHERE id = ?",
			hash, seat.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := svc.AcceptSeatInvite(ctx, plain)
		return err
	}
	if err := acceptOn("hana", time.Now().Add(240*time.Hour)); err != nil {
		t.Fatalf("accept on a canceled license inside its period: %v", err)
	}
	if err := acceptOn("ivan", time.Now().Add(-time.Hour)); func() bool { st, c := code(err); return st != 404 || c != "SEAT_INVITE_INVALID" }() {
		t.Fatalf("accept on a canceled license past its period = %v, want SEAT_INVITE_INVALID", err)
	}
	if seat, _ := s.FindSeatByEmail(ctx, lic.ID, email("ivan")); seat.AcceptedAt != nil {
		t.Fatal("the refused accept left the seat accepted")
	}
}

// Removing a member tells the product's webhooks once: seat.removed,
// and license.deactivated for each device that went with them; removing
// again tells nobody. An accept rolled back on a license not in use
// reports the devices it removed the same way.
func TestSeatRemovalWebhooks(t *testing.T) {
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
	prod := &model.Product{Name: "Hooks", Slug: "hooks-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "hooks-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 10, MaxSeats: 10, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-hooks-" + suffix + "@example.com",
		LicenseKey: "KG-HOOKS-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	hook := &model.Webhook{ProductID: prod.ID, URL: "http://127.0.0.1:9/hook", Secret: "s",
		Events: []string{model.EventSeatRemoved, model.EventLicenseDeactivated}, Active: true}
	if err := s.CreateWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	svc := NewSeatService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "")
	sent := func(event string) int {
		t.Helper()
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ?", hook.ID, event).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seat := func(who string, accept bool) *model.Seat {
		t.Helper()
		plain, hash := store.GenerateSeatInviteToken()
		expires := time.Now().Add(time.Hour)
		st := &model.Seat{LicenseID: lic.ID, Email: who + "-hooks-" + suffix + "@example.com", Role: "member",
			InviteTokenHash: hash, InviteExpiresAt: &expires}
		if _, _, err := s.AddSeatWithinLimit(ctx, st, 0); err != nil {
			t.Fatal(err)
		}
		if accept {
			if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
				t.Fatal(err)
			}
		}
		st, _ = s.FindSeatByID(ctx, st.ID)
		return st
	}

	alice := seat("alice", true)
	key, err := s.MemberKey(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"alice-pc", "alice-laptop"} {
		if err := s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID, Identifier: d, IdentifierType: "device", SeatID: alice.ID}, 10, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RemoveSeat(ctx, lic.LicenseKey, alice.ID, "", "", lic.Email); err != nil {
		t.Fatal(err)
	}
	if got := [2]int{sent(model.EventSeatRemoved), sent(model.EventLicenseDeactivated)}; got != [2]int{1, 2} {
		t.Fatalf("removing alice sent seat.removed, license.deactivated = %v, want [1 2]", got)
	}
	if err := svc.RemoveSeat(ctx, lic.LicenseKey, alice.ID, "", "", lic.Email); err != nil {
		t.Fatal(err)
	}
	if got := [2]int{sent(model.EventSeatRemoved), sent(model.EventLicenseDeactivated)}; got != [2]int{1, 2} {
		t.Fatalf("removing alice again sent more: %v", got)
	}

	// A device on a seat whose accept is then rolled back (as when its
	// key was used in the moment the seat stood accepted).
	eve := seat("eve", false)
	plain, hash := store.GenerateSeatInviteToken()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE seats SET invite_token_hash = ?, invite_expires_at = now() + interval '1 hour' WHERE id = ?", []any{hash, eve.ID}},
		{"INSERT INTO activations (id, license_id, identifier, identifier_type, seat_id) VALUES (?, ?, 'eve-pc', 'device', ?)", []any{"eve-act-" + suffix, lic.ID, eve.ID}},
		{"UPDATE licenses SET status = ? WHERE id = ?", []any{model.StatusSuspended, lic.ID}},
	} {
		if _, err := s.DB.NewRaw(q.sql, q.args...).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AcceptSeatInvite(ctx, plain); err == nil {
		t.Fatal("accept on a suspended license went through")
	}
	if got := sent(model.EventLicenseDeactivated); got != 3 {
		t.Fatalf("the rolled back accept sent %d license.deactivated in all, want 3", got)
	}
}

// Sending a pending invite again mails a new link but adds no seat, so
// seat.added fires for the invite and for bringing a removed member
// back, not for the resend; the audit still records it.
func TestSeatResendWebhook(t *testing.T) {
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
	prod := &model.Product{Name: "Resend", Slug: "resend-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "resend-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 10, MaxSeats: 2, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-resend-" + suffix + "@example.com",
		LicenseKey: "KG-RESEND-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	hook := &model.Webhook{ProductID: prod.ID, URL: "http://127.0.0.1:9/hook", Secret: "s",
		Events: []string{model.EventSeatAdded}, Active: true}
	if err := s.CreateWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	svc := NewSeatService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "")
	added := func() int {
		t.Helper()
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ?", hook.ID, model.EventSeatAdded).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	who := "member-resend-" + suffix + "@example.com"
	invite := func() *model.Seat {
		t.Helper()
		seat, err := svc.AddSeat(ctx, AddSeatInput{LicenseKey: lic.LicenseKey, Email: who, ActorEmail: lic.Email})
		if err != nil {
			t.Fatal(err)
		}
		return seat
	}

	seat := invite()
	if n := added(); n != 1 {
		t.Fatalf("seat.added after the invite = %d, want 1", n)
	}
	// The team is full (owner + one member): sending again still works.
	invite()
	invite()
	if n := added(); n != 1 {
		t.Fatalf("seat.added after two resends = %d, want still 1", n)
	}
	var resent int
	for range 40 {
		_ = s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'seat_invited' AND changes->>'outcome' = 'resent'",
			seat.ID).Scan(ctx, &resent)
		if resent == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if resent != 2 {
		t.Fatalf("%d resends audited, want 2", resent)
	}

	if err := svc.RemoveSeat(ctx, lic.LicenseKey, seat.ID, "", "", lic.Email); err != nil {
		t.Fatal(err)
	}
	invite()
	if n := added(); n != 2 {
		t.Fatalf("seat.added after bringing the member back = %d, want 2", n)
	}
}

// A seat removed by an older version kept its key and devices; inviting
// the address back removes them, and each is reported as deactivated,
// as a removal reports them, so an integration that saw them activated
// sees them go.
func TestSeatRestoreReportsStaleDevices(t *testing.T) {
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
	prod := &model.Product{Name: "Stale", Slug: "stale-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "stale-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 10, MaxSeats: 5, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-stale-" + suffix + "@example.com",
		LicenseKey: "KG-STALE-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	hook := &model.Webhook{ProductID: prod.ID, URL: "http://127.0.0.1:9/hook", Secret: "s",
		Events: []string{model.EventLicenseDeactivated}, Active: true}
	if err := s.CreateWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	svc := NewSeatService(s, NewWebhookService(s, slog.Default(), time.Second, 1, true), nil, slog.Default(), "")
	who := "member-stale-" + suffix + "@example.com"
	invite := func() {
		t.Helper()
		if _, err := svc.AddSeat(ctx, AddSeatInput{LicenseKey: lic.LicenseKey, Email: who, ActorEmail: lic.Email}); err != nil {
			t.Fatal(err)
		}
	}
	invite()
	seat, err := s.FindSeatByEmail(ctx, lic.ID, who)
	if err != nil {
		t.Fatal(err)
	}
	plain, hash := store.GenerateSeatInviteToken()
	if _, err := s.DB.NewRaw("UPDATE seats SET invite_token_hash = ?, invite_expires_at = now() + interval '1 hour' WHERE id = ?", hash, seat.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
		t.Fatal(err)
	}
	seat, _ = s.FindSeatByID(ctx, seat.ID)
	key, err := s.MemberKey(ctx, seat)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"m-pc1", "m-pc2"} {
		if err := s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID, Identifier: d, IdentifierType: "device", SeatID: seat.ID}, 10, key); err != nil {
			t.Fatal(err)
		}
	}
	// Removed the old way: removed_at alone.
	if _, err := s.DB.NewRaw("UPDATE seats SET removed_at = now() WHERE id = ?", seat.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	invite()
	var sent int
	if err := s.DB.NewRaw("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ?", hook.ID, model.EventLicenseDeactivated).Scan(ctx, &sent); err != nil || sent != 2 {
		t.Fatalf("license.deactivated after bringing the member back = %d (%v), want one per stale device", sent, err)
	}
}

// The hourly sweep finishes what an older version left during a rolling
// upgrade: a member it removed loses the key and devices it kept (each
// device reported as deactivated, once), and a seat it gave the admin
// role becomes a member. A seat on the team is never touched, and a
// second sweep does nothing more.
func TestFinishSeatRemovals(t *testing.T) {
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
	prod := &model.Product{Name: "Sweep", Slug: "sweep-" + suffix, Type: "hybrid"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "sweep-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 10, MaxSeats: 5, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "owner-sweep-" + suffix + "@example.com",
		LicenseKey: "KG-SWEEP-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	hook := &model.Webhook{ProductID: prod.ID, URL: "http://127.0.0.1:9/hook", Secret: "s",
		Events: []string{model.EventLicenseDeactivated}, Active: true}
	if err := s.CreateWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	member := func(who string) (*model.Seat, string) {
		t.Helper()
		plain, hash := store.GenerateSeatInviteToken()
		expires := time.Now().Add(time.Hour)
		seat := &model.Seat{LicenseID: lic.ID, Email: who + "-sweep-" + suffix + "@example.com", Role: "member",
			InviteTokenHash: hash, InviteExpiresAt: &expires}
		if _, _, err := s.AddSeatWithinLimit(ctx, seat, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
			t.Fatal(err)
		}
		seat, _ = s.FindSeatByID(ctx, seat.ID)
		key, err := s.MemberKey(ctx, seat)
		if err != nil {
			t.Fatal(err)
		}
		return seat, key
	}
	gone, goneKey := member("gone")
	stays, staysKey := member("stays")
	back, backKey := member("back")
	for _, d := range []struct{ seat, key, device string }{
		{gone.ID, goneKey, "gone-pc1"}, {gone.ID, goneKey, "gone-pc2"}, {stays.ID, staysKey, "stays-pc"},
		{back.ID, backKey, "back-pc"},
	} {
		if err := s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID, Identifier: d.device, IdentifierType: "device", SeatID: d.seat}, 10, d.key); err != nil {
			t.Fatal(err)
		}
	}
	// What an older version leaves: removed_at alone, and an admin seat.
	if _, err := s.DB.NewRaw("UPDATE seats SET removed_at = now() WHERE id = ?", gone.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE seats SET role = 'admin' WHERE id = ?", stays.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// Removed and invited back by an older version: pending again, still
	// holding the key and device it had.
	_, backHash := store.GenerateSeatInviteToken()
	if _, err := s.DB.NewRaw(`UPDATE seats SET accepted_at = NULL, user_id = NULL, invite_token_hash = ?,
		invite_expires_at = now() + interval '1 hour' WHERE id = ?`, backHash, back.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	c := NewExpiryChecker(s, nil, NewWebhookService(s, slog.Default(), time.Second, 1, true), slog.Default())
	sent := func() int {
		t.Helper()
		var n int
		if err := s.DB.NewRaw("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ?", hook.ID, model.EventLicenseDeactivated).Scan(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for range 2 {
		c.FinishSeatRemovals(ctx)
	}
	if n := sent(); n != 3 {
		t.Fatalf("license.deactivated after two sweeps = %d, want one per leftover device (3)", n)
	}
	b, _ := s.FindSeatByID(ctx, back.ID)
	if b.MemberKeyHash != "" || b.AcceptedAt != nil || b.InviteTokenHash != backHash {
		t.Fatalf("the seat invited back: key kept %v, accepted %v, invite kept %v; want no key, still pending with its invite",
			b.MemberKeyHash != "", b.AcceptedAt != nil, b.InviteTokenHash == backHash)
	}
	if _, err := s.FindActivation(ctx, lic.ID, "back-pc"); err == nil {
		t.Fatal("the device of the seat invited back is still there")
	}
	g, _ := s.FindSeatByID(ctx, gone.ID)
	if g.MemberKeyHash != "" || g.MemberKey != "" {
		t.Fatal("the removed seat kept its key")
	}
	var left int
	if err := s.DB.NewRaw("SELECT count(*) FROM activations WHERE seat_id = ?", gone.ID).Scan(ctx, &left); err != nil || left != 0 {
		t.Fatalf("%d devices left on the removed seat (%v)", left, err)
	}
	st, _ := s.FindSeatByID(ctx, stays.ID)
	if st.Role != "member" || st.MemberKeyHash == "" {
		t.Fatalf("the seat on the team: role %q, key kept %v; want a member with its key", st.Role, st.MemberKeyHash != "")
	}
	if _, err := s.FindActivation(ctx, lic.ID, "stays-pc"); err != nil {
		t.Fatal("the device of a seat on the team was removed")
	}
	// One cleared already is done: another sweep (another replica's)
	// reports nothing, and audits nothing more.
	if devices, done, err := s.ClearSeatLeftovers(ctx, gone.ID); err != nil || done || len(devices) != 0 {
		t.Fatalf("clearing it again = %v, %v, %v; want nothing done", devices, done, err)
	}
	// A seat on the team is not a leftover, whatever is asked.
	if devices, done, err := s.ClearSeatLeftovers(ctx, stays.ID); err != nil || done || len(devices) != 0 {
		t.Fatalf("clearing a seat on the team = %v, %v, %v; want nothing done", devices, done, err)
	}
}
