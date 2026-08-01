package posts

import (
	"fmt"

	"Coves/internal/core/embeds"
	"Coves/internal/validation"
)

// Embed union member $type discriminators accepted by
// social.coves.community.post.create. Keep this set in sync with the embed
// union refs declared in the post.create lexicon — a ref added there must be
// added here too, or it will be rejected at the API boundary as unknown.
//
// The names come from internal/core/embeds, which is the package that reads
// these same discriminators when projecting a stored embed into its view. Two
// independent spellings of one lexicon contract can drift, and the failure —
// the validator accepting a type the projector then ignores — is silent.
//
// The get endpoint projects embeds through a separate, output-only "#view"
// union (social.coves.embed.images#view and the sibling video, external and
// post projections); those view types are never valid on create input and are
// correctly rejected here as unknown.
const (
	embedTypeImages   = embeds.TypeImages
	embedTypeVideo    = embeds.TypeVideo
	embedTypeExternal = embeds.TypeExternal
	embedTypePost     = embeds.TypePost
)

// maxEmbedSources mirrors the maxLength on the sources array in
// social.coves.embed.external. Without it an authenticated client can post an
// arbitrarily long array, and the AppView would normalize every entry and then
// sign a record violating the schema this normalization exists to guarantee.
const maxEmbedSources = 50

// validateEmbed verifies that a client-provided embed conforms to one of the
// post.create lexicon union members: it must carry a recognized $type
// discriminator and the required nested field(s) for that type.
//
// atProto unions require an explicit $type. Without this guard the create
// handler persists whatever JSON object the client sends straight to the PDS
// (which does not validate custom social.coves.* lexicons) — for example a
// bare {"uri": "..."} — producing a record that carries no recognizable embed
// type. Readers key off $type: the AppView blob transform no-ops and forwards
// the embed verbatim, and every frontend that switches on $type has no branch
// to render it. The embed is stored and returned but never displays — a silent
// failure with no error anywhere in the pipeline. Rejecting malformed embeds at
// the API boundary turns that silent data-corruption into a clear 400.
//
// A nil embed is valid: the embed field is optional.
func validateEmbed(embed map[string]interface{}) error {
	if embed == nil {
		return nil
	}

	rawType, present := embed["$type"]
	if !present {
		return NewValidationError("embed",
			"embed is missing its required $type discriminator (expected one of "+
				embedTypeImages+", "+embedTypeVideo+", "+embedTypeExternal+", "+embedTypePost+")")
	}
	embedType, isString := rawType.(string)
	if !isString {
		return NewValidationError("embed",
			fmt.Sprintf("embed $type must be a string, got %T", rawType))
	}

	switch embedType {
	case embedTypeExternal:
		external, ok := embed["external"].(map[string]interface{})
		if !ok {
			return NewValidationError("embed",
				fmt.Sprintf("%s requires an 'external' object", embedTypeExternal))
		}
		if uri, ok := external["uri"].(string); !ok || uri == "" {
			return NewValidationError("embed",
				fmt.Sprintf("%s requires a non-empty 'external.uri' string", embedTypeExternal))
		}

	case embedTypeImages:
		images, ok := embed["images"].([]interface{})
		if !ok || len(images) == 0 {
			return NewValidationError("embed",
				fmt.Sprintf("%s requires a non-empty 'images' array", embedTypeImages))
		}
		for i, raw := range images {
			img, ok := raw.(map[string]interface{})
			if !ok {
				return NewValidationError("embed",
					fmt.Sprintf("%s images[%d] must be an object", embedTypeImages, i))
			}
			// A blob always decodes to an object; reject a bare URL string the
			// same way the video case does, so the sibling blob cases stay
			// symmetric (deeper $type:blob/ref/mimeType validation is left to
			// the blob-handling pipeline).
			if _, ok := img["image"].(map[string]interface{}); !ok {
				return NewValidationError("embed",
					fmt.Sprintf("%s images[%d] requires an 'image' blob object", embedTypeImages, i))
			}
		}

	case embedTypeVideo:
		// A blob always decodes to an object; a bare URL string is the common
		// client mistake and must be rejected (same intent as the stricter
		// external.thumb blob guard in the post service).
		if _, ok := embed["video"].(map[string]interface{}); !ok {
			return NewValidationError("embed",
				fmt.Sprintf("%s requires a 'video' blob object", embedTypeVideo))
		}

	case embedTypePost:
		post, ok := embed["post"].(map[string]interface{})
		if !ok {
			return NewValidationError("embed",
				fmt.Sprintf("%s requires a 'post' strong reference", embedTypePost))
		}
		if uri, ok := post["uri"].(string); !ok || uri == "" {
			return NewValidationError("embed",
				fmt.Sprintf("%s requires a non-empty 'post.uri' string", embedTypePost))
		}
		if cid, ok := post["cid"].(string); !ok || cid == "" {
			return NewValidationError("embed",
				fmt.Sprintf("%s requires a non-empty 'post.cid' string", embedTypePost))
		}

	default:
		return NewValidationError("embed",
			fmt.Sprintf("unknown embed $type %q (allowed: %s, %s, %s, %s)",
				embedType, embedTypeImages, embedTypeVideo, embedTypeExternal, embedTypePost))
	}

	return nil
}

