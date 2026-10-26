package posts

import (
	"errors"
	"fmt"
	"time"

	"Coves/internal/atproto/embedimages"
	"Coves/internal/validation"
)

// WithLexiconValidator injects the lexicon validator run on the final postv2
// record before it is written, on both create and update. Without one, no
// lexicon validation runs.
func WithLexiconValidator(v *validation.LexiconValidator) PostServiceOption {
	return func(s *postService) { s.lexiconValidator = v }
}

// LexiconValidator returns the injected lexicon validator, or nil.
func (s *postService) LexiconValidator() *validation.LexiconValidator { return s.lexiconValidator }

// validatePostV2Record checks the record about to be written: first its
// images (validateEmbedImages), whether or not a lexicon validator is wired,
// then against social.coves.community.postv2.
//
// The PDS stores social.coves.* records without validating them, so this is
// the last point at which a record this AppView signs can be held to the
// schema it declares. A create runs it twice: on the client's record before
// admission (validateSubmissionLexicon), and on the FINAL record — after
// normalization and unfurl enhancement — because that is what reaches the
// repo. An update runs it once, on the merged record.
//
// A failure is located by validating the embed on its own against its union
// member's schema: if the embed fails, the error is ErrInvalidEmbed on field
// "embed" whatever else is wrong; otherwise it is a plain validation error.
func (s *postService) validatePostV2Record(record PostV2Record) error {
	if err := validateEmbedImages(record.Embed); err != nil {
		return err
	}
	if s.lexiconValidator == nil {
		return nil
	}
	recordErr := s.lexiconValidator.ValidateRecord(record, PostV2Collection)
	if recordErr == nil {
		return nil
	}
	if record.Embed != nil {
		embedType, _ := record.Embed["$type"].(string)
		if embedErr := s.lexiconValidator.ValidateObject(record.Embed, embedType); embedErr != nil {
			return NewValidationErrorFrom("embed", fmt.Errorf("%w: %v", ErrInvalidEmbed, embedErr))
		}
	}
	return NewValidationError("record", fmt.Sprintf("post record does not satisfy %s: %v", PostV2Collection, recordErr))
}

// submissionCheckCommunityDID stands in for the community in the
// pre-admission check. The community is resolved during admission and the
// written record carries the resolved DID; its format is the server's
// responsibility, not the client's, so the check needs only a syntactically
// valid DID there. It is never written.
const submissionCheckCommunityDID = "did:web:community.invalid"

// validateSubmissionLexicon checks the record a create request describes —
// as the client sent it, before unfurl enhancement — against
// social.coves.community.postv2. It runs before admission, so a client
// mistake is refused without a reservation, an unfurl fetch or a thumbnail
// upload. The community and createdAt are stamped by the server later; the
// final record is checked again before the write.
func (s *postService) validateSubmissionLexicon(req CreatePostRequest) error {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	return s.validatePostV2Record(postV2From(postRecordFor(req, submissionCheckCommunityDID, createdAt)))
}

// validateEmbedImages applies embedimages.Check to an images embed. It runs
// ahead of the lexicon check, because the lexicon's `images` maxLength would
// otherwise report a ninth image as an invalid embed rather than as too many
// images, and it runs without a lexicon validator too.
func validateEmbedImages(embed map[string]interface{}) error {
	err := embedimages.Check(embed)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, embedimages.ErrTooManyImages):
		return &ValidationError{Field: "embed", Message: err.Error(), Err: ErrTooManyImages}
	default:
		return newInvalidEmbedErrorFrom("embed", err)
	}
}

// newInvalidEmbedError is a validation error on field whose message is message
// and which matches ErrInvalidEmbed, for the embed gates that run before the
// lexicon check.
func newInvalidEmbedError(field, message string) error {
	return &ValidationError{Field: field, Message: message, Err: ErrInvalidEmbed}
}

// newInvalidEmbedErrorFrom is newInvalidEmbedError keeping cause matchable.
func newInvalidEmbedErrorFrom(field string, cause error) error {
	return &ValidationError{
		Field:   field,
		Message: cause.Error(),
		Err:     fmt.Errorf("%w: %w", ErrInvalidEmbed, cause),
	}
}
