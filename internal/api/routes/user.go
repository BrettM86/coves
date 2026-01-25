package routes

import (
	"Coves/internal/api/handlers/user"
	"Coves/internal/api/middleware"
	"Coves/internal/core/users"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
)

// UserHandler handles user-related XRPC endpoints
type UserHandler struct {
	userService users.UserService
}

// NewUserHandler creates a new user handler
func NewUserHandler(userService users.UserService) *UserHandler {
	return &UserHandler{
		userService: userService,
	}
}

// UserRouteOptions contains optional configuration for user routes.
// Use this to inject test dependencies like custom PDS client factories.
type UserRouteOptions struct {
	// PDSClientFactory overrides the default OAuth-based PDS client creation.
	// If nil, uses OAuth with DPoP (production behavior).
	// Set this in E2E tests to use password-based authentication.
	PDSClientFactory user.PDSClientFactory
}

// RegisterUserRoutes registers user-related XRPC endpoints on the router
// Implements social.coves.actor.* lexicon endpoints
func RegisterUserRoutes(r chi.Router, service users.UserService, authMiddleware *middleware.OAuthAuthMiddleware, oauthClient *oauth.ClientApp) {
	RegisterUserRoutesWithOptions(r, service, authMiddleware, oauthClient, nil)
}

// RegisterUserRoutesWithOptions registers user-related XRPC endpoints with optional configuration.
// Use opts to inject test dependencies like custom PDS client factories.
func RegisterUserRoutesWithOptions(r chi.Router, service users.UserService, authMiddleware *middleware.OAuthAuthMiddleware, oauthClient *oauth.ClientApp, opts *UserRouteOptions) {
	h := NewUserHandler(service)

	// social.coves.actor.getprofile - query endpoint (public)
	r.Get("/xrpc/social.coves.actor.getprofile", h.GetProfile)

	// social.coves.actor.signup - procedure endpoint (public)
	r.Post("/xrpc/social.coves.actor.signup", h.Signup)

	// social.coves.actor.deleteAccount - procedure endpoint (authenticated)
	// Deletes the authenticated user's account from the Coves AppView.
	// This ONLY deletes AppView indexed data, NOT the user's atProto identity on their PDS.
	deleteHandler := user.NewDeleteHandler(service)
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.actor.deleteAccount", deleteHandler.HandleDeleteAccount)

	// social.coves.actor.updateProfile - procedure endpoint (authenticated)
	// Updates the authenticated user's profile on their PDS (avatar, banner, displayName, bio).
	// This writes directly to the user's PDS and the Jetstream consumer will index the change.
	var updateProfileHandler *user.UpdateProfileHandler
	if opts != nil && opts.PDSClientFactory != nil {
		// Use custom factory (for E2E tests with password auth)
		updateProfileHandler = user.NewUpdateProfileHandlerWithFactory(opts.PDSClientFactory)
	} else {
		// Use OAuth client for DPoP-authenticated PDS requests (production)
		updateProfileHandler = user.NewUpdateProfileHandler(oauthClient)
	}
	r.With(authMiddleware.RequireAuth).Post("/xrpc/social.coves.actor.updateProfile", updateProfileHandler.ServeHTTP)
}

// GetProfile handles social.coves.actor.getprofile
// Query endpoint that retrieves a user profile by DID or handle
// Returns profileViewDetailed with stats per lexicon specification
func (h *UserHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get actor parameter (DID or handle)
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		writeXRPCError(w, "InvalidRequest", "actor parameter is required", http.StatusBadRequest)
		return
	}

	// Resolve actor to DID
	var did string
	if strings.HasPrefix(actor, "did:") {
		did = actor
	} else {
		// Resolve handle to DID
		resolvedDID, err := h.userService.ResolveHandleToDID(ctx, actor)
		if err != nil {
			writeXRPCError(w, "ProfileNotFound", "user not found", http.StatusNotFound)
			return
		}
		did = resolvedDID
	}

	// Get full profile with stats
	profile, err := h.userService.GetProfile(ctx, did)
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			writeXRPCError(w, "ProfileNotFound", "user not found", http.StatusNotFound)
			return
		}
		log.Printf("Failed to get profile for %s: %v", did, err)
		writeXRPCError(w, "InternalError", "failed to get profile", http.StatusInternalServerError)
		return
	}

	// Marshal to bytes first to avoid partial writes on encoding errors
	responseBytes, err := json.Marshal(profile)
	if err != nil {
		log.Printf("Failed to marshal profile response: %v", err)
		writeXRPCError(w, "InternalError", "failed to encode response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(responseBytes); err != nil {
		log.Printf("Failed to write response: %v", err)
	}
}

// writeXRPCError writes a standardized XRPC error response
func writeXRPCError(w http.ResponseWriter, errorName, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   errorName,
		"message": message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}

// Signup handles social.coves.actor.signup
// Procedure endpoint that registers a new account on the Coves instance
func (h *UserHandler) Signup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse request body
	var req users.RegisterAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Call service to register account
	resp, err := h.userService.RegisterAccount(ctx, req)
	if err != nil {
		// Map service errors to lexicon error types with proper HTTP status codes
		respondWithLexiconError(w, err)
		return
	}

	// Return response matching lexicon output schema
	response := map[string]interface{}{
		"did":        resp.DID,
		"handle":     resp.Handle,
		"accessJwt":  resp.AccessJwt,
		"refreshJwt": resp.RefreshJwt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

// respondWithLexiconError maps domain errors to lexicon error types and HTTP status codes
// Error names match the lexicon definition in social.coves.actor.signup
func respondWithLexiconError(w http.ResponseWriter, err error) {
	var (
		statusCode int
		errorName  string
		message    string
	)

	// Map domain errors to lexicon error types
	var invalidHandleErr *users.InvalidHandleError
	var handleNotAvailableErr *users.HandleNotAvailableError
	var invalidInviteCodeErr *users.InvalidInviteCodeError
	var invalidEmailErr *users.InvalidEmailError
	var weakPasswordErr *users.WeakPasswordError
	var pdsErr *users.PDSError

	switch {
	case errors.As(err, &invalidHandleErr):
		statusCode = http.StatusBadRequest
		errorName = "InvalidHandle"
		message = invalidHandleErr.Error()

	case errors.As(err, &handleNotAvailableErr):
		statusCode = http.StatusBadRequest
		errorName = "HandleNotAvailable"
		message = handleNotAvailableErr.Error()

	case errors.As(err, &invalidInviteCodeErr):
		statusCode = http.StatusBadRequest
		errorName = "InvalidInviteCode"
		message = invalidInviteCodeErr.Error()

	case errors.As(err, &invalidEmailErr):
		statusCode = http.StatusBadRequest
		errorName = "InvalidEmail"
		message = invalidEmailErr.Error()

	case errors.As(err, &weakPasswordErr):
		statusCode = http.StatusBadRequest
		errorName = "WeakPassword"
		message = weakPasswordErr.Error()

	case errors.As(err, &pdsErr):
		// PDS errors get mapped based on status code
		statusCode = pdsErr.StatusCode
		errorName = "PDSError"
		message = pdsErr.Message

	default:
		// Generic error handling (avoid leaking internal details)
		statusCode = http.StatusInternalServerError
		errorName = "InternalServerError"
		message = "An error occurred while processing your request"
	}

	// XRPC error response format
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   errorName,
		"message": message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}
