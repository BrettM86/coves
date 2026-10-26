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
