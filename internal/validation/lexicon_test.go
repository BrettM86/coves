package validation

import (
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

	// Valid profile
	validProfile := map[string]interface{}{
		"$type":       "social.coves.actor.profile",
		"handle":      "test.example.com",
		"displayName": "Test User",
		"createdAt":   "2024-01-01T00:00:00Z",
	}

	if err := validator.ValidateActorProfile(validProfile); err != nil {
		t.Errorf("Valid profile failed validation: %v", err)
	}

	// Invalid profile - missing required field
	invalidProfile := map[string]interface{}{
		"$type":       "social.coves.actor.profile",
		"displayName": "Test User",
	}

	if err := validator.ValidateActorProfile(invalidProfile); err == nil {
		t.Error("Invalid profile passed validation when it should have failed")
	}
}

func TestValidatePost(t *testing.T) {
	validator, err := NewLexiconValidator("../../internal/atproto/lexicon", false)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	// Valid post
	validPost := map[string]interface{}{
		"$type":     "social.coves.post.record",
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
		"$type":     "social.coves.post.record",
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
		"$type": "social.coves.interaction.vote",
		"subject": "at://did:plc:test/social.coves.post.text/abc123",
		"createdAt": "2024-01-01T00:00:00Z"
	}`

	if err := validator.ValidateRecord(jsonString, "social.coves.interaction.vote"); err != nil {
		t.Errorf("Failed to validate JSON string: %v", err)
	}

	// Test with JSON bytes
	jsonBytes := []byte(jsonString)
	if err := validator.ValidateRecord(jsonBytes, "social.coves.interaction.vote"); err != nil {
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
