package comments

import (
	"Coves/internal/core/comments"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// errorResponse represents a standardized JSON error response
type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeError writes a JSON error response with the given status code
func writeError(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(errorResponse{
		Error:   errorType,
		Message: message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}

// handleServiceError maps service-layer errors to HTTP responses
// This follows the error handling pattern from other handlers (post, community)
func handleServiceError(w http.ResponseWriter, err error) {
	switch {
	case comments.IsNotFound(err):
		// Map specific not found errors to appropriate messages
		switch {
		case errors.Is(err, comments.ErrCommentNotFound):
			writeError(w, http.StatusNotFound, "CommentNotFound", "Comment not found")
		case errors.Is(err, comments.ErrParentNotFound):
			writeError(w, http.StatusNotFound, "ParentNotFound", "Parent post or comment not found")
		case errors.Is(err, comments.ErrRootNotFound):
			writeError(w, http.StatusNotFound, "RootNotFound", "Root post not found")
		default:
			writeError(w, http.StatusNotFound, "NotFound", err.Error())
		}

	case comments.IsValidationError(err):
		// Map specific validation errors to appropriate messages
		switch {
		case errors.Is(err, comments.ErrInvalidReply):
			writeError(w, http.StatusBadRequest, "InvalidReply", "The reply reference is invalid or malformed")
		case errors.Is(err, comments.ErrContentTooLong):
			writeError(w, http.StatusBadRequest, "ContentTooLong", "Comment content exceeds 10000 graphemes")
		case errors.Is(err, comments.ErrContentEmpty):
			writeError(w, http.StatusBadRequest, "ContentEmpty", "Comment content is required")
		default:
			writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		}

	case errors.Is(err, comments.ErrNotAuthorized):
		writeError(w, http.StatusForbidden, "NotAuthorized", "User is not authorized to perform this action")

	case errors.Is(err, comments.ErrBanned):
		writeError(w, http.StatusForbidden, "Banned", "User is banned from this community")

	// NOTE: IsConflict case removed - the PDS handles duplicate detection via CreateRecord,
	// so ErrCommentAlreadyExists is never returned from the service layer. If the PDS rejects
	// a duplicate record, it returns an auth/validation error which is handled by other cases.
	// Keeping this code would be dead code that never executes.

	default:
		// Don't leak internal error details to clients
		log.Printf("Unexpected error in comments handler: %v", err)
		writeError(w, http.StatusInternalServerError, "InternalServerError",
			"An internal error occurred")
	}
}
