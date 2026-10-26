package comments

import (
	"errors"
	"fmt"

	"Coves/internal/atproto/embedimages"
	"Coves/internal/validation"
)

// commentEmbedTypes are the embed types a comment may carry. The comment
// lexicon's embed union is open, so the lexicon validator alone would accept
// any $type (social.coves.embed.external among them); this list closes it.
var commentEmbedTypes = map[string]bool{
	"social.coves.embed.images":               true,
	"social.coves.embed.imagesWithAttachment": true,
	"social.coves.embed.post":                 true,
}

// WithLexiconValidator injects the lexicon validator run on the final comment
// record before it is written, on both create and update. Without one, no
// lexicon validation runs; the embedimages checks on an images embed still do.
func WithLexiconValidator(v *validation.LexiconValidator) CommentServiceOption {
	return func(s *commentService) { s.lexiconValidator = v }
}

// LexiconValidator returns the injected lexicon validator, or nil.
func (s *commentService) LexiconValidator() *validation.LexiconValidator {
	return s.lexiconValidator
}

// validateCommentEmbed checks a comment embed on its own. An images embed is
// held to embedimages.Check first, with or without a lexicon validator: more
// than eight images is ErrTooManyImages, and an image that is not a well-formed
// image blob ref wraps ErrInvalidEmbed. With a validator, the embed must also
// be an object whose $type is one of commentEmbedTypes and which satisfies that
// type's lexicon; a failure wraps ErrInvalidEmbed and names the failing part.
// A composite may attach only a quoted post, with or without a validator.
// An absent embed passes.
func (s *commentService) validateCommentEmbed(embed interface{}) error {
	if embed == nil {
		return nil
	}
	object, isObject := embed.(map[string]interface{})
	if isObject {
		if err := embedimages.Check(object); err != nil {
			if errors.Is(err, embedimages.ErrTooManyImages) {
				return fmt.Errorf("%w: %w", ErrTooManyImages, err)
			}
			return fmt.Errorf("%w: %w", ErrInvalidEmbed, err)
		}
		if object["$type"] == "social.coves.embed.imagesWithAttachment" {
			images, ok := object["images"].(map[string]interface{})
			if !ok || images["$type"] != "social.coves.embed.images" {
				return fmt.Errorf("%w: imagesWithAttachment requires an images embed", ErrInvalidEmbed)
			}
			attachment, _ := object["attachment"].(map[string]interface{})
			if attachment["$type"] != "social.coves.embed.post" {
				return fmt.Errorf("%w: imagesWithAttachment on a comment requires a quoted post attachment", ErrInvalidEmbed)
			}
		}
	}
	if s.lexiconValidator == nil {
		return nil
	}
	if !isObject {
		return fmt.Errorf("%w: embed must be an object", ErrInvalidEmbed)
	}
	embedType, ok := object["$type"].(string)
	if !ok {
		return fmt.Errorf("%w: embed.$type is missing or not a string", ErrInvalidEmbed)
	}
	if !commentEmbedTypes[embedType] {
		return fmt.Errorf("%w: embed.$type %q is not allowed on a comment (allowed: social.coves.embed.images, social.coves.embed.imagesWithAttachment, social.coves.embed.post)", ErrInvalidEmbed, embedType)
	}
	if err := s.lexiconValidator.ValidateObject(object, embedType); err != nil {
		return fmt.Errorf("%w: embed (%s): %v", ErrInvalidEmbed, embedType, err)
	}
	return nil
}

// validateCommentRecord checks the record about to be written against
// social.coves.community.comment. The PDS stores social.coves.* records
// without validating them, so this is the last point at which a record this
// AppView signs can be held to its schema.
//
// The embed is checked on its own first, even without a lexicon validator, so
// an invalid embed is ErrInvalidEmbed (or ErrTooManyImages) whatever else is
// wrong. With a valid embed and a validator, any remaining lexicon failure of
// the record is ErrInvalidRecord.
func (s *commentService) validateCommentRecord(record CommentRecord) error {
	if err := s.validateCommentEmbed(record.Embed); err != nil {
		return err
	}
	if s.lexiconValidator == nil {
		return nil
	}
	if err := s.lexiconValidator.ValidateRecord(record, commentCollection); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecord, err)
	}
	return nil
}
