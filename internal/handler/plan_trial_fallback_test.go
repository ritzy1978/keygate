package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Which plans a trial plan may name as its fallback, on create and on
// update: only a free, non-trial plan of the same product, and only on a
// trial plan.
func TestTrialFallbackPlanValidation(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	mk := func(name, licenseType, price string) *model.Plan {
		p := &model.Plan{ProductID: f.prodA.ID, Name: name, Slug: "tf-" + name + "-" + f.suffix, LicenseType: licenseType,
			LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, StripePriceID: price, Active: true}
		if licenseType == "subscription" {
			p.BillingInterval = "month"
		}
		if err := f.s.CreatePlan(f.ctx, p); err != nil {
			t.Fatalf("create plan %s: %v", name, err)
		}
		return p
	}
	free := mk("free", "subscription", "")
	inactiveFree := mk("hidden", "subscription", "")
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("active = false").Where("id = ?", inactiveFree.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	paid := mk("paid", "subscription", "price_tf_"+f.suffix)
	otherTrial := mk("othertrial", "trial", "")

	n := 0
	create := func(licenseType, fallback string) (int, map[string]any) {
		n++
		body, _ := json.Marshal(map[string]any{
			"product_id": f.prodA.ID, "name": "Trial " + licenseType, "license_type": licenseType,
			"slug": "tf-new-" + strconv.Itoa(n) + "-" + f.suffix, "billing_interval": "month",
			"trial_days": 14, "trial_fallback_plan_id": fallback,
		})
		return f.call(h.CreatePlan, http.MethodPost, "/admin/plans", string(body), nil)
	}
	for _, tc := range []struct {
		name, licenseType, fallback, wantErr string
	}{
		{"free plan", "trial", free.ID, ""},
		{"inactive free plan", "trial", inactiveFree.ID, ""},
		{"none", "trial", "", ""},
		{"paid plan", "trial", paid.ID, "free plan"},
		{"trial plan", "trial", otherTrial.ID, "trial plan"},
		{"other product", "trial", f.planB.ID, "same product"},
		{"unknown", "trial", "00000000-0000-0000-0000-000000000000", "not found"},
		{"not a trial plan", "subscription", free.ID, "only available on trial plans"},
	} {
		t.Run("create/"+tc.name, func(t *testing.T) {
			status, out := create(tc.licenseType, tc.fallback)
			if tc.wantErr == "" {
				if status != http.StatusCreated {
					t.Fatalf("status %d: %v", status, out)
				}
				data, _ := out["data"].(map[string]any)
				if got, _ := data["trial_fallback_plan_id"].(string); got != tc.fallback {
					t.Fatalf("saved fallback %q, want %q", got, tc.fallback)
				}
				return
			}
			e, _ := out["error"].(map[string]any)
			msg, _ := e["message"].(string)
			if status != http.StatusBadRequest || !strings.Contains(msg, tc.wantErr) {
				t.Fatalf("status %d, message %q; want 400 mentioning %q", status, msg, tc.wantErr)
			}
		})
	}

	// On update: the plan itself is refused; a valid fallback is saved;
	// an edit of another field is not blocked by a fallback that has
	// since stopped qualifying; leaving the trial type clears it.
	trial := mk("trial", "trial", "")
	update := func(body string) (int, map[string]any) {
		return f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+trial.ID, body, nil, gin.Param{Key: "id", Value: trial.ID})
	}
	if status, out := update(`{"trial_fallback_plan_id":"` + trial.ID + `"}`); status != http.StatusBadRequest {
		t.Fatalf("self as fallback: status %d %v", status, out)
	}
	if status, out := update(`{"trial_fallback_plan_id":"` + free.ID + `"}`); status != http.StatusOK {
		t.Fatalf("set fallback: status %d %v", status, out)
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("stripe_price_id = ?", "price_tf2_"+f.suffix).
		Where("id = ?", free.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if status, out := update(`{"name":"Renamed trial"}`); status != http.StatusOK {
		t.Fatalf("unrelated edit blocked by the saved fallback: status %d %v", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, trial.ID); p.TrialFallbackPlanID != free.ID {
		t.Fatalf("unrelated edit changed the fallback to %q", p.TrialFallbackPlanID)
	}
	// The admin dialog re-sends the saved fallback with every save: still
	// not re-checked when it is unchanged.
	if status, out := update(`{"name":"Renamed again","trial_fallback_plan_id":"` + free.ID + `"}`); status != http.StatusOK {
		t.Fatalf("unchanged fallback re-sent with an edit was refused: status %d %v", status, out)
	}
	// Leaving the trial type while naming the fallback is refused, even
	// when it is the one already saved.
	if status, out := update(`{"license_type":"subscription","billing_interval":"month","trial_fallback_plan_id":"` + free.ID + `"}`); status != http.StatusBadRequest {
		t.Fatalf("type change keeping the fallback: status %d %v, want 400", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, trial.ID); p.LicenseType != "trial" || p.TrialFallbackPlanID != free.ID {
		t.Fatalf("refused update changed the plan: %s, fallback %q", p.LicenseType, p.TrialFallbackPlanID)
	}
	// The database holds it too, should two edits race past the API.
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("license_type = 'subscription'").
		Where("id = ?", trial.ID).Exec(f.ctx); err == nil || !store.IsTrialFallbackPlanConflict(err) {
		t.Fatalf("a non-trial plan with a fallback was stored: %v", err)
	}
	// An empty value clears it.
	if status, out := update(`{"trial_fallback_plan_id":""}`); status != http.StatusOK {
		t.Fatalf("clear fallback: status %d %v", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, trial.ID); p.TrialFallbackPlanID != "" {
		t.Fatalf("fallback %q after clearing", p.TrialFallbackPlanID)
	}
	if status, out := update(`{"trial_fallback_plan_id":"` + inactiveFree.ID + `"}`); status != http.StatusOK {
		t.Fatalf("set fallback again: status %d %v", status, out)
	}
	if status, out := update(`{"license_type":"subscription","billing_interval":"month"}`); status != http.StatusOK {
		t.Fatalf("type change: status %d %v", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, trial.ID); p.TrialFallbackPlanID != "" {
		t.Fatalf("fallback %q kept on a plan that is no longer a trial", p.TrialFallbackPlanID)
	}
}
