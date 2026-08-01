package tests

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
)

// lexiconDir holds every schema the AppView publishes, one JSON file per NSID.
const lexiconDir = "../internal/atproto/lexicon"

// TestMain controls test setup for the tests package.
// Set LOG_ENABLED=false to suppress application log output during tests.
func TestMain(m *testing.M) {
	// Silence logs when LOG_ENABLED=false (what .env.ci sets for the gate)
	if os.Getenv("LOG_ENABLED") == "false" {
		log.SetOutput(io.Discard)
	}

	os.Exit(m.Run())
}

// lexiconFile is one schema file on disk, already parsed far enough to know
// whether it declares a primary type. A lexicon's primary type lives under the
// "main" key of its defs map; a file without one carries only shared
// definitions that other schemas reference by fragment.
type lexiconFile struct {
	schemaID string
	path     string
	defNames []string
	hasMain  bool
}

// collectLexiconFiles reads every lexicon JSON under lexiconDir and derives its
// schema ID from the path, e.g.
// ../internal/atproto/lexicon/social/coves/actor/profile.json
// -> social.coves.actor.profile.
func collectLexiconFiles(t *testing.T) []lexiconFile {
	t.Helper()

	var files []lexiconFile
	err := filepath.Walk(lexiconDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}

		relPath, err := filepath.Rel(lexiconDir, path)
		if err != nil {
			return err
		}
		schemaID := strings.ReplaceAll(strings.TrimSuffix(relPath, ".json"), string(filepath.Separator), ".")

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var doc struct {
			Defs map[string]json.RawMessage `json:"defs"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s is not valid lexicon JSON: %v", schemaID, err)
			return nil
		}

		defNames := make([]string, 0, len(doc.Defs))
		for name := range doc.Defs {
			if name != "main" {
				defNames = append(defNames, name)
			}
		}
		sort.Strings(defNames)

		_, hasMain := doc.Defs["main"]
		files = append(files, lexiconFile{
			schemaID: schemaID,
			path:     path,
			defNames: defNames,
			hasMain:  hasMain,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to walk %s: %v", lexiconDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("No lexicon files found under %s", lexiconDir)
	}
	return files
}

func TestLexiconSchemaValidation(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	if err := catalog.LoadDirectory(lexiconDir); err != nil {
		t.Fatalf("Failed to load lexicon schemas: %v", err)
	}

	// Only files that declare a primary type are resolvable by NSID alone.
	// Definition-only files are covered by TestLexiconDefinitionOnlySchemas,
	// which resolves each of their definitions by fragment.
	var validated int
	for _, f := range collectLexiconFiles(t) {
		if !f.hasMain {
			continue
		}
		validated++
		t.Run(f.schemaID, func(t *testing.T) {
			if _, err := catalog.Resolve(f.schemaID); err != nil {
				t.Errorf("Failed to resolve schema %s: %v", f.schemaID, err)
			}
		})
	}

	if validated == 0 {
		t.Fatalf("No lexicon files under %s declare a primary type", lexiconDir)
	}
	t.Logf("Resolved %d lexicon schemas with a primary type", validated)
}

// TestLexiconDefinitionOnlySchemas covers the files TestLexiconSchemaValidation
// has nothing to resolve by NSID: those carrying only shared definitions. Each
// definition must still resolve by its fragment reference, and the *.defs
// naming must match what the file actually contains — otherwise a schema could
// lose its primary type and quietly drop out of the suite above.
func TestLexiconDefinitionOnlySchemas(t *testing.T) {
	catalog := lexicon.NewBaseCatalog()
	if err := catalog.LoadDirectory(lexiconDir); err != nil {
		t.Fatalf("Failed to load lexicon schemas: %v", err)
	}

	var definitionOnly, resolved int
	for _, f := range collectLexiconFiles(t) {
		namedAsDefs := strings.HasSuffix(f.schemaID, ".defs")
		switch {
		case f.hasMain && namedAsDefs:
			t.Errorf("%s is named as a definition-only lexicon but declares a primary type (%s)", f.schemaID, f.path)
		case !f.hasMain && !namedAsDefs:
			t.Errorf("%s declares no primary type; a definition-only lexicon must be named *.defs (%s)", f.schemaID, f.path)
		}
		if f.hasMain {
			continue
		}

		definitionOnly++
		if len(f.defNames) == 0 {
			t.Errorf("%s has neither a primary type nor any definitions", f.schemaID)
			continue
		}
		for _, name := range f.defNames {
			ref := f.schemaID + "#" + name
			if _, err := catalog.Resolve(ref); err != nil {
				t.Errorf("Failed to resolve definition %s: %v", ref, err)
				continue
			}
			resolved++
		}
	}

	if definitionOnly == 0 {
		t.Fatalf("No definition-only lexicons found under %s", lexiconDir)
	}
	t.Logf("Resolved %d definitions across %d definition-only lexicons", resolved, definitionOnly)
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
		"social.coves.richtext.facet#byteSlice":      "byteSlice definition in facet schema",
		"social.coves.community.rules#rule":          "rule definition in community rules",
		"social.coves.actor.defs#profileView":        "profileView definition in actor defs",
		"social.coves.actor.defs#profileStats":       "profileStats definition in actor defs",
		"social.coves.actor.defs#viewerState":        "viewerState definition in actor defs",
		"social.coves.community.defs#communityView":  "communityView definition in community defs",
		"social.coves.community.defs#communityStats": "communityStats definition in community defs",
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
				"displayName": "Alice Johnson",
				"createdAt":   "2024-01-15T10:30:00Z",
			},
			shouldFail: false,
		},
		{
			name:       "Invalid actor profile - wrong field type",
			recordType: "social.coves.actor.profile",
			recordData: map[string]interface{}{
				"$type":       "social.coves.actor.profile",
				"displayName": int64(12345),
				"createdAt":   "2024-01-15T10:30:00Z",
			},
			shouldFail: true,
		},
		{
			name:       "Valid community profile",
			recordType: "social.coves.community.profile",
			recordData: map[string]interface{}{
				"$type":          "social.coves.community.profile",
				"name":           "programming",
				"displayName":    "Programming Community",
				"createdBy":      "did:plc:creator123",
				"hostedBy":       "did:plc:coves123",
				"visibility":     "public",
				"moderationType": "moderator",
				"createdAt":      "2023-12-01T08:00:00Z",
			},
			shouldFail: false,
		},
		{
			name:       "Valid post record",
			recordType: "social.coves.community.post",
			recordData: map[string]interface{}{
				"$type":     "social.coves.community.post",
				"community": "did:plc:programming123",
				"author":    "did:plc:testauthor123",
				"title":     "Test Post",
				"content":   "This is a test post",
				"createdAt": "2025-01-09T14:30:00Z",
			},
			shouldFail: false,
		},
		{
			name:       "Valid post record with block-level richtext facets",
			recordType: "social.coves.community.post",
			recordData: map[string]interface{}{
				"$type":     "social.coves.community.post",
				"community": "did:plc:programming123",
				"author":    "did:plc:testauthor123",
				"title":     "Cross-posted from Lemmy",
				"content":   "The Button\nThey said\nDo not press\nUse:\nfmt.Println(\"hi\")",
				"facets": []interface{}{
					map[string]interface{}{
						"index": map[string]interface{}{"byteStart": int64(0), "byteEnd": int64(10)},
						"features": []interface{}{
							map[string]interface{}{"$type": "social.coves.richtext.facet#heading", "level": int64(2)},
						},
					},
					map[string]interface{}{
						"index": map[string]interface{}{"byteStart": int64(11), "byteEnd": int64(20)},
						"features": []interface{}{
							map[string]interface{}{"$type": "social.coves.richtext.facet#blockquote", "level": int64(1)},
						},
					},
					map[string]interface{}{
						"index": map[string]interface{}{"byteStart": int64(21), "byteEnd": int64(33)},
						"features": []interface{}{
							map[string]interface{}{"$type": "social.coves.richtext.facet#blockquote", "level": int64(2)},
						},
					},
					map[string]interface{}{
						"index": map[string]interface{}{"byteStart": int64(39), "byteEnd": int64(56)},
						"features": []interface{}{
							map[string]interface{}{"$type": "social.coves.richtext.facet#codeBlock", "language": "go"},
						},
					},
					map[string]interface{}{
						// inline code on "hi" (bytes 52-54 of the content above)
						"index": map[string]interface{}{"byteStart": int64(52), "byteEnd": int64(54)},
						"features": []interface{}{
							map[string]interface{}{"$type": "social.coves.richtext.facet#code"},
						},
					},
				},
				"createdAt": "2026-07-22T10:00:00Z",
			},
			shouldFail: false,
		},
		{
			name:       "Invalid post record - heading facet missing required level",
			recordType: "social.coves.community.post",
			recordData: map[string]interface{}{
				"$type":     "social.coves.community.post",
				"community": "did:plc:programming123",
				"author":    "did:plc:testauthor123",
				"content":   "Some heading text",
				"facets": []interface{}{
					map[string]interface{}{
						"index": map[string]interface{}{"byteStart": int64(0), "byteEnd": int64(12)},
						"features": []interface{}{
							map[string]interface{}{"$type": "social.coves.richtext.facet#heading"},
						},
					},
				},
				"createdAt": "2026-07-22T10:00:00Z",
			},
			shouldFail:    true,
			errorContains: "required field missing",
		},
		{
			name:       "Invalid post record - heading facet level out of range",
			recordType: "social.coves.community.post",
			recordData: map[string]interface{}{
				"$type":     "social.coves.community.post",
				"community": "did:plc:programming123",
				"author":    "did:plc:testauthor123",
				"content":   "Some heading text",
				"facets": []interface{}{
					map[string]interface{}{
						"index": map[string]interface{}{"byteStart": int64(0), "byteEnd": int64(12)},
						"features": []interface{}{
							map[string]interface{}{"$type": "social.coves.richtext.facet#heading", "level": int64(9)},
						},
					},
				},
				"createdAt": "2026-07-22T10:00:00Z",
			},
			shouldFail: true,
		},
		{
			name:       "Invalid post record - missing required field",
			recordType: "social.coves.community.post",
			recordData: map[string]interface{}{
				"$type":     "social.coves.community.post",
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
