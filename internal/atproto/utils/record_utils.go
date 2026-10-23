package utils

import (
	"database/sql"
	"log"
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

// ExtractCollectionFromURI extracts the collection from an AT-URI
// Format: at://did/collection/rkey -> collection
//
// Returns:
//   - Collection name (e.g., "social.coves.community.comment") if URI is well-formed
//   - Empty string if URI is malformed or doesn't contain a collection segment
//
// Note: Empty string indicates "unknown/unsupported collection" and should be
// handled as an invalid or unparseable URI by the caller. Callers should validate
// the return value before using it for database queries or business logic.
func ExtractCollectionFromURI(uri string) string {
	withoutScheme := strings.TrimPrefix(uri, "at://")
	parts := strings.Split(withoutScheme, "/")
	if len(parts) >= 2 {
		return parts[1]
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
// A present createdAt goes through ParseRecordCreatedAt, so future timestamps
// are clamped to now. uri names the record in any warning.
func ParseCreatedAt(record map[string]interface{}, uri string) time.Time {
	if record == nil {
		return time.Now()
	}

	createdAtStr, ok := record["createdAt"].(string)
	if !ok || createdAtStr == "" {
		return time.Now()
	}

	return ParseRecordCreatedAt(createdAtStr, uri)
}

// ParseRecordCreatedAt reads an indexed record's author-supplied createdAt,
// falling back to now when it does not parse.
//
// SECURITY: future timestamps are clamped to now. created_at-class fields drive
// orderings such as the "new" sort, the hot-rank age, and subscribed_at/blocked_at
// DESC lists, so a record asserting a future date (hostile or clock-skewed
// federated repo) could otherwise pin itself to the top of those orderings
// until wall-clock catches up.
func ParseRecordCreatedAt(raw, uri string) time.Time {
	createdAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		log.Printf("Warning: Failed to parse createdAt timestamp for %s, using current time: %v", uri, err)
		return time.Now()
	}
	if now := time.Now(); createdAt.After(now) {
		log.Printf("Warning: record %s has future createdAt %s, clamping to now", uri, raw)
		return now
	}
	return createdAt
}
