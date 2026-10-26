// Package embedimages checks standalone and composite main image galleries:
// at most MaxImages total entries, with well-formed main image blob refs.
//
// The post and comment write paths both run it, with or without a lexicon
// validator wired, so it imports only the standard library and the embedblobs
// leaf, and either service can depend on it without importing the other.
package embedimages

import (
	"errors"
	"fmt"
	"math"

	"Coves/internal/atproto/embedblobs"
)

// MaxImages is the most entries `images` may hold: the maxLength of `images`
// in social.coves.embed.images.
const MaxImages = embedblobs.MaxEmbedImages

// MaxImageSize is the largest image blob, in bytes: the maxSize of
// social.coves.embed.images#image.image.
const MaxImageSize = 10_000_000

// acceptedMimeTypes is the accept list of social.coves.embed.images#image.image.
var acceptedMimeTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

var (
	// ErrTooManyImages marks an images embed with more than MaxImages entries.
	ErrTooManyImages = errors.New("too many images")

	// ErrInvalidImageBlob marks an images entry whose image is not a
	// well-formed image blob ref.
	ErrInvalidImageBlob = errors.New("invalid image blob")
)

// Check checks an images embed or the main gallery of imagesWithAttachment.
// The total raw count is checked first, so a long list is
// reported as ErrTooManyImages whatever its entries hold; then each entry's
// image must be a blob ref: `$type` "blob", `ref` an object whose `$link` is a
// sha2-256 blob CID (embedblobs.CanonicalBlobCID, the form a blob reference is
// recorded under), `mimeType` an accepted image type, and `size` a
// non-negative integer no larger than MaxImageSize. A failure wraps
// ErrTooManyImages or ErrInvalidImageBlob.
//
// A nil embed, an embed of any other type, and an `images` value that is not an
// array are left to the embed shape checks and return nil.
func Check(embed map[string]interface{}) error {
	embedType, _ := embed["$type"].(string)
	if embedType != embedblobs.TypeImages && embedType != embedblobs.TypeImagesWithAttachment {
		return nil
	}
	// Count both raw galleries before inspecting any blob. External gallery
	// entries retain their existing validation policy; only the main gallery
	// receives the standalone image blob checks below.
	count := embedblobs.ImageCount(embed)
	if count > MaxImages {
		return fmt.Errorf("%w: embed has %d images, at most %d allowed", ErrTooManyImages, count, MaxImages)
	}
	if embedType == embedblobs.TypeImagesWithAttachment {
		main, _ := embed["images"].(map[string]interface{})
		return Check(main)
	}
	images, ok := embed["images"].([]interface{})
	if !ok {
		return nil
	}
	for i, entry := range images {
		if err := checkEntry(entry); err != nil {
			return fmt.Errorf("%w: images[%d]: %s", ErrInvalidImageBlob, i, err)
		}
	}
	return nil
}

// checkEntry reports what is wrong with one images entry's image blob.
func checkEntry(entry interface{}) error {
	image, ok := entry.(map[string]interface{})
	if !ok {
		return errors.New("entry must be an object")
	}
	blob, ok := image["image"].(map[string]interface{})
	if !ok {
		return errors.New("image must be a blob object")
	}
	if blobType, _ := blob["$type"].(string); blobType != "blob" {
		return errors.New(`image must have $type "blob"`)
	}
	ref, ok := blob["ref"].(map[string]interface{})
	if !ok {
		return errors.New(`image must have a ref object carrying a "$link" CID`)
	}
	link, ok := ref["$link"].(string)
	if !ok {
		return errors.New(`image ref must carry a "$link" string`)
	}
	if _, ok := embedblobs.CanonicalBlobCID(link); !ok {
		return errors.New("image ref $link is not a sha2-256 blob CID")
	}
	if mimeType, _ := blob["mimeType"].(string); !acceptedMimeTypes[mimeType] {
		return fmt.Errorf("image mimeType %q is not one of image/png, image/jpeg, image/webp, image/gif", mimeType)
	}
	size, ok := integerSize(blob["size"])
	if !ok {
		return errors.New("image size must be a non-negative integer")
	}
	if size > MaxImageSize {
		return fmt.Errorf("image size %d exceeds the %d-byte limit", size, MaxImageSize)
	}
	return nil
}

// integerSize returns a blob size as a non-negative integer. A size decoded
// from JSON is a float64; one a Go caller builds into the embed map is an int.
// A fractional, negative, non-numeric or int64-overflowing value is refused.
func integerSize(value interface{}) (int64, bool) {
	switch size := value.(type) {
	case float64:
		// Compared as a float: math.MaxInt64 rounds up to 2^63 as a float64, and
		// converting 2^63 to int64 is implementation-defined.
		if size < 0 || size != math.Trunc(size) || size >= 1<<63 {
			return 0, false
		}
		return int64(size), true
	case int:
		return int64(size), size >= 0
	default:
		return 0, false
	}
}
