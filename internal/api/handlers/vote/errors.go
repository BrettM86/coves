package vote

import (
	"Coves/internal/core/votes"
	"encoding/json"
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
func handleServiceError(w http.ResponseWriter, err error) {
	switch err {
	case votes.ErrVoteNotFound:
		// Matches: social.coves.feed.vote.delete#VoteNotFound
		writeError(w, http.StatusNotFound, "VoteNotFound", "No vote found for this subject")
	case votes.ErrSubjectNotFound:
		// Matches: social.coves.feed.vote.create#SubjectNotFound
		writeError(w, http.StatusNotFound, "SubjectNotFound", "The subject post or comment was not found")
	case votes.ErrInvalidDirection:
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Vote direction must be 'up' or 'down'")
	case votes.ErrInvalidSubject:
		// Matches: social.coves.feed.vote.create#InvalidSubject
		writeError(w, http.StatusBadRequest, "InvalidSubject", "The subject reference is invalid or malformed")
	case votes.ErrVoteAlreadyExists:
		writeError(w, http.StatusConflict, "AlreadyExists", "Vote already exists")
	case votes.ErrNotAuthorized:
		// Matches: social.coves.feed.vote.create#NotAuthorized, social.coves.feed.vote.delete#NotAuthorized
		writeError(w, http.StatusForbidden, "NotAuthorized", "User is not authorized to vote on this content")
	case votes.ErrBanned:
		writeError(w, http.StatusForbidden, "NotAuthorized", "User is not authorized to vote on this content")
	default:
		// Internal server error - log the actual error for debugging
		log.Printf("XRPC handler error: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
	}
}
