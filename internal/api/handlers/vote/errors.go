package vote

import (
	"Coves/internal/core/votes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// XRPCError represents an XRPC error response
type XRPCError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes an XRPC error response
func writeError(w http.ResponseWriter, status int, error, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(XRPCError{
		Error:   error,
		Message: message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}

// handleServiceError converts service errors to appropriate HTTP responses
// Error names MUST match lexicon definitions exactly (UpperCamelCase)
// Uses errors.Is() to handle wrapped errors correctly
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, votes.ErrVoteNotFound):
		// Matches: social.coves.feed.vote.delete#VoteNotFound
		writeError(w, http.StatusNotFound, "VoteNotFound", "No vote found for this subject")
	case errors.Is(err, votes.ErrInvalidDirection):
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Vote direction must be 'up' or 'down'")
	case errors.Is(err, votes.ErrInvalidSubject):
		// Matches: social.coves.feed.vote.create#InvalidSubject
		writeError(w, http.StatusBadRequest, "InvalidSubject", "The subject reference is invalid or malformed")
	case errors.Is(err, votes.ErrVoteAlreadyExists):
		writeError(w, http.StatusConflict, "AlreadyExists", "Vote already exists")
	case errors.Is(err, votes.ErrNotAuthorized):
		// Matches: social.coves.feed.vote.create#NotAuthorized, social.coves.feed.vote.delete#NotAuthorized
		writeError(w, http.StatusForbidden, "NotAuthorized", "User is not authorized to vote on this content")
	case errors.Is(err, votes.ErrBanned):
		writeError(w, http.StatusForbidden, "NotAuthorized", "User is not authorized to vote on this content")
	default:
		// Internal server error - log the actual error for debugging
		log.Printf("XRPC handler error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
