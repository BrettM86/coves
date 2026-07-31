//go:build integration

// Embeds are the part of a post record the AppView writes into somebody else's
// repository, so a malformed one is not a rendering bug — it is a permanently
// invalid record on a PDS that no later fix can retract. The create path
// therefore validates the embed union before it writes anything: the $type
// discriminator must be present and known, and an external embed's thumb must
// be a real blob reference rather than the URL string clients keep sending.
//
// These cases run against the real post and community services rather than a
// fake, because the regression they guard against is not "validateEmbed is
// wrong" — that has unit tests — but "validateEmbed is no longer called".
// A validation call moved behind an early return, or reordered after the
// record is assembled, keeps every unit test green and silently starts writing
// the corrupt records the validation was added to prevent. Only the wired path
// can catch that.
//
// The file is in the external test package because it imports
// internal/db/postgres and Coves/tests/fixtures, both of which pull in the
// domain; the established form for every relocated integration test in this
// tree is package foo_test.
package post_test

import (
	"context"
	"net/http"
	"testing"

	"Coves/internal/core/communities"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedCommunityWithCredentials inserts a community that looks provisioned:
// it carries the PDS credentials the create path reads before it attempts a
// write.
//
// The credentials are inert — the access token is a syntactically valid JWT
// with a far-future expiry and nothing on the other end honours it — which is
// exactly what these tests want. They need the request to travel past
// "community not found" and past "credentials expired" so that it reaches embed
// validation; whether the eventual PDS write would succeed is the pipeline
// tier's question, not this file's.
func seedCommunityWithCredentials(t *testing.T, repo communities.Repository, pdsURL string) *communities.Community {
	t.Helper()

	id := testkit.UniqueID(t)
	community, err := repo.Create(context.Background(), &communities.Community{
		DID:         fixtures.DID("community" + id),
		Name:        "embedtest-" + id,
		Handle:      "c-embedtest-" + id + ".coves.local",
		Description: "Community used to reach embed validation on the create path",
		Visibility:  "public",
		PDSEmail:    "c-embedtest-" + id + "@coves.local",
		PDSPassword: "inert-test-password",
		// Header and payload of an unsigned JWT whose "exp" is in the year
		// 2286, so nothing short-circuits on an expired token.
		PDSAccessToken:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkaWQ6cGxjOnRlc3Rjb21tdW5pdHkiLCJleHAiOjk5OTk5OTk5OTl9.test",
		PDSRefreshToken: "inert-refresh-token",
		PDSURL:          pdsURL,
	})
	require.NoError(t, err, "seeding a community with PDS credentials")
	return community
}

// TestPostCreate_ExternalEmbedThumb covers the thumb field of
// social.coves.embed.external, which must be an atProto blob reference.
//
// Clients repeatedly send a URL string here because that is what the rendered
// post looks like, and accepting one would write a record no other atProto
// implementation can read. Each rejection is asserted on its message, not just
// its status, because the message is what tells a client which of the blob's
// four required parts it left out.
func TestPostCreate_ExternalEmbedThumb(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	stack := newCreateStack(t, db)

	author := fixtures.User(t, db, "thumbtest.bsky.social", fixtures.DID("thumb"+testkit.UniqueID(t)))
	community := seedCommunityWithCredentials(t, stack.communityRepo, stack.communityPDSURL)

	// postWithThumb sends an external embed carrying thumb (or none, for a nil
	// thumb) and reports the rejection message, if the request was rejected at
	// all.
	postWithThumb := func(t *testing.T, thumb any) (message any, rejected bool) {
		t.Helper()

		external := map[string]any{"uri": "https://streamable.com/test"}
		if thumb != nil {
			external["thumb"] = thumb
		}
		rec := createPost(t, stack, author.DID, map[string]any{
			"community": community.DID,
			"title":     "Test Post",
			"content":   "Test content",
			"embed": map[string]any{
				"$type":    "social.coves.embed.external",
				"external": external,
			},
		})
		if rec.Code != http.StatusBadRequest {
			return nil, false
		}
		return decodeXRPCError(t, rec)["message"], true
	}

	t.Run("a URL string is not a blob", func(t *testing.T) {
		message, rejected := postWithThumb(t, "https://example.com/thumb.jpg")
		require.True(t, rejected, "a URL-string thumb must be rejected with 400")
		assert.Contains(t, message, "thumb must be a blob reference")
		assert.Contains(t, message, "not URL string")
	})

	t.Run("a blob without $type is rejected", func(t *testing.T) {
		message, rejected := postWithThumb(t, map[string]any{
			"ref":      map[string]any{"$link": "bafyrei123"},
			"mimeType": "image/jpeg",
			"size":     12345,
		})
		require.True(t, rejected, "a thumb missing $type must be rejected with 400")
		assert.Contains(t, message, "thumb must have $type: blob")
	})

	t.Run("a blob without ref is rejected", func(t *testing.T) {
		message, rejected := postWithThumb(t, map[string]any{
			"$type":    "blob",
			"mimeType": "image/jpeg",
			"size":     12345,
		})
		require.True(t, rejected, "a thumb missing ref must be rejected with 400")
		assert.Contains(t, message, "thumb blob missing required 'ref' field")
	})

	t.Run("a blob without mimeType is rejected", func(t *testing.T) {
		message, rejected := postWithThumb(t, map[string]any{
			"$type": "blob",
			"ref":   map[string]any{"$link": "bafyrei123"},
			"size":  12345,
		})
		require.True(t, rejected, "a thumb missing mimeType must be rejected with 400")
		assert.Contains(t, message, "thumb blob missing required 'mimeType' field")
	})

	t.Run("a well-formed blob passes validation", func(t *testing.T) {
		// The write itself still fails — the credentials seeded above are inert
		// and the CID names no blob anybody uploaded — so this asserts the
		// negative: whatever goes wrong afterwards, it is not thumb validation.
		// Without it, the four cases above would also pass if validation
		// rejected every thumb it ever saw.
		message, rejected := postWithThumb(t, map[string]any{
			"$type":    "blob",
			"ref":      map[string]any{"$link": "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"},
			"mimeType": "image/jpeg",
			"size":     52813,
		})
		if rejected {
			assert.NotContains(t, message, "thumb must be")
			assert.NotContains(t, message, "thumb blob missing")
		}
	})

	t.Run("an absent thumb passes validation", func(t *testing.T) {
		// The common case: the client sends a bare link and the unfurl service
		// fills the thumbnail in later, so a missing thumb is legal.
		message, rejected := postWithThumb(t, nil)
		if rejected {
			assert.NotContains(t, message, "thumb must be")
		}
	})
}

// TestPostCreate_EmbedUnionDiscriminator covers the $type discriminator that
// decides which member of the embed union a record claims to be.
func TestPostCreate_EmbedUnionDiscriminator(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	stack := newCreateStack(t, db)

	author := fixtures.User(t, db, "embedtest.bsky.social", fixtures.DID("embed"+testkit.UniqueID(t)))
	community := seedCommunityWithCredentials(t, stack.communityRepo, stack.communityPDSURL)

	postEmbed := func(t *testing.T, embed any) map[string]any {
		t.Helper()

		rec := createPost(t, stack, author.DID, map[string]any{
			"community": community.DID,
			"title":     "Test Post",
			"embed":     embed,
		})
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"an undiscriminated embed must be refused, got %d: %s", rec.Code, rec.Body.String())
		return decodeXRPCError(t, rec)
	}

	t.Run("an embed without $type is rejected", func(t *testing.T) {
		// The exact shape the frontend was sending when link posts were being
		// written as unreadable records: a bare {uri} with no discriminator and
		// no external wrapper.
		assert.Contains(t, postEmbed(t, map[string]any{"uri": "https://example.com"})["message"], "$type")
	})

	t.Run("an embed with an unknown $type is rejected", func(t *testing.T) {
		// A typo in the NSID must not fall through to "no embed": the record
		// would be written without the content the author attached.
		envelope := postEmbed(t, map[string]any{
			"$type":    "social.coves.embed.externl",
			"external": map[string]any{"uri": "https://example.com"},
		})
		assert.Contains(t, envelope["message"], "unknown embed")
	})
}
