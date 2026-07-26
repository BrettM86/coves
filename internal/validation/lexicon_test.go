package validation

import (
	"strings"
	"testing"
)

func TestNewLexiconValidator(t *testing.T) {
	// Test creating validator with valid schema path
	validator, err := NewLexiconValidator("../../internal/atproto/lexicon", false)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}
	if validator == nil {
		t.Fatal("Expected validator to be non-nil")
	}

	// Test creating validator with invalid schema path
	_, err = NewLexiconValidator("/nonexistent/path", false)
	if err == nil {
		t.Error("Expected error when creating validator with invalid path")
	}
}

func TestValidateActorProfile(t *testing.T) {
	validator, err := NewLexiconValidator("../../internal/atproto/lexicon", false)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	// Valid profile. Note social.coves.actor.profile has no required fields:
	// every property is optional, matching app.bsky.actor.profile. A handle is
	// NOT part of the record - it lives in the DID document - so this record
	// carries only profile presentation fields.
	validProfile := map[string]interface{}{
		"$type":       "social.coves.actor.profile",
		"displayName": "Test User",
		"description": "A test bio",
		"createdAt":   "2024-01-01T00:00:00Z",
	}

	if err := validator.ValidateActorProfile(validProfile); err != nil {
		t.Errorf("Valid profile failed validation: %v", err)
	}

	// A profile with no fields at all is valid, precisely because the schema
	// requires nothing. Asserting this pins the "required: []" decision so a
	// future schema change that reintroduces a required field fails loudly here.
	minimalProfile := map[string]interface{}{
		"$type": "social.coves.actor.profile",
	}

	if err := validator.ValidateActorProfile(minimalProfile); err != nil {
		t.Errorf("Minimal profile failed validation: %v", err)
	}

	// Invalid profiles - since no field is required, the enforceable failures
	// are constraint violations on the fields that ARE present.
	invalidProfiles := map[string]map[string]interface{}{
		"wrong $type": {
			"$type":       "social.coves.community.post",
			"displayName": "Test User",
		},
		"displayName over maxLength": {
			"$type":       "social.coves.actor.profile",
			"displayName": strings.Repeat("a", 641),
		},
		"displayName over maxGraphemes": {
			"$type":       "social.coves.actor.profile",
			"displayName": strings.Repeat("é", 65),
		},
		"description over maxLength": {
			"$type":       "social.coves.actor.profile",
			"description": strings.Repeat("a", 2561),
		},
		"createdAt not a datetime": {
			"$type":     "social.coves.actor.profile",
			"createdAt": "January 1st, 2024",
		},
		"displayName wrong JSON type": {
			"$type":       "social.coves.actor.profile",
			"displayName": 12345,
		},
	}

	for name, profile := range invalidProfiles {
		if err := validator.ValidateActorProfile(profile); err == nil {
			t.Errorf("Invalid profile (%s) passed validation when it should have failed", name)
		}
	}
}

func TestValidatePost(t *testing.T) {
	validator, err := NewLexiconValidator("../../internal/atproto/lexicon", false)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	// Valid post
	validPost := map[string]interface{}{
		"$type":     "social.coves.community.post",
		"community": "did:plc:test123",
		"author":    "did:plc:author123",
		"title":     "Test Post",
		"content":   "This is a test",
		"createdAt": "2024-01-01T00:00:00Z",
	}

	if err := validator.ValidatePost(validPost); err != nil {
		t.Errorf("Valid post failed validation: %v", err)
	}

	// Invalid post - missing required field (author)
	invalidPost := map[string]interface{}{
		"$type":     "social.coves.community.post",
		"community": "did:plc:test123",
		// Missing required "author" field
		"title":     "Test Post",
		"content":   "This is a test",
		"createdAt": "2024-01-01T00:00:00Z",
	}

	if err := validator.ValidatePost(invalidPost); err == nil {
		t.Error("Invalid post passed validation when it should have failed")
	}
}

func TestValidateRecordWithDifferentInputTypes(t *testing.T) {
	validator, err := NewLexiconValidator("../../internal/atproto/lexicon", false)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	// Test with JSON string
	jsonString := `{
		"$type": "social.coves.feed.vote",
		"subject": {
			"uri": "at://did:plc:test/social.coves.community.post/abc123",
			"cid": "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
		},
		"direction": "up",
		"createdAt": "2024-01-01T00:00:00Z"
	}`

	if err := validator.ValidateRecord(jsonString, "social.coves.feed.vote"); err != nil {
		t.Errorf("Failed to validate JSON string: %v", err)
	}

	// Test with JSON bytes
	jsonBytes := []byte(jsonString)
	if err := validator.ValidateRecord(jsonBytes, "social.coves.feed.vote"); err != nil {
		t.Errorf("Failed to validate JSON bytes: %v", err)
	}
}

func TestStrictValidation(t *testing.T) {
	// Create validator with strict mode
	validator, err := NewLexiconValidator("../../internal/atproto/lexicon", true)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	// Profile with datetime missing timezone (should fail in strict mode)
	profile := map[string]interface{}{
		"$type":     "social.coves.actor.profile",
		"handle":    "test.example.com",
		"createdAt": "2024-01-01T00:00:00", // Missing Z
	}

	if err := validator.ValidateActorProfile(profile); err == nil {
		t.Error("Expected strict validation to fail on datetime without timezone")
	}
}
