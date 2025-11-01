package handlers

import (
	"encoding/json"
	"log"
	"net/http"
)

// WriteError writes a standardized JSON error response
func WriteError(w http.ResponseWriter, statusCode int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   errorType,
		"message": message,
	}); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}
