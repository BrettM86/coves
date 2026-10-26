// Package embedblobs extracts the blob CIDs an embed references, with the kind
// of reference each one is. It imports only the standard library and go-cid so
// that any package, including the database migrations, can depend on it.
package embedblobs

import "github.com/ipfs/go-cid"

// Kind is a blob reference kind, matching the blob_references.kind values.
type Kind string

const (
	KindPostImage     Kind = "post_image"
	KindCommentImage  Kind = "comment_image"
	KindExternalThumb Kind = "external_thumb"
	KindVideoThumb    Kind = "video_thumb"
	KindAvatar        Kind = "avatar"
	KindBanner        Kind = "banner"
)

// Embed $type values the extractors switch on.
const (
	TypeImages               = "social.coves.embed.images"
	TypeImagesWithAttachment = "social.coves.embed.imagesWithAttachment"
	TypeExternal             = "social.coves.embed.external"
	TypeVideo                = "social.coves.embed.video"
)

// Reference is one canonical blob CID and the kind of reference it is.
type Reference struct {
	CID  string
	Kind Kind
}

// BlobCID extracts the raw CID string from an atproto blob reference, without
// decoding or canonicalizing it.
//
// It returns "" for anything that is not a blob carrying a CID, including a ref
// in a shape we do not recognize, which is left as-is rather than guessed at.
//
// Two encodings are accepted. ref.$link is the current form, and is the only
// one consulted when ref is an object — a ref object without a usable $link
// yields "" rather than falling through. A top-level cid is the legacy blob
// encoding that predates the CID-link format, used whenever ref is absent or is
// not an object; records carrying it are still in circulation on the network
// and reach our index through federation.
func BlobCID(value interface{}) string {
	blob, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}

	if ref, ok := blob["ref"].(map[string]interface{}); ok {
		if link, ok := ref["$link"].(string); ok {
			return link
		}
		return ""
	}

	if legacy, ok := blob["cid"].(string); ok {
		return legacy
	}

	return ""
}

// MaxEmbedImages is the most images an embed may carry: the maxLength of
// `images` in social.coves.embed.images and of `external.images` in
// social.coves.embed.external.
const MaxEmbedImages = 8

// ImageCount returns the raw number of images, including both galleries in
// imagesWithAttachment. Every entry counts, valid blob or not. A non-array
// value, or an unsupported embed type, counts as 0.
func ImageCount(embed map[string]interface{}) int {
	var images interface{}
	switch embedType, _ := embed["$type"].(string); embedType {
	case TypeImagesWithAttachment:
		gallery, _ := embed["images"].(map[string]interface{})
		attachment, _ := embed["attachment"].(map[string]interface{})
		count := 0
		if gallery["$type"] == TypeImages {
			count = ImageCount(gallery)
		}
		if attachment["$type"] == TypeExternal {
			count += ImageCount(attachment)
		}
		return count
	case TypeImages:
		images = embed["images"]
	case TypeExternal:
		if external, ok := embed["external"].(map[string]interface{}); ok {
			images = external["images"]
		}
	}
	entries, _ := images.([]interface{})
	return len(entries)
}

// ExceedsImageLimit reports whether the total image count exceeds MaxEmbedImages.
func ExceedsImageLimit(embed map[string]interface{}) bool {
	return ImageCount(embed) > MaxEmbedImages
}

// PostReferences returns canonical blob references served by the image proxy
// for a post embed, in first-seen order. Video blobs themselves are excluded.
func PostReferences(embed map[string]interface{}) []Reference {
	references, _ := PostReferencesAndSkipped(embed)
	return references
}

// PostReferencesAndSkipped is PostReferences plus the number of blob slots that
// yielded no reference: an image entry, or a present thumbnail, that is
// malformed, not a sha2-256 blob CID, or a repeat of an earlier reference.
func PostReferencesAndSkipped(embed map[string]interface{}) ([]Reference, int) {
	var references referenceCollector
	switch embedType, _ := embed["$type"].(string); embedType {
	case TypeImages:
		references.addImages(embed["images"], KindPostImage)
	case TypeImagesWithAttachment:
		gallery, _ := embed["images"].(map[string]interface{})
		if gallery["$type"] == TypeImages {
			references.addImages(gallery["images"], KindPostImage)
		}
		attachment, _ := embed["attachment"].(map[string]interface{})
		if attachment["$type"] == TypeExternal {
			references.addExternal(attachment)
		}
	case TypeVideo:
		references.addBlob(embed["thumbnail"], KindVideoThumb)
	case TypeExternal:
		references.addExternal(embed)
	}
	return references.items, references.skipped
}

