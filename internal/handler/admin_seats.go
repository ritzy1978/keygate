package handler

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// ─── Seats (admin) ───
//
// The vendor's admin does for a licence's members what its owner does in
// the portal, and what support needs on top: sees each member's key (by
// its hint, revealed on request and audited, as the licence's own key
// is), takes a member off or cancels an invitation, and reissues a
// member's key when it got out.

// adminSeat is a seat as the admin's seat list shows it.
type adminSeat struct {
	*model.Seat
	// KeyPrefix and KeyHint are the member's own key's prefix and last
	// four characters; empty until they have a key.
	KeyPrefix string `json:"key_prefix,omitempty"`
	KeyHint   string `json:"key_hint,omitempty"`
	// DeviceCount is how many devices the member's own key activated.
	DeviceCount int `json:"device_count"`
}

// GET /api/v1/admin/licenses/:id/seats
func (h *AdminHandler) ListLicenseSeats(c *gin.Context) {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return
	}
	page := listPage(c)
	seats, total, err := h.Store.ListSeats(c, id, page)
	if err != nil {
		response.Internal(c, err)
		return
	}
	devices, err := h.Store.CountDevicesBySeat(c, id)
	if err != nil {
		response.Internal(c, err)
		return
	}
	if err := h.Store.MarkPausedSeats(c, id, seats); err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]*adminSeat, len(seats))
	for i, seat := range seats {
		prefix, hint := keyHint(h.Store.DecryptMemberKey(seat))
		out[i] = &adminSeat{Seat: seat, KeyPrefix: prefix, KeyHint: hint, DeviceCount: devices[seat.ID]}
	}
	count, _ := h.Store.CountActiveSeats(c, id)
	listOK(c, "seats", out, total, page, gin.H{"active_count": count})
}

// licenseSeat is the seat the request names, when it is on the licence
// the request names. It writes the response and returns nil otherwise.
func (h *AdminHandler) licenseSeat(c *gin.Context) *model.Seat {
	id := c.Param("id")
	if !h.checkLicenseScope(c, id) {
		return nil
	}
	seat, err := h.Store.FindSeatByID(c, c.Param("seat_id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && seat.LicenseID != id) {
		response.Err(c, http.StatusNotFound, "SEAT_NOT_FOUND", "seat not found on this license")
		return nil
	}
	if err != nil {
		response.Internal(c, err)
		return nil
	}
	return seat
}

func seatNotJoined(c *gin.Context) {
	response.Err(c, http.StatusConflict, "SEAT_NOT_JOINED",
		"the seat is an invitation not yet accepted, or removed: it has no key")
}

// GET /api/v1/admin/licenses/:id/seats/:seat_id/key
//
// One member's key in full, as GET /admin/licenses/:id/key gives the
// licence's: a deliberate act, audited, never cached. A member who has
// not opened the portal yet gets their key made here, as the portal
// would make it.
func (h *AdminHandler) RevealMemberKey(c *gin.Context) {
	seat := h.licenseSeat(c)
	if seat == nil {
		return
	}
	if seat.AcceptedAt == nil || seat.RemovedAt != nil {
		seatNotJoined(c)
		return
	}
	key, err := h.Store.MemberKey(c.Request.Context(), seat)
	if err != nil {
		response.Internal(c, err)
		return
	}
	// Removed between the read and making the key.
	if key == "" {
		seatNotJoined(c)
		return
	}
	// The audit records that a reveal happened, never the key.
	h.Store.Audit(c, &model.AuditLog{
		Entity: "seat", EntityID: seat.ID, Action: "member_key_revealed",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"license_id": seat.LicenseID, "email": seat.Email},
	})
	c.Header("Cache-Control", "no-store")
	response.OK(c, gin.H{"license_key": key})
}

// POST /api/v1/admin/licenses/:id/seats/:seat_id/rotate-key
//
// Reissues one member's key, as POST /admin/licenses/:id/rotate-key
// does the licence's: the old key stops at once, the devices and
// floating sessions on it are removed, each device reported as
// deactivated, and everyone else's keys and devices stay. The member
// sees the new key in the portal; the response carries it too.
func (h *AdminHandler) RotateMemberKey(c *gin.Context) {
	seat := h.licenseSeat(c)
	if seat == nil {
		return
	}
	key, devices, err := h.Store.RotateMemberKey(c.Request.Context(), seat.ID)
	if errors.Is(err, store.ErrSeatNotJoined) {
		seatNotJoined(c)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		response.Err(c, http.StatusNotFound, "SEAT_NOT_FOUND", "seat not found on this license")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.Store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "seat", EntityID: seat.ID, Action: "member_key_rotated",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"license_id": seat.LicenseID, "email": seat.Email, "devices_removed": len(devices)},
	})
	if h.Webhook != nil && len(devices) > 0 {
		// The licence's product, read after the change: the key is
		// replaced whatever this lookup does, and a failure here only
		// loses the reports, which the log then records.
		productID, err := h.Store.GetLicenseProductID(c, seat.LicenseID)
		if err != nil {
			slog.Warn("member key reissued; devices not reported", "seat_id", seat.ID, "error", err)
		} else {
			for _, a := range devices {
				h.Webhook.Dispatch(c.Request.Context(), productID, "license.deactivated", map[string]any{
					"license_id": seat.LicenseID, "identifier": a.Identifier,
				})
			}
		}
	}
	c.Header("Cache-Control", "no-store")
	response.OK(c, gin.H{"license_key": key, "devices_removed": len(devices)})
}

// DELETE /api/v1/admin/licenses/:id/seats/:seat_id
//
// Takes a member off the licence, or cancels an invitation, as the owner
// does in the portal (SeatService.AdminRemoveSeat): webhooks, devices
// and audit alike. Removing a seat already removed answers the same.
func (h *AdminHandler) RemoveLicenseSeat(c *gin.Context) {
	if h.Seats == nil {
		response.Internal(c, errors.New("seat service not configured"))
		return
	}
	seat := h.licenseSeat(c)
	if seat == nil {
		return
	}
	if err := h.Seats.AdminRemoveSeat(c.Request.Context(), seat.LicenseID, seat.ID, adminID(c), c.ClientIP()); err != nil {
		writeAppErr(c, err)
		return
	}
	response.OK(c, gin.H{"status": "removed"})
}
