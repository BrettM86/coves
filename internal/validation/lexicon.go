package validation

import (
	"encoding/json"
	"fmt"

	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
)

// LexiconValidator provides a convenient interface for validating atproto records
type LexiconValidator struct {
	catalog *lexicon.BaseCatalog
	flags   lexicon.ValidateFlags
}

// NewLexiconValidator creates a new validator with the specified schema directory
func NewLexiconValidator(schemaPath string, strict bool) (*LexiconValidator, error) {
	catalog := lexicon.NewBaseCatalog()

	if err := catalog.LoadDirectory(schemaPath); err != nil {
		return nil, fmt.Errorf("failed to load lexicon schemas: %w", err)
	}

	flags := lexicon.ValidateFlags(0)
	if strict {
		flags |= lexicon.StrictRecursiveValidation
	} else {
		flags |= lexicon.AllowLenientDatetime
	}

	return &LexiconValidator{
		catalog: &catalog,
		flags:   flags,
	}, nil
}

// ValidateRecord validates a record against its schema
func (v *LexiconValidator) ValidateRecord(recordData interface{}, recordType string) error {
	// Convert to map if needed
	var data map[string]interface{}

	switch rd := recordData.(type) {
	case map[string]interface{}:
		data = rd
	case []byte:
		if err := json.Unmarshal(rd, &data); err != nil {
			return fmt.Errorf("failed to parse JSON: %w", err)
		}
	case string:
		if err := json.Unmarshal([]byte(rd), &data); err != nil {
			return fmt.Errorf("failed to parse JSON: %w", err)
		}
	default:
		// Try to marshal and unmarshal to convert struct to map
		jsonBytes, err := json.Marshal(recordData)
		if err != nil {
			return fmt.Errorf("failed to convert record to JSON: %w", err)
		}
		if err := json.Unmarshal(jsonBytes, &data); err != nil {
			return fmt.Errorf("failed to parse JSON: %w", err)
		}
	}

	// Ensure $type field matches recordType
	if typeField, ok := data["$type"].(string); ok && typeField != recordType {
		return fmt.Errorf("$type field '%s' does not match expected type '%s'", typeField, recordType)
	}

	return lexicon.ValidateRecord(v.catalog, data, recordType, v.flags)
}

// ValidateActorProfile validates an actor profile record
func (v *LexiconValidator) ValidateActorProfile(profile map[string]interface{}) error {
	return v.ValidateRecord(profile, "social.coves.actor.profile")
}

// ValidateCommunityProfile validates a community profile record
func (v *LexiconValidator) ValidateCommunityProfile(profile map[string]interface{}) error {
	return v.ValidateRecord(profile, "social.coves.community.profile")
}

// ValidatePost validates a post record
func (v *LexiconValidator) ValidatePost(post map[string]interface{}) error {
	return v.ValidateRecord(post, "social.coves.community.post")
}

// ValidateComment validates a comment record
func (v *LexiconValidator) ValidateComment(comment map[string]interface{}) error {
	return v.ValidateRecord(comment, "social.coves.community.comment")
}

// ValidateVote validates a vote record
func (v *LexiconValidator) ValidateVote(vote map[string]interface{}) error {
	return v.ValidateRecord(vote, "social.coves.feed.vote")
}

// ValidateModerationAction validates a moderation action (ban, tribunalVote, etc.)
func (v *LexiconValidator) ValidateModerationAction(action map[string]interface{}, actionType string) error {
	return v.ValidateRecord(action, fmt.Sprintf("social.coves.moderation.%s", actionType))
}

// ResolveReference resolves a schema reference (e.g., "social.coves.community.post.get#postView")
func (v *LexiconValidator) ResolveReference(ref string) (interface{}, error) {
	return v.catalog.Resolve(ref)
}

// GetCatalog returns the underlying lexicon catalog for advanced usage
func (v *LexiconValidator) GetCatalog() *lexicon.BaseCatalog {
	return v.catalog
}
