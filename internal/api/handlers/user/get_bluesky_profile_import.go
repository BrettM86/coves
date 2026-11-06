package user

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
	"Coves/internal/core/users"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// Statuses of getBlueskyProfileImport, in precedence order.
const (
	importStatusNoBlueskyProfile = "noBlueskyProfile"
	importStatusNothingToImport  = "nothingToImport"
	importStatusDismissed        = "dismissed"
	importStatusComplete         = "complete"
	importStatusOffer            = "offer"
)

// BlueskyImportDismissalReader reads when the user dismissed the Bluesky
// profile import: nil when never.
type BlueskyImportDismissalReader interface {
	GetBlueskyProfileImportDismissedAt(ctx context.Context, did string) (*time.Time, error)
}

// BlueskyProfileImportField is one importable field of the user's Bluesky
// profile. Text fields carry their value in Bluesky; image fields carry a
// preview URL in BlueskyImageURL, never the blob ref.
type BlueskyProfileImportField struct {
	Field           string `json:"field"`
	Bluesky         string `json:"bluesky,omitempty"`
	BlueskyImageURL string `json:"blueskyImageUrl,omitempty"`
	CovesIsEmpty    bool   `json:"covesIsEmpty"`
}

// GetBlueskyProfileImportResponse is the response of getBlueskyProfileImport.
type GetBlueskyProfileImportResponse struct {
	Status string                      `json:"status"`
	Fields []BlueskyProfileImportField `json:"fields"`
}

// GetBlueskyProfileImportHandler handles social.coves.actor.getBlueskyProfileImport.
// It reads the user's app.bsky.actor.profile and social.coves.actor.profile
// records from their PDS and reports which Bluesky fields could be imported.
type GetBlueskyProfileImportHandler struct {
	oauthClient      *oauth.ClientApp
	pdsClientFactory PDSClientFactory
	reader           BlueskyImportDismissalReader
	// imagePreviewSource selects where image preview URLs point; the zero
	// value is the Bluesky CDN.
	imagePreviewSource blobs.BlueskyProfileImagePreviewSource
}

// NewGetBlueskyProfileImportHandlerWithFactory creates the handler with a custom PDS client factory.
// Panics if factory or reader is nil.
func NewGetBlueskyProfileImportHandlerWithFactory(factory PDSClientFactory, reader BlueskyImportDismissalReader) *GetBlueskyProfileImportHandler {
	if factory == nil {
		panic("NewGetBlueskyProfileImportHandlerWithFactory: factory is required")
	}
	if reader == nil {
		panic("NewGetBlueskyProfileImportHandlerWithFactory: reader is required")
	}
	return &GetBlueskyProfileImportHandler{pdsClientFactory: factory, reader: reader}
}

// NewGetBlueskyProfileImportHandler creates the handler backed by OAuth sessions.
// Panics if oauthClient or reader is nil.
func NewGetBlueskyProfileImportHandler(oauthClient *oauth.ClientApp, reader BlueskyImportDismissalReader) *GetBlueskyProfileImportHandler {
	if oauthClient == nil {
		panic("NewGetBlueskyProfileImportHandler: oauthClient is required")
	}
	if reader == nil {
		panic("NewGetBlueskyProfileImportHandler: reader is required")
	}
	return &GetBlueskyProfileImportHandler{oauthClient: oauthClient, reader: reader}
}

// WithImagePreviewSource sets where the preview URLs of image fields point.
func (h *GetBlueskyProfileImportHandler) WithImagePreviewSource(source blobs.BlueskyProfileImagePreviewSource) *GetBlueskyProfileImportHandler {
	h.imagePreviewSource = source
	return h
}

