package xrpc

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error represents an XRPC error response
type Error struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// WriteError writes an XRPC error response with the given status code.
//
// The body is marshalled before any header is sent, so an encoding failure
// cannot leave the client with a committed status and a truncated body. That
// should be unreachable for two plain strings, but the fallback keeps the
// response well-formed if it ever happens.
func WriteError(w http.ResponseWriter, statusCode int, errorType, message string) {
	// A malformed mapping is a bug in the rule table, but net/http panics on an
	// out-of-range status and an empty code would leave the client with no
	// contract to switch on. Answer a plain 500 and record the bug instead of
	// taking down the request goroutine.
	if statusCode < 100 || statusCode > 599 || errorType == "" {
		slog.Error("invalid XRPC error mapping; substituting 500",
			"status", statusCode, "errorType", errorType)
		statusCode, errorType, message = http.StatusInternalServerError,
			"InternalServerError", "An internal error occurred"
	}

	body, err := json.Marshal(Error{Error: errorType, Message: message})
	if err != nil {
		slog.Error("failed to marshal XRPC error response",
			"error", err, "errorType", errorType)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(statusCode)
		if _, err := w.Write([]byte(message)); err != nil {
			slog.Warn("failed to write XRPC error fallback response", "error", err)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if _, err := w.Write(body); err != nil {
		// Status and body are already committed; the client most likely
		// disconnected. Nothing to do but record it.
		slog.Warn("failed to write XRPC error response", "error", err)
	}
}
