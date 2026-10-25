package handler

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// PortalActivationsHandler exposes self-service activation management
// to the license owner (the user matching license.email), who manages
// every device, and to any accepted member, who manages the devices
// their own key activated. The "member can also manage activation
// slots" rule matches the use case: a teammate who lost their laptop
// must be able to free their own slot without filing a support ticket.
//
//	GET    /api/v1/portal/licenses/:license_key/activations
//	DELETE /api/v1/portal/licenses/:license_key/activations/:activation_id
//
// The classic UX problem this solves: "I lost my old laptop, all 3
// activation slots are taken, I can't activate the new machine".
// Without self-service, that's a customer-support ticket every time.
type PortalActivationsHandler struct {
	store   *store.Store
	webhook *service.WebhookService
}

func NewPortalActivationsHandler(s *store.Store, webhook *service.WebhookService) *PortalActivationsHandler {
	return &PortalActivationsHandler{store: s, webhook: webhook}
}

// portalActivationView is the slimmed-down activation row the portal
// shows: every device to the owner, a member's own to them. Hides
// internal fields (license_id) but keeps anything useful for "which
// device am I deleting" decisions.
type portalActivationView struct {
	ID             string    `json:"id"`
	Identifier     string    `json:"identifier"`
	IdentifierType string    `json:"identifier_type"`
	Label          string    `json:"label,omitempty"`
	IPAddress      string    `json:"ip_address,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	// No omitempty: it does nothing on a struct, and this column has a
	// default of now(), so the field is always present anyway.
	LastVerified time.Time `json:"last_verified"`
	// SeatID and MemberEmail name the member whose key activated the
	// device; both are absent for the owner's key.
	SeatID      string `json:"seat_id,omitempty"`
	MemberEmail string `json:"member_email,omitempty"`
}

// PortalViewerSeat is the seat the signed-in address holds on the
// licence: nil for its owner, who holds no seat row, and for anyone
// without an accepted seat. lic.Seats must be loaded.
func PortalViewerSeat(lic *model.License, email string) *model.Seat {
	if strings.EqualFold(lic.Email, email) {
		return nil
	}
	// The oldest, should seats made before addresses were lower-cased
	// leave two for one person: the seat Store.FindSeatByEmail picks.
	var found *model.Seat
	for _, s := range lic.Seats {
		if s.RemovedAt != nil || s.AcceptedAt == nil || !strings.EqualFold(s.Email, email) {
			continue
		}
		if found == nil || s.CreatedAt.Before(found.CreatedAt) || (s.CreatedAt.Equal(found.CreatedAt) && s.ID < found.ID) {
			found = s
		}
	}
	return found
}

// OwnDevices keeps the devices the member's own key activated.
func OwnDevices(rows []*model.Activation, seatID string) []*model.Activation {
	out := make([]*model.Activation, 0, len(rows))
	for _, a := range rows {
		if a.SeatID == seatID {
			out = append(out, a)
		}
	}
	return out
}

// resolveOwnedLicense loads the license at :license_key and verifies
// the logged-in user owns it (matches license.email) or holds an
// accepted seat on it.
// viewer is their seat, nil for the owner. The response is already
// written when ok is false.
func (h *PortalActivationsHandler) resolveOwnedLicense(c *gin.Context) (lic *model.License, viewer *model.Seat, ok bool) {
	keyRaw := strings.TrimSpace(c.Param("license_key"))
	if keyRaw == "" {
		response.BadRequest(c, "license_key is required in the URL path")
		return nil, nil, false
	}

	emailVal, _ := c.Get("email")
	emailStr, _ := emailVal.(string)
	if emailStr == "" {
		response.Unauthorized(c, "unauthorized")
		return nil, nil, false
	}

	lic, err := h.store.FindLicenseByKey(c.Request.Context(), keyRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return nil, nil, false
	}
	if err != nil {
		// Don't differentiate "no such license" from "you don't own it"
		// — both 404. Stops portal from being a license-key existence
		// oracle for an authenticated but malicious user.
		response.NotFound(c, "license not found")
		return nil, nil, false
	}

	if strings.EqualFold(lic.Email, emailStr) {
		return lic, nil, true
	}
	// An ACCEPTED member can also manage their own activations on
	// the license. Pending invites confer no authority — possessing
	// an unclaimed invite token shouldn't let an OTP-logged-in user
	// burn through activation slots before going through accept.
	seat, err := h.store.FindSeatByEmail(c.Request.Context(), lic.ID, emailStr)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return nil, nil, false
	}
	if err == nil && seat.AcceptedAt != nil {
		return lic, seat, true
	}
	response.NotFound(c, "license not found")
	return nil, nil, false
}

// visibleDevices is the licence's devices the viewer sees, and may
// remove: every one for the owner (viewer nil), the devices their own
// key activated for a member.
func (h *PortalActivationsHandler) visibleDevices(c *gin.Context, lic *model.License, viewer *model.Seat) ([]*model.Activation, error) {
	rows, err := h.store.ListActivations(c.Request.Context(), lic.ID)
	if err != nil || viewer == nil {
		return rows, err
	}
	return OwnDevices(rows, viewer.ID), nil
}

// GET /api/v1/portal/licenses/:license_key/activations
func (h *PortalActivationsHandler) List(c *gin.Context) {
	lic, viewer, ok := h.resolveOwnedLicense(c)
	if !ok {
		return
	}
	rows, err := h.visibleDevices(c, lic, viewer)
	if err != nil {
		response.Internal(c, err)
		return
	}
	if err := h.store.FillMemberEmails(c.Request.Context(), rows); err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]portalActivationView, 0, len(rows))
	for _, a := range rows {
		out = append(out, portalActivationView{
			ID:             a.ID,
			Identifier:     a.Identifier,
			IdentifierType: a.IdentifierType,
			Label:          a.Label,
			IPAddress:      a.IPAddress,
			CreatedAt:      a.CreatedAt,
			LastVerified:   a.LastVerified,
			SeatID:         a.SeatID,
			MemberEmail:    a.MemberEmail,
		})
	}
	response.OK(c, gin.H{"activations": response.Array(out), "max": maxActivationsForLicense(lic)})
}

// DELETE /api/v1/portal/licenses/:license_key/activations/:activation_id
func (h *PortalActivationsHandler) Delete(c *gin.Context) {
	lic, viewer, ok := h.resolveOwnedLicense(c)
	if !ok {
		return
	}
	activationID := strings.TrimSpace(c.Param("activation_id"))
	if activationID == "" {
		response.BadRequest(c, "activation_id is required")
		return
	}

	// Verify the activation actually belongs to THIS license, and is
	// one the viewer sees: a member removes only their own devices.
	// Without this check the URL pattern would let any portal user
	// delete any activation if they knew its UUID.
	rows, err := h.visibleDevices(c, lic, viewer)
	if err != nil {
		response.Internal(c, err)
		return
	}
	var removed *model.Activation
	for _, a := range rows {
		if a.ID == activationID {
			removed = a
			break
		}
	}
	if removed == nil {
		response.NotFound(c, "activation not found")
		return
	}

	if err := h.store.DeleteActivationByID(c.Request.Context(), activationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Race: activation was already gone. Treat as success.
			response.OK(c, gin.H{"status": "deleted"})
			return
		}
		response.Internal(c, err)
		return
	}

	h.store.Audit(c.Request.Context(), &model.AuditLog{
		Entity: "activation", EntityID: activationID, Action: "deleted",
		ActorType: "portal", ActorID: emailFromContext(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"license_id": lic.ID, "via": "self_service"},
	})
	if h.webhook != nil {
		h.webhook.Dispatch(c.Request.Context(), lic.ProductID, "license.deactivated", map[string]any{
			"license_id": lic.ID, "identifier": removed.Identifier,
		})
	}
	response.OK(c, gin.H{"status": "deleted"})
}

// maxActivationsForLicense returns the activation cap for a license
// (the plan's max_activations, per seat on a plan sold that way). Used
// for UI hints like "2 / 3 devices in use"; 0 when there is no plan to
// say.
func maxActivationsForLicense(lic *model.License) int {
	if lic.Plan == nil {
		return 0
	}
	_, activations := lic.Limits()
	return activations
}

func emailFromContext(c *gin.Context) string {
	v, _ := c.Get("email")
	s, _ := v.(string)
	return s
}
