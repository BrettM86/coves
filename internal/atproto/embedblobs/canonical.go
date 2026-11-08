package embedblobs

import (
	"errors"
	"fmt"
)

// ErrEmbedRefTypeMismatch marks a $type at a non-union ref position that is not that position's schema type.
var ErrEmbedRefTypeMismatch = errors.New("embed ref $type does not match its schema position")

// RefPosition is one non-union ref position inside the embed lexicons.
type RefPosition struct {
	Lexicon  string // NSID of the lexicon file, e.g. "social.coves.embed.imagesWithAttachment"
	Def      string // def name, e.g. "main", "image", "external", "source"
	Property string // property name within that def's object
	Array    bool   // the property is an array whose items are the ref
	Type     string // the ref's full $type, e.g. "social.coves.embed.images#image"
}

const (
	refTypePost           = "social.coves.embed.post"
	refTypeImage          = TypeImages + "#image"
	refTypeAspectRatio    = TypeImages + "#aspectRatio"
	refTypeExternalObject = TypeExternal + "#external"
	refTypeSource         = TypeExternal + "#source"
	refTypeStrongRef      = "com.atproto.repo.strongRef"
)

// refPositions is every non-union ref position the embed lexicons declare in
// their record defs. A $type at one of them is optional under atproto, so
// readers must not require it; TestRefPositionsMatchTheEmbedLexicons keeps
// this table in step with the lexicon files.
var refPositions = []RefPosition{
	{Lexicon: TypeImagesWithAttachment, Def: "main", Property: "images", Type: TypeImages},
	{Lexicon: TypeImages, Def: "main", Property: "images", Array: true, Type: refTypeImage},
	{Lexicon: TypeImages, Def: "image", Property: "aspectRatio", Type: refTypeAspectRatio},
	{Lexicon: TypeExternal, Def: "main", Property: "external", Type: refTypeExternalObject},
	{Lexicon: TypeExternal, Def: "external", Property: "images", Array: true, Type: refTypeImage},
	{Lexicon: TypeExternal, Def: "external", Property: "sources", Array: true, Type: refTypeSource},
	{Lexicon: TypeExternal, Def: "source", Property: "sourcePost", Type: refTypeStrongRef},
	{Lexicon: refTypePost, Def: "main", Property: "post", Type: refTypeStrongRef},
}

// RefPositions returns a copy of the non-union ref positions in the embed lexicons.
func RefPositions() []RefPosition {
	return append([]RefPosition(nil), refPositions...)
}

// schemaType is the $type a def is known by: the bare NSID for main.
func schemaType(lexicon, def string) string {
	if def == "main" {
		return lexicon
	}
	return lexicon + "#" + def
}

// CanonicalizeEmbed returns embed with $type filled at the non-union ref
// positions the code dispatches on, which today is only the imagesWithAttachment
// gallery. Every non-union ref position is checked: a present $type that is not
// the position's schema type is an error wrapping ErrEmbedRefTypeMismatch.
//
// The walk follows the top-level $type and, inside imagesWithAttachment, the
// attachment's own $type. Union $types are never filled; a missing one is left
// to the embed validation. Every object the walk visits is copied, so embed is
// never mutated and the result's visited objects are its own; values the walk
// does not visit are shared with embed. A nil embed returns nil.
func CanonicalizeEmbed(embed map[string]any) (map[string]any, error) {
	if embed == nil {
		return nil, nil
	}
	embedType, _ := embed["$type"].(string)
	return canonicalizeObject(embed, embedType, "embed")
}

// canonicalizeObject copies object, the schema type ownerType at path, and
// canonicalizes the ref positions it owns.
func canonicalizeObject(object map[string]any, ownerType, path string) (map[string]any, error) {
	copied := make(map[string]any, len(object))
	for key, value := range object {
		copied[key] = value
	}
	for _, position := range refPositions {
		if schemaType(position.Lexicon, position.Def) != ownerType {
			continue
		}
		propertyPath := path + "." + position.Property
		if !position.Array {
			child, isObject := copied[position.Property].(map[string]any)
			if !isObject {
				continue
			}
			canonical, err := canonicalizeRef(child, position, propertyPath)
			if err != nil {
				return nil, err
			}
			copied[position.Property] = canonical
			continue
		}
		entries, isArray := copied[position.Property].([]any)
		if !isArray {
			continue
		}
		copiedEntries := make([]any, len(entries))
		for i, entry := range entries {
			copiedEntries[i] = entry
			child, isObject := entry.(map[string]any)
			if !isObject {
				continue
			}
			canonical, err := canonicalizeRef(child, position, fmt.Sprintf("%s[%d]", propertyPath, i))
			if err != nil {
				return nil, err
			}
			copiedEntries[i] = canonical
		}
		copied[position.Property] = copiedEntries
	}
	if ownerType == TypeImagesWithAttachment {
		if attachment, isObject := copied["attachment"].(map[string]any); isObject {
			attachmentType, _ := attachment["$type"].(string)
			canonical, err := canonicalizeObject(attachment, attachmentType, path+".attachment")
			if err != nil {
				return nil, err
			}
			copied["attachment"] = canonical
		}
	}
	return copied, nil
}

// canonicalizeRef checks the $type of child, the ref at position, fills it on
// the imagesWithAttachment gallery, and descends into child.
func canonicalizeRef(child map[string]any, position RefPosition, path string) (map[string]any, error) {
	if value, present := child["$type"]; present && value != position.Type {
		return nil, fmt.Errorf("%w: %s.$type is not %s", ErrEmbedRefTypeMismatch, path, position.Type)
	}
	canonical, err := canonicalizeObject(child, position.Type, path)
	if err != nil {
		return nil, err
	}
	if position.Lexicon == TypeImagesWithAttachment && position.Property == "images" {
		canonical["$type"] = TypeImages
	}
	return canonical, nil
}

// MainGallery returns the images gallery of an images embed (the embed itself)
// or of an imagesWithAttachment embed (its images object, when that carries no
// $type or the images type). Readers use it rather than checking the nested
// $type, which atproto leaves optional and rows stored before canonicalization
// may lack. The returned map is the embed's own, not a copy, so a caller that
// modifies it must own the embed.
func MainGallery(embed map[string]any) (map[string]any, bool) {
	switch embed["$type"] {
	case TypeImages:
		return embed, true
	case TypeImagesWithAttachment:
		gallery, isObject := embed["images"].(map[string]any)
		if !isObject {
			return nil, false
		}
		if value, present := gallery["$type"]; present && value != TypeImages {
			return nil, false
		}
		return gallery, true
	}
	return nil, false
}
