package handler

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// The admin's key change: refused to an API key bound to another
// product, 404 for an unknown licence; otherwise a new key, the old one
// dead, the devices on it removed and reported, and the change audited
// without the key.
func TestAdminRotateLicenseKey(t *testing.T) {
	f := newAPIFixture(t)
	wh := service.NewWebhookService(f.s, slog.Default(), time.Second, 1, true)
	h := &AdminHandler{Store: f.s, Webhook: wh}
	lic := f.licA
	hook := &model.Webhook{ProductID: f.prodA.ID, URL: "http://127.0.0.1:9/hook", Secret: "s",
		Events: []string{model.EventLicenseDeactivated}, Active: true}
	if err := f.s.CreateWebhook(f.ctx, hook); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ActivateForKey(f.ctx, &model.Activation{LicenseID: lic.ID, Identifier: "owner-pc", IdentifierType: "device"}, 3, lic.LicenseKey); err != nil {
		t.Fatal(err)
	}
	rotate := func(id string, key *model.APIKey) (int, map[string]any) {
		return f.call(h.RotateLicenseKey, http.MethodPost, "/admin/licenses/"+id+"/rotate-key", "", key, gin.Param{Key: "id", Value: id})
	}

	other := &model.APIKey{ProductID: f.prodB.ID, Scopes: []string{model.ScopeLicensesWrite}}
	if code, out := rotate(lic.ID, other); code != http.StatusForbidden || errCode(out) != "PRODUCT_SCOPE_MISMATCH" {
		t.Fatalf("rotate with another product's key = %d %v, want 403 PRODUCT_SCOPE_MISMATCH", code, out)
	}
	if code, _ := rotate("no-such-licence", nil); code != http.StatusNotFound {
		t.Fatalf("rotate an unknown licence = %d, want 404", code)
	}
	if _, err := f.s.FindLicenseByKey(f.ctx, lic.LicenseKey); err != nil {
		t.Fatalf("a refused rotate changed the key: %v", err)
	}

	bound := &model.APIKey{ProductID: f.prodA.ID, Scopes: []string{model.ScopeLicensesWrite}}
	code, out := rotate(lic.ID, bound)
	d, _ := out["data"].(map[string]any)
	newKey, _ := d["license_key"].(string)
	if code != http.StatusOK || newKey == "" || newKey == lic.LicenseKey || d["devices_removed"] != float64(1) {
		t.Fatalf("rotate = %d %v", code, out)
	}
	if _, err := f.s.FindLicenseByKey(f.ctx, lic.LicenseKey); err == nil {
		t.Fatal("the old key still opens the licence")
	}
	if found, err := f.s.FindLicenseByKey(f.ctx, newKey); err != nil || found.ID != lic.ID {
		t.Fatalf("the new key: %v", err)
	}
	var sent int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ?",
		hook.ID, model.EventLicenseDeactivated).Scan(f.ctx, &sent); err != nil || sent != 1 {
		t.Fatalf("%d license.deactivated sent (%v), want 1", sent, err)
	}
	// Audit rows are written in the background.
	var changes string
	for range 40 {
		_ = f.s.DB.NewRaw("SELECT changes::text FROM audit_logs WHERE entity_id = ? AND action = 'key_rotated' AND actor_type = 'admin'",
			lic.ID).Scan(f.ctx, &changes)
		if changes != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if changes == "" {
		t.Fatal("no key_rotated audit by the admin")
	}
	for _, k := range []string{lic.LicenseKey, newKey} {
		if strings.Contains(changes, k) {
			t.Fatalf("the audit holds a key: %s", changes)
		}
	}
}
