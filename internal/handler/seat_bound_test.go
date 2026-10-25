package handler

import (
	"context"
	"encoding/json"
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

// max_seats counts the owner, and the migration that made it so raised
// a plan at the old cap of 100000 to 100001. That value must stay a
// valid plan: UpdatePlan checks the merged row, so a cap left at 100000
// would refuse every later edit of such a plan, even a rename.
func TestSeatBoundFitsTheMigratedMaximum(t *testing.T) {
	// Literal values, not maxPlanSeats: the number that matters is what
	// the migration produced, whatever the constant says.
	if err := validatePlanNumericBounds(planBounds{MaxSeats: 100001}); err != nil {
		t.Errorf("max_seats 100001 refused: %v", err)
	}
	if err := validatePlanNumericBounds(planBounds{MaxSeats: 100002}); err == nil {
		t.Error("max_seats 100002 accepted")
	}

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
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Big", Slug: "big-seats-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	// As the migration leaves a plan that was at 100000.
	plan := &model.Plan{ProductID: prod.ID, Name: "Enterprise", Slug: "big-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxSeats: 100001, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	h := &AdminHandler{Store: s, FeedURLTTL: time.Hour}
	update := func(body string) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/admin/plans/"+plan.ID, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: plan.ID}}
		h.UpdatePlan(c)
		return w.Code
	}
	if code := update(`{"name":"Enterprise Plus"}`); code != http.StatusOK {
		t.Errorf("rename of a migrated 100001 seat plan = %d, want 200", code)
	}
	if code := update(`{"active":false}`); code != http.StatusOK {
		t.Errorf("deactivating a migrated 100001 seat plan = %d, want 200", code)
	}
	if code := update(`{"max_seats":100002}`); code != http.StatusBadRequest {
		t.Errorf("max_seats 100002 = %d, want 400", code)
	}
}

// Before an admin lowers max_seats the dashboard asks how many licenses
// would hold more people than the new limit, the owner counted, a seat
// at the owner's own address and removed seats not.
func TestPlanSeatImpact(t *testing.T) {
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
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Impact", Slug: "impact-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "impact-" + suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxSeats: 10, Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	licence := func(name string, members ...string) *model.License {
		l := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: name + "-" + suffix + "@example.com",
			LicenseKey: "KG-IMPACT-" + name + "-" + suffix, Status: model.StatusActive}
		if err := s.CreateLicense(ctx, l); err != nil {
			t.Fatal(err)
		}
		joined := time.Now()
		for _, m := range members {
			if err := s.CreateSeat(ctx, &model.Seat{LicenseID: l.ID, Email: m + "-" + suffix + "@example.com", Role: "member",
				AcceptedAt: &joined}); err != nil {
				t.Fatal(err)
			}
		}
		return l
	}
	licence("big", "a", "b", "c") // 4 people
	licence("small", "d")         // 2 people
	// 1 person: invitations not yet accepted do not count; nobody would
	// be paused.
	invited := licence("invited", "p1", "p2")
	if _, err := s.DB.NewUpdate().Model((*model.Seat)(nil)).Set("accepted_at = NULL").
		Where("license_id = ?", invited.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// 2 people: a removed seat and one at the owner's own address do not count.
	mixed := licence("mixed", "e", "gone", "MIXED")
	if _, err := s.DB.NewUpdate().Model((*model.Seat)(nil)).Set("removed_at = now()").
		Where("license_id = ? AND email = ?", mixed.ID, "gone-"+suffix+"@example.com").Exec(ctx); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	h := &AdminHandler{Store: s}
	impact := func(q string) (int, map[string]any) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/plans/"+plan.ID+"/seat-impact?"+q, nil)
		c.Params = gin.Params{{Key: "id", Value: plan.ID}}
		h.PlanSeatImpact(c)
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Data
	}
	for _, tc := range []struct {
		maxSeats string
		over     float64
	}{
		{"10", 0}, // nobody over
		{"4", 0},  // big is exactly at the limit
		{"3", 1},  // big over
		{"2", 1},  // small and mixed exactly at it
		{"1", 3},  // everyone over
		{"0", 0},  // no limit
	} {
		code, data := impact("max_seats=" + tc.maxSeats)
		if code != http.StatusOK || data["licenses_over"] != tc.over {
			t.Errorf("max_seats=%s: %d %v, want licenses_over %v", tc.maxSeats, code, data, tc.over)
		}
	}
	for _, bad := range []string{"max_seats=-1", "max_seats=abc", ""} {
		if code, _ := impact(bad); code != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", bad, code)
		}
	}
}

