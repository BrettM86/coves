package utils

import (
	"database/sql"
	"strings"
	"time"
)

// ExtractRKeyFromURI extracts the record key from an AT-URI
// Format: at://did/collection/rkey -> rkey
func ExtractRKeyFromURI(uri string) string {
	parts := strings.Split(uri, "/")
	if len(parts) >= 4 {
		return parts[len(parts)-1]
	}
	return ""
}

// StringFromNull converts sql.NullString to string
// Returns empty string if the NullString is not valid
func StringFromNull(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}

// ParseCreatedAt extracts and parses the createdAt timestamp from an atProto record
// Falls back to time.Now() if the field is missing or invalid
// This preserves chronological ordering during Jetstream replays and backfills
func ParseCreatedAt(record map[string]interface{}) time.Time {
	if record == nil {
		return time.Now()
	}

	createdAtStr, ok := record["createdAt"].(string)
	if !ok || createdAtStr == "" {
		return time.Now()
	}

	// atProto uses RFC3339 format for datetime fields
	createdAt, err := time.Parse(time.RFC3339, createdAtStr)
	if err != nil {
		// Fallback to now if parsing fails
		return time.Now()
	}

	return createdAt
}
