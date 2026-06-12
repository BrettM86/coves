package posts

import "fmt"

// Embed union member $type discriminators accepted by
// social.coves.community.post.create. Keep this set in sync with the embed
// union refs declared in the post.create lexicon — a ref added there must be
// added here too, or it will be rejected at the API boundary as unknown.
//
// The get endpoint projects embeds through a separate, output-only "#view"
// union (e.g. social.coves.embed.record#view); those view types are never
// valid on create input and are correctly rejected here as unknown.
const (
	embedTypeImages   = "social.coves.embed.images"
	embedTypeVideo    = "social.coves.embed.video"
	embedTypeExternal = "social.coves.embed.external"
	embedTypePost     = "social.coves.embed.post"
)

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