// normalizeEmbedURIs rewrites, in place, every field of an external embed that
// the lexicon declares as `format: uri` — external.uri and each
// external.sources[].uri — into a form that satisfies that format.
//
// The AppView signs community post records into the community's PDS itself, and
// the PDS does not validate custom social.coves.* lexicons. That makes this the
// only point in the pipeline that can guarantee a schema-conforming record no
// matter which client produced it, which matters because these URIs federate:
// any third-party tool that resolves our lexicons and validates the firehose
// judges the bytes we wrote. An unencoded character in a URL is a client bug,
// not user intent, so it is repaired rather than rejected — see
// validation.NormalizeURI. Input that carries no recoverable URI at all still
// fails loudly instead of being persisted as a broken link.
//
// Must run after validateEmbed, which establishes the structure this walks.
// Non-external embeds carry no `format: uri` fields and are left untouched.
func normalizeEmbedURIs(embed map[string]interface{}) error {
	if embed == nil {
		return nil
	}
	if embedType, _ := embed["$type"].(string); embedType != embedTypeExternal {
		return nil
	}
	external, ok := embed["external"].(map[string]interface{})
	if !ok {
		// validateEmbed already rejects this; nothing to normalize.
		return nil
	}

	if raw, ok := external["uri"].(string); ok {
		normalized, err := validation.NormalizeURI(raw)
		if err != nil {
			return NewValidationErrorFrom("embed.external.uri", err)
		}
		external["uri"] = normalized
	}

	rawSources, present := external["sources"]
	if !present {
		return nil
	}
	// A present-but-wrong-typed sources value must be rejected, not skipped:
	// validateEmbed does not inspect sources at all, so returning nil here would
	// sign the very schema-invalid record this function exists to prevent.
	sources, ok := rawSources.([]interface{})
	if !ok {
		return NewValidationError("embed.external.sources",
			fmt.Sprintf("embed.external.sources must be an array, got %T", rawSources))
	}
	if len(sources) > maxEmbedSources {
		return NewValidationError("embed.external.sources",
			fmt.Sprintf("too many sources: %d (max %d)", len(sources), maxEmbedSources))
	}
	for i, entry := range sources {
		field := fmt.Sprintf("embed.external.sources[%d].uri", i)
		source, ok := entry.(map[string]interface{})
		if !ok {
			return NewValidationError(field,
				fmt.Sprintf("embed.external.sources[%d] must be an object", i))
		}
		// uri is required on #source; a source without one is an unusable
		// dangling entry that would fail schema validation downstream.
		raw, ok := source["uri"].(string)
		if !ok || raw == "" {
			return NewValidationError(field,
				fmt.Sprintf("embed.external.sources[%d] requires a non-empty 'uri' string", i))
		}
		normalized, err := validation.NormalizeURI(raw)
		if err != nil {
			return NewValidationErrorFrom(field, err)
		}
		source["uri"] = normalized
	}

	return nil
}
