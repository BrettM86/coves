package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
)

func TestLexiconSchemaValidation(t *testing.T) {
	// Create a new catalog
	catalog := lexicon.NewBaseCatalog()

	// Load all schemas from the lexicon directory
	schemaPath := "../internal/atproto/lexicon"
	if err := catalog.LoadDirectory(schemaPath); err != nil {
		t.Fatalf("Failed to load lexicon schemas: %v", err)
	}

	// Walk through the directory and find all lexicon files
	var lexiconFiles []string
	err := filepath.Walk(schemaPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".json") && !info.IsDir() {
			lexiconFiles = append(lexiconFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk directory: %v", err)
	}

	t.Logf("Found %d lexicon files to validate", len(lexiconFiles))

	// Extract schema IDs from file paths and test resolution
	for _, filePath := range lexiconFiles {
		// Convert file path to schema ID
		// e.g., ../internal/atproto/lexicon/social/coves/actor/profile.json -> social.coves.actor.profile
		relPath, err := filepath.Rel(schemaPath, filePath)
		if err != nil {
			t.Fatalf("Failed to get relative path for %s: %v", filePath, err)
		}
		relPath = strings.TrimSuffix(relPath, ".json")
		schemaID := strings.ReplaceAll(relPath, string(filepath.Separator), ".")

		t.Run(schemaID, func(t *testing.T) {
			if _, resolveErr := catalog.Resolve(schemaID); resolveErr != nil {
				t.Errorf("Failed to resolve schema %s: %v", schemaID, resolveErr)
			}
		})
	}
}

func TestLexiconCrossReferences(t *testing.T) {
	// Create a new catalog
	catalog := lexicon.NewBaseCatalog()

	// Load all schemas
	if err := catalog.LoadDirectory("../internal/atproto/lexicon"); err != nil {
		t.Fatalf("Failed to load lexicon schemas: %v", err)
	}

	// Test specific cross-references that should work
	crossRefs := map[string]string{
		"social.coves.richtext.facet#byteSlice":  "byteSlice definition in facet schema",
		"social.coves.actor.profile#geoLocation": "geoLocation definition in actor profile",
		"social.coves.community.rules#rule":      "rule definition in community rules",
	}

	for ref, description := range crossRefs {
		t.Run(ref, func(t *testing.T) {
			if _, err := catalog.Resolve(ref); err != nil {
				t.Errorf("Failed to resolve cross-reference %s (%s): %v", ref, description, err)
			}
		})
	}
}

func TestValidateRecord(t *testing.T) {
	// Create a new catalog
	catalog := lexicon.NewBaseCatalog()

	// Load all schemas
	if err := catalog.LoadDirectory("../internal/atproto/lexicon"); err != nil {
		t.Fatalf("Failed to load lexicon schemas: %v", err)
	}

	// Test cases for ValidateRecord
	tests := []struct {
		recordData    map[string]interface{}
		name          string
		recordType    string
		errorContains string
		shouldFail    bool
	}{
		{
			name:       "Valid actor profile",
			recordType: "social.coves.actor.profile",
			recordData: map[string]interface{}{
				"$type":       "social.coves.actor.profile",
				"handle":      "alice.example.com",
				"displayName": "Alice Johnson",
				"createdAt":   "2024-01-15T10:30:00Z",
			},
			shouldFail: false,
		},
		{
			name:       "Invalid actor profile - missing required field",
			recordType: "social.coves.actor.profile",
			recordData: map[string]interface{}{
				"$type":       "social.coves.actor.profile",
				"displayName": "Alice Johnson",
			},
			shouldFail:    true,
			errorContains: "required field missing: handle",
		},
		{
			name:       "Valid community profile",
			recordType: "social.coves.community.profile",
			recordData: map[string]interface{}{
				"$type":          "social.coves.community.profile",
				"handle":         "programming.community.coves.social",
				"name":           "programming",
				"displayName":    "Programming Community",
				"createdBy":      "did:plc:creator123",
				"hostedBy":       "did:plc:coves123",
				"visibility":     "public",
				"moderationType": "moderator",
				"federatedFrom":  "coves",
				"createdAt":      "2023-12-01T08:00:00Z",
			},
			shouldFail: false,
		},
		{
			name:       "Valid post record",
			recordType: "social.coves.post.record",
			recordData: map[string]interface{}{
				"$type":     "social.coves.post.record",
				"community": "did:plc:programming123",
				"author":    "did:plc:testauthor123",
				"title":     "Test Post",
				"content":   "This is a test post",
				"createdAt": "2025-01-09T14:30:00Z",
			},
			shouldFail: false,
		},
		{
			name:       "Invalid post record - missing required field",
			recordType: "social.coves.post.record",
			recordData: map[string]interface{}{
				"$type":     "social.coves.post.record",
				"community": "did:plc:programming123",
				// Missing required "author" field
				"title":     "Test Post",
				"content":   "This is a test post",
				"createdAt": "2025-01-09T14:30:00Z",
			},
			shouldFail:    true,
			errorContains: "required field missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := lexicon.ValidateRecord(&catalog, tt.recordData, tt.recordType, lexicon.AllowLenientDatetime)

			if tt.shouldFail {
				if err == nil {
					t.Errorf("Expected validation to fail but it passed")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("Expected error to contain '%s', got: %v", tt.errorContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("Expected validation to pass but got error: %v", err)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && strings.Contains(s, substr))
}

func TestValidateRecordWithStrictMode(t *testing.T) {
	// Create a new catalog
	catalog := lexicon.NewBaseCatalog()

	// Load all schemas
	if err := catalog.LoadDirectory("../internal/atproto/lexicon"); err != nil {
		t.Fatalf("Failed to load lexicon schemas: %v", err)
	}

	// Test with strict validation flags
	recordData := map[string]interface{}{
		"$type":       "social.coves.actor.profile",
		"handle":      "alice.example.com",
		"displayName": "Alice Johnson",
		"createdAt":   "2024-01-15T10:30:00", // Missing timezone
	}

	// Should fail with strict validation
	err := lexicon.ValidateRecord(&catalog, recordData, "social.coves.actor.profile", lexicon.StrictRecursiveValidation)
	if err == nil {
		t.Error("Expected strict validation to fail on datetime without timezone")
	}

	// Should pass with lenient datetime validation
	err = lexicon.ValidateRecord(&catalog, recordData, "social.coves.actor.profile", lexicon.AllowLenientDatetime)
	if err != nil {
		t.Errorf("Expected lenient validation to pass, got error: %v", err)
	}
}
