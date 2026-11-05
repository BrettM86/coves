package routes

import (
	"time"

	mediahandlers "Coves/internal/api/handlers/media"
	"Coves/internal/api/middleware"
	"Coves/internal/core/media"
	"Coves/internal/core/posts"

	"github.com/go-chi/chi/v5"
)

// RegisterMediaRoutes registers social.coves.media.* XRPC endpoints.
//
// uploadImage stores the image in the caller's own repo through repoFactory,
// using a session granted the blob scope: an OAuth caller's own session, or,
// for an aggregator authenticated by API key or service JWT, its stored OAuth
// session loaded through storedSessions. uploadReadTimeout and writeTimeout
// set the connection deadlines uploadImage applies before reading the body.
func RegisterMediaRoutes(r chi.Router, dualAuth *middleware.DualAuthMiddleware, uploads *media.UploadService, repoFactory posts.AuthorRepoFactory, storedSessions mediahandlers.StoredSessionLoader, uploadReadTimeout, writeTimeout time.Duration) {
	uploadImageHandler := mediahandlers.NewUploadImageHandler(uploads, repoFactory, storedSessions, uploadReadTimeout, writeTimeout)
	r.With(dualAuth.RequireAuth).Post("/xrpc/social.coves.media.uploadImage", uploadImageHandler.ServeHTTP)
}
