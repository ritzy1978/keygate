package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// newTeamFixture is the API fixture with licA's product made hybrid: a
// team needs a product with seats, which a desktop product has not.
func newTeamFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	if _, err := f.s.DB.NewRaw("UPDATE products SET type = 'hybrid' WHERE id = ?", f.prodA.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// portalTeam is licA with an accepted member, alice, who has a device of
// her own; a pending invite; a removed member; and an owner's device.
type portalTeam struct {
	f                    *apiFixture
	alice, pending, gone *model.Seat
	aliceKey             string
}

func newPortalTeam(t *testing.T) *portalTeam {
	t.Helper()
	f := newTeamFixture(t)
	lic := f.licA
	join := func(who string, accept bool) *model.Seat {
		t.Helper()
		plain, hash := store.GenerateSeatInviteToken()
		expires := time.Now().Add(time.Hour)
		seat := &model.Seat{LicenseID: lic.ID, Email: who + "-pa-" + f.suffix + "@example.com", Role: "member",
			InviteTokenHash: hash, InviteExpiresAt: &expires}
		if _, _, err := f.s.AddSeatWithinLimit(f.ctx, seat, 0); err != nil {
			t.Fatal(err)
		}
		if accept {
			if _, _, err := f.s.AcceptSeatInvite(f.ctx, plain); err != nil {
				t.Fatal(err)
			}
		}
		seat, _ = f.s.FindSeatByID(f.ctx, seat.ID)
		return seat
	}
	p := &portalTeam{f: f, alice: join("alice", true), pending: join("pending", false), gone: join("gone", true)}
	var err error
	if p.aliceKey, err = f.s.MemberKey(f.ctx, p.alice); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.RemoveSeat(f.ctx, p.gone.ID); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct{ seat, key, device string }{
		{"", lic.LicenseKey, "owner-pc"}, {p.alice.ID, p.aliceKey, "alice-pc"},
	} {
		if err := f.s.ActivateForKey(f.ctx, &model.Activation{LicenseID: lic.ID, Identifier: d.device,
			IdentifierType: "device", SeatID: d.seat}, 3, d.key); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// listed is the licence as the portal's list reads it for email.
func (p *portalTeam) listed(t *testing.T, email string) *model.License {
	t.Helper()
	ls, err := p.f.s.ListLicensesByEmail(p.f.ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.ID == p.f.licA.ID {
			return l
		}
	}
	// Not listed: shape the licence as if it had been, so the view
	// itself is what refuses.
	l, err := p.f.s.FindLicenseByKey(p.f.ctx, p.f.licA.LicenseKey)
	if err != nil {
		t.Fatal(err)
	}
	seats, _, _ := p.f.s.ListSeats(p.f.ctx, l.ID, store.All)
	l.Seats = seats
	return l
}

func devicesOf(l *model.License) map[string]string {
	out := map[string]string{}
	for _, a := range l.Activations {
		out[a.Identifier] = a.MemberEmail
	}
	return out
}

// The portal's licence list gives each person their own key: the owner
// the licence's with every device, a member theirs with their own
// devices, and anyone else, including a pending invite and a removed
// member, nothing, never the owner's key.
func TestPortalLicenseView(t *testing.T) {
	p := newPortalTeam(t)
	ctx, s, lic := p.f.ctx, p.f.s, p.f.licA

	l := p.listed(t, lic.Email)
	key, ok, err := PortalLicenseView(ctx, s, l, strings.ToUpper(lic.Email))
	if err != nil || !ok || key != lic.LicenseKey {
		t.Fatalf("owner view = %q, %v, %v; want the licence key", key, ok, err)
	}
	if got := devicesOf(l); len(got) != 2 || got["owner-pc"] != "" || got["alice-pc"] != p.alice.Email {
		t.Fatalf("owner sees %v", got)
	}
	pendingListed := false
	for _, seat := range l.Seats {
		pendingListed = pendingListed || seat.ID == p.pending.ID
	}
	if !pendingListed {
		t.Fatal("the owner's list leaves out the pending invitation")
	}

	l = p.listed(t, p.alice.Email)
	l.StripeCustomerID, l.StripeSubscriptionID, l.Notes, l.MarketingConsent, l.UserID = "cus_1", "sub_1", "vip", true, "u1"
	key, ok, err = PortalLicenseView(ctx, s, l, p.alice.Email)
	if err != nil || !ok || key != p.aliceKey {
		t.Fatalf("member view = %q, %v, %v; want her own key", key, ok, err)
	}
	// The owner's billing, notes and consent are not hers to see.
	if l.StripeCustomerID != "" || l.StripeSubscriptionID != "" || l.Notes != "" || l.MarketingConsent || l.UserID != "" {
		t.Fatalf("member view keeps the owner's details: %q %q %q %v %q",
			l.StripeCustomerID, l.StripeSubscriptionID, l.Notes, l.MarketingConsent, l.UserID)
	}
	for _, seat := range l.Seats {
		if seat.UserID != "" {
			t.Fatalf("member view shows %s's account", seat.Email)
		}
	}
	if got := devicesOf(l); len(got) != 1 || got["alice-pc"] != p.alice.Email {
		t.Fatalf("member sees %v, want her alice-pc only", got)
	}
	// The team as a member sees it: who has joined, not the pending
	// invitation or the address it went to.
	for _, seat := range l.Seats {
		if seat.AcceptedAt == nil || seat.RemovedAt != nil {
			t.Fatalf("member's list holds %s, not a joined member", seat.Email)
		}
	}
	if len(l.Seats) == 0 {
		t.Fatal("member's list holds no one, not even herself")
	}

	for _, who := range []string{p.pending.Email, p.gone.Email, "stranger-" + p.f.suffix + "@example.com"} {
		l = p.listed(t, who)
		key, ok, err = PortalLicenseView(ctx, s, l, who)
		if err != nil || ok || key != "" {
			t.Fatalf("view for %s = %q, %v, %v; want nothing", who, key, ok, err)
		}
	}

	// While the product has no seats (its type changed), a member gets
	// nothing: their key opens nothing then. The owner is unaffected.
	if _, err := s.DB.NewRaw("UPDATE products SET type = 'desktop' WHERE id = ?", lic.ProductID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	l = p.listed(t, p.alice.Email)
	if key, ok, err = PortalLicenseView(ctx, s, l, p.alice.Email); err != nil || ok || key != "" {
		t.Fatalf("member view on a product without seats = %q, %v, %v; want nothing", key, ok, err)
	}
	l = p.listed(t, lic.Email)
	if key, ok, err = PortalLicenseView(ctx, s, l, lic.Email); err != nil || !ok || key != lic.LicenseKey {
		t.Fatalf("owner view on a product without seats = %q, %v, %v", key, ok, err)
	}
	if _, err := s.DB.NewRaw("UPDATE products SET type = 'hybrid' WHERE id = ?", lic.ProductID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// A member removed after the list was read gets nothing either, not
	// the key the list's copy of their seat still holds.
	l = p.listed(t, p.alice.Email)
	if _, _, err := s.RemoveSeat(ctx, p.alice.ID); err != nil {
		t.Fatal(err)
	}
	key, ok, err = PortalLicenseView(ctx, s, l, p.alice.Email)
	if err != nil || ok || key != "" {
		t.Fatalf("view for a member removed since the list = %q, %v, %v; want nothing", key, ok, err)
	}
}

// The portal guards: licence operations for the owner and accepted
// members, team changes for the owner alone; anyone else gets the 404
// an unknown licence gets.
func TestPortalGuards(t *testing.T) {
	p := newPortalTeam(t)
	lic := p.f.licA
	gin.SetMode(gin.TestMode)
	call := func(guard gin.HandlerFunc, email, body string) int {
		t.Helper()
		r := gin.New()
		r.POST("/x", func(c *gin.Context) {
			if email != "" {
				c.Set("email", email)
			}
			c.Next()
		}, guard, func(c *gin.Context) { c.Status(http.StatusTeapot) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)))
		return w.Code
	}
	withKey := func(key string) string { return fmt.Sprintf(`{"license_key":%q}`, key) }
	const passed = http.StatusTeapot

	licenseGuard := PortalLicenseGuard(p.f.s)
	for _, c := range []struct {
		name, email, body string
		want              int
	}{
		{"owner", lic.Email, withKey(lic.LicenseKey), passed},
		{"owner, other case", strings.ToUpper(lic.Email), withKey(lic.LicenseKey), passed},
		{"member with her key", p.alice.Email, withKey(p.aliceKey), passed},
		{"member with the licence key", p.alice.Email, withKey(lic.LicenseKey), passed},
		{"pending invite", p.pending.Email, withKey(lic.LicenseKey), http.StatusNotFound},
		{"removed member", p.gone.Email, withKey(lic.LicenseKey), http.StatusNotFound},
		{"stranger", "stranger@example.com", withKey(lic.LicenseKey), http.StatusNotFound},
		{"no key: the handler answers", p.pending.Email, `{}`, passed},
		{"no session email", "", withKey(lic.LicenseKey), http.StatusUnauthorized},
		{"body too large", lic.Email, `{"license_key":"` + strings.Repeat("x", portalGuardMaxBody) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		if got := call(licenseGuard, c.email, c.body); got != c.want {
			t.Errorf("licence guard, %s = %d, want %d", c.name, got, c.want)
		}
	}

	// A lookup that fails for any reason but "no such licence" stops at
	// the guard instead of skipping its check.
	broken, err := store.New(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	_ = broken.Close()
	for name, guard := range map[string]gin.HandlerFunc{"licence": PortalLicenseGuard(broken), "owner": PortalSeatOwnerGuard(broken)} {
		if got := call(guard, lic.Email, withKey(lic.LicenseKey)); got != http.StatusInternalServerError {
			t.Errorf("%s guard on a failed lookup = %d, want 500", name, got)
		}
	}

	ownerGuard := PortalSeatOwnerGuard(p.f.s)
	for _, c := range []struct {
		name, email, body string
		want              int
	}{
		{"owner", lic.Email, withKey(lic.LicenseKey), passed},
		{"owner with a member's key", lic.Email, withKey(p.aliceKey), passed},
		{"member", p.alice.Email, withKey(p.aliceKey), http.StatusNotFound},
		{"member with the licence key", p.alice.Email, withKey(lic.LicenseKey), http.StatusNotFound},
		{"pending invite", p.pending.Email, withKey(lic.LicenseKey), http.StatusNotFound},
		{"stranger", "stranger@example.com", withKey(lic.LicenseKey), http.StatusNotFound},
		{"no session email", "", withKey(lic.LicenseKey), http.StatusUnauthorized},
	} {
		if got := call(ownerGuard, c.email, c.body); got != c.want {
			t.Errorf("owner guard, %s = %d, want %d", c.name, got, c.want)
		}
	}
}

// With two accepted seats for one person (left in different case by
// older code), the portal takes the oldest, as Store.FindSeatByEmail
// does, so the key it lists and the devices it shows agree.
func TestPortalViewerSeat_Oldest(t *testing.T) {
	now := time.Now()
	older := &model.Seat{ID: "b-older", Email: "Bob@x.com", AcceptedAt: &now, CreatedAt: now.Add(-time.Hour)}
	newer := &model.Seat{ID: "a-newer", Email: "bob@x.com", AcceptedAt: &now, CreatedAt: now}
	pending := &model.Seat{ID: "c-pending", Email: "BOB@x.com", CreatedAt: now.Add(-2 * time.Hour)}
	lic := &model.License{Email: "owner@x.com", Seats: []*model.Seat{newer, pending, older}}
	if got := PortalViewerSeat(lic, "bob@x.com"); got == nil || got.ID != older.ID {
		t.Fatalf("viewer seat = %v, want the oldest accepted %s", got, older.ID)
	}
	if got := PortalViewerSeat(lic, "owner@x.com"); got != nil {
		t.Fatalf("the owner got seat %v", got)
	}
}

// Over the seat limit, the portal tells the paused member why their key
// does not work, and marks them for the owner.
func TestPortalLicenseView_PausedMember(t *testing.T) {
	p := newPortalTeam(t)
	ctx, s, lic := p.f.ctx, p.f.s, p.f.licA
	if _, err := s.DB.NewRaw("UPDATE plans SET max_seats = 1 WHERE id = ?", lic.PlanID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	l := p.listed(t, p.alice.Email)
	if _, ok, err := PortalLicenseView(ctx, s, l, p.alice.Email); err != nil || !ok || !l.KeySeatPaused {
		t.Fatalf("member view: ok %v, paused %v, %v; want her told she is paused", ok, l.KeySeatPaused, err)
	}
	l = p.listed(t, lic.Email)
	if _, ok, err := PortalLicenseView(ctx, s, l, lic.Email); err != nil || !ok || l.KeySeatPaused {
		t.Fatalf("owner view: ok %v, paused %v, %v; the owner is never paused", ok, l.KeySeatPaused, err)
	}
	marked := false
	for _, seat := range l.Seats {
		if seat.ID == p.alice.ID {
			marked = seat.Paused
		}
		if seat.ID == p.pending.ID && seat.Paused {
			t.Fatal("a pending invitation is marked paused; it has no access to pause")
		}
	}
	if !marked {
		t.Fatal("the owner's list does not mark alice paused")
	}
}
