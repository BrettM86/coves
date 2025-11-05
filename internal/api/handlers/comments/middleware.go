package comments

import (
	"Coves/internal/api/middleware"
	"net/http"
)

// OptionalAuthMiddleware wraps the existing OptionalAuth middleware from the middleware package.
// This ensures comment handlers can access viewer identity when available, but don't require authentication.
//
// Usage in router setup:
//   commentHandler := comments.NewGetCommentsHandler(commentService)
//   router.Handle("/xrpc/social.coves.feed.getComments",
//       comments.OptionalAuthMiddleware(authMiddleware, commentHandler.HandleGetComments))
//
// The middleware extracts the viewer DID from the Authorization header if present and valid,
// making it available via middleware.GetUserDID(r) in the handler.
// If no valid token is present, the request continues as anonymous (empty DID).
func OptionalAuthMiddleware(authMiddleware *middleware.AtProtoAuthMiddleware, next http.HandlerFunc) http.Handler {
	return authMiddleware.OptionalAuth(http.HandlerFunc(next))
}