// CommentReferences returns canonical image references from an images embed
// or the gallery of imagesWithAttachment. Quote attachments carry no media.
func CommentReferences(embed map[string]interface{}) []Reference {
	references, _ := CommentReferencesAndSkipped(embed)
	return references
}

// CommentReferencesAndSkipped is CommentReferences plus the number of image
// entries that yielded no reference, counted as in PostReferencesAndSkipped.
func CommentReferencesAndSkipped(embed map[string]interface{}) ([]Reference, int) {
	if embed["$type"] == TypeImagesWithAttachment {
		embed, _ = embed["images"].(map[string]interface{})
	}
	if embed["$type"] != TypeImages {
		return nil, 0
	}
	var references referenceCollector
	references.addImages(embed["images"], KindCommentImage)
	return references.items, references.skipped
}

func (c *referenceCollector) addExternal(embed map[string]interface{}) {
	if external, ok := embed["external"].(map[string]interface{}); ok {
		c.addBlob(external["thumb"], KindExternalThumb)
		c.addImages(external["images"], KindPostImage)
	}
}

// referenceCollector accumulates canonical references, dropping repeats of the
// same (CID, kind) pair while keeping first-seen order. skipped counts the blob
// slots that yielded no new reference.
type referenceCollector struct {
	seen    map[Reference]bool
	items   []Reference
	skipped int
}

// sha256MultihashCode is the multihash code for sha2-256, the only hash an
// atproto blob CID uses.
const sha256MultihashCode = 0x12

// addBlob records a blob CID only when it decodes to a sha2-256 multihash with a
// 32-byte digest, as every atproto blob CID does. Anything else is skipped,
// which also bounds the CID length: go-cid accepts identity-multihash CIDs of
// any size.
//
// An absent value is not a slot; anything else that adds no reference counts as
// skipped.
func (c *referenceCollector) addBlob(value interface{}, kind Kind) {
	if value == nil {
		return
	}
	canonical, ok := CanonicalBlobCID(BlobCID(value))
	if !ok {
		c.skipped++
		return
	}
	reference := Reference{CID: canonical, Kind: kind}
	if c.seen[reference] {
		c.skipped++
		return
	}
	if c.seen == nil {
		c.seen = make(map[Reference]bool)
	}
	c.seen[reference] = true
	c.items = append(c.items, reference)
}

// CanonicalBlobCID returns raw re-encoded in go-cid's default (base32 for
// CIDv1) string form, the form blob references store. It reports false when raw
// does not decode as a CID with a 32-byte sha2-256 multihash.
func CanonicalBlobCID(raw string) (string, bool) {
	parsed, err := cid.Decode(raw)
	if err != nil {
		return "", false
	}
	if prefix := parsed.Prefix(); prefix.MhType != sha256MultihashCode || prefix.MhLength != 32 {
		return "", false
	}
	return parsed.String(), true
}

func (c *referenceCollector) addImages(value interface{}, kind Kind) {
	images, _ := value.([]interface{})
	for _, entry := range images {
		image, ok := entry.(map[string]interface{})
		if !ok || image["image"] == nil {
			c.skipped++
			continue
		}
		c.addBlob(image["image"], kind)
	}
}

// ProfileReferences returns canonical avatar and banner references from stored
// profile CID columns, in that order. Empty or invalid CIDs are omitted.
func ProfileReferences(avatarCID, bannerCID string) []Reference {
	references, _ := ProfileReferencesAndSkipped(avatarCID, bannerCID)
	return references
}

// ProfileReferencesAndSkipped also counts non-empty CIDs that could not be
// decoded as sha2-256 blob CIDs.
func ProfileReferencesAndSkipped(avatarCID, bannerCID string) ([]Reference, int) {
	var references []Reference
	var skipped int
	for _, slot := range []struct {
		cid  string
		kind Kind
	}{
		{avatarCID, KindAvatar},
		{bannerCID, KindBanner},
	} {
		if slot.cid == "" {
			continue
		}
		canonical, ok := CanonicalBlobCID(slot.cid)
		if !ok {
			skipped++
			continue
		}
		references = append(references, Reference{CID: canonical, Kind: slot.kind})
	}
	return references, skipped
}
