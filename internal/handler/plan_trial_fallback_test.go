package handler

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// Free of charge, but sold per seat: a licence moved onto it would
	// hold one seat, below the three the plan sells.
	perSeat := &model.Plan{ProductID: f.prodA.ID, Name: "perseat", Slug: "tf-perseat-" + f.suffix, LicenseType: "subscription",
		BillingInterval: "month", LicenseModel: "standard", MaxActivations: 1, PerSeat: true, MinQuantity: 3, MaxQuantity: 10, Active: true}
	if err := f.s.CreatePlan(f.ctx, perSeat); err != nil {
		t.Fatal(err)
	}

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
		{"per-seat plan", "trial", perSeat.ID, "sold per seat"},
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
	} else if e, _ := out["error"].(map[string]any); e["message"] != "trial_fallback_plan_id is only available on trial plans" {
		t.Fatalf("type change keeping the fallback: message %v, want the API's own check, not the constraint's", e["message"])
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

// A plan trial plans fall back to keeps qualifying: an edit that would
// make it refuse them, or deleting it, is refused with the plans named,
// while other edits go through.
func TestTrialFallbackTargetStaysEligible(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	target := &model.Plan{ProductID: f.prodA.ID, Name: "Community", Slug: "tfu-free-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := f.s.CreatePlan(f.ctx, target); err != nil {
		t.Fatal(err)
	}
	trial := &model.Plan{ProductID: f.prodA.ID, Name: "Pro Trial", Slug: "tfu-trial-" + f.suffix, LicenseType: "trial",
		LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, Active: true, TrialFallbackPlanID: target.ID}
	if err := f.s.CreatePlan(f.ctx, trial); err != nil {
		t.Fatal(err)
	}
	param := gin.Param{Key: "id", Value: target.ID}
	update := func(body string) (int, map[string]any) {
		return f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+target.ID, body, nil, param)
	}
	refused := func(status int, out map[string]any) bool {
		e, _ := out["error"].(map[string]any)
		msg, _ := e["message"].(string)
		return status == http.StatusConflict && e["code"] == "TRIAL_FALLBACK_IN_USE" && strings.Contains(msg, "Pro Trial")
	}
	if status, out := update(`{"stripe_price_id":"price_tfu_` + f.suffix + `"}`); !refused(status, out) {
		t.Fatalf("pricing a fallback: %d %v, want 409 TRIAL_FALLBACK_IN_USE naming the trial plan", status, out)
	}
	if status, out := update(`{"license_type":"trial","trial_days":7}`); !refused(status, out) {
		t.Fatalf("making a fallback a trial: %d %v, want 409", status, out)
	}
	// An update period is sold, so not free either (with what selling
	// one needs at all: the switch on, the product's feeds gated).
	prev, _ := f.s.GetSetting(f.ctx, store.SettingMaintenanceFeatures)
	t.Cleanup(func() { _ = f.s.SetSettings(f.ctx, map[string]string{store.SettingMaintenanceFeatures: prev}) })
	if err := f.s.SetSettings(f.ctx, map[string]string{store.SettingMaintenanceFeatures: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.Product)(nil)).Set("feed_license_required = true").Where("id = ?", f.prodA.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if status, out := update(`{"updates_days":365}`); !refused(status, out) {
		t.Fatalf("an update period on a fallback: %d %v, want 409", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, target.ID); p.StripePriceID != "" || p.LicenseType != "perpetual" || p.UpdatesDays != 0 {
		t.Fatalf("a refused edit was stored: price %q, type %s", p.StripePriceID, p.LicenseType)
	}
	if status, out := update(`{"name":"Community Edition","max_activations":2}`); status != http.StatusOK {
		t.Fatalf("an edit that keeps it free: %d %v", status, out)
	}
	if status, out := f.call(h.DeletePlan, http.MethodDelete, "/admin/plans/"+target.ID, "", nil, param); !refused(status, out) {
		t.Fatalf("deleting a fallback: %d %v, want 409", status, out)
	}
	// Once no trial plan names it, it is an ordinary plan again.
	if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("trial_fallback_plan_id = NULL").Where("id = ?", trial.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if status, out := update(`{"stripe_price_id":"price_tfu_` + f.suffix + `"}`); status != http.StatusOK {
		t.Fatalf("pricing a plan no trial falls back to: %d %v", status, out)
	}
}

// The fallback is checked again in the write, under its lock: a plan
// priced between the request's own check and the write is refused, on
// create and on update alike.
func TestTrialFallbackCheckedInTheWrite(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	n := 0
	free := func() *model.Plan {
		n++
		p := &model.Plan{ProductID: f.prodA.ID, Name: "Free " + strconv.Itoa(n), Slug: "tfw-free-" + strconv.Itoa(n) + "-" + f.suffix,
			LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: true}
		if err := f.s.CreatePlan(f.ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	priceMeanwhile := func(p *model.Plan) {
		h.beforeCutoffWrite = func() {
			if _, err := f.s.DB.NewUpdate().Model((*model.Plan)(nil)).Set("stripe_price_id = ?", "price_tfw_"+p.ID[:8]).
				Where("id = ?", p.ID).Exec(f.ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	target := free()
	priceMeanwhile(target)
	body, _ := json.Marshal(map[string]any{"product_id": f.prodA.ID, "name": "Trial W", "license_type": "trial",
		"slug": "tfw-trial-" + f.suffix, "trial_days": 14, "trial_fallback_plan_id": target.ID})
	if status, out := f.call(h.CreatePlan, http.MethodPost, "/admin/plans", string(body), nil); status != http.StatusBadRequest {
		t.Fatalf("create naming a plan priced meanwhile: %d %v, want 400", status, out)
	}

	h.beforeCutoffWrite = nil
	trial := &model.Plan{ProductID: f.prodA.ID, Name: "Trial U", Slug: "tfw-trialu-" + f.suffix, LicenseType: "trial",
		LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, Active: true}
	if err := f.s.CreatePlan(f.ctx, trial); err != nil {
		t.Fatal(err)
	}
	target = free()
	priceMeanwhile(target)
	status, out := f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+trial.ID, `{"trial_fallback_plan_id":"`+target.ID+`"}`, nil,
		gin.Param{Key: "id", Value: trial.ID})
	if status != http.StatusBadRequest {
		t.Fatalf("update naming a plan priced meanwhile: %d %v, want 400", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, trial.ID); p.TrialFallbackPlanID != "" {
		t.Fatalf("the refused fallback was stored: %q", p.TrialFallbackPlanID)
	}
}

// Whether the edit may break a fallback is decided on the row under its
// lock, not on the request's snapshot: a plan read as paid, made free
// and named as a fallback before the write, is not priced again.
func TestTrialFallbackTargetGuardUsesTheLockedRow(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	target := &model.Plan{ProductID: f.prodA.ID, Name: "Was Paid", Slug: "tfg-paid-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, StripePriceID: "price_tfg_a_" + f.suffix, Active: true}
	if err := f.s.CreatePlan(f.ctx, target); err != nil {
		t.Fatal(err)
	}
	trial := &model.Plan{ProductID: f.prodA.ID, Name: "Guard Trial", Slug: "tfg-trial-" + f.suffix, LicenseType: "trial",
		LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, Active: true}
	if err := f.s.CreatePlan(f.ctx, trial); err != nil {
		t.Fatal(err)
	}
	h.beforeCutoffWrite = func() {
		if _, err := f.s.DB.NewRaw("UPDATE plans SET stripe_price_id = '' WHERE id = ?", target.ID).Exec(f.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.DB.NewRaw("UPDATE plans SET trial_fallback_plan_id = ? WHERE id = ?", target.ID, trial.ID).Exec(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	status, out := f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+target.ID, `{"stripe_price_id":"price_tfg_b_`+f.suffix+`"}`, nil,
		gin.Param{Key: "id", Value: target.ID})
	if e, _ := out["error"].(map[string]any); status != http.StatusConflict || e["code"] != "TRIAL_FALLBACK_IN_USE" {
		t.Fatalf("pricing a plan made a fallback meanwhile: %d %v, want 409 TRIAL_FALLBACK_IN_USE", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, target.ID); p.StripePriceID != "" {
		t.Fatalf("the price was written: %q", p.StripePriceID)
	}
}

// Two free plans turned into trials naming each other at once: one
// wins, the other is refused (its fallback is now a trial), and neither
// fails on a deadlock, the locks being taken in plan ID order.
func TestTrialFallbackMutualNamingDoesNotDeadlock(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	for round := range 8 {
		mk := func(name string) *model.Plan {
			p := &model.Plan{ProductID: f.prodA.ID, Name: name, Slug: "tfm-" + name + "-" + strconv.Itoa(round) + "-" + f.suffix,
				LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: true}
			if err := f.s.CreatePlan(f.ctx, p); err != nil {
				t.Fatal(err)
			}
			return p
		}
		x, y := mk("x"), mk("y")
		statuses := make(chan int, 2)
		var wg sync.WaitGroup
		for _, pair := range [][2]*model.Plan{{x, y}, {y, x}} {
			wg.Add(1)
			go func(self, other *model.Plan) {
				defer wg.Done()
				status, _ := f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+self.ID,
					`{"license_type":"trial","trial_days":7,"trial_fallback_plan_id":"`+other.ID+`"}`, nil, gin.Param{Key: "id", Value: self.ID})
				statuses <- status
			}(pair[0], pair[1])
		}
		wg.Wait()
		close(statuses)
		var got []int
		for s := range statuses {
			got = append(got, s)
		}
		slices.Sort(got)
		if !slices.Equal(got, []int{http.StatusOK, http.StatusBadRequest}) {
			t.Fatalf("round %d: statuses %v, want one 200 and one 400", round, got)
		}
	}
}

// Leaving the trial type clears the fallback even when one was named
// after the request read the plan, instead of failing on the constraint.
func TestTrialFallbackClearedOnTypeChangeAfterAConcurrentSet(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	free := &model.Plan{ProductID: f.prodA.ID, Name: "Free C", Slug: "tfc-free-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	trial := &model.Plan{ProductID: f.prodA.ID, Name: "Trial C", Slug: "tfc-trial-" + f.suffix, LicenseType: "trial",
		LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, Active: true}
	for _, p := range []*model.Plan{free, trial} {
		if err := f.s.CreatePlan(f.ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	h.beforeCutoffWrite = func() {
		if _, err := f.s.DB.NewRaw("UPDATE plans SET trial_fallback_plan_id = ? WHERE id = ?", free.ID, trial.ID).Exec(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	status, out := f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+trial.ID, `{"license_type":"perpetual"}`, nil,
		gin.Param{Key: "id", Value: trial.ID})
	if status != http.StatusOK {
		t.Fatalf("type change after a fallback was named meanwhile: %d %v", status, out)
	}
	if p, _ := f.s.FindPlanByID(f.ctx, trial.ID); p.LicenseType != "perpetual" || p.TrialFallbackPlanID != "" {
		t.Fatalf("plan is %s with fallback %q, want perpetual with none", p.LicenseType, p.TrialFallbackPlanID)
	}
}

// A plan deleted between the request's read and its write answers 404,
// not a server error.
func TestUpdatePlanDeletedMeanwhile(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	p := &model.Plan{ProductID: f.prodA.ID, Name: "Gone", Slug: "tfd-gone-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := f.s.CreatePlan(f.ctx, p); err != nil {
		t.Fatal(err)
	}
	h.beforeCutoffWrite = func() {
		if err := f.s.DeletePlan(f.ctx, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	status, out := f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+p.ID, `{"stripe_price_id":"price_tfd_`+f.suffix+`"}`, nil,
		gin.Param{Key: "id", Value: p.ID})
	if status != http.StatusNotFound {
		t.Fatalf("pricing a plan deleted meanwhile: %d %v, want 404", status, out)
	}
}

// A plan trial plans still name that already stopped qualifying (priced
// by hand, say) is not held hostage: editing its price again goes
// through, since it does not break anything that was not broken.
func TestTrialFallbackTargetAlreadyInvalidCanBeEdited(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	target := &model.Plan{ProductID: f.prodA.ID, Name: "Priced By Hand", Slug: "tfa-target-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := f.s.CreatePlan(f.ctx, target); err != nil {
		t.Fatal(err)
	}
	trial := &model.Plan{ProductID: f.prodA.ID, Name: "Trial A", Slug: "tfa-trial-" + f.suffix, LicenseType: "trial",
		LicenseModel: "standard", MaxActivations: 1, TrialDays: 14, Active: true, TrialFallbackPlanID: target.ID}
	if err := f.s.CreatePlan(f.ctx, trial); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.NewRaw("UPDATE plans SET stripe_price_id = ? WHERE id = ?", "price_tfa_1_"+f.suffix, target.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	status, out := f.call(h.UpdatePlan, http.MethodPut, "/admin/plans/"+target.ID, `{"stripe_price_id":"price_tfa_2_`+f.suffix+`"}`, nil,
		gin.Param{Key: "id", Value: target.ID})
	if status != http.StatusOK {
		t.Fatalf("repricing a fallback that was already priced: %d %v, want 200", status, out)
	}
}

// A plan with licences cannot be deleted, checked under the plan's lock.
func TestDeletePlanWithLicences(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	p := &model.Plan{ProductID: f.prodA.ID, Name: "In Use", Slug: "tfh-inuse-" + f.suffix, LicenseType: "perpetual",
		LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := f.s.CreatePlan(f.ctx, p); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{ProductID: f.prodA.ID, PlanID: p.ID, Email: "tfh-" + f.suffix + "@example.com",
		LicenseKey: "KEY-TFH-" + f.suffix, Status: model.StatusActive}
	if err := f.s.CreateLicense(f.ctx, lic); err != nil {
		t.Fatal(err)
	}
	status, out := f.call(h.DeletePlan, http.MethodDelete, "/admin/plans/"+p.ID, "", nil, gin.Param{Key: "id", Value: p.ID})
	if e, _ := out["error"].(map[string]any); status != http.StatusConflict || e["code"] != "HAS_LICENSES" {
		t.Fatalf("deleting a plan with a licence: %d %v, want 409 HAS_LICENSES", status, out)
	}
	if _, err := f.s.FindPlanByID(f.ctx, p.ID); err != nil {
		t.Fatalf("the plan is gone: %v", err)
	}
}
