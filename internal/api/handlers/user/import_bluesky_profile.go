package user

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/users"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// blueskyActorProfileCollection is the atProto collection of a user's Bluesky profile.
const blueskyActorProfileCollection = "app.bsky.actor.profile"

// maxImportProfileRecordBytes bounds each getRecord response the import reads.
// A legitimate profile record is well under this; the cap protects against a
// misbehaving or malicious PDS streaming an unbounded body.
const maxImportProfileRecordBytes = 1 << 20 // 1 MiB

// Field names accepted by importBlueskyProfile.
const (
	importFieldDisplayName = "displayName"
	importFieldDescription = "description"
	importFieldAvatar      = "avatar"
	importFieldBanner      = "banner"
)

// BlueskyImportDismisser records that the user no longer wants the Bluesky
// profile import offered.
type BlueskyImportDismisser interface {
	DismissBlueskyProfileImport(ctx context.Context, did string) error
}

// ImportBlueskyProfileRequest is the request body of importBlueskyProfile:
// field names only, because the values are always re-read from the PDS.
type ImportBlueskyProfileRequest struct {
	Fields []string `json:"fields"`
}

// ImportBlueskyProfileResponse is the response of importBlueskyProfile.
type ImportBlueskyProfileResponse struct {
	URI      string   `json:"uri"`
	CID      string   `json:"cid"`
	Imported []string `json:"imported"`
}

// ImportBlueskyProfileHandler handles social.coves.actor.importBlueskyProfile.
// It copies requested fields of the user's app.bsky.actor.profile record onto
// their social.coves.actor.profile record. Avatar and banner are copied as
// blob refs; the blobs are already in the user's repo, so nothing is uploaded.
type ImportBlueskyProfileHandler struct {
	oauthClient      *oauth.ClientApp
	pdsClientFactory PDSClientFactory
	dismisser        BlueskyImportDismisser
	profileIndexer   ProfileIndexer
}

// NewImportBlueskyProfileHandlerWithFactory creates the handler with a custom PDS client factory.
// Panics if factory or dismisser is nil.
func NewImportBlueskyProfileHandlerWithFactory(factory PDSClientFactory, dismisser BlueskyImportDismisser) *ImportBlueskyProfileHandler {
	if factory == nil {
		panic("NewImportBlueskyProfileHandlerWithFactory: factory is required")
	}
	if dismisser == nil {
		panic("NewImportBlueskyProfileHandlerWithFactory: dismisser is required")
	}
	return &ImportBlueskyProfileHandler{pdsClientFactory: factory, dismisser: dismisser}
}

// NewImportBlueskyProfileHandler creates the handler backed by OAuth sessions.
// Panics if oauthClient or dismisser is nil.
func NewImportBlueskyProfileHandler(oauthClient *oauth.ClientApp, dismisser BlueskyImportDismisser) *ImportBlueskyProfileHandler {
	if oauthClient == nil {
		panic("NewImportBlueskyProfileHandler: oauthClient is required")
	}
	if dismisser == nil {
		panic("NewImportBlueskyProfileHandler: dismisser is required")
	}
	return &ImportBlueskyProfileHandler{oauthClient: oauthClient, dismisser: dismisser}
}

// WithProfileIndexer sets the indexer the imported profile is written through to.
func (h *ImportBlueskyProfileHandler) WithProfileIndexer(indexer ProfileIndexer) *ImportBlueskyProfileHandler {
	h.profileIndexer = indexer
	return h
}

// ServeHTTP handles POST /xrpc/social.coves.actor.importBlueskyProfile.
func (h *ImportBlueskyProfileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
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

	var req ImportBlueskyProfileRequest
	if !xrpc.DecodeJSON(w, r, reqbody.LimitTiny, &req) {
		return
	}
	if message, valid := validateImportFields(req.Fields); !valid {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", message)
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

	// Re-read the Bluesky record: the request names fields, never values.
	blueskyRecord, err := reader.GetRecordBounded(ctx, blueskyActorProfileCollection, "self", maxImportProfileRecordBytes)
	if errors.Is(err, pds.ErrNotFound) {
		writeNothingToImport(w)
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

	changes, imported := importChanges(users.ParseBlueskyProfileRecord(blueskyRecord.Value), req.Fields)
	if len(imported) == 0 {
		writeNothingToImport(w)
		return
	}

	existing, err := standingProfileRecord(reader.GetRecordBounded(ctx, CovesProfileCollection, "self", maxImportProfileRecordBytes))
	if err != nil {
		slog.Error("failed to read existing profile record",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
		putProfileMapper.Write(w, err)
		return
	}

	profile, swapRecord := mergeProfileRecord(existing, changes)
	commit, ok := commitProfileRecord(ctx, w, pdsClient, h.profileIndexer, userDID, session.HostURL, profile, swapRecord)
	if !ok {
		return
	}

	// The import is done once the record is written; a failed dismissal only
	// means the automatic offer may be shown again, so it is logged, not returned.
	// It runs without the request's cancellation so a client disconnecting after
	// the write does not leave the offer in place.
	dismissCtx, cancelDismiss := detachedWriteContext(ctx)
	defer cancelDismiss()
	if err := h.dismisser.DismissBlueskyProfileImport(dismissCtx, userDID); err != nil {
		slog.Error("failed to record Bluesky profile import dismissal after import",
			slog.String("did", userDID),
			slog.String("error", err.Error()),
		)
	}

	xrpc.WriteJSON(w, http.StatusOK, ImportBlueskyProfileResponse{URI: commit.URI, CID: commit.CID, Imported: imported})
}

// validateImportFields checks that fields is a non-empty set of known field
// names without duplicates. It returns the client-facing message when not.
func validateImportFields(fields []string) (string, bool) {
	if len(fields) == 0 {
		return "fields must name at least one field", false
	}
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		switch field {
		case importFieldDisplayName, importFieldDescription, importFieldAvatar, importFieldBanner:
		default:
			return "fields may contain only displayName, description, avatar and banner", false
		}
		if seen[field] {
			return "fields must not repeat a field", false
		}
		seen[field] = true
	}
	return "", true
}

// importChanges selects the requested fields that are importable from the
// Bluesky profile, in request order. Requested fields absent on Bluesky are
// skipped.
func importChanges(bluesky users.ImportableBlueskyProfile, fields []string) (profileChanges, []string) {
	var changes profileChanges
	imported := make([]string, 0, len(fields))
	for _, field := range fields {
		switch field {
		case importFieldDisplayName:
			if bluesky.DisplayName != "" {
				displayName := bluesky.DisplayName
				changes.DisplayName = &displayName
				imported = append(imported, field)
			}
		case importFieldDescription:
			if bluesky.Description != "" {
				description := bluesky.Description
				changes.Description = &description
				imported = append(imported, field)
			}
		case importFieldAvatar:
			if bluesky.Avatar != nil {
				changes.Avatar = bluesky.Avatar
				imported = append(imported, field)
			}
		case importFieldBanner:
			if bluesky.Banner != nil {
				changes.Banner = bluesky.Banner
				imported = append(imported, field)
			}
		}
	}
	return changes, imported
}

func writeNothingToImport(w http.ResponseWriter) {
	xrpc.WriteError(w, http.StatusBadRequest, "NothingToImport", "None of the requested fields can be imported from your Bluesky profile")
}
