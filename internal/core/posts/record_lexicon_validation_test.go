//go:build integration

package posts_test

import (
	"testing"
	"time"

	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A real repo accepts the post records a link-carrying post produces.
//
// # WHAT IS ACTUALLY BEING PROVEN
//
// The AppView builds a post record and hands it to a PDS, and the PDS is the
// first — and only — component that inspects the bytes before they become a
// commit. Everything upstream of it, including every test that stops at the
// service's return value, will happily accept a record the PDS would refuse.
// The failure this guards against is a real one: an embed assembled with an
// empty strongRef CID, produced when a URL was recognised but the record it
// pointed at was never fetched. Nothing in the AppView objects to an empty
// string; the repo does.
//
// So these tests write records to a REAL repository on the local test PDS and
// assert the write succeeds. The two shapes are the two a post containing a
// link can take: the URL left in the content as text, and a
// social.coves.embed.external describing it. A bsky.app URL is used for the
// first because that is the case the empty-CID bug came from — a Bluesky link
// is the one kind the service can try to resolve into a strongRef.
//
// # WHY THE RECORD GOES INTO THE AUTHOR'S REPO
//
// Posts really live in the COMMUNITY's repo, and where a post belongs is
// covered by service_writeforward_test.go. It is irrelevant here: what is under
// test is whether a PDS will store these bytes at all, and the answer does not
// depend on whose repo they land in. Writing as the author costs one account
// instead of a provisioned community.
//
// Nothing in this test reads the AppView index, so nothing seeds it — the
// community field only has to be DID-shaped for the record to be well formed.
func TestPostRecord_LinkCarryingPostsAreAcceptedByTheRepo(t *testing.T) {
	t.Parallel()

	author := testkit.NewPDS(t).CreateAccount(t, testkit.WithHandlePrefix("bsky"))
	communityDID := fixtures.DID("bskycrosspost")

	t.Run("A Bluesky URL left in the content", func(t *testing.T) {
		record := map[string]interface{}{
			"$type":     postCollection,
			"community": communityDID,
			"author":    author.DID,
			"title":     "Post with Bluesky Link",
			"content":   "Check out this Bluesky post: https://bsky.app/profile/jay.bsky.team/post/3l7bsovn5rz2n",
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		}

		written := author.CreateRecord(t, postCollection, record)
		require.NotEmpty(t, written.URI)
		require.NotEmpty(t, written.CID)

		// Read it back: a 200 from createRecord means the write was accepted,
		// and this means the commit is in the repo and the content survived it
		// as text rather than being rewritten into something else.
		stored := author.GetRecord(t, postCollection, written.RKey)
		assert.Equal(t, record["content"], stored.Value["content"])
	})

	t.Run("An external embed", func(t *testing.T) {
		written := author.CreateRecord(t, postCollection, map[string]interface{}{
			"$type":     postCollection,
			"community": communityDID,
			"author":    author.DID,
			"title":     "Post with External Link",
			"content":   "Check out this article",
			"embed": map[string]interface{}{
				"$type": "social.coves.embed.external",
				"external": map[string]interface{}{
					"uri":         "https://example.com/article",
					"title":       "Example Article",
					"description": "An interesting article about testing",
				},
			},
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		})

		require.NotEmpty(t, written.URI)
		require.NotEmpty(t, written.CID)
	})
}