// ServeHTTP handles GET /xrpc/social.coves.actor.getBlueskyProfileImport.
func (h *GetBlueskyProfileImportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "Method not allowed")
		return
	}

	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	session := middleware.GetOAuthSession(r)
	if session == nil {
		xrpc.WriteError(w, http.StatusUnauthorized, "MissingSession", "Missing PDS credentials")
		return
	}
	if session.HostURL == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "MissingCredentials", "Missing PDS credentials")
		return
	}

	pdsClient, err := newPDSClient(ctx, h.pdsClientFactory, h.oauthClient, session)
	if err != nil {
		slog.Error("failed to create PDS client",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		sessionRestoreMapper.Write(w, err)
		return
	}
	reader, ok := pdsClient.(pds.BoundedRecordGetter)
	if !ok {
		slog.Error("PDS client does not support bounded record reads", slog.String("did", userDID))
		xrpc.WriteError(w, http.StatusInternalServerError, "InternalError", "Failed to read profile")
		return
	}

	blueskyRecord, err := reader.GetRecordBounded(ctx, blueskyActorProfileCollection, "self", maxImportProfileRecordBytes)
	if errors.Is(err, pds.ErrNotFound) {
		writeImportOffer(w, importStatusNoBlueskyProfile, nil)
		return
	}
	if err != nil {
		slog.Error("failed to read Bluesky profile record",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		putProfileMapper.Write(w, err)
		return
	}

	bluesky := users.ParseBlueskyProfileRecord(blueskyRecord.Value)
	if bluesky.DisplayName == "" && bluesky.Description == "" && bluesky.Avatar == nil && bluesky.Banner == nil {
		writeImportOffer(w, importStatusNothingToImport, nil)
		return
	}

	covesRecord, err := standingProfileRecord(reader.GetRecordBounded(ctx, CovesProfileCollection, "self", maxImportProfileRecordBytes))
	if err != nil {
		slog.Error("failed to read existing profile record",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		putProfileMapper.Write(w, err)
		return
	}
	var coves map[string]interface{}
	if covesRecord != nil {
		coves = covesRecord.Value
	}

	// imageURL returns the preview URL of a Bluesky image; the parser admits
	// only blobs with a canonical CID.
	imageURL := func(blob map[string]interface{}, preset string) string {
		return h.imagePreviewSource.URL(session.HostURL, userDID, profileBlobCID(blob), preset)
	}
	var fields []BlueskyProfileImportField
	if bluesky.DisplayName != "" {
		fields = append(fields, BlueskyProfileImportField{
			Field: importFieldDisplayName, Bluesky: bluesky.DisplayName,
			CovesIsEmpty: covesTextIsEmpty(coves[importFieldDisplayName]),
		})
	}
	if bluesky.Description != "" {
		fields = append(fields, BlueskyProfileImportField{
			Field: importFieldDescription, Bluesky: bluesky.Description,
			CovesIsEmpty: covesTextIsEmpty(coves[importFieldDescription]),
		})
	}
	if bluesky.Avatar != nil {
		fields = append(fields, BlueskyProfileImportField{
			Field: importFieldAvatar, BlueskyImageURL: imageURL(bluesky.Avatar, "avatar"),
			CovesIsEmpty: profileBlobCID(coves[importFieldAvatar]) == "",
		})
	}
	if bluesky.Banner != nil {
		fields = append(fields, BlueskyProfileImportField{
			Field: importFieldBanner, BlueskyImageURL: imageURL(bluesky.Banner, "banner"),
			CovesIsEmpty: profileBlobCID(coves[importFieldBanner]) == "",
		})
	}

	dismissedAt, err := h.reader.GetBlueskyProfileImportDismissedAt(ctx, userDID)
	if err != nil {
		handleServiceError(w, err, userDID, "Bluesky profile import dismissal")
		return
	}

	status := importStatusComplete
	if dismissedAt != nil {
		status = importStatusDismissed
	} else {
		for _, field := range fields {
			if field.CovesIsEmpty {
				status = importStatusOffer
				break
			}
		}
	}
	writeImportOffer(w, status, fields)
}

// covesTextIsEmpty reports whether a Coves profile text value is unset: absent,
// not a string, or whitespace only.
func covesTextIsEmpty(value interface{}) bool {
	text, ok := value.(string)
	return !ok || strings.TrimSpace(text) == ""
}

// writeImportOffer writes the response; fields is always a JSON array.
func writeImportOffer(w http.ResponseWriter, status string, fields []BlueskyProfileImportField) {
	if fields == nil {
		fields = []BlueskyProfileImportField{}
	}
	xrpc.WriteJSON(w, http.StatusOK, GetBlueskyProfileImportResponse{Status: status, Fields: fields})
}
