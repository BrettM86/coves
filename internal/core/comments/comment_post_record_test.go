package comments

import (
	"testing"
	"time"

	"Coves/internal/core/posts"

	"github.com/stretchr/testify/assert"
)

// The post record a comment thread is served with.
//
// getComments hydrates the post it is a thread on and hands the client back
// `postView.record` — the post record verbatim. Its `$type` is what tells a
// consumer which lexicon the rest of the object obeys, and for the two post
// collections those lexicons disagree about the one field that matters:
// the deprecated community-repo record carries an `author`, and the author-repo
// successor deliberately has none, because under §3.1 authorship IS the repo the
// record lives in. Stamping the deprecated NSID on an author-owned post
// therefore does the exact thing PRD §3.0 says a new NSID exists to prevent — it
// tells the reader to derive community = repo DID for a record whose repo is the
// AUTHOR — and hands back a fabricated author field alongside it.
func TestBuildPostRecord_TypeFollowsThePostsCollection(t *testing.T) {
	t.Parallel()

	const (
		authorDID    = "did:plc:recordauthor"
		communityDID = "did:plc:recordcommunity"
	)
	createdAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	post := func(uri string) *posts.Post {
		return &posts.Post{
			URI:          uri,
			AuthorDID:    authorDID,
			CommunityDID: communityDID,
			CreatedAt:    createdAt,
		}
	}
	service := &commentService{}

	t.Run("an author-owned post is labelled postv2 and carries no author", func(t *testing.T) {
		t.Parallel()
		record := service.buildPostRecord(post("at://" + authorDID + "/" + posts.PostV2Collection + "/abc123"))

		assert.Equal(t, posts.PostV2Collection, record["$type"],
			"a postv2 post labelled with the deprecated NSID tells the client to read the authority of its URI as a community, when it is the author")
		assert.NotContains(t, record, "author",
			"the postv2 lexicon has no author field: synthesising one hands the reader back exactly the forgeable claim the flip removed")
		assert.Equal(t, communityDID, record["community"],
			"postv2 keeps community — it is the author's submission target")
	})

	t.Run("a legacy post keeps its own NSID and its author field", func(t *testing.T) {
		t.Parallel()
		record := service.buildPostRecord(post("at://" + communityDID + "/" + posts.LegacyPostCollection + "/abc123"))

		assert.Equal(t, posts.LegacyPostCollection, record["$type"])
		assert.Equal(t, authorDID, record["author"],
			"the deprecated record does carry an author, and it is part of what a client may verify against the community's repo")
	})

	t.Run("a URI naming neither collection falls back to the pre-flip shape", func(t *testing.T) {
		t.Parallel()
		record := service.buildPostRecord(post("not-an-at-uri"))

		assert.Equal(t, posts.LegacyPostCollection, record["$type"])
		assert.Equal(t, authorDID, record["author"])
	})
}
