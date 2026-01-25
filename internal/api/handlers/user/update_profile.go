package user

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/pds"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// CovesProfileCollection is the atProto collection for Coves user profiles.
// NOTE: This constant is intentionally duplicated in internal/atproto/jetstream/user_consumer.go
// to avoid circular dependencies between packages. Keep both definitions in sync.
const CovesProfileCollection = "social.coves.actor.profile"

// PDSClientFactory creates PDS clients from session data.
// Used to allow injection of different auth mechanisms (OAuth for production, password for E2E tests).
type PDSClientFactory func(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error)

const (
	// MaxDisplayNameLength is the maximum allowed length for display names (per atProto lexicon)
	MaxDisplayNameLength = 64
	// MaxBioLength is the maximum allowed length for bio/description (per atProto lexicon)
	MaxBioLength = 256
	// MaxAvatarBlobSize is the maximum allowed avatar size in bytes (1MB per lexicon)
	MaxAvatarBlobSize = 1_000_000
	// MaxBannerBlobSize is the maximum allowed banner size in bytes (2MB per lexicon)
	MaxBannerBlobSize = 2_000_000
	// MaxRequestBodySize is the maximum request body size (10MB to accommodate base64 overhead)
	MaxRequestBodySize = 10_000_000
)

// UpdateProfileRequest represents the request body for updating a user profile
type UpdateProfileRequest struct {
	DisplayName    *string `json:"displayName,omitempty"`
	Bio            *string `json:"bio,omitempty"`
	AvatarBlob     []byte  `json:"avatarBlob,omitempty"`
	AvatarMimeType string  `json:"avatarMimeType,omitempty"`
	BannerBlob     []byte  `json:"bannerBlob,omitempty"`
	BannerMimeType string  `json:"bannerMimeType,omitempty"`
}

// UpdateProfileResponse represents the response from updating a profile
type UpdateProfileResponse struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// UpdateProfileHandler handles POST /xrpc/social.coves.actor.updateProfile
// This endpoint allows authenticated users to update their Coves profile on their PDS.
// It validates inputs, uploads any provided blobs, and writes the profile record.
type UpdateProfileHandler struct {
	oauthClient      *oauth.ClientApp // For creating authenticated PDS clients (production)
	pdsClientFactory PDSClientFactory // Optional: custom factory for testing
}

// NewUpdateProfileHandler creates a new update profile handler.
// Panics if oauthClient is nil - use NewUpdateProfileHandlerWithFactory for testing.
func NewUpdateProfileHandler(oauthClient *oauth.ClientApp) *UpdateProfileHandler {
	if oauthClient == nil {
		panic("NewUpdateProfileHandler: oauthClient is required")
	}
	return &UpdateProfileHandler{
		oauthClient: oauthClient,
	}
}

// NewUpdateProfileHandlerWithFactory creates a new update profile handler with a custom PDS client factory.
// This is primarily for E2E testing with password-based authentication instead of OAuth.
// Panics if factory is nil.
func NewUpdateProfileHandlerWithFactory(factory PDSClientFactory) *UpdateProfileHandler {
	if factory == nil {
		panic("NewUpdateProfileHandlerWithFactory: factory is required")
	}
	return &UpdateProfileHandler{
		pdsClientFactory: factory,
	}
}

// getPDSClient creates a PDS client from an OAuth session.
// If a custom factory was provided (for testing), uses that.
// Otherwise, uses DPoP authentication via indigo's ClientApp for proper OAuth token handling.
func (h *UpdateProfileHandler) getPDSClient(ctx context.Context, session *oauth.ClientSessionData) (pds.Client, error) {
	// Use custom factory if provided (e.g., for E2E testing with password auth)
	if h.pdsClientFactory != nil {
		return h.pdsClientFactory(ctx, session)
	}

	// Production path: use OAuth with DPoP
	if h.oauthClient == nil {
		return nil, fmt.Errorf("OAuth client not configured")
	}

	return pds.NewFromOAuthSession(ctx, h.oauthClient, session)
}

