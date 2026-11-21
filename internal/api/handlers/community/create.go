package community

import (
	"encoding/json"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/communities"
)

// CreateHandler handles community creation, gated by the instance's
// community creation policy.
type CreateHandler struct {
	service  communities.Service
	creation communities.CreationPolicy
	admins   communities.InstanceAdminChecker
}

// NewCreateHandler creates a new create handler. Unless creation is
// CreationAnyone, only callers admins recognizes as instance admins may create
// a community; any other value, including the zero value, fails closed. It
// panics if admins is nil.
func NewCreateHandler(service communities.Service, creation communities.CreationPolicy, admins communities.InstanceAdminChecker) *CreateHandler {
	if admins == nil {
		panic("community.NewCreateHandler: instance admin checker is required")
	}
	return &CreateHandler{service: service, creation: creation, admins: admins}
}

// HandleCreate creates a new community
// POST /xrpc/social.coves.community.create
// Body matches CreateCommunityRequest
func (h *CreateHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body under the image tier: the request carries inline
	// base64 avatarBlob/bannerBlob bytes (up to 1MB + 2MB raw per the
	// lexicon), so a text-sized cap would reject legitimate uploads.
	var req communities.CreateCommunityRequest
	if !xrpc.DecodeJSON(w, r, reqbody.LimitImage, &req) {
		return
	}

	// Extract authenticated user DID from request context (injected by auth middleware)
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	if h.creation != communities.CreationAnyone && !h.admins.IsInstanceAdmin(userDID) {
		writeError(w, http.StatusForbidden, "CommunityCreationRestricted",
			"Community creation is restricted to instance admins")
		return
	}

	// Client should not send createdByDid - we derive it from authenticated user
	if req.CreatedByDID != "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"createdByDid must not be provided - derived from authenticated user")
		return
	}

	// Client should not send hostedByDid - we derive it from the instance
	if req.HostedByDID != "" {
		writeError(w, http.StatusBadRequest, "InvalidRequest",
			"hostedByDid must not be provided - derived from instance")
		return
	}

	// Set the authenticated user as the creator
	req.CreatedByDID = userDID
	// Note: hostedByDID will be set by the service layer based on instance configuration

	// Create community via service (write-forward to PDS)
	community, err := h.service.CreateCommunity(r.Context(), req)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Return success response matching lexicon output
	response := map[string]interface{}{
		"uri":    community.RecordURI,
		"cid":    community.RecordCID,
		"did":    community.DID,
		"handle": community.Handle,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Log encoding errors but don't return error response (headers already sent)
		// This follows Go's standard practice for HTTP handlers
		_ = err
	}
}
