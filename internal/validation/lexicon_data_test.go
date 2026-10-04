package validation

import (
	"testing"

	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validTestCID = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"

func loadTestCatalog(t *testing.T) *lexicon.BaseCatalog {
	t.Helper()

	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../internal/atproto/lexicon"),
		"the ValidateData contract cannot be tested without the repository lexicons")
	return catalog
}

func validBanView(indexedAt string) map[string]any {
	return map[string]any{
		"$type":     "social.coves.moderation.defs#banView",
		"uri":       "at://did:plc:abc123/social.coves.moderation.ban/3k7a3dmb5bk2c",
		"cid":       validTestCID,
		"record":    map[string]any{"active": true},
		"indexedAt": indexedAt,
	}
}

// TestValidateData pins one validation entry point for both repository records
// and endpoint/view objects, preventing object fixtures from being rejected
// merely because their resolved definition is not a record.
func TestValidateData(t *testing.T) {
	catalog := loadTestCatalog(t)

	t.Run("record ref still validates", func(t *testing.T) {
		valid := map[string]any{
			"$type": "social.coves.community.removal",
			"subject": map[string]any{
				"uri": "at://did:plc:abc123/social.coves.community.postv2/3k7a3dmb5bk2c",
				"cid": validTestCID,
			},
			"code":      "spam",
			"createdAt": "2026-09-21T10:00:00Z",
		}
		require.NoError(t, ValidateData(catalog, valid, "social.coves.community.removal", 0),
			"routing records through ValidateData must preserve record validation")

		missingCode := map[string]any{
			"$type": "social.coves.community.removal",
			"subject": map[string]any{
				"uri": "at://did:plc:abc123/social.coves.community.postv2/3k7a3dmb5bk2c",
				"cid": validTestCID,
			},
			"createdAt": "2026-09-21T10:00:00Z",
		}
		require.ErrorContains(t,
			ValidateData(catalog, missingCode, "social.coves.community.removal", 0), "code",
			"ValidateData must not bypass required fields on record definitions")
	})

	t.Run("object ref accepts valid data", func(t *testing.T) {
		const ref = "social.coves.moderation.defs#banView"
		valid := validBanView("2026-09-21T10:00:00Z")
		require.NoError(t, ValidateData(catalog, valid, ref, 0),
			"a valid endpoint view object must not be rejected as a non-record schema")
	})

	t.Run("object ref enforces required fields", func(t *testing.T) {
		const ref = "social.coves.moderation.defs#banView"
		missingCID := validBanView("2026-09-21T10:00:00Z")
		delete(missingCID, "cid")
		require.ErrorContains(t, ValidateData(catalog, missingCID, ref, 0), "cid",
			"object validation must enforce banView's required cid field")
	})

	t.Run("object ref rejects mismatched $type", func(t *testing.T) {
		const ref = "social.coves.moderation.defs#banView"
		data := validBanView("2026-09-21T10:00:00Z")
		data["$type"] = "social.coves.moderation.defs#moderationView"
		require.ErrorContains(t, ValidateData(catalog, data, ref, 0), "does not match",
			"a declared $type that disagrees with the schema ref must not be silently overwritten")
	})

	t.Run("object ref accepts absent $type", func(t *testing.T) {
		const ref = "social.coves.moderation.defs#banView"
		data := validBanView("2026-09-21T10:00:00Z")
		delete(data, "$type")
		require.NoError(t, ValidateData(catalog, data, ref, 0),
			"object bodies without a declared $type remain valid")
	})

	t.Run("object ref resolves same-file nested ref", func(t *testing.T) {
		data := map[string]any{
			"$type": "social.coves.community.defs#communityViewDetailed",
			"did":   "did:plc:abc123",
			"name":  "programming",
			"stats": map[string]any{"memberCount": "not-an-integer"},
		}
		require.ErrorContains(t,
			ValidateData(catalog, data, "social.coves.community.defs#communityViewDetailed", 0),
			"expected an integer",
			"same-file #communityStats references must validate their nested values")
	})

	t.Run("object ref resolves cross-file nested ref", func(t *testing.T) {
		data := map[string]any{
			"$type":            "social.coves.community.defs#communityViewDetailed",
			"did":              "did:plc:abc123",
			"name":             "programming",
			"createdByProfile": map[string]any{"did": "not-a-did"},
		}
		require.ErrorContains(t,
			ValidateData(catalog, data, "social.coves.community.defs#communityViewDetailed", 0),
			"DID",
			"cross-file profileView references must validate their nested DID")
	})

	t.Run("unsupported refs return errors", func(t *testing.T) {
		require.Error(t,
			ValidateData(catalog, map[string]any{}, "social.coves.moderation.defs#missing", 0),
			"an unresolvable definition must not be treated as valid data")
		require.Error(t,
			ValidateData(catalog, map[string]any{}, "social.coves.moderation.listBans", 0),
			"a query definition is neither record data nor an object body")
	})

	t.Run("caller map is not mutated", func(t *testing.T) {
		const ref = "social.coves.moderation.defs#banView"
		data := validBanView("2026-09-21T10:00:00Z")
		before := validBanView("2026-09-21T10:00:00Z")

		assert.NoError(t, ValidateData(catalog, data, ref, 0),
			"a valid object is needed to exercise the successful validation path")
		assert.Equal(t, before, data,
			"validation must not remove or rewrite caller data, including $type")
	})

	t.Run("flags are forwarded", func(t *testing.T) {
		const ref = "social.coves.moderation.defs#banView"
		data := validBanView("2026-09-21T10:00:00")

		require.Error(t, ValidateData(catalog, data, ref, 0),
			"a timezone-less datetime must fail strict object validation")
		require.NoError(t, ValidateData(catalog, data, ref, lexicon.AllowLenientDatetime),
			"AllowLenientDatetime must reach nested string validation for object refs")
	})
}
