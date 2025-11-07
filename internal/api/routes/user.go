package routes

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"Coves/internal/core/users"

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

// RegisterUserRoutes registers user-related XRPC endpoints on the router
// Implements social.coves.actor.* lexicon endpoints
func RegisterUserRoutes(r chi.Router, service users.UserService) {
	h := NewUserHandler(service)

	// social.coves.actor.getProfile - query endpoint
	r.Get("/xrpc/social.coves.actor.getProfile", h.GetProfile)

	// social.coves.actor.signup - procedure endpoint
	r.Post("/xrpc/social.coves.actor.signup", h.Signup)
}

// GetProfile handles social.coves.actor.getProfile
// Query endpoint that retrieves a user profile by DID or handle
func (h *UserHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get actor parameter (DID or handle)
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		http.Error(w, "actor parameter is required", http.StatusBadRequest)
		return
	}

	var user *users.User
	var err error

	// Determine if actor is a DID or handle
	// DIDs start with "did:", handles don't
	if len(actor) > 4 && actor[:4] == "did:" {
		user, err = h.userService.GetUserByDID(ctx, actor)
	} else {
		user, err = h.userService.GetUserByHandle(ctx, actor)
	}

	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// Minimal profile response (matching lexicon structure)
	response := map[string]interface{}{
		"did": user.DID,
		"profile": map[string]interface{}{
			"handle":    user.Handle,
			"createdAt": user.CreatedAt.Format(time.RFC3339),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
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
