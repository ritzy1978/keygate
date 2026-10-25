package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// Plans sold per seat, as the admin API creates and edits them.
func TestPerSeatPlans(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	data := func(out map[string]any) map[string]any {
		d, _ := out["data"].(map[string]any)
		return d
	}
	product := func(typ string) *model.Product {
		p := &model.Product{Name: typ, Slug: "ps-" + typ + "-" + f.suffix, Type: typ}
		if err := f.s.CreateProduct(f.ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hybrid, desktop := product("hybrid"), product("desktop")
	create := func(prod *model.Product, slug, extra string) (int, map[string]any) {
		body := fmt.Sprintf(`{"product_id":%q,"name":"Team","slug":"ps-%s-%s","license_type":"subscription","max_activations":2%s}`,
			prod.ID, slug, f.suffix, extra)
		return f.call(h.CreatePlan, http.MethodPost, "/admin/plans", body, nil)
	}

	// Sold per seat: the bounds default to 1 and 100, and max_seats is
	// the 1 an older replica reads as the owner alone (0 where the
	// product has no seats), whatever was sent.
	code, out := create(hybrid, "a", `,"per_seat":true,"max_seats":40`)
	if d := data(out); code != http.StatusCreated || d["per_seat"] != true || d["min_quantity"] != float64(1) ||
		d["max_quantity"] != float64(100) || d["max_seats"] != float64(1) {
		t.Fatalf("create per seat on hybrid = %d %v", code, out)
	}
	team := data(out)
	if code, out := create(desktop, "b", `,"per_seat":true,"min_quantity":5,"max_quantity":25`); code != http.StatusCreated ||
		data(out)["max_seats"] != float64(0) || data(out)["min_quantity"] != float64(5) {
		t.Fatalf("create per seat on desktop = %d %v", code, out)
	}

	// The last license_type in a body is the one Go's decoder keeps.
	for i, tc := range []struct{ name, extra string }{
		{"perpetual", `,"per_seat":true,"license_type":"perpetual"`},
		{"min above max", `,"per_seat":true,"min_quantity":10,"max_quantity":5`},
		{"max too large", `,"per_seat":true,"max_quantity":10001`},
		{"min below one", `,"per_seat":true,"min_quantity":-1`},
	} {
		if code, out := create(hybrid, fmt.Sprintf("bad%d", i), tc.extra); code != http.StatusBadRequest {
			t.Errorf("%s: create = %d %v, want 400", tc.name, code, out)
		}
	}

	// A plan not sold per seat keeps the max_seats it was given.
	code, out = create(hybrid, "c", `,"max_seats":5`)
	if code != http.StatusCreated || data(out)["per_seat"] != false || data(out)["max_seats"] != float64(5) {
		t.Fatalf("create per licence = %d %v", code, out)
	}
	plain := data(out)

	update := func(id, body string) (int, map[string]any) {
		return f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+id, body, nil, gin.Param{Key: "id", Value: id})
	}
	// How a plan sells is set when it is created, licences or not:
	// checkouts already open for its price were sold the other way.
	if code, out := update(plain["id"].(string), `{"per_seat":true,"min_quantity":2,"max_quantity":30}`); code != http.StatusConflict || errCode(out) != "PER_SEAT_LOCKED" {
		t.Fatalf("switch to per seat without licences = %d %v, want 409 PER_SEAT_LOCKED", code, out)
	}
	if code, out := update(team["id"].(string), `{"per_seat":false}`); code != http.StatusConflict || errCode(out) != "PER_SEAT_LOCKED" {
		t.Fatalf("switch per seat off without licences = %d %v, want 409 PER_SEAT_LOCKED", code, out)
	}
	// Sent as it is, with other edits, it is no switch.
	if code, out := update(team["id"].(string), `{"per_seat":true,"name":"Team renamed"}`); code != http.StatusOK || data(out)["name"] != "Team renamed" {
		t.Fatalf("per_seat unchanged with a rename = %d %v, want 200", code, out)
	}
	// A per-seat plan cannot become perpetual while sold per seat.
	if code, _ := update(team["id"].(string), `{"license_type":"perpetual"}`); code != http.StatusBadRequest {
		t.Fatalf("make a per-seat plan perpetual = %d, want 400", code)
	}
	// With a licence, it cannot: every licence's limits would move.
	lic := &model.License{ProductID: hybrid.ID, PlanID: team["id"].(string), Email: "ps-" + f.suffix + "@example.com",
		LicenseKey: "KEY-PS-H-" + f.suffix, Status: model.StatusActive}
	if err := f.s.CreateLicense(f.ctx, lic); err != nil {
		t.Fatal(err)
	}
	if code, out := update(team["id"].(string), `{"per_seat":false}`); code != http.StatusConflict || errCode(out) != "PER_SEAT_LOCKED" {
		t.Fatalf("switch per seat off with a licence = %d %v, want 409 PER_SEAT_LOCKED", code, out)
	}
	// The max_seats a per-seat plan keeps cannot be set through the API:
	// an older replica reads it as the plan's limit.
	if code, out := update(team["id"].(string), `{"max_seats":5}`); code != http.StatusOK || data(out)["max_seats"] != float64(1) {
		t.Fatalf("set max_seats on a per-seat plan = %d %v, want it kept at 1", code, out)
	}
	// Its bounds and devices per seat still can.
	if code, out := update(team["id"].(string), `{"max_quantity":60,"max_activations":4}`); code != http.StatusOK ||
		data(out)["max_quantity"] != float64(60) || data(out)["max_activations"] != float64(4) {
		t.Fatalf("edit bounds = %d %v", code, out)
	}
}

// An admin sets the seats of a licence nothing bills, and the detail
// shows the limits they buy.
func TestSetLicenseQuantity(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	prod := &model.Product{Name: "Q", Slug: "q-" + f.suffix, Type: "hybrid"}
	if err := f.s.CreateProduct(f.ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Team", Slug: "q-team-" + f.suffix, LicenseType: "subscription",
		LicenseModel: "standard", MaxActivations: 3, MaxSeats: 1, PerSeat: true, MinQuantity: 1, MaxQuantity: 50, Active: true}
	if err := f.s.CreatePlan(f.ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "q-" + f.suffix + "@example.com",
		LicenseKey: "KEY-Q-" + f.suffix, Status: model.StatusActive}
	if err := f.s.CreateLicense(f.ctx, lic); err != nil {
		t.Fatal(err)
	}
	set := func(body string) (int, map[string]any) {
		return f.call(h.SetLicenseQuantity, http.MethodPost, "/admin/licenses/"+lic.ID+"/quantity", body, nil,
			gin.Param{Key: "id", Value: lic.ID})
	}
	code, out := set(`{"quantity":4}`)
	d, _ := out["data"].(map[string]any)
	if code != http.StatusOK || d["quantity"] != float64(4) || d["seat_limit"] != float64(4) || d["activation_limit"] != float64(12) {
		t.Fatalf("set 4 = %d %v, want 4 seats, 4 people, 12 devices", code, out)
	}
	for _, bad := range []string{`{"quantity":0}`, `{"quantity":10001}`, `{}`} {
		if code, _ := set(bad); code != http.StatusBadRequest {
			t.Errorf("set %s = %d, want 400", bad, code)
		}
	}
	code, out = f.call(h.GetLicense, http.MethodGet, "/admin/licenses/"+lic.ID, "", nil, gin.Param{Key: "id", Value: lic.ID})
	if d, _ := out["data"].(map[string]any); code != http.StatusOK || d["seat_limit"] != float64(4) || d["activation_limit"] != float64(12) {
		t.Fatalf("detail = %d %v, want the limits", code, out)
	}
	// Outside what the plan sells, 1 to 50, as a checkout would refuse.
	if code, out := set(`{"quantity":51}`); code != http.StatusConflict || errCode(out) != "QUANTITY_OUT_OF_RANGE" {
		t.Fatalf("set 51 on a 1 to 50 plan = %d %v, want 409 QUANTITY_OUT_OF_RANGE, as the portal answers", code, out)
	}
	// Not below what is in use: two people (the owner and a member) and
	// seven devices, on four seats of three devices each.
	member := &model.Seat{LicenseID: lic.ID, Email: "q-member-" + f.suffix + "@example.com", Role: "member"}
	if _, _, err := f.s.AddSeatWithinLimit(f.ctx, member, 0); err != nil {
		t.Fatal(err)
	}
	if code, out := set(`{"quantity":1}`); code != http.StatusConflict || errCode(out) != "SEATS_IN_USE" {
		t.Fatalf("set 1 with two people = %d %v, want 409 SEATS_IN_USE", code, out)
	}
	for i := range 7 {
		if err := f.s.ActivateWithinLimit(f.ctx, &model.Activation{LicenseID: lic.ID, Identifier: fmt.Sprintf("q-pc%d", i),
			IdentifierType: "device"}, 12); err != nil {
			t.Fatal(err)
		}
	}
	code, out = set(`{"quantity":2}`)
	e, _ := out["error"].(map[string]any)
	det, _ := e["details"].(map[string]any)
	if code != http.StatusConflict || errCode(out) != "DEVICES_IN_USE" || det["in_use"] != float64(7) || det["activation_limit"] != float64(6) {
		t.Fatalf("set 2 with seven devices = %d %v, want 409 DEVICES_IN_USE, 7 in use, a limit of 6", code, out)
	}
	if got, _ := f.s.FindLicenseByID(f.ctx, lic.ID); got.Quantity != 4 {
		t.Fatalf("a refused change moved the seats to %d", got.Quantity)
	}
	// Once the room is freed it goes ahead; raising is never refused.
	if _, err := f.s.DB.NewRaw("DELETE FROM activations WHERE license_id = ? AND identifier = 'q-pc6'", lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if code, out := set(`{"quantity":2}`); code != http.StatusOK {
		t.Fatalf("set 2 with two people and six devices = %d %v", code, out)
	}
	if code, out := set(`{"quantity":4}`); code != http.StatusOK {
		t.Fatalf("raise back to 4 = %d %v", code, out)
	}
	if _, err := f.s.DB.NewRaw("UPDATE licenses SET stripe_subscription_id = ? WHERE id = ?", "sub_q_"+f.suffix, lic.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if code, out := set(`{"quantity":5}`); code != http.StatusConflict || errCode(out) != "STRIPE_MANAGED" {
		t.Fatalf("set on a Stripe-billed licence = %d %v, want 409 STRIPE_MANAGED", code, out)
	}
	// A licence on a plan not sold per seat has nothing to set.
	if code, out := f.call(h.SetLicenseQuantity, http.MethodPost, "/admin/licenses/"+f.licA.ID+"/quantity", `{"quantity":2}`, nil,
		gin.Param{Key: "id", Value: f.licA.ID}); code != http.StatusConflict || errCode(out) != "NOT_PER_SEAT" {
		t.Fatalf("set on a plan not sold per seat = %d %v, want 409 NOT_PER_SEAT", code, out)
	}
}