// An admin's change of plan removes nobody, as the portal's downgrade:
// one seating fewer people than are on the licence is refused rather
// than pausing the members who joined last. Moving to a plan sold per
// seat takes the people as the seats, within what the plan sells.
func TestChangePlanKeepsRoomForEveryone(t *testing.T) {
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
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	prod := &model.Product{Name: "Move", Slug: "move-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := func(name string, maxSeats int, perSeat bool, minQ, maxQ int) *model.Plan {
		p := &model.Plan{ProductID: prod.ID, Name: name, Slug: name + "-" + suffix, LicenseType: "perpetual",
			LicenseModel: "standard", MaxSeats: maxSeats, MaxActivations: 2, PerSeat: perSeat,
			MinQuantity: minQ, MaxQuantity: maxQ, Active: true}
		if perSeat { // sold per seat only as a subscription
			p.LicenseType, p.BillingInterval = "subscription", "month"
		}
		if err := s.CreatePlan(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	team := plan("team", 5, false, 0, 0)
	pair := plan("pair", 2, false, 0, 0)
	trio := plan("trio", 3, false, 0, 0)
	seats := plan("seats", 0, true, 4, 10)
	lic := &model.License{ProductID: prod.ID, PlanID: team.ID, Email: "owner-" + suffix + "@example.com",
		LicenseKey: "KG-MOVE-" + suffix, Status: model.StatusActive}
	if err := s.CreateLicense(ctx, lic); err != nil {
		t.Fatal(err)
	}
	joined := time.Now()
	for _, m := range []string{"a", "b"} {
		if err := s.CreateSeat(ctx, &model.Seat{LicenseID: lic.ID, Email: m + "-" + suffix + "@example.com",
			Role: "member", AcceptedAt: &joined}); err != nil {
			t.Fatal(err)
		}
	}
	h := &AdminHandler{Store: s}
	move := func(planID string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/admin/licenses/"+lic.ID+"/change-plan", strings.NewReader(`{"plan_id":"`+planID+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: lic.ID}}
		h.ChangeLicensePlan(c)
		var out struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Error.Code
	}
	current := func() *model.License {
		got, err := s.FindLicenseByID(ctx, lic.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Three people: two seats is too few, and nothing changes.
	if code, ec := move(pair.ID); code != http.StatusConflict || ec != "SEATS_IN_USE" {
		t.Fatalf("to 2 seats with 3 people: %d %s, want 409 SEATS_IN_USE", code, ec)
	}
	if got := current(); got.PlanID != team.ID {
		t.Fatalf("plan after the refusal = %s, want it unchanged", got.PlanID)
	}
	// Exactly enough.
	if code, ec := move(trio.ID); code != http.StatusOK {
		t.Fatalf("to 3 seats with 3 people: %d %s", code, ec)
	}
	// Per seat: 3 people, but the plan sells at least 4.
	if code, ec := move(seats.ID); code != http.StatusOK {
		t.Fatalf("to the per-seat plan: %d %s", code, ec)
	}
	if got := current(); got.PlanID != seats.ID || got.Quantity != 4 {
		t.Fatalf("after moving to the per-seat plan: plan %s, quantity %d; want it with 4 seats", got.PlanID, got.Quantity)
	}
	var audited int
	for i := 0; i < 50 && audited == 0; i++ { // audits are written in the background
		if err := s.DB.NewRaw(`SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = 'plan_changed'
			AND changes->>'old_quantity' = '1' AND changes->>'new_quantity' = '4'`, lic.ID).Scan(ctx, &audited); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if audited != 1 {
		t.Fatalf("audits of the seats carried over: %d, want 1", audited)
	}
	// "Moving" to the plan it is on changes no seats, even outside what
	// the plan now sells: they are Stripe's, or the quantity control's.
	if _, err := s.DB.NewRaw("UPDATE plans SET max_quantity = 3, min_quantity = 1 WHERE id = ?", seats.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if code, ec := move(seats.ID); code != http.StatusOK {
		t.Fatalf("to the plan it is on: %d %s", code, ec)
	}
	if got := current(); got.Quantity != 4 {
		t.Fatalf("quantity after a move to the same plan = %d, want 4 kept", got.Quantity)
	}
}
