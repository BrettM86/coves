package xrpc

import (
	"encoding/json"
	"log"
	"net/http"
)

// Error represents an XRPC error response
type Error struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// WriteError writes an XRPC error response with the given status code
func WriteError(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(Error{
		Error:   errorType,
		Message: message,
	}); err != nil {
		log.Printf("Failed to encode XRPC error response: %v", err)
	}
}
