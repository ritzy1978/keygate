package handler

import (
	"database/sql"
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/pkg/response"
)

// POST /api/v1/admin/licenses/:id/rotate-key
//
// Reissues the licence's key. A key is fixed once issued as far as its
// customer is concerned; reissuing one, as when it got out, is the
// vendor's call. The old key stops working, and the devices it
// activated are removed and reported as deactivated; the members' own
// keys and devices are left alone.
func (h *AdminHandler) RotateLicenseKey(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	lic, err := h.Store.FindLicenseByID(c, id)
	if errors.Is(err, sql.ErrNoRows) {
		response.NotFound(c, "license not found")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	key, devices, err := h.Store.RotateLicenseKey(c.Request.Context(), lic.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.NotFound(c, "license not found")
			return
		}
		response.Internal(c, err)
		return
	}
	// The audit records that the key changed, never the key.
	h.Store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: "key_rotated",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"devices_removed": len(devices)},
	})
	if h.Webhook != nil {
		for _, a := range devices {
			h.Webhook.Dispatch(c.Request.Context(), lic.ProductID, "license.deactivated", map[string]any{
				"license_id": lic.ID, "identifier": a.Identifier,
			})
		}
	}
	c.Header("Cache-Control", "no-store")
	response.OK(c, gin.H{"license_key": key, "devices_removed": len(devices)})
}
