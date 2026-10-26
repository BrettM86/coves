package validation

import (
	"encoding/json"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"

	lexiconschemas "Coves/internal/atproto/lexicon"
)

const syntheticObjectRecordID = "social.coves.validation.syntheticObject"

type objectValidationCatalog struct {
	lexicon.Catalog
	record lexicon.SchemaRecord
}

func (c objectValidationCatalog) Resolve(ref string) (*lexicon.Schema, error) {
	if ref == syntheticObjectRecordID {
		return &lexicon.Schema{ID: syntheticObjectRecordID, Def: c.record}, nil
	}
	return c.Catalog.Resolve(ref)
}

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

	return newLexiconValidator(catalog, strict), nil
}

// NewEmbeddedLexiconValidator creates a validator from the schemas compiled
// into the binary, so it works whatever the process's working directory is.
func NewEmbeddedLexiconValidator(strict bool) (*LexiconValidator, error) {
	catalog := lexicon.NewBaseCatalog()

	if err := catalog.LoadEmbedFS(lexiconschemas.Schemas); err != nil {
		return nil, fmt.Errorf("failed to load embedded lexicon schemas: %w", err)
	}

	return newLexiconValidator(catalog, strict), nil
}

func newLexiconValidator(catalog *lexicon.BaseCatalog, strict bool) *LexiconValidator {
	flags := lexicon.ValidateFlags(0)
	if strict {
		flags |= lexicon.StrictRecursiveValidation
	} else {
		flags |= lexicon.AllowLenientDatetime
	}

	return &LexiconValidator{
		catalog: catalog,
		flags:   flags,
	}
}

// ValidateRecord validates a record against its schema
func (v *LexiconValidator) ValidateRecord(recordData interface{}, recordType string) error {
	data, err := toAtprotoData(recordData)
	if err != nil {
		return err
	}

	// Ensure $type field matches recordType
	if typeField, ok := data["$type"].(string); ok && typeField != recordType {
		return fmt.Errorf("$type field '%s' does not match expected type '%s'", typeField, recordType)
	}

	return lexicon.ValidateRecord(v.catalog, data, recordType, v.flags)
}

// ValidateObject validates an object (for example one member of a union, such
// as an embed) against the schema definition ref names.
func (v *LexiconValidator) ValidateObject(objectData interface{}, ref string) error {
	data, err := toAtprotoData(objectData)
	if err != nil {
		return err
	}
	return ValidateData(v.catalog, data, ref, v.flags)
}

// toAtprotoData converts record data (a JSON document, a decoded JSON map or a
// struct that marshals to one) into the atproto data model the lexicon
// validator expects. A blob reference held as a plain JSON map
// ({"$type":"blob","ref":{"$link":...},...}) only validates as a blob once it
// has been parsed into atdata.Blob; the same goes for CID links and bytes.
func toAtprotoData(recordData interface{}) (map[string]any, error) {
	var encoded []byte
	switch rd := recordData.(type) {
	case []byte:
		encoded = rd
	case string:
		encoded = []byte(rd)
	default:
		var err error
		encoded, err = json.Marshal(recordData)
		if err != nil {
			return nil, fmt.Errorf("failed to convert record to JSON: %w", err)
		}
	}

	data, err := atdata.UnmarshalJSON(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to parse record data: %w", err)
	}
	return data, nil
}

// ValidateActorProfile validates an actor profile record
func (v *LexiconValidator) ValidateActorProfile(profile map[string]interface{}) error {
	return v.ValidateRecord(profile, "social.coves.actor.profile")
}

// ValidateCommunityProfile validates a community profile record
func (v *LexiconValidator) ValidateCommunityProfile(profile map[string]interface{}) error {
	return v.ValidateRecord(profile, "social.coves.community.profile")
}

// ValidatePost validates a post record against the DEPRECATED community-repo
// lexicon, social.coves.community.post.
//
// It is deliberately not collection-agnostic. An author-owned post
// (social.coves.community.postv2) obeys a different schema — no author field,
// and its repo is the author's rather than the community's — so validating one
// here would either fail on the missing field or, worse, pass a record whose
// shape the deprecated schema never described. A caller holding a postv2 record
// wants ValidateRecord with that NSID.
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

// ValidateData validates record and object data against a schema definition.
// Objects are wrapped in a synthetic record because Indigo only exports record
// validation, while the delegating catalog preserves resolution of nested refs.
func ValidateData(catalog lexicon.Catalog, data map[string]any, ref string, flags lexicon.ValidateFlags) error {
	schema, err := catalog.Resolve(ref)
	if err != nil {
		return fmt.Errorf("resolving schema %q: %w", ref, err)
	}

	switch definition := schema.Def.(type) {
	case lexicon.SchemaRecord:
		return lexicon.ValidateRecord(catalog, data, ref, flags)
	case lexicon.SchemaObject:
		// Mirror the record branch: a declared $type that disagrees with the
		// requested schema is a mismatch, not something to overwrite.
		if declared, ok := data["$type"].(string); ok && declared != ref {
			return fmt.Errorf("data $type %q does not match schema %s", declared, ref)
		}
		record := lexicon.SchemaRecord{
			Type:   "record",
			Key:    "any",
			Record: definition,
		}
		decoratedCatalog := objectValidationCatalog{Catalog: catalog, record: record}
		validationData := make(map[string]any, len(data)+1)
		for key, value := range data {
			validationData[key] = value
		}
		validationData["$type"] = syntheticObjectRecordID
		return lexicon.ValidateRecord(decoratedCatalog, validationData, syntheticObjectRecordID, flags)
	default:
		return fmt.Errorf("schema is not a record or object type: %s", ref)
	}
}
