package jetstream

import (
	"fmt"

	"Coves/internal/atproto/embedblobs"
)

// MaxEmbedImages is the most images an embed may carry: the maxLength of
// `images` in social.coves.embed.images and of `external.images` in
// social.coves.embed.external.
const MaxEmbedImages = embedblobs.MaxEmbedImages

// validateEmbedImageCount rejects an embed whose image list is longer than the
// lexicon allows. A generic PDS does not validate social.coves records, so the
// consumer enforces the bound before indexing writes one blob reference per
// image. The raw array length is counted, valid blob or not. A non-array value
// is left alone. Migrations 062 and 065 apply the same rule to backfilled rows
// through embedblobs.ExceedsImageLimit.
//
// PERMANENT: the count is fixed in the record, so no retry can make it valid.
func validateEmbedImageCount(embed map[string]interface{}) error {
	count := embedblobs.ImageCount(embed)
	if count <= MaxEmbedImages {
		return nil
	}
	return fmt.Errorf("%w: embed has %d images, more than the lexicon maximum of %d",
		ErrPermanentEvent, count, MaxEmbedImages)
}

// canonicalizeRecordEmbed returns a firehose record's embed in the canonical
// shape the index stores (embedblobs.CanonicalizeEmbed), so blob references,
// the image cap and hydration see an untyped gallery the same as a typed one.
//
// PERMANENT: a $type naming the wrong schema at a non-union ref position is
// fixed in the record, so no retry can make it valid.
func canonicalizeRecordEmbed(embed map[string]interface{}) (map[string]interface{}, error) {
	canonical, err := embedblobs.CanonicalizeEmbed(embed)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPermanentEvent, err)
	}
	return canonical, nil
}
