package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/core/blobs"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
)

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

// pdsError represents an error returned from the PDS with a specific status code
type pdsError struct {
	StatusCode int
}

func (e *pdsError) Error() string {
	return fmt.Sprintf("PDS returned error %d", e.StatusCode)
}

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

// userBlobOwner implements blobs.BlobOwner for users
// This allows us to use the blob service to upload blobs on behalf of users
type userBlobOwner struct {
	pdsURL      string
	accessToken string
}

// GetPDSURL returns the PDS URL for this user
func (u *userBlobOwner) GetPDSURL() string {
	return u.pdsURL
}

// GetPDSAccessToken returns the access token for authenticating with the PDS
func (u *userBlobOwner) GetPDSAccessToken() string {
	return u.accessToken
}

// UpdateProfileHandler handles POST /xrpc/social.coves.actor.updateProfile
// This endpoint allows authenticated users to update their profile on their PDS.
// The handler:
// 1. Validates the user is authenticated via OAuth
// 2. Validates avatar/banner size and mime type constraints
// 3. Uploads any provided blobs to the user's PDS
// 4. Puts the profile record to the user's PDS via com.atproto.repo.putRecord
type UpdateProfileHandler struct {
	blobService blobs.Service
	httpClient  *http.Client // For making PDS calls
}

// NewUpdateProfileHandler creates a new update profile handler
func NewUpdateProfileHandler(blobService blobs.Service, httpClient *http.Client) *UpdateProfileHandler {
	// Use default client if none provided
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}
	return &UpdateProfileHandler{
		blobService: blobService,
		httpClient:  httpClient,
	}
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

	pdsURL := session.HostURL
	accessToken := session.AccessToken
	if pdsURL == "" || accessToken == "" {
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

	// 4. Create blob owner for user (implements blobs.BlobOwner interface)
	owner := &userBlobOwner{pdsURL: pdsURL, accessToken: accessToken}

	// 5. Build profile record
	profile := map[string]interface{}{
		"$type": "app.bsky.actor.profile",
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
		avatarRef, err := h.blobService.UploadBlob(ctx, owner, req.AvatarBlob, req.AvatarMimeType)
		if err != nil {
			slog.Error("failed to upload avatar blob",
				slog.String("did", userDID),
				slog.String("error", err.Error()),
			)
			writeUpdateProfileError(w, http.StatusInternalServerError, "BlobUploadFailed", "Failed to upload avatar")
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
		bannerRef, err := h.blobService.UploadBlob(ctx, owner, req.BannerBlob, req.BannerMimeType)
		if err != nil {
			slog.Error("failed to upload banner blob",
				slog.String("did", userDID),
				slog.String("error", err.Error()),
			)
			writeUpdateProfileError(w, http.StatusInternalServerError, "BlobUploadFailed", "Failed to upload banner")
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
	uri, cid, err := h.putProfileRecord(ctx, session, userDID, profile)
	if err != nil {
		slog.Error("failed to put profile record to PDS",
			slog.String("did", userDID),
			slog.String("pds_url", pdsURL),
			slog.String("error", err.Error()),
		)
		// Map PDS status codes to user-friendly messages
		var pdsErr *pdsError
		if errors.As(err, &pdsErr) {
			switch pdsErr.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				writeUpdateProfileError(w, http.StatusUnauthorized, "AuthExpired", "Your session may have expired. Please re-authenticate.")
				return
			case http.StatusTooManyRequests:
				writeUpdateProfileError(w, http.StatusTooManyRequests, "RateLimited", "Too many requests. Please try again later.")
				return
			case http.StatusRequestEntityTooLarge:
				writeUpdateProfileError(w, http.StatusBadRequest, "PayloadTooLarge", "Profile data exceeds PDS limits.")
				return
			}
		}
		writeUpdateProfileError(w, http.StatusInternalServerError, "PDSError", "Failed to update profile")
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

// putProfileRecord calls com.atproto.repo.putRecord on the user's PDS
// This creates or updates the user's profile record at:
// at://{did}/app.bsky.actor.profile/self
func (h *UpdateProfileHandler) putProfileRecord(ctx context.Context, session *oauthlib.ClientSessionData, did string, profile map[string]interface{}) (string, string, error) {
	pdsURL := session.HostURL
	accessToken := session.AccessToken

	// Build the putRecord request body
	putRecordReq := map[string]interface{}{
		"repo":       did,
		"collection": "app.bsky.actor.profile",
		"rkey":       "self",
		"record":     profile,
	}

	reqBody, err := json.Marshal(putRecordReq)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal putRecord request: %w", err)
	}

	// Build the endpoint URL
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.repo.putRecord", pdsURL)

	// Create the HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", "", fmt.Errorf("failed to create PDS request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	// Execute the request
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("PDS request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close PDS response body", slog.String("error", closeErr.Error()))
		}
	}()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read PDS response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		// Truncate error body for logging to prevent leaking sensitive data
		bodyPreview := string(body)
		if len(bodyPreview) > 200 {
			bodyPreview = bodyPreview[:200] + "... (truncated)"
		}
		slog.Error("PDS putRecord failed",
			slog.Int("status", resp.StatusCode),
			slog.String("body", bodyPreview),
		)
		return "", "", &pdsError{StatusCode: resp.StatusCode}
	}

	// Parse the successful response
	var result struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", fmt.Errorf("failed to parse PDS response: %w", err)
	}

	if result.URI == "" || result.CID == "" {
		return "", "", fmt.Errorf("PDS response missing required fields (uri or cid)")
	}

	return result.URI, result.CID, nil
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
