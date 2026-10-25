package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/crypto"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// acceptedMember invites who onto the licence and accepts the invite.
func acceptedMember(t *testing.T, s *store.Store, ctx context.Context, lic *model.License, who string) *model.Seat {
	t.Helper()
	plain, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	seat := &model.Seat{LicenseID: lic.ID, Email: who + "-" + lic.Email, Role: "member",
		InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := s.AddSeatWithinLimit(ctx, seat, 0); err != nil {
		t.Fatalf("invite %s: %v", who, err)
	}
	if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
		t.Fatalf("accept %s: %v", who, err)
	}
	accepted, err := s.FindSeatByID(ctx, seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}

func memberKey(t *testing.T, s *store.Store, ctx context.Context, seat *model.Seat) string {
	t.Helper()
	key, err := s.MemberKey(ctx, seat)
	if err != nil {
		t.Fatalf("member key: %v", err)
	}
	return key
}

func activateFor(s *store.Store, ctx context.Context, lic *model.License, seatID, key, device string) error {
	return s.ActivateForKey(ctx, &model.Activation{LicenseID: lic.ID, Identifier: device,
		IdentifierType: "device", SeatID: seatID}, 10, key)
}

func deviceSeat(t *testing.T, s *store.Store, ctx context.Context, lic *model.License, device string) (string, bool) {
	t.Helper()
	a, err := s.FindActivation(ctx, lic.ID, device)
	if err != nil {
		return "", false
	}
	return a.SeatID, true
}

// A member's key is their own: made once, kept, with the product's
// prefix, and it opens the licence as that member. An invite not yet
// accepted has none.
func TestMemberKey_MadeOnceAndOpensTheLicence(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, lic := perSeatLicense(t, s, ctx, 5, false)

	plain, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	pending := &model.Seat{LicenseID: lic.ID, Email: "pending-" + lic.Email, Role: "member",
		InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := s.AddSeatWithinLimit(ctx, pending, 0); err != nil {
		t.Fatal(err)
	}
	if key := memberKey(t, s, ctx, pending); key != "" {
		t.Fatalf("an invite not yet accepted got key %q", key)
	}
	if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
		t.Fatal(err)
	}
	seat, err := s.FindSeatByID(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Many first visits at once still make one key.
	keys := make([]string, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fresh, err := s.FindSeatByID(ctx, seat.ID)
			if err != nil {
				t.Error(err)
				return
			}
			keys[i] = memberKey(t, s, ctx, fresh)
		}(i)
	}
	wg.Wait()
	for _, k := range keys {
		if k == "" || k != keys[0] {
			t.Fatalf("first visits made different keys: %q", keys)
		}
	}
	if !strings.HasPrefix(keys[0], prod.LicensePrefix+"-") {
		t.Fatalf("member key %q lacks the product prefix %q", keys[0], prod.LicensePrefix)
	}
	if keys[0] == lic.LicenseKey {
		t.Fatal("the member key is the owner's key")
	}
	seat, _ = s.FindSeatByID(ctx, seat.ID)
	if again := memberKey(t, s, ctx, seat); again != keys[0] {
		t.Fatalf("member key changed from %q to %q", keys[0], again)
	}

	found, err := s.FindLicenseByKey(ctx, keys[0])
	if err != nil {
		t.Fatalf("find by member key: %v", err)
	}
	if found.ID != lic.ID || found.KeySeat == nil || found.KeySeat.ID != seat.ID {
		t.Fatalf("member key found licence %s as seat %v, want %s as %s", found.ID, found.KeySeat, lic.ID, seat.ID)
	}
	owner, err := s.FindLicenseByKey(ctx, lic.LicenseKey)
	if err != nil || owner.KeySeat != nil {
		t.Fatalf("owner key found seat %v, err %v; want no seat", owner.KeySeat, err)
	}
}

// Removing a member clears their key and takes away the devices it
// activated, and only those.
func TestRemoveSeat_ClearsKeyAndDevices(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	bob := acceptedMember(t, s, ctx, lic, "bob")
	aliceKey, bobKey := memberKey(t, s, ctx, alice), memberKey(t, s, ctx, bob)

	for _, d := range []struct{ seat, key, device string }{
		{"", lic.LicenseKey, "owner-pc"},
		{alice.ID, aliceKey, "alice-pc"},
		{alice.ID, aliceKey, "alice-laptop"},
		{bob.ID, bobKey, "bob-pc"},
	} {
		if err := activateFor(s, ctx, lic, d.seat, d.key, d.device); err != nil {
			t.Fatalf("activate %s: %v", d.device, err)
		}
	}

	removed, now, err := s.RemoveSeat(ctx, alice.ID)
	if err != nil || !now {
		t.Fatalf("remove alice = %v, removed now %v", err, now)
	}
	if again, now, err := s.RemoveSeat(ctx, alice.ID); err != nil || now || len(again) != 0 {
		t.Fatalf("removing alice again = %v devices, removed now %v, %v; want nothing", len(again), now, err)
	}
	if len(removed) != 2 {
		t.Fatalf("removing alice removed %d devices, want her 2", len(removed))
	}
	if _, err := s.FindLicenseByKey(ctx, aliceKey); err == nil {
		t.Fatal("a removed member's key still opens the licence")
	}
	for _, d := range []string{"alice-pc", "alice-laptop"} {
		if _, ok := deviceSeat(t, s, ctx, lic, d); ok {
			t.Fatalf("%s stayed after its member was removed", d)
		}
	}
	for _, d := range []string{"owner-pc", "bob-pc"} {
		if _, ok := deviceSeat(t, s, ctx, lic, d); !ok {
			t.Fatalf("%s went with alice", d)
		}
	}
	if _, err := s.FindLicenseByKey(ctx, bobKey); err != nil {
		t.Fatalf("bob's key stopped working: %v", err)
	}
	// Activating with the cleared key is refused.
	if err := activateFor(s, ctx, lic, alice.ID, aliceKey, "alice-new"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("activate with a removed member's key = %v, want ErrKeyRevoked", err)
	}

	// Invited again, she starts with a new key.
	back := acceptedMember(t, s, ctx, lic, "alice")
	if back.ID != alice.ID {
		t.Fatalf("re-invite made seat %s, want the old row %s back", back.ID, alice.ID)
	}
	if k := memberKey(t, s, ctx, back); k == "" || k == aliceKey {
		t.Fatalf("back on the team with key %q, want a new one (old %q)", k, aliceKey)
	}
}

// A device stays whoever's activated it: another member's key is
// refused, and so is one on a device activated with the licence's key,
// which is the owner's; the licence's key finds any activated. The one
// move is a device from before members had keys of their own, when they
// used the licence's: it goes, once, to the member whose key activates
// it, and so outlives a later change of the licence's key.
func TestActivateForKey_WhoseDevice(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	aliceKey := memberKey(t, s, ctx, alice)
	bob := acceptedMember(t, s, ctx, lic, "bob")
	bobKey := memberKey(t, s, ctx, bob)

	// The owner's device, activated now, is not a member's to take.
	if err := activateFor(s, ctx, lic, "", lic.LicenseKey, "owner-pc"); err != nil {
		t.Fatal(err)
	}
	if err := activateFor(s, ctx, lic, alice.ID, aliceKey, "owner-pc"); !errors.Is(err, store.ErrDeviceHeld) {
		t.Fatalf("activate the owner's device with alice's key = %v, want ErrDeviceHeld", err)
	}
	if seat, _ := deviceSeat(t, s, ctx, lic, "owner-pc"); seat != "" {
		t.Fatalf("the owner's device moved to %q", seat)
	}

	// One from the shared-key days moves to the first member who
	// activates it, and only once.
	if err := activateFor(s, ctx, lic, "", lic.LicenseKey, "shared-pc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE activations SET predates_member_keys = true WHERE license_id = ? AND identifier = 'shared-pc'",
		lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := activateFor(s, ctx, lic, alice.ID, aliceKey, "shared-pc"); !errors.Is(err, store.ErrDeviceMoved) {
		t.Fatalf("activate a shared-key device with alice's key = %v, want ErrDeviceMoved", err)
	}
	if seat, _ := deviceSeat(t, s, ctx, lic, "shared-pc"); seat != alice.ID {
		t.Fatalf("device is %q's, want moved to alice", seat)
	}
	if err := activateFor(s, ctx, lic, bob.ID, bobKey, "shared-pc"); !errors.Is(err, store.ErrDeviceHeld) {
		t.Fatalf("activate alice's machine with bob's key = %v, want ErrDeviceHeld", err)
	}
	if err := activateFor(s, ctx, lic, "", lic.LicenseKey, "shared-pc"); !errors.Is(err, store.ErrAlreadyActivated) || errors.Is(err, store.ErrDeviceMoved) {
		t.Fatalf("activate alice's machine with the licence's key = %v, want ErrAlreadyActivated, nothing moved", err)
	}
	if seat, _ := deviceSeat(t, s, ctx, lic, "shared-pc"); seat != alice.ID {
		t.Fatalf("alice's device moved to %q", seat)
	}
	var stillShared bool
	if err := s.DB.NewRaw("SELECT predates_member_keys FROM activations WHERE license_id = ? AND identifier = 'shared-pc'",
		lic.ID).Scan(ctx, &stillShared); err != nil || stillShared {
		t.Fatalf("moved device still marked from the shared-key days (%v)", err)
	}
	if n, _ := s.CountActivations(ctx, lic.ID); n != 2 {
		t.Fatalf("activating again made %d activations, want 2", n)
	}

	// Moved onto her key, her machine outlives a change of the licence's;
	// the owner's goes with the old key.
	if _, _, err := s.RotateLicenseKey(ctx, lic.ID); err != nil {
		t.Fatal(err)
	}
	if seat, ok := deviceSeat(t, s, ctx, lic, "shared-pc"); !ok || seat != alice.ID {
		t.Fatal("alice's moved device went with the licence's old key")
	}
	if _, ok := deviceSeat(t, s, ctx, lic, "owner-pc"); ok {
		t.Fatal("the owner's device stayed after the key changed")
	}
}

// Floating sessions follow the same rules as devices: whose key checked
// a session out, a member renewing or releasing only their own and
// refused anyone else's running session, the licence's key renewing
// any, and removal or a key change ending the sessions that went with
// them. Being short leases, none ever moves: a lapsed one goes to
// whoever checks the machine out next.
func TestFloatingSessionsWithMemberKeys(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, true)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	bob := acceptedMember(t, s, ctx, lic, "bob")
	aliceKey, bobKey := memberKey(t, s, ctx, alice), memberKey(t, s, ctx, bob)

	checkout := func(seatID, key, machine string) error {
		_, err := s.CheckOutFloatingForKey(ctx, &model.FloatingSession{LicenseID: lic.ID, Identifier: machine,
			SeatID: seatID, ExpiresAt: time.Now().Add(time.Hour)}, 10, key)
		return err
	}
	holder := func(machine string) (string, bool) {
		t.Helper()
		sess, err := s.FindFloatingSession(ctx, lic.ID, machine)
		if err != nil {
			return "", false
		}
		return sess.SeatID, true
	}
	for _, c := range []struct{ seat, key, machine string }{
		{alice.ID, aliceKey, "alice-ws"}, {bob.ID, bobKey, "bob-ws"}, {"", lic.LicenseKey, "owner-ws"}, {"", lic.LicenseKey, "legacy-ws"},
	} {
		if err := checkout(c.seat, c.key, c.machine); err != nil {
			t.Fatalf("checkout %s: %v", c.machine, err)
		}
	}
	if who, _ := holder("alice-ws"); who != alice.ID {
		t.Fatalf("alice-ws is %q's", who)
	}

	// Another member's running session is not bob's to take, renew or end.
	if err := checkout(bob.ID, bobKey, "alice-ws"); !errors.Is(err, store.ErrSessionHeld) {
		t.Fatalf("bob checking out alice's machine = %v, want ErrSessionHeld", err)
	}
	if ok, err := s.HeartbeatFloating(ctx, lic.ID, "alice-ws", bob.ID, time.Now().Add(time.Hour)); err != nil || ok {
		t.Fatalf("bob renewed alice's session (%v, %v)", ok, err)
	}
	if err := s.CheckInFloating(ctx, lic.ID, "alice-ws", bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := holder("alice-ws"); !ok {
		t.Fatal("bob ended alice's session")
	}
	// The licence's key renews anyone's, which stays theirs.
	if err := checkout("", lic.LicenseKey, "alice-ws"); err != nil {
		t.Fatal(err)
	}
	if who, _ := holder("alice-ws"); who != alice.ID {
		t.Fatalf("renewed with the licence's key, alice-ws became %q's", who)
	}
	// Nor is a running session on the licence's key a member's to take.
	if err := checkout(bob.ID, bobKey, "legacy-ws"); !errors.Is(err, store.ErrSessionHeld) {
		t.Fatalf("bob checking out the licence key's session = %v, want ErrSessionHeld", err)
	}
	if who, _ := holder("legacy-ws"); who != "" {
		t.Fatalf("legacy-ws is %q's, want still the licence key's", who)
	}

	// A lapsed session is no one's: the machine checked out again is the
	// new holder's, who can then renew it.
	if err := checkout(alice.ID, aliceKey, "shared-ws"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE floating_sessions SET expires_at = now() - interval '1 minute' WHERE license_id = ? AND identifier = 'shared-ws'",
		lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := checkout(bob.ID, bobKey, "shared-ws"); err != nil {
		t.Fatalf("bob checking out a lapsed session: %v", err)
	}
	if who, _ := holder("shared-ws"); who != bob.ID {
		t.Fatalf("shared-ws is %q's after bob checked it out again, want bob's", who)
	}
	if ok, err := s.HeartbeatFloating(ctx, lic.ID, "shared-ws", bob.ID, time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("bob cannot renew the session he checked out (%v, %v)", ok, err)
	}

	// Removing alice ends her session; changing the key ends the
	// sessions on the licence's key; bob's stay.
	if _, _, err := s.RemoveSeat(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := holder("alice-ws"); ok {
		t.Fatal("a removed member's session stayed")
	}
	if err := checkout(alice.ID, aliceKey, "alice-ws"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("checkout with a removed member's key = %v, want ErrKeyRevoked", err)
	}
	if _, _, err := s.RotateLicenseKey(ctx, lic.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"owner-ws", "legacy-ws"} {
		if _, ok := holder(m); ok {
			t.Fatalf("%s, on the old licence key, stayed", m)
		}
	}
	for _, m := range []string{"bob-ws", "shared-ws"} {
		if _, ok := holder(m); !ok {
			t.Fatalf("bob's %s went with the licence's key", m)
		}
	}
}

// Changing the licence's key stops the old one at once and removes the
// devices it activated; the members keep their keys and devices.
func TestRotateLicenseKey(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	aliceKey := memberKey(t, s, ctx, alice)
	if err := activateFor(s, ctx, lic, "", lic.LicenseKey, "owner-pc"); err != nil {
		t.Fatal(err)
	}
	if err := activateFor(s, ctx, lic, alice.ID, aliceKey, "alice-pc"); err != nil {
		t.Fatal(err)
	}

	key, removed, err := s.RotateLicenseKey(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if key == lic.LicenseKey || !strings.HasPrefix(key, prod.LicensePrefix+"-") {
		t.Fatalf("new key %q (old %q, prefix %q)", key, lic.LicenseKey, prod.LicensePrefix)
	}
	if len(removed) != 1 || removed[0].Identifier != "owner-pc" {
		t.Fatalf("rotation removed %v, want owner-pc only", removed)
	}
	if _, err := s.FindLicenseByKey(ctx, lic.LicenseKey); err == nil {
		t.Fatal("the old key still opens the licence")
	}
	found, err := s.FindLicenseByKey(ctx, key)
	if err != nil || found.ID != lic.ID {
		t.Fatalf("new key: %v", err)
	}
	reread, _ := s.FindLicenseByID(ctx, lic.ID)
	if got := s.DecryptLicenseKey(reread); got != key {
		t.Fatalf("stored key reads %q, want %q", got, key)
	}
	if _, err := s.FindLicenseByKey(ctx, aliceKey); err != nil {
		t.Fatalf("alice's key stopped working: %v", err)
	}
	if _, ok := deviceSeat(t, s, ctx, lic, "alice-pc"); !ok {
		t.Fatal("alice's device went with the owner's key")
	}
	// The old key cannot activate even with the licence in hand.
	if err := activateFor(s, ctx, lic, "", lic.LicenseKey, "owner-new"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("activate with the old key = %v, want ErrKeyRevoked", err)
	}
	if err := activateFor(s, ctx, lic, "", key, "owner-new"); err != nil {
		t.Fatalf("activate with the new key: %v", err)
	}
}

// A member removed while their activations race the removal is left
// with no device: each activation lands before the removal, which takes
// it away, or after, which refuses it.
func TestRemoveSeat_RacingActivations(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 50, false)

	for round := range 10 {
		who := "racer" + string(rune('a'+round))
		seat := acceptedMember(t, s, ctx, lic, who)
		key := memberKey(t, s, ctx, seat)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				err := activateFor(s, ctx, lic, seat.ID, key, who+"-dev-"+string(rune('0'+i)))
				if err != nil && !errors.Is(err, store.ErrKeyRevoked) {
					t.Errorf("activate: %v", err)
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.RemoveSeat(ctx, seat.ID); err != nil {
				t.Errorf("remove: %v", err)
			}
		}()
		wg.Wait()
		var left int
		if err := s.DB.NewRaw("SELECT COUNT(*) FROM activations WHERE seat_id = ?", seat.ID).Scan(ctx, &left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Fatalf("round %d: %d devices left on a removed member", round, left)
		}
	}
}

// The portal lists a licence to its owner and its accepted members; an
// invite not yet accepted shows nothing.
func TestListLicensesByEmail_LeavesOutPendingInvites(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	_, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	pending := &model.Seat{LicenseID: lic.ID, Email: "pending-" + lic.Email, Role: "member",
		InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := s.AddSeatWithinLimit(ctx, pending, 0); err != nil {
		t.Fatal(err)
	}
	member := acceptedMember(t, s, ctx, lic, "member")

	has := func(email string) bool {
		t.Helper()
		ls, err := s.ListLicensesByEmail(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			if l.ID == lic.ID {
				return true
			}
		}
		return false
	}
	if has(pending.Email) {
		t.Fatal("an invite not yet accepted lists the licence")
	}
	if !has(member.Email) || !has(lic.Email) {
		t.Fatal("the owner or an accepted member does not see the licence")
	}
}

// With license keys encrypted, a member key and a changed licence key
// are stored encrypted too, and read back the same.
func TestMemberKeys_Encrypted(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	aead, err := crypto.NewAESGCMFromHex(strings.Repeat("cd", crypto.MasterKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	s.LicenseKeyAEAD = aead
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	key := memberKey(t, s, ctx, alice)

	stored, err := s.FindSeatByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.MemberKeyEncrypted) == 0 {
		t.Fatal("member key stored without its ciphertext")
	}
	if got := memberKey(t, s, ctx, stored); got != key {
		t.Fatalf("member key reads %q, want %q", got, key)
	}

	rotated, _, err := s.RotateLicenseKey(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	reread, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reread.LicenseKeyEncrypted) == 0 {
		t.Fatal("changed key stored without its ciphertext")
	}
	// Read from the ciphertext alone.
	reread.LicenseKey = ""
	if got := s.DecryptLicenseKey(reread); got != rotated {
		t.Fatalf("changed key decrypts to %q, want %q", got, rotated)
	}
}

// Removing a member and changing the licence's key both wait on the
// licence row lock that activations decide under; without it, an
// activation checking its key just before either commits would leave a
// device on a key that no longer works.
func TestKeyChangesTakeTheLicenceLock(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	bob := acceptedMember(t, s, ctx, lic, "bob")
	carol := acceptedMember(t, s, ctx, lic, "carol")

	for name, change := range map[string]func() error{
		"remove a member":        func() error { _, _, err := s.RemoveSeat(ctx, alice.ID); return err },
		"change the key":         func() error { _, _, err := s.RotateLicenseKey(ctx, lic.ID); return err },
		"roll back an accept":    func() error { _, err := s.RollbackSeatAccept(ctx, bob.ID); return err },
		"reissue a member's key": func() error { _, _, err := s.RotateMemberKey(ctx, carol.ID); return err },
	} {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, "SELECT id FROM licenses WHERE id = ? FOR UPDATE", lic.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- change() }()
		select {
		case err := <-done:
			_ = tx.Rollback()
			t.Fatalf("%s finished (%v) while the licence row was locked", name, err)
		case <-time.After(300 * time.Millisecond):
		}
		_ = tx.Rollback()
		if err := <-done; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A member key is made only for a seat that is on the team and has none
// yet, whatever the caller's copy of the seat says.
func TestMemberKey_StaleSeat(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)

	alice := acceptedMember(t, s, ctx, lic, "alice")
	stale := *alice
	key := memberKey(t, s, ctx, alice)
	if got := memberKey(t, s, ctx, &stale); got != key {
		t.Fatalf("a stale copy of the seat made key %q over %q", got, key)
	}
	if _, err := s.FindLicenseByKey(ctx, key); err != nil {
		t.Fatalf("the first key was overwritten: %v", err)
	}

	bob := acceptedMember(t, s, ctx, lic, "bob")
	staleBob := *bob
	if _, _, err := s.RemoveSeat(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if got := memberKey(t, s, ctx, &staleBob); got != "" {
		t.Fatalf("a removed member got key %q", got)
	}
	var hash *string
	if err := s.DB.NewRaw("SELECT member_key_hash FROM seats WHERE id = ?", bob.ID).Scan(ctx, &hash); err != nil {
		t.Fatal(err)
	}
	if hash != nil {
		t.Fatal("a removed member's seat was given a key")
	}

	// Back on the team, the key from before stays refused, even by an
	// activation that found the licence with it before the removal.
	if _, _, err := s.RemoveSeat(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	back := acceptedMember(t, s, ctx, lic, "alice")
	if k := memberKey(t, s, ctx, back); k == "" || k == key {
		t.Fatalf("back on the team with key %q (old %q)", k, key)
	}
	if err := activateFor(s, ctx, lic, back.ID, key, "old-key-pc"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("activate with the key from before = %v, want ErrKeyRevoked", err)
	}
}

// An accept rolled back (the licence turned out unusable) leaves the
// seat as it was before: no key, and nothing a key made in the moment
// it stood accepted could have taken.
func TestRollbackSeatAccept_ClearsWhatTheKeyTook(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	key := memberKey(t, s, ctx, alice)
	if err := activateFor(s, ctx, lic, alice.ID, key, "alice-pc"); err != nil {
		t.Fatal(err)
	}
	gone, err := s.RollbackSeatAccept(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].Identifier != "alice-pc" {
		t.Fatalf("rollback reported %v removed, want alice-pc", gone)
	}
	if _, ok := deviceSeat(t, s, ctx, lic, "alice-pc"); ok {
		t.Fatal("a rolled back accept left the device its key activated")
	}
	if _, err := s.FindLicenseByKey(ctx, key); err == nil {
		t.Fatal("a rolled back accept left its key working")
	}
	if err := activateFor(s, ctx, lic, alice.ID, key, "alice-pc"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("activate with the rolled back key = %v, want ErrKeyRevoked", err)
	}
}

// The member keys migration, run on rows from before it: a team admin
// becomes a member, every existing activation is marked as predating
// members' keys while a new one is not, and seat_id starts empty. Run
// again it changes nothing; down drops what it added. The file runs as
// written against temporary tables of the same names, which the
// unqualified names resolve to first, so other tests' rows are untouched.
func TestMemberKeysMigration(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("../../db/migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("20261028_member_keys.up.sql"), read("20261028_member_keys.down.sql")

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{
		"CREATE TEMP TABLE seats (id text PRIMARY KEY, role text NOT NULL) ON COMMIT DROP",
		"CREATE TEMP TABLE activations (id text PRIMARY KEY) ON COMMIT DROP",
		"CREATE TEMP TABLE floating_sessions (id text PRIMARY KEY) ON COMMIT DROP",
		"INSERT INTO pg_temp.seats VALUES ('admin-seat', 'admin'), ('member-seat', 'member')",
		"INSERT INTO pg_temp.activations VALUES ('old-device')",
		"INSERT INTO pg_temp.floating_sessions VALUES ('old-session')",
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for run := 1; run <= 2; run++ {
		if _, err := tx.ExecContext(ctx, up); err != nil {
			t.Fatalf("up, run %d: %v", run, err)
		}
		var roles []string
		if err := tx.NewRaw("SELECT role FROM pg_temp.seats ORDER BY id").Scan(ctx, &roles); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(roles, []string{"member", "member"}) {
			t.Fatalf("run %d: roles %v, want both member", run, roles)
		}
		if run == 1 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO pg_temp.activations (id) VALUES ('new-device')"); err != nil {
				t.Fatal(err)
			}
		}
		var flags []bool
		if err := tx.NewRaw("SELECT predates_member_keys FROM pg_temp.activations ORDER BY id").Scan(ctx, &flags); err != nil {
			t.Fatal(err)
		}
		// new-device sorts before old-device.
		if !slices.Equal(flags, []bool{false, true}) {
			t.Fatalf("run %d: predates_member_keys (new, old) = %v, want [false true]", run, flags)
		}
		var withSeat int
		if err := tx.NewRaw(`SELECT (SELECT count(*) FROM pg_temp.activations WHERE seat_id IS NOT NULL) +
			(SELECT count(*) FROM pg_temp.floating_sessions WHERE seat_id IS NOT NULL)`).Scan(ctx, &withSeat); err != nil {
			t.Fatal(err)
		}
		if withSeat != 0 {
			t.Fatalf("run %d: %d rows start with a seat", run, withSeat)
		}
	}
	if _, err := tx.ExecContext(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left int
	if err := tx.NewRaw(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema LIKE 'pg_temp%' AND column_name IN ('seat_id', 'predates_member_keys', 'member_key', 'member_key_hash', 'member_key_encrypted')`).Scan(ctx, &left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("down left %d of the migration's columns", left)
	}
}

// A seat made before addresses were lower-cased is found whatever the
// case it is asked for in, as the portal's licence list finds it.
func TestFindSeatByEmail_IgnoresCase(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	seat := acceptedMember(t, s, ctx, lic, "mixed")
	if _, err := s.DB.NewRaw("UPDATE seats SET email = ? WHERE id = ?", "Mixed-Case-"+lic.Email, seat.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	for _, asked := range []string{"mixed-case-" + strings.ToLower(lic.Email), "MIXED-CASE-" + strings.ToUpper(lic.Email)} {
		found, err := s.FindSeatByEmail(ctx, lic.ID, asked)
		if err != nil || found.ID != seat.ID {
			t.Fatalf("find %q = %v, %v; want seat %s", asked, found.ID, err, seat.ID)
		}
	}
}

// A member's key opens the licence only while its product has seats:
// with the type changed to one without, the team cannot be managed, so
// a key that could not be taken away must not work either. Changed back,
// it works again.
func TestMemberKey_NeedsSeats(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	prod, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	key := memberKey(t, s, ctx, alice)
	setType := func(typ string) {
		t.Helper()
		if _, err := s.DB.NewRaw("UPDATE products SET type = ? WHERE id = ?", typ, prod.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	setType(model.ProductTypeDesktop)
	if _, err := s.FindLicenseByKey(ctx, key); err == nil {
		t.Fatal("a member's key opened a licence whose product has no seats")
	}
	if _, err := s.FindLicenseByKey(ctx, lic.LicenseKey); err != nil {
		t.Fatalf("the licence's own key stopped: %v", err)
	}
	if err := activateFor(s, ctx, lic, alice.ID, key, "alice-pc"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("activate with a member key on a product without seats = %v, want ErrKeyRevoked", err)
	}

	setType(model.ProductTypeHybrid)
	if found, err := s.FindLicenseByKey(ctx, key); err != nil || found.KeySeat == nil {
		t.Fatalf("member key with seats back = %v", err)
	}
	if err := activateFor(s, ctx, lic, alice.ID, key, "alice-pc"); err != nil {
		t.Fatalf("activate with seats back: %v", err)
	}
}

// A seat from before addresses were lower-cased is the same person as
// their address in lower case: inviting them again renews that seat
// rather than adding a second, and where both exist the accepted one is
// the one found.
func TestSeats_LegacyMixedCase(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	legacy := "Legacy-" + lic.Email
	if _, err := s.DB.NewRaw("INSERT INTO seats (id, license_id, email, role) VALUES (?, ?, ?, 'member')",
		"legacy-"+lic.ID, lic.ID, legacy).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	_, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	again := &model.Seat{LicenseID: lic.ID, Email: strings.ToLower(legacy), Role: "member", InviteTokenHash: hash, InviteExpiresAt: &expires}
	outcome, _, err := s.AddSeatWithinLimit(ctx, again, 0)
	if err != nil || outcome != store.SeatResent || again.ID != "legacy-"+lic.ID {
		t.Fatalf("invite the legacy address again = %v, %v, seat %s; want the legacy seat renewed", outcome, err, again.ID)
	}
	var live int
	if err := s.DB.NewRaw("SELECT count(*) FROM seats WHERE license_id = ? AND lower(email) = ? AND removed_at IS NULL",
		lic.ID, strings.ToLower(legacy)).Scan(ctx, &live); err != nil || live != 1 {
		t.Fatalf("%d live seats for the address (%v), want 1", live, err)
	}

	// Two live seats in different case (left by older code): the
	// accepted one is the one found.
	if _, err := s.DB.NewRaw("INSERT INTO seats (id, license_id, email, role, accepted_at) VALUES (?, ?, ?, 'member', now())",
		"accepted-"+lic.ID, lic.ID, strings.ToLower(legacy)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	found, err := s.FindSeatByEmail(ctx, lic.ID, legacy)
	if err != nil || found.ID != "accepted-"+lic.ID {
		t.Fatalf("find = %v, %v; want the accepted seat", found.ID, err)
	}
	// Invited again, they are already on the team: nothing to renew,
	// and no second seat to take up.
	_, hash2 := store.GenerateSeatInviteToken()
	again2 := &model.Seat{LicenseID: lic.ID, Email: strings.ToLower(legacy), Role: "member", InviteTokenHash: hash2, InviteExpiresAt: &expires}
	if outcome, _, err := s.AddSeatWithinLimit(ctx, again2, 0); err != nil || outcome != store.SeatExisted || again2.ID != "accepted-"+lic.ID {
		t.Fatalf("invite again = %v, %v, seat %s; want the accepted seat, unchanged", outcome, err, again2.ID)
	}
}

// A release of a device that races a member's activate moving it from
// the shared-key days waits for the move's decision, or the move waits
// for it: the member never hears "moved" about a device that is gone.
// Here the release is first; the activate then finds no device and
// activates the machine afresh as the member's.
func TestActivateForKey_MoveAfterRelease(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	key := memberKey(t, s, ctx, alice)
	if err := activateFor(s, ctx, lic, "", lic.LicenseKey, "old-pc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE activations SET predates_member_keys = true WHERE license_id = ? AND identifier = 'old-pc'", lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM activations WHERE license_id = ? AND identifier = 'old-pc'", lic.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- activateFor(s, ctx, lic, alice.ID, key, "old-pc") }()
	select {
	case err := <-done:
		_ = tx.Rollback()
		t.Fatalf("activate finished (%v) while the release held the device", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("activate after the release = %v, want a fresh activation", err)
	}
	if seat, ok := deviceSeat(t, s, ctx, lic, "old-pc"); !ok || seat != alice.ID {
		t.Fatalf("old-pc is %q's (present %v), want alice's", seat, ok)
	}
}

// Sending a pending invite again replaces its link and dates it anew:
// the old link stops, the new one joins, and "invited" is the latest
// send. Bringing a removed member back is dated anew the same way.
func TestSeats_InviteSentAgain(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	invite := func(email string) (*model.Seat, string, store.SeatAddOutcome) {
		t.Helper()
		plain, hash := store.GenerateSeatInviteToken()
		expires := time.Now().Add(time.Hour)
		seat := &model.Seat{LicenseID: lic.ID, Email: email, Role: "member", InviteTokenHash: hash, InviteExpiresAt: &expires}
		outcome, _, err := s.AddSeatWithinLimit(ctx, seat, 0)
		if err != nil {
			t.Fatal(err)
		}
		return seat, plain, outcome
	}
	invitedAt := func(id string) time.Time {
		t.Helper()
		seat, err := s.FindSeatByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return seat.InvitedAt
	}
	email := "again-" + lic.ID + "@example.com"
	first, oldLink, _ := invite(email)
	sentFirst := invitedAt(first.ID)
	time.Sleep(10 * time.Millisecond)
	again, newLink, outcome := invite(email)
	if outcome != store.SeatResent || again.ID != first.ID {
		t.Fatalf("send again = %v, seat %s; want seat %s renewed", outcome, again.ID, first.ID)
	}
	if got := invitedAt(first.ID); !got.After(sentFirst) || !again.InvitedAt.Equal(got) {
		t.Fatalf("invited %v after sending again (returned %v), first %v; want the latest send", got, again.InvitedAt, sentFirst)
	}
	if _, _, err := s.AcceptSeatInvite(ctx, oldLink); err == nil {
		t.Fatal("the replaced link still joins")
	}
	if _, _, err := s.AcceptSeatInvite(ctx, newLink); err != nil {
		t.Fatalf("the new link: %v", err)
	}

	if _, _, err := s.RemoveSeat(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	removedAt := invitedAt(first.ID)
	time.Sleep(10 * time.Millisecond)
	if back, _, outcome := invite(email); outcome != store.SeatRestored || back.ID != first.ID || !invitedAt(first.ID).After(removedAt) {
		t.Fatalf("bring back = %v, seat %s; want seat %s back and dated anew", outcome, back.ID, first.ID)
	}
}

// A pending invite whose old link is accepted while it is being sent
// again ends up accepted, not given a new link on top: the resend waits
// for the accept, and then finds the seat accepted.
func TestSeats_ResendRacingAccept(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	email := "race-" + lic.ID + "@example.com"
	_, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	seat := &model.Seat{LicenseID: lic.ID, Email: email, Role: "member", InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := s.AddSeatWithinLimit(ctx, seat, 0); err != nil {
		t.Fatal(err)
	}

	// Hold the seat row as an accept in flight does, then send again.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewRaw("SELECT id FROM seats WHERE id = ? FOR UPDATE", seat.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		outcome store.SeatAddOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		_, hash2 := store.GenerateSeatInviteToken()
		again := &model.Seat{LicenseID: lic.ID, Email: email, Role: "member", InviteTokenHash: hash2, InviteExpiresAt: &expires}
		outcome, _, err := s.AddSeatWithinLimit(ctx, again, 0)
		done <- result{outcome, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the resend did not wait for the seat row: %v, %v", r.outcome, r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.NewRaw("UPDATE seats SET accepted_at = now(), invite_token_hash = NULL WHERE id = ?", seat.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.outcome != store.SeatExisted {
		t.Fatalf("resend after the accept = %v, %v; want SeatExisted", r.outcome, r.err)
	}
	got, err := s.FindSeatByID(ctx, seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AcceptedAt == nil || got.InviteTokenHash != "" {
		t.Fatalf("seat after the race: accepted %v, token %q; want accepted with no invite", got.AcceptedAt, got.InviteTokenHash)
	}
}

// Member keys made before license key encryption was turned on are
// encrypted by the backfill, as licence keys are, and still read back
// the same; a removed member's cleared key is left alone.
func TestBackfill_EncryptsMemberKeys(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	key := memberKey(t, s, ctx, alice)
	bob := acceptedMember(t, s, ctx, lic, "bob")
	memberKey(t, s, ctx, bob)
	if _, _, err := s.RemoveSeat(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}

	aead, err := crypto.NewAESGCMFromHex(strings.Repeat("ef", crypto.MasterKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	s.LicenseKeyAEAD = aead
	if _, err := s.BackfillLicenseKeyEncrypted(ctx, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := s.FindSeatByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.MemberKeyEncrypted) == 0 {
		t.Fatal("the backfill left the member key without its ciphertext")
	}
	stored.MemberKey = "" // read from the ciphertext alone
	if got := memberKey(t, s, ctx, stored); got != key {
		t.Fatalf("member key reads %q after the backfill, want %q", got, key)
	}
	var left int
	if err := s.DB.NewRaw("SELECT count(*) FROM seats WHERE license_id = ? AND member_key IS NOT NULL AND member_key_encrypted IS NULL",
		lic.ID).Scan(ctx, &left); err != nil || left != 0 {
		t.Fatalf("%d member keys left unencrypted (%v)", left, err)
	}
}

// A seat removed by an older version kept its key and devices (that
// RemoveSeat only set removed_at); inviting the address back clears
// them, so the old key does not come back to life with the seat.
func TestSeats_RestoreClearsAnOldKey(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	key := memberKey(t, s, ctx, alice)
	if err := activateFor(s, ctx, lic, alice.ID, key, "alice-pc"); err != nil {
		t.Fatal(err)
	}
	// Removed the old way: removed_at alone.
	if _, err := s.DB.NewRaw("UPDATE seats SET removed_at = now() WHERE id = ?", alice.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	_, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	back := &model.Seat{LicenseID: lic.ID, Email: alice.Email, Role: "member", InviteTokenHash: hash, InviteExpiresAt: &expires}
	outcome, stale, err := s.AddSeatWithinLimit(ctx, back, 0)
	if err != nil || outcome != store.SeatRestored || back.ID != alice.ID {
		t.Fatalf("bring alice back = %v, %v, seat %s", outcome, err, back.ID)
	}
	// Returned, so the caller reports them as deactivated.
	if len(stale) != 1 || stale[0].Identifier != "alice-pc" {
		t.Fatalf("devices returned with the restore = %v, want alice-pc", stale)
	}
	got, err := s.FindSeatByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemberKeyHash != "" || got.MemberKey != "" || len(got.MemberKeyEncrypted) != 0 {
		t.Fatal("the seat came back with its old key")
	}
	if _, err := s.FindLicenseByKey(ctx, key); err == nil {
		t.Fatal("the old member key opens the licence again")
	}
	if _, ok := deviceSeat(t, s, ctx, lic, "alice-pc"); ok {
		t.Fatal("the old key's device came back with the seat")
	}
}

// Reissuing one member's key replaces theirs alone: the old key stops,
// the devices it activated go, and the new key opens the licence as
// theirs; the owner's and other members' keys and devices stay. A seat
// with no key to replace, pending or removed, is refused.
func TestRotateMemberKey(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	bob := acceptedMember(t, s, ctx, lic, "bob")
	aliceKey, bobKey := memberKey(t, s, ctx, alice), memberKey(t, s, ctx, bob)
	for _, d := range []struct{ seat, key, device string }{
		{"", lic.LicenseKey, "owner-pc"}, {alice.ID, aliceKey, "alice-pc"}, {bob.ID, bobKey, "bob-pc"},
	} {
		if err := activateFor(s, ctx, lic, d.seat, d.key, d.device); err != nil {
			t.Fatal(err)
		}
	}

	newKey, removed, err := s.RotateMemberKey(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newKey == "" || newKey == aliceKey || len(removed) != 1 || removed[0].Identifier != "alice-pc" {
		t.Fatalf("reissue = %q, removed %v", newKey, removed)
	}
	if _, err := s.FindLicenseByKey(ctx, aliceKey); err == nil {
		t.Fatal("alice's old key still opens the licence")
	}
	found, err := s.FindLicenseByKey(ctx, newKey)
	if err != nil || found.KeySeat == nil || found.KeySeat.ID != alice.ID {
		t.Fatalf("alice's new key = %v", err)
	}
	// The portal shows her the new one.
	fresh, err := s.FindSeatByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := memberKey(t, s, ctx, fresh); got != newKey {
		t.Fatalf("her key reads %q, want the new %q", got, newKey)
	}
	for _, k := range []string{lic.LicenseKey, bobKey} {
		if _, err := s.FindLicenseByKey(ctx, k); err != nil {
			t.Fatalf("another key stopped with hers: %v", err)
		}
	}
	for device, want := range map[string]bool{"alice-pc": false, "owner-pc": true, "bob-pc": true} {
		if _, ok := deviceSeat(t, s, ctx, lic, device); ok != want {
			t.Fatalf("%s present = %v, want %v", device, ok, want)
		}
	}
	// The old key cannot land a device after the reissue.
	if err := activateFor(s, ctx, lic, alice.ID, aliceKey, "late-pc"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("activate with the old key = %v, want ErrKeyRevoked", err)
	}

	_, hash := store.GenerateSeatInviteToken()
	expires := time.Now().Add(time.Hour)
	pending := &model.Seat{LicenseID: lic.ID, Email: "pending-" + lic.ID + "@example.com", Role: "member", InviteTokenHash: hash, InviteExpiresAt: &expires}
	if _, _, err := s.AddSeatWithinLimit(ctx, pending, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RemoveSeat(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"a pending invitation": pending.ID, "a removed member": bob.ID} {
		if _, _, err := s.RotateMemberKey(ctx, id); !errors.Is(err, store.ErrSeatNotJoined) {
			t.Fatalf("reissue for %s = %v, want ErrSeatNotJoined", name, err)
		}
	}
	if _, _, err := s.RotateMemberKey(ctx, "no-such-seat"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("reissue for no seat = %v, want sql.ErrNoRows", err)
	}
}

// The portal tells an owner their subscription is set to end: the
// licences whose subscription is recorded as ending at the period end.
func TestCancelScheduledLicenses(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, ending := perSeatLicense(t, s, ctx, 5, false)
	_, _, renewing := perSeatLicense(t, s, ctx, 5, false)
	_, _, unlinked := perSeatLicense(t, s, ctx, 5, false)
	_, _, ended := perSeatLicense(t, s, ctx, 5, false)
	for _, l := range []*model.License{ending, renewing, unlinked, ended} {
		if _, err := s.DB.NewRaw("UPDATE licenses SET payment_provider = 'stripe', stripe_subscription_id = ? WHERE id = ?",
			"sub_cs_"+l.ID, l.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSubscriptionCancelScheduled(ctx, l.ID, l != renewing, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// Unlinked from Stripe by an admin, or ended: nothing left to end.
	if _, err := s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = NULL WHERE id = ?", unlinked.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = 'canceled' WHERE id = ?", ended.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.CancelScheduledLicenses(ctx, []string{ending.ID, renewing.ID, unlinked.ID, ended.ID, "no-such-licence"})
	if err != nil {
		t.Fatal(err)
	}
	if !got[ending.ID] || len(got) != 1 {
		t.Fatalf("cancel scheduled = %v, want only the running licence set to end", got)
	}
	if got, err := s.CancelScheduledLicenses(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("for no licences = %v, %v", got, err)
	}
}

// A pending seat that still holds a key (an older version removed and
// invited it back without clearing it) gives its member a new key on
// accepting, not the one they had before.
func TestAccept_DropsALeftoverKey(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	old := memberKey(t, s, ctx, alice)
	plain, hash := store.GenerateSeatInviteToken()
	if _, err := s.DB.NewRaw(`UPDATE seats SET accepted_at = NULL, user_id = NULL, invite_token_hash = ?,
		invite_expires_at = now() + interval '1 hour' WHERE id = ?`, hash, alice.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptSeatInvite(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindLicenseByKey(ctx, old); err == nil {
		t.Fatal("the key from before opens the licence again")
	}
	fresh, err := s.FindSeatByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k := memberKey(t, s, ctx, fresh); k == "" || k == old {
		t.Fatalf("key after accepting = %q, want a new one", k)
	}
}

// With more people than seats, the members who joined last are paused:
// their key opens the licence but activates nothing, until a place is
// free again, by a removal or more seats, which needs nothing stored.
// The owner, and a seat at the owner's address, are never paused.
func TestSeatsBeyondTheLimitArePaused(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, plan, lic := perSeatLicense(t, s, ctx, 5, false)
	var seats []*model.Seat
	var keys []string
	for _, who := range []string{"a", "b", "c"} {
		seat := acceptedMember(t, s, ctx, lic, who)
		seats, keys = append(seats, seat), append(keys, memberKey(t, s, ctx, seat))
		time.Sleep(2 * time.Millisecond) // a join order that is unambiguous
	}
	paused := func() string {
		t.Helper()
		got, err := s.PausedSeats(ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for i, seat := range seats {
			if got[seat.ID] {
				out += []string{"a", "b", "c"}[i]
			}
		}
		return out
	}
	if p := paused(); p != "" {
		t.Fatalf("paused with room for everyone: %q", p)
	}

	// Three seats: the owner, a and b. c joined last and waits.
	setQuantity(t, s, ctx, lic.ID, 3)
	if p := paused(); p != "c" {
		t.Fatalf("paused on 3 seats = %q, want c", p)
	}
	if found, err := s.FindLicenseByKey(ctx, keys[2]); err != nil || !found.KeySeatPaused {
		t.Fatalf("c's key: %v, paused %v; want it found and paused", err, found != nil && found.KeySeatPaused)
	}
	if found, err := s.FindLicenseByKey(ctx, keys[0]); err != nil || found.KeySeatPaused {
		t.Fatalf("a's key: %v; want it found and not paused", err)
	}
	if err := activateFor(s, ctx, lic, seats[2].ID, keys[2], "c-pc"); !errors.Is(err, store.ErrKeyRevoked) {
		t.Fatalf("c activating while paused = %v, want ErrKeyRevoked", err)
	}
	if err := activateFor(s, ctx, lic, seats[0].ID, keys[0], "a-pc"); err != nil {
		t.Fatalf("a activating: %v", err)
	}

	// a leaves: c's turn, with nothing to do but read again.
	if _, _, err := s.RemoveSeat(ctx, seats[0].ID); err != nil {
		t.Fatal(err)
	}
	if p := paused(); p != "" {
		t.Fatalf("paused after a left = %q, want nobody", p)
	}
	if err := activateFor(s, ctx, lic, seats[2].ID, keys[2], "c-pc"); err != nil {
		t.Fatalf("c activating once a place is free: %v", err)
	}

	// One seat: the owner alone; b and c both wait.
	setQuantity(t, s, ctx, lic.ID, 1)
	if p := paused(); p != "bc" {
		t.Fatalf("paused on 1 seat = %q, want bc", p)
	}

	// A plan not sold per seat counts max_seats; 0 is no limit.
	if _, err := s.DB.NewRaw("UPDATE plans SET per_seat = false, license_type = 'subscription', max_seats = 2 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if p := paused(); p != "c" {
		t.Fatalf("paused on a 2-person plan = %q, want c", p)
	}
	if _, err := s.DB.NewRaw("UPDATE plans SET max_seats = 0 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if p := paused(); p != "" {
		t.Fatalf("paused with no limit = %q, want nobody", p)
	}

	// The licence's address moves to b's: b is the owner now, never
	// paused, and holds no member's place.
	if _, err := s.DB.NewRaw("UPDATE plans SET max_seats = 1 WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET email = ? WHERE id = ?", strings.ToUpper(seats[1].Email), lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if p := paused(); p != "c" {
		t.Fatalf("paused with b as the owner on 1 seat = %q, want c", p)
	}
}

// A trial a Stripe subscription bills is left to Stripe at its end, for
// the plan's grace days (at least one), so a customer whose trial is
// turning into a paid subscription is not expired before Stripe says so;
// past that, with nothing heard, it is expired as any trial is.
func TestFindExpiredTrials_LeavesStripeTrialsToStripe(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	trial := func(name string, ago time.Duration, stripe bool) *model.License {
		t.Helper()
		_, _, lic := perSeatLicense(t, s, ctx, 1, false)
		sub := ""
		if stripe {
			sub = "sub_trial_" + name + "_" + lic.ID
		}
		if _, err := s.DB.NewRaw(`UPDATE licenses SET status = 'trialing', valid_until = ?, stripe_subscription_id = NULLIF(?, '')
			WHERE id = ?`, time.Now().Add(-ago), sub, lic.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.NewRaw("UPDATE plans SET grace_days = 0 WHERE id = (SELECT plan_id FROM licenses WHERE id = ?)", lic.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		return lic
	}
	plain := trial("plain", time.Hour, false)
	converting := trial("converting", time.Hour, true)
	silent := trial("silent", 50*time.Hour, true)
	found, err := s.FindExpiredTrials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, l := range found {
		got[l.ID] = true
	}
	if !got[plain.ID] || got[converting.ID] || !got[silent.ID] {
		t.Fatalf("expired trials: plain %v, converting Stripe trial %v, Stripe trial silent past a day %v; want true, false, true",
			got[plain.ID], got[converting.ID], got[silent.ID])
	}
}

// An invitation accepted while every seat is taken would only make a
// paused member: it is refused with nothing changed, the invitation
// still good, and accepted once a seat is free.
func TestAccept_WaitsForAFreeSeat(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 3, false)
	alice := acceptedMember(t, s, ctx, lic, "alice")
	acceptedMember(t, s, ctx, lic, "bob")
	plain, hash := store.GenerateSeatInviteToken()
	invited := &model.Seat{LicenseID: lic.ID, Email: "carol-" + lic.ID[:8] + "@example.com", Role: "member",
		InviteTokenHash: hash}
	until := time.Now().Add(time.Hour)
	invited.InviteExpiresAt = &until
	if err := s.CreateSeat(ctx, invited); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptSeatInvite(ctx, plain); !errors.Is(err, store.ErrSeatLimitReached) {
		t.Fatalf("accepting with every seat taken = %v, want ErrSeatLimitReached", err)
	}
	if got, err := s.FindSeatByID(ctx, invited.ID); err != nil || got.AcceptedAt != nil || got.InviteTokenHash != hash {
		t.Fatalf("after the refusal: %v, %+v; want the invitation as it was", err, got)
	}
	if _, _, err := s.RemoveSeat(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, seat, err := s.AcceptSeatInvite(ctx, plain); err != nil || seat.AcceptedAt == nil {
		t.Fatalf("accepting once a seat is free: %v", err)
	}
}

// The join order decides, ties broken by seat ID; a seat at the owner's
// address and a pending invitation neither wait nor hold a place; and
// the licence's list and each member's own check always agree.
func TestPausedSeats_OrderAndAgreement(t *testing.T) {
	s := setupTestDB(t)
	defer s.Close()
	ctx := context.Background()
	_, _, lic := perSeatLicense(t, s, ctx, 5, false)
	x := acceptedMember(t, s, ctx, lic, "x")
	y := acceptedMember(t, s, ctx, lic, "y")
	z := acceptedMember(t, s, ctx, lic, "z")
	joined := time.Now().Add(-time.Hour)
	if _, err := s.DB.NewRaw("UPDATE seats SET accepted_at = ? WHERE id IN (?, ?)", joined, x.ID, y.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ownerSeat := &model.Seat{LicenseID: lic.ID, Email: strings.ToUpper(lic.Email), Role: "member", AcceptedAt: &joined}
	pending := &model.Seat{LicenseID: lic.ID, Email: "pending-" + lic.ID[:8] + "@example.com", Role: "member"}
	for _, seat := range []*model.Seat{ownerSeat, pending} {
		if err := s.CreateSeat(ctx, seat); err != nil {
			t.Fatal(err)
		}
	}
	first, second := x, y // tied on the time: the lower ID came first
	if y.ID < x.ID {
		first, second = y, x
	}
	// Rewritten, the first is stored after the second, so an order that
	// fell back on how rows lie would get the tie the wrong way round.
	if _, err := s.DB.NewRaw("UPDATE seats SET role = role WHERE id = ?", first.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		quantity int
		want     []*model.Seat
	}{
		{4, nil},              // the owner and three members
		{3, []*model.Seat{z}}, // z joined last
		{2, []*model.Seat{second, z}},
		{1, []*model.Seat{first, second, z}},
	} {
		setQuantity(t, s, ctx, lic.ID, tc.quantity)
		got, err := s.PausedSeats(ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{}
		for _, seat := range tc.want {
			want[seat.ID] = true
		}
		for _, seat := range []*model.Seat{x, y, z, ownerSeat, pending} {
			one, err := s.SeatPaused(ctx, seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got[seat.ID] != want[seat.ID] || one != want[seat.ID] {
				t.Errorf("%d seats, %s: listed %v, own check %v, want %v", tc.quantity, seat.Email, got[seat.ID], one, want[seat.ID])
			}
		}
	}
}
