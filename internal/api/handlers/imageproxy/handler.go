// Package imageproxy provides HTTP handlers for the image proxy service.
// It handles requests for proxied and transformed images from AT Protocol PDSes.
package imageproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/imageproxy"
)

// Service defines the interface for the image proxy service.
// This interface is implemented by the imageproxy package's service layer.
type Service interface {
	// GetImage retrieves and processes an image from a PDS.
	// preset: the image transformation preset (e.g., "avatar", "banner")
	// did: the DID of the user who owns the blob
	// cid: the content identifier of the blob
	// pdsURL: the URL of the user's PDS
	GetImage(ctx context.Context, preset, did, cid, pdsURL string) ([]byte, error)
}

// Handler handles HTTP requests for the image proxy.
type Handler struct {
	service          Service
	identityResolver identity.Resolver
}

// NewHandler creates a new image proxy handler.
func NewHandler(service Service, resolver identity.Resolver) *Handler {
	return &Handler{
		service:          service,
		identityResolver: resolver,
	}
}

// HandleImage handles GET /img/{preset}/plain/{did}/{cid}
// It fetches the image from the user's PDS, transforms it according to the preset,
// and returns the result with appropriate caching headers.
func (h *Handler) HandleImage(w http.ResponseWriter, r *http.Request) {
	// Parse URL parameters
	preset := chi.URLParam(r, "preset")
	did := chi.URLParam(r, "did")
	cid := chi.URLParam(r, "cid")

	// Validate required parameters
	if preset == "" || did == "" || cid == "" {
		writeErrorResponse(w, http.StatusBadRequest, "missing required parameters")
		return
	}

	// Validate preset exists before proceeding
	if _, err := imageproxy.GetPreset(preset); err != nil {
		if errors.Is(err, imageproxy.ErrInvalidPreset) {
			writeErrorResponse(w, http.StatusBadRequest, "invalid preset: "+preset)
			return
		}
		writeErrorResponse(w, http.StatusBadRequest, "invalid preset")
		return
	}

	// Validate DID format (must be did:plc: or did:web:)
	if err := imageproxy.ValidateDID(did); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid DID format")
		return
	}

	// Validate CID format (must be valid base32/base58 CID)
	if err := imageproxy.ValidateCID(cid); err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid CID format")
		return
	}

	// Generate ETag for caching
	etag := fmt.Sprintf(`"%s-%s"`, preset, cid)

	// Check If-None-Match header for 304 response
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Resolve DID to get PDS URL
	didDoc, err := h.identityResolver.ResolveDID(r.Context(), did)
	if err != nil {
		slog.Warn("[IMAGE-PROXY] failed to resolve DID",
			"did", did,
			"error", err,
		)
		writeErrorResponse(w, http.StatusBadGateway, "failed to resolve DID")
		return
	}

	// Extract PDS URL from DID document
	pdsURL := getPDSEndpoint(didDoc)
	if pdsURL == "" {
		slog.Warn("[IMAGE-PROXY] no PDS endpoint found in DID document",
			"did", did,
		)
		writeErrorResponse(w, http.StatusBadGateway, "no PDS endpoint found")
		return
	}

	// Fetch and process the image
	imageData, err := h.service.GetImage(r.Context(), preset, did, cid, pdsURL)
	if err != nil {
		handleServiceError(w, err)
		return
	}

	// Set response headers
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", etag)

	// Write image data
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(imageData); err != nil {
		slog.Warn("[IMAGE-PROXY] failed to write image response",
			"preset", preset,
			"did", did,
			"cid", cid,
			"error", err,
		)
	}
}

// getPDSEndpoint extracts the PDS service endpoint from a DID document.
func getPDSEndpoint(doc *identity.DIDDocument) string {
	if doc == nil {
		return ""
	}
	for _, service := range doc.Service {
		if service.Type == "AtprotoPersonalDataServer" {
			return service.ServiceEndpoint
		}
	}
	return ""
}

// handleServiceError converts service errors to appropriate HTTP responses.
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, imageproxy.ErrPDSNotFound):
		writeErrorResponse(w, http.StatusNotFound, "blob not found")
	case errors.Is(err, imageproxy.ErrPDSTimeout):
		writeErrorResponse(w, http.StatusGatewayTimeout, "request timed out")
	// ONE BRANCH FOR BOTH, so the status and the body cannot drift apart. A
	// guard refusal must be indistinguishable from a PDS that is simply
	// unreachable: the endpoint comes from a DID document anyone can mint, and
	// this route needs no credential, so a distinct status — or the same status
	// with a distinct body, which is the same oracle read one line further
	// down — tells a stranger probing addresses which ones are internal.
	//
	// The internal distinction survives in the log line below and in
	// errors.Is at every other caller; only the response is flattened.
	case errors.Is(err, imageproxy.ErrPDSFetchFailed), errors.Is(err, imageproxy.ErrPDSBlocked):
		if errors.Is(err, imageproxy.ErrPDSBlocked) {
			slog.Warn("[IMAGE-PROXY] SSRF guard refused a PDS endpoint",
				"error", err,
			)
		}
		writeErrorResponse(w, http.StatusBadGateway, "failed to fetch blob from PDS")
	case errors.Is(err, imageproxy.ErrInvalidPreset):
		writeErrorResponse(w, http.StatusBadRequest, "invalid preset")
	case errors.Is(err, imageproxy.ErrInvalidDID):
		writeErrorResponse(w, http.StatusBadRequest, "invalid DID format")
	case errors.Is(err, imageproxy.ErrInvalidCID):
		writeErrorResponse(w, http.StatusBadRequest, "invalid CID format")
	case errors.Is(err, imageproxy.ErrUnsupportedFormat):
		writeErrorResponse(w, http.StatusBadRequest, "unsupported image format")
	case errors.Is(err, imageproxy.ErrImageTooLarge):
		writeErrorResponse(w, http.StatusBadRequest, "image too large")
	case errors.Is(err, imageproxy.ErrProcessingFailed):
		writeErrorResponse(w, http.StatusInternalServerError, "image processing failed")
	default:
		slog.Error("[IMAGE-PROXY] unhandled service error",
			"error", err,
		)
		writeErrorResponse(w, http.StatusInternalServerError, "internal server error")
	}
}

// writeErrorResponse writes a plain text error response.
// For the image proxy, we use simple text responses rather than JSON
// since the expected response is binary image data.
//
// Errors are explicitly uncacheable. Success responses advertise a one-year
// immutable lifetime, which is correct for content-addressed blobs but
// catastrophic for a failure: this route sits behind a CDN, and a transient
// PDS timeout or a DID that had not yet propagated would otherwise be pinned
// at the edge for a year, long after the image became fetchable.
func writeErrorResponse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(message)); err != nil {
		slog.Warn("[IMAGE-PROXY] failed to write error response",
			"status", status,
			"message", message,
			"error", err,
		)
	}
}
