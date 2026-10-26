// Package media serves the image upload endpoint.
package media

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	coremedia "Coves/internal/core/media"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// blobUploadScope is the OAuth scope Coves requests for blob uploads. The
// sanitized output's MIME type is only known after ingest (an oriented WebP is
// stored as JPEG or PNG), so a narrower blob scope is not enough.
const blobUploadScope = "blob:*/*"

// UploadImageHandler handles POST /xrpc/social.coves.media.uploadImage: it
// sanitizes the raw image body and stores it as a blob in the caller's repo.
// An OAuth caller writes with its own session. An aggregator authenticated by
// API key or service JWT has its stored OAuth session checked for the blob
// scope, then its repo opened the way post.create opens it: the repo factory
// resumes the stored session itself.
type UploadImageHandler struct {
	uploads        *coremedia.UploadService
	repoFactory    posts.AuthorRepoFactory
	storedSessions StoredSessionLoader
}

// StoredSessionLoader loads the stored OAuth session of a caller that
// authenticated without one (an aggregator's API key or service JWT).
type StoredSessionLoader interface {
	StoredSession(ctx context.Context, aggregatorDID string) (*oauth.ClientSessionData, error)
}

// NewUploadImageHandler builds the handler. It does not validate its
// arguments, so route tables can be built without a PDS-capable factory.
func NewUploadImageHandler(uploads *coremedia.UploadService, repoFactory posts.AuthorRepoFactory, storedSessions StoredSessionLoader) *UploadImageHandler {
	return &UploadImageHandler{uploads: uploads, repoFactory: repoFactory, storedSessions: storedSessions}
}

type aspectRatioResponse struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type uploadImageResponse struct {
	Blob        *blobs.BlobRef      `json:"blob"`
	MimeType    string              `json:"mimeType"`
	Size        int                 `json:"size"`
	AspectRatio aspectRatioResponse `json:"aspectRatio"`
}

func (h *UploadImageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userDID := middleware.GetUserDID(r)
	if userDID == "" {
		xrpc.WriteError(w, http.StatusUnauthorized, "AuthRequired", "Authentication required")
		return
	}
	session := middleware.GetOAuthSession(r)
	// The repo factory gets the caller's own OAuth session, or nil for a
	// caller without one, which it then resumes from the store.
	repoSession := session
	if session == nil {
		// Authenticated without an OAuth session (an aggregator's API key or
		// service JWT): the stored session is loaded only for the scope check,
		// before the body is read, so a caller that cannot upload sends
		// nothing more.
		var ok bool
		if session, ok = h.loadStoredSession(ctx, w, userDID); !ok {
			return
		}
	}
	if !slices.Contains(session.Scopes, blobUploadScope) {
		xrpc.WriteError(w, http.StatusForbidden, "MissingBlobScope",
			"Your session lacks the "+blobUploadScope+" scope; sign in again to grant it")
		return
	}

	data, err := reqbody.ReadBytes(w, r, reqbody.LimitImage)
	if err != nil {
		var tooLarge *reqbody.TooLargeError
		if errors.As(err, &tooLarge) {
			xrpc.WriteError(w, http.StatusRequestEntityTooLarge, string(coremedia.ErrorCodeImageTooLarge),
				"Image exceeds the 10,000,000 byte limit")
			return
		}
		cause := classifyBodyReadFailure(err)
		slog.Warn("image upload: reading the request body failed", "did", userDID, "cause", cause)
		if cause == "timeout" {
			// A slow uplink hitting the server's read deadline, not a malformed request.
			xrpc.WriteError(w, http.StatusRequestTimeout, "RequestTimeout",
				"The request body was not received in time; try again")
			return
		}
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "Could not read the request body")
		return
	}

	repo, err := h.repoFactory(ctx, userDID, repoSession)
	if err != nil {
		// The factory's error text carries DIDs and the OAuth session ID, so
		// only its classified cause is logged.
		mapping, _ := openRepoErrorMapper.Resolve(err)
		logFailure(ctx, "image upload: opening the caller's repo failed", userDID, mapping, err)
		xrpc.WriteError(w, mapping.Status, mapping.Code, mapping.Message)
		return
	}

	uploaded, err := h.uploads.Upload(ctx, userDID, repo, coremedia.Input{
		Data:                data,
		DeclaredContentType: r.Header.Get("Content-Type"),
	})
	if err != nil {
		h.writeUploadError(ctx, w, userDID, err)
		return
	}

	slog.Info("image uploaded", "did", userDID, "size", uploaded.Size, "mimeType", uploaded.MimeType)
	xrpc.WriteJSON(w, http.StatusOK, uploadImageResponse{
		Blob:     uploaded.Blob,
		MimeType: uploaded.MimeType,
		Size:     uploaded.Size,
		AspectRatio: aspectRatioResponse{
			Width:  uploaded.AspectRatio.Width,
			Height: uploaded.AspectRatio.Height,
		},
	})
}

// loadStoredSession loads the stored OAuth session of a caller that
// authenticated without one, for the scope check, writing the error response
// when there is none.
func (h *UploadImageHandler) loadStoredSession(ctx context.Context, w http.ResponseWriter, userDID string) (*oauth.ClientSessionData, bool) {
	var session *oauth.ClientSessionData
	err := aggregators.ErrOAuthSessionDead // no loader wired: no stored credentials
	if h.storedSessions != nil {
		session, err = h.storedSessions.StoredSession(ctx, userDID)
		if err == nil && session == nil {
			err = aggregators.ErrOAuthSessionDead
		}
	}
	if err != nil {
		// The store's error text can carry DIDs and session IDs, so only its
		// classified cause is logged.
		mapping := xrpc.Mapping{
			Status: http.StatusServiceUnavailable, Code: "ServiceUnavailable",
			Message: "Could not load your stored credentials; try again",
		}
		if errors.Is(err, aggregators.ErrOAuthSessionDead) {
			mapping = xrpc.Mapping{
				Status: http.StatusServiceUnavailable, Code: "NoAuthorCredentials",
				Message: "No credentials are available to write to your repository",
			}
		}
		logFailure(ctx, "image upload: loading the caller's stored session failed", userDID, mapping, err)
		xrpc.WriteError(w, mapping.Status, mapping.Code, mapping.Message)
		return nil, false
	}
	return session, true
}

func (h *UploadImageHandler) writeUploadError(ctx context.Context, w http.ResponseWriter, userDID string, err error) {
	var mediaErr *coremedia.Error
	if !errors.As(err, &mediaErr) {
		// Not a pipeline verdict: the decode slot wait was cancelled or timed out.
		slog.Warn("image upload: processing did not complete", "did", userDID, "error", err)
		xrpc.WriteError(w, http.StatusServiceUnavailable, "ServiceUnavailable",
			"Image processing is busy; try again")
		return
	}

	mapping, matched := uploadErrorMapper.Resolve(err)
	if mediaErr.Code == coremedia.ErrorCodePDSUploadFailed || !matched {
		// The wrapped error carries the PDS's response text; log only its cause.
		logFailure(ctx, "image upload: the PDS did not store the blob", userDID, mapping, err)
	}
	xrpc.WriteError(w, mapping.Status, mapping.Code, mapping.Message)
}

// classifyBodyReadFailure names why reading the request body failed, without
// the error's text.
func classifyBodyReadFailure(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "other"
	}
}
