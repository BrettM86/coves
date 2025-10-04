package routes

import (
	"encoding/json"
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

// UserRoutes returns user-related XRPC routes
// Implements social.coves.actor.* lexicon endpoints
func UserRoutes(service users.UserService) chi.Router {
	h := NewUserHandler(service)
	r := chi.NewRouter()

	// social.coves.actor.getProfile - query endpoint
	r.Get("/profile", h.GetProfile)

	return r
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
	json.NewEncoder(w).Encode(response)
}