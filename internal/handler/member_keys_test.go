package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// The portal's device endpoints with members' own keys: a member sees
// and removes only their own devices, the owner sees everyone's and
// whose each is, and only an admin reissues the licence's key.
func TestPortalDevicesWithMemberKeys(t *testing.T) {
	f := newTeamFixture(t)
	h := NewPortalActivationsHandler(f.s, nil)
	lic := f.licA

	join := func(who string, accept bool) *model.Seat {
		t.Helper()
		plain, hash := store.GenerateSeatInviteToken()
		expires := time.Now().Add(time.Hour)
		seat := &model.Seat{LicenseID: lic.ID, Email: who + "-" + f.suffix + "@example.com", Role: "member",
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
	alice, bob := join("alice", true), join("bob", true)
	pending := join("pending", false)
	aliceKey, err := f.s.MemberKey(f.ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct{ seat, key, device string }{
		{"", lic.LicenseKey, "owner-pc"}, {alice.ID, aliceKey, "alice-pc"},
	} {
		if err := f.s.ActivateForKey(f.ctx, &model.Activation{LicenseID: lic.ID, Identifier: d.device,
			IdentifierType: "device", SeatID: d.seat}, 5, d.key); err != nil {
			t.Fatal(err)
		}
	}
	deviceID := func(device string) string {
		a, err := f.s.FindActivation(f.ctx, lic.ID, device)
		if err != nil {
			return ""
		}
		return a.ID
	}

	call := func(fn gin.HandlerFunc, method, email, key string, extra ...gin.Param) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/portal/licenses/x", nil)
		c.Params = append(gin.Params{{Key: "license_key", Value: key}}, extra...)
		c.Set("email", email)
		fn(c)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return c.Writer.Status(), out
	}
	devices := func(email, key string) map[string]string {
		t.Helper()
		code, out := call(h.List, http.MethodGet, email, key)
		if code != http.StatusOK {
			t.Fatalf("list as %s = %d %v", email, code, out)
		}
		d, _ := out["data"].(map[string]any)
		rows, _ := d["activations"].([]any)
		got := map[string]string{}
		for _, r := range rows {
			m := r.(map[string]any)
			who, _ := m["member_email"].(string)
			got[m["identifier"].(string)] = who
		}
		return got
	}

	if got := devices(alice.Email, aliceKey); len(got) != 1 || got["alice-pc"] != alice.Email {
		t.Fatalf("alice sees %v, want her alice-pc only", got)
	}
	if got := devices(lic.Email, lic.LicenseKey); len(got) != 2 || got["owner-pc"] != "" || got["alice-pc"] != alice.Email {
		t.Fatalf("owner sees %v, want both devices and whose each is", got)
	}
	// Another member sees none of them, whichever key is in the URL.
	if got := devices(bob.Email, lic.LicenseKey); len(got) != 0 {
		t.Fatalf("bob sees %v, want none", got)
	}
	if code, _ := call(h.List, http.MethodGet, pending.Email, lic.LicenseKey); code != http.StatusNotFound {
		t.Fatalf("an invite not yet accepted lists devices: %d", code)
	}

	// A member removes their own devices, not the owner's.
	if code, _ := call(h.Delete, http.MethodDelete, alice.Email, aliceKey,
		gin.Param{Key: "activation_id", Value: deviceID("owner-pc")}); code != http.StatusNotFound {
		t.Fatalf("alice removing the owner's device = %d, want 404", code)
	}
	if deviceID("owner-pc") == "" {
		t.Fatal("alice removed the owner's device")
	}

	// The portal has no way to change the licence's key: only an admin
	// reissues one. When they do, the owner's devices go with the old
	// key and the members keep theirs.
	admin := &AdminHandler{Store: f.s}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/licenses/"+lic.ID+"/rotate-key", nil)
	c.Params = gin.Params{{Key: "id", Value: lic.ID}}
	admin.RotateLicenseKey(c)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	d, _ := out["data"].(map[string]any)
	newKey, _ := d["license_key"].(string)
	if w.Code != http.StatusOK || newKey == "" || newKey == lic.LicenseKey || d["devices_removed"] != float64(1) {
		t.Fatalf("admin reissuing the key = %d %v", w.Code, out)
	}
	if code, _ := call(h.List, http.MethodGet, lic.Email, lic.LicenseKey); code != http.StatusNotFound {
		t.Fatalf("the old key still lists devices: %d", code)
	}
	if got := devices(lic.Email, newKey); len(got) != 1 || got["alice-pc"] != alice.Email {
		t.Fatalf("after the change the owner sees %v, want alice-pc only", got)
	}
	if got := devices(alice.Email, aliceKey); len(got) != 1 {
		t.Fatalf("alice's key stopped working with the owner's: %v", got)
	}
}

// Removing devices in the portal: the owner any, a member their own;
// another member's device is not theirs to see, let alone remove.
func TestPortalDeviceRemoval(t *testing.T) {
	f := newTeamFixture(t)
	h := NewPortalActivationsHandler(f.s, nil)
	lic := f.licA

	join := func(who string) (*model.Seat, string) {
		t.Helper()
		plain, hash := store.GenerateSeatInviteToken()
		expires := time.Now().Add(time.Hour)
		seat := &model.Seat{LicenseID: lic.ID, Email: who + "-rm-" + f.suffix + "@example.com", Role: "member",
			InviteTokenHash: hash, InviteExpiresAt: &expires}
		if _, _, err := f.s.AddSeatWithinLimit(f.ctx, seat, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.s.AcceptSeatInvite(f.ctx, plain); err != nil {
			t.Fatal(err)
		}
		seat, _ = f.s.FindSeatByID(f.ctx, seat.ID)
		key, err := f.s.MemberKey(f.ctx, seat)
		if err != nil {
			t.Fatal(err)
		}
		return seat, key
	}
	alice, aliceKey := join("alice")
	bob, bobKey := join("bob")
	for _, d := range []struct{ seat, key, device string }{
		{"", lic.LicenseKey, "owner-pc"}, {alice.ID, aliceKey, "alice-pc"}, {bob.ID, bobKey, "bob-pc"},
	} {
		if err := f.s.ActivateForKey(f.ctx, &model.Activation{LicenseID: lic.ID, Identifier: d.device,
			IdentifierType: "device", SeatID: d.seat}, 3, d.key); err != nil {
			t.Fatal(err)
		}
	}
	deviceID := func(device string) string {
		a, err := f.s.FindActivation(f.ctx, lic.ID, device)
		if err != nil {
			return ""
		}
		return a.ID
	}
	remove := func(email, key, device string) int {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodDelete, "/portal/licenses/x", nil)
		c.Params = gin.Params{{Key: "license_key", Value: key}, {Key: "activation_id", Value: deviceID(device)}}
		c.Set("email", email)
		h.Delete(c)
		return c.Writer.Status()
	}

	for _, device := range []string{"owner-pc", "bob-pc"} {
		if code := remove(alice.Email, aliceKey, device); code != http.StatusNotFound {
			t.Fatalf("alice removing %s = %d, want 404", device, code)
		}
		if deviceID(device) == "" {
			t.Fatalf("alice removed %s", device)
		}
	}
	if code := remove(alice.Email, aliceKey, "alice-pc"); code != http.StatusOK {
		t.Fatalf("alice removing her own device = %d", code)
	}
	if code := remove(lic.Email, lic.LicenseKey, "bob-pc"); code != http.StatusOK {
		t.Fatalf("owner removing bob's device = %d", code)
	}
}