// ServeHTTP handles the update profile request
func (h *UpdateProfileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Check HTTP method
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 1. Get authenticated user from context
	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		writeUpdateProfileError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}

	// Get OAuth session for PDS URL and access token
	session := middleware.GetOAuthSession(r)
	if session == nil {
		writeUpdateProfileError(w, http.StatusUnauthorized, "MissingSession", "Missing PDS credentials")
		return
	}

	if session.HostURL == "" {
		writeUpdateProfileError(w, http.StatusUnauthorized, "MissingCredentials", "Missing PDS credentials")
		return
	}

	// 2. Parse request
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodySize)
	var req UpdateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeUpdateProfileError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
		return
	}

	// Validate displayName length
	if req.DisplayName != nil && len(*req.DisplayName) > MaxDisplayNameLength {
		writeUpdateProfileError(w, http.StatusBadRequest, "DisplayNameTooLong",
			fmt.Sprintf("Display name exceeds %d character limit", MaxDisplayNameLength))
		return
	}

	// Validate bio length
	if req.Bio != nil && len(*req.Bio) > MaxBioLength {
		writeUpdateProfileError(w, http.StatusBadRequest, "BioTooLong",
			fmt.Sprintf("Bio exceeds %d character limit", MaxBioLength))
		return
	}

	// 3. Validate blob sizes and mime types
	if len(req.AvatarBlob) > 0 {
		// Validate mime type is provided when blob is provided
		if req.AvatarMimeType == "" {
			writeUpdateProfileError(w, http.StatusBadRequest, "InvalidRequest", "Avatar blob provided without mime type")
			return
		}
		// Validate size (1MB max for avatar per lexicon)
		if len(req.AvatarBlob) > MaxAvatarBlobSize {
			writeUpdateProfileError(w, http.StatusBadRequest, "AvatarTooLarge", "Avatar exceeds 1MB limit")
			return
		}
		if !isValidImageMimeType(req.AvatarMimeType) {
			writeUpdateProfileError(w, http.StatusBadRequest, "InvalidMimeType", "Invalid avatar mime type")
			return
		}
	}

	if len(req.BannerBlob) > 0 {
		// Validate mime type is provided when blob is provided
		if req.BannerMimeType == "" {
			writeUpdateProfileError(w, http.StatusBadRequest, "InvalidRequest", "Banner blob provided without mime type")
			return
		}
		// Validate size (2MB max for banner per lexicon)
		if len(req.BannerBlob) > MaxBannerBlobSize {
			writeUpdateProfileError(w, http.StatusBadRequest, "BannerTooLarge", "Banner exceeds 2MB limit")
			return
		}
		if !isValidImageMimeType(req.BannerMimeType) {
			writeUpdateProfileError(w, http.StatusBadRequest, "InvalidMimeType", "Invalid banner mime type")
			return
		}
	}

	// 4. Create PDS client (uses factory if provided, otherwise OAuth with DPoP)
	pdsClient, err := h.getPDSClient(ctx, session)
	if err != nil {
		slog.Error("failed to create PDS client",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		writeUpdateProfileError(w, http.StatusUnauthorized, "SessionError",
			"Failed to restore session. Please sign in again.")
		return
	}

	// 5. Build profile record
	profile := map[string]interface{}{
		"$type": CovesProfileCollection,
	}

	// Add displayName if provided
	if req.DisplayName != nil {
		profile["displayName"] = *req.DisplayName
	}

	// Add bio (description) if provided
	if req.Bio != nil {
		profile["description"] = *req.Bio
	}

	// 6. Upload avatar blob if provided
	if len(req.AvatarBlob) > 0 {
		avatarRef, err := pdsClient.UploadBlob(ctx, req.AvatarBlob, req.AvatarMimeType)
		if err != nil {
			slog.Error("failed to upload avatar blob",
				slog.String("did", userDID),
				slog.String("error", err.Error()),
			)
			// Map specific PDS errors to user-friendly messages
			switch {
			case errors.Is(err, pds.ErrUnauthorized), errors.Is(err, pds.ErrForbidden):
				writeUpdateProfileError(w, http.StatusUnauthorized, "AuthExpired", "Your session may have expired. Please re-authenticate.")
			case errors.Is(err, pds.ErrRateLimited):
				writeUpdateProfileError(w, http.StatusTooManyRequests, "RateLimited", "Too many requests. Please try again later.")
			case errors.Is(err, pds.ErrPayloadTooLarge):
				writeUpdateProfileError(w, http.StatusRequestEntityTooLarge, "AvatarTooLarge", "Avatar exceeds PDS size limit.")
			default:
				writeUpdateProfileError(w, http.StatusInternalServerError, "BlobUploadFailed", "Failed to upload avatar")
			}
			return
		}
		if avatarRef == nil || avatarRef.Ref == nil || avatarRef.Type == "" {
			slog.Error("invalid blob reference returned from avatar upload", slog.String("did", userDID))
			writeUpdateProfileError(w, http.StatusInternalServerError, "BlobUploadFailed", "Invalid avatar blob reference")
			return
		}
		profile["avatar"] = map[string]interface{}{
			"$type":    avatarRef.Type,
			"ref":      avatarRef.Ref,
			"mimeType": avatarRef.MimeType,
			"size":     avatarRef.Size,
		}
	}

	// 7. Upload banner blob if provided
	if len(req.BannerBlob) > 0 {
		bannerRef, err := pdsClient.UploadBlob(ctx, req.BannerBlob, req.BannerMimeType)
		if err != nil {
			slog.Error("failed to upload banner blob",
				slog.String("did", userDID),
				slog.String("error", err.Error()),
			)
			// Map specific PDS errors to user-friendly messages
			switch {
			case errors.Is(err, pds.ErrUnauthorized), errors.Is(err, pds.ErrForbidden):
				writeUpdateProfileError(w, http.StatusUnauthorized, "AuthExpired", "Your session may have expired. Please re-authenticate.")
			case errors.Is(err, pds.ErrRateLimited):
				writeUpdateProfileError(w, http.StatusTooManyRequests, "RateLimited", "Too many requests. Please try again later.")
			case errors.Is(err, pds.ErrPayloadTooLarge):
				writeUpdateProfileError(w, http.StatusRequestEntityTooLarge, "BannerTooLarge", "Banner exceeds PDS size limit.")
			default:
				writeUpdateProfileError(w, http.StatusInternalServerError, "BlobUploadFailed", "Failed to upload banner")
			}
			return
		}
		if bannerRef == nil || bannerRef.Ref == nil || bannerRef.Type == "" {
			slog.Error("invalid blob reference returned from banner upload", slog.String("did", userDID))
			writeUpdateProfileError(w, http.StatusInternalServerError, "BlobUploadFailed", "Invalid banner blob reference")
			return
		}
		profile["banner"] = map[string]interface{}{
			"$type":    bannerRef.Type,
			"ref":      bannerRef.Ref,
			"mimeType": bannerRef.MimeType,
			"size":     bannerRef.Size,
		}
	}

	// 8. Put profile record to PDS using com.atproto.repo.putRecord
	uri, cid, err := pdsClient.PutRecord(ctx, CovesProfileCollection, "self", profile, "")
	if err != nil {
		slog.Error("failed to put profile record to PDS",
			slog.String("did", userDID),
			slog.String("pds_url", session.HostURL),
			slog.String("error", err.Error()),
		)
		// Map PDS errors to user-friendly messages
		switch {
		case errors.Is(err, pds.ErrUnauthorized), errors.Is(err, pds.ErrForbidden):
			writeUpdateProfileError(w, http.StatusUnauthorized, "AuthExpired", "Your session may have expired. Please re-authenticate.")
		case errors.Is(err, pds.ErrRateLimited):
			writeUpdateProfileError(w, http.StatusTooManyRequests, "RateLimited", "Too many requests. Please try again later.")
		case errors.Is(err, pds.ErrPayloadTooLarge):
			writeUpdateProfileError(w, http.StatusRequestEntityTooLarge, "PayloadTooLarge", "Profile data exceeds PDS size limit.")
		default:
			writeUpdateProfileError(w, http.StatusInternalServerError, "PDSError", "Failed to update profile")
		}
		return
	}

	// 9. Return success response
	resp := UpdateProfileResponse{URI: uri, CID: cid}

	// Marshal to bytes first to catch encoding errors before writing headers
	responseBytes, err := json.Marshal(resp)
	if err != nil {
		slog.Error("failed to marshal update profile response",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		writeUpdateProfileError(w, http.StatusInternalServerError, "InternalError", "Failed to encode response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, writeErr := w.Write(responseBytes); writeErr != nil {
		slog.Warn("failed to write update profile response",
			slog.String("did", userDID),
			slog.String("error", writeErr.Error()),
		)
	}
}

// isValidImageMimeType checks if the MIME type is allowed for profile images
func isValidImageMimeType(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp":
		return true
	default:
		return false
	}
}

// writeUpdateProfileError writes a JSON error response for update profile failures
func writeUpdateProfileError(w http.ResponseWriter, statusCode int, errorType, message string) {
	responseBytes, err := json.Marshal(map[string]interface{}{
		"error":   errorType,
		"message": message,
	})
	if err != nil {
		// Fallback to plain text if JSON encoding fails
		slog.Error("failed to marshal error response", slog.String("error", err.Error()))
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(message))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if _, writeErr := w.Write(responseBytes); writeErr != nil {
		slog.Warn("failed to write error response", slog.String("error", writeErr.Error()))
	}
}
