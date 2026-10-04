package moderation

import "strconv"

// InitialVersion is the opaque state token of a subject with no moderation
// rows. Readers treat later tokens as opaque.
const InitialVersion = "v0"

func versionToken(version int64) string {
	return "v" + strconv.FormatInt(version, 10)
}
