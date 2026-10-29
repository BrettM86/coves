package routes

import (
	"github.com/go-chi/chi/v5"

	defaultavatarhandlers "Coves/internal/api/handlers/defaultavatars"
)

// RegisterDefaultAvatarRoutes registers GET and HEAD
// /img/default-avatars/{version}/{file}.
func RegisterDefaultAvatarRoutes(r chi.Router, handler *defaultavatarhandlers.Handler) {
	r.Get("/img/default-avatars/{version}/{file}", handler.HandleImage)
	r.Head("/img/default-avatars/{version}/{file}", handler.HandleImage)
}
