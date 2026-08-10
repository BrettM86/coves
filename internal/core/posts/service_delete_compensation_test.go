//go:build integration

package posts_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A DELETE MUST COMPENSATE ITSELF, exactly as a create settles itself (§4.2
// steps 4 and 5, §5.3).
//
// The create path does not wait for the firehose: it writes the postv2 into the
// author's repo, seeds the admission row, and — for a community THIS AppView
// hosts — writes the community's acceptance and stamps the row, all before it
// answers the author. settleSubmission is that compensation.
//
// The delete path has no such thing. It deletes the postv2 from the author's
// repo and returns, leaving the local index row, the community's acceptance
// record and the admission row to be cleaned up by the firehose consumer's
// tombstoneAuthorPost/withdrawAcceptance when the delete event comes back
// around.
//
// THE EVENT IS NOT GUARANTEED TO COME BACK. An author's PDS only reaches this
// AppView if it is on one of the configured jetstream feeds; a self-hosted PDS,
// a feed reconfiguration, or a consumer that is simply down for the afternoon
// all produce the same outcome, and it is silent:
//
//   - the post keeps being served from the index, after its author deleted it;
//   - the community's repo keeps a signed acceptance record citing a record
//     nobody can fetch — the CAR that its whole portability argument rests on,
//     permanently asserting content the author withdrew;
//   - the admission row still says `accepted`, so getStatus tells the author
//     their deleted post is live in the community.
//
// This test runs the delete with NO consumer anywhere near it. Everything it
// asserts is the AppView's own account of a post it hosts both halves of, and
// every one of them is reachable synchronously — the same credentials that
// wrote the acceptance a moment ago can withdraw it.
//
// The firehose copy of this deletion is NOT made redundant by any of this: it
// still arrives on every other AppView, and it still arrives here on redelivery.
// Both paths are idempotent by construction (DeleteAcceptance reports "nothing
// to withdraw" as a skip; the admission CAS refuses a rev that does not win), so
// doing the work twice is a no-op — while doing it zero times is the bug above.
func TestService_DeleteWithdrawsTheAcceptanceWithoutWaitingForTheFirehose(t *testing.T) {
	t.Parallel()

	f := newPostFixture(t)
	ctx := context.Background()

	// ── GIVEN ────────────────────────────────────────────────────────────────
	// An accepted post in a community this AppView hosts. The create fast path
	// does all of this for real: a postv2 in the author's repo, an acceptance
	// record in the community's, and an `accepted` admission row.
	const title = "a post its author will withdraw"
	const content = "the body of a post that will be deleted"
	created := f.createPost(t, f.author.DID, title, content)
	require.Equalf(t, posts.PostStatusAccepted, created.Status,
		"fixture: this test is about withdrawing an acceptance, so there has to be one — the fast path "+
			"must have settled %s synchronously", created.URI)

	// And the firehose consumer indexed the CREATE, back when it was running.
	// That is why the post is being served at all, and it is what makes the
	// missing delete-side compensation observable: the row is here, the events
	// that would retract it are not.
	f.indexTheCreate(t, created, title, content)

	acceptanceRkey := posts.SubjectRkey(created.URI)
	community := f.communityAccount(t)

	before := f.admissionOf(t, created.URI)
	require.Equal(t, posts.AdmissionStatusAccepted, before.Status, "fixture: the row must be accepted")
	require.NotNil(t, before.AcceptanceURI, "fixture: an accepted row names its acceptance record")
	require.NotNil(t, before.LastCommunityEvent,
		"fixture: the acceptance write stamped a §5.2 watermark, which the withdrawal has to advance past")
	require.Equalf(t, []string{acceptanceRkey}, listRecordKeys(t, community, posts.AcceptanceCollection),
		"fixture: exactly one acceptance must stand in %s before the author deletes the post", f.community.DID)
	require.NotNilf(t, f.getPost(t, created.URI).Post,
		"fixture: post.get must SERVE this post before the delete, or (d) below proves nothing — a URI "+
			"that was never served would answer notFound whether or not the delete compensated")

	communityHeadBefore := repoHead(t, community)

	// ── WHEN ─────────────────────────────────────────────────────────────────
	// The author deletes their post. No consumer is running: this call is the
	// only thing that happens.
	require.NoError(t, f.service.DeletePost(ctx, sessionFor(t, f.author, f.pds.URL()),
		posts.DeletePostRequest{URI: created.URI}))

	// The half that already works: the author's record is gone from their repo.
	// Asserted so that a failure below cannot be read as "the delete did not
	// happen" — it did, and what follows is what it left behind.
	require.Emptyf(t, listRecordKeys(t, f.author, posts.PostV2Collection),
		"the postv2 record is still standing in %s — the delete itself failed, so nothing after this "+
			"assertion is meaningful", f.author.DID)

	// ── THEN ─────────────────────────────────────────────────────────────────
	// Four facts, each its own subtest so that ONE run names every half of the
	// compensation that is missing rather than stopping at the first.

	t.Run("(a) the local index row is soft-deleted", func(t *testing.T) {
		row := f.rawIndexedRow(t, created.URI)
		require.NotNilf(t, row, "the indexed row of %s vanished entirely; a delete is a SOFT delete — "+
			"the row stays so that comments, votes and the removal path can still resolve the subject",
			created.URI)

		assert.NotNilf(t, row.DeletedAt,
			"LOCAL HALF MISSING: deleteAuthorPost deleted the record from the author's repo and returned "+
				"without soft-deleting the index row, so this AppView is still serving a post its author "+
				"withdrew. The firehose consumer's tombstoneAuthorPost is the only thing that clears it "+
				"today, and it never runs for an author PDS that is not on a configured jetstream feed")
	})

	t.Run("(b) the community's acceptance is withdrawn from its repo", func(t *testing.T) {
		assert.Emptyf(t, listRecordKeys(t, community, posts.AcceptanceCollection),
			"REMOTE HALF MISSING: the acceptance at %s still stands in %s, signed by the community and "+
				"pointing at a postv2 record that no longer exists. This AppView holds the community's "+
				"credentials — it wrote that acceptance moments ago — so withdrawing it is a call it can "+
				"make, not something to leave to a firehose event that may never arrive",
			acceptanceRkey, f.community.DID)

		// AND NOTHING WAS PUBLISHED IN ITS PLACE. A removal record here would be
		// the community declaring, permanently and portably, that it moderated
		// this post — a public accusation about an author who simply deleted
		// their own words. That is why §5.3 gives the withdrawal its own writer
		// instead of spelling it as a removal with a special code.
		assert.Emptyf(t, listRecordKeys(t, community, posts.RemovalCollection),
			"the compensation published a removal record in %s: the AUTHOR deleted this post, and a "+
				"removal record says the COMMUNITY took it down", f.community.DID)
	})

	t.Run("(c) the admission row is pending again, stamped with the withdrawal's rev", func(t *testing.T) {
		after := f.admissionOf(t, created.URI)

		assert.Equalf(t, posts.AdmissionStatusPending, after.Status,
			"STAMP MISSING: the admission of %s still reads %q, so getStatus answers the author that "+
				"their deleted post is live in the community. ApplyAcceptanceDelete moves an accepted (or "+
				"pending_reacceptance) row back to pending — NOT to removed, which would record a "+
				"moderation decision nobody made", created.URI, after.Status)

		assert.Nilf(t, after.AcceptanceURI,
			"the row still names acceptance %v, which is the guard the firehose sweep itself keys off "+
				"(authorpost.go withdrawAcceptance tests the AcceptanceURI, not the status) — a row that "+
				"keeps it is a row that claims a record standing in the community's repo",
			derefOrNil(after.AcceptanceURI))
		assert.Nil(t, after.AcceptedCID,
			"the row still pins a CID for an acceptance that no longer exists")

		// THE WATERMARK IS THE WITHDRAWAL'S OWN REV, and it is what makes the
		// firehose copy of this same deletion a no-op instead of a second
		// decision. Anchored to the community repo's head because the withdrawal
		// is the last thing committed there.
		communityHeadAfter := repoHead(t, community)
		require.NotEqualf(t, communityHeadBefore, communityHeadAfter,
			"the community's repo never committed anything: no withdrawal was written, so there is no "+
				"rev for the row to be stamped with (see (b))")

		require.NotNil(t, after.LastCommunityEvent, "the row carries no community watermark at all")
		assert.Equalf(t, communityHeadAfter, after.LastCommunityEvent.Rev,
			"the row is stamped with rev %q but the withdrawal committed in %q: the stamp must carry the "+
				"rev DeleteAcceptance reported, or the firehose copy of this deletion wins the §5.2 CAS "+
				"and re-applies a decision that has already been made",
			after.LastCommunityEvent.Rev, communityHeadAfter)
		assert.Greaterf(t, after.LastCommunityEvent.Rev, before.LastCommunityEvent.Rev,
			"the watermark did not advance past the acceptance's own rev %q", before.LastCommunityEvent.Rev)
		assert.Equalf(t, posts.CommunityOpDelete, after.LastCommunityEvent.OpRank,
			"a withdrawal is a DELETE within its commit, and the op rank is what orders it against a put "+
				"sharing the same rev")
	})

	t.Run("(d) post.get answers notFound — not the post, and not a tombstone", func(t *testing.T) {
		result := f.getPost(t, created.URI)

		assert.Nilf(t, result.Post,
			"STILL SERVED: post.get returned the full view of %s after its author deleted it. This is the "+
				"user-visible shape of the missing compensation — every permalink, feed hydration and "+
				"cold load keeps rendering a withdrawn post", created.URI)

		// A TOMBSTONE IS A DIFFERENT CLAIM. #removedPost tells the reader the
		// community took this post down and carries the moderator's code; an
		// author deleting their own post is a plain disappearance, and dressing
		// it as a removal attributes a takedown to a community that never made
		// one. removedMarkers gets this right only because it skips a
		// soft-deleted row — which is another way of saying (a) is load-bearing
		// here, and that a compensation which stamped the row `removed` instead
		// of `pending` would fail this assertion.
		assert.Nilf(t, result.Removed,
			"post.get answered a #removedPost tombstone for %s: the AUTHOR deleted this post, so it is "+
				"gone, not moderated — a tombstone here advertises a community takedown that never "+
				"happened", created.URI)

		assert.NotNilf(t, result.NotFound,
			"post.get must answer #notFoundPost for a post its author deleted; got %#v", result)
	})
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// indexTheCreate is the firehose consumer's half of the CREATE, performed by
// hand: the postv2 commit reached this AppView, was indexed, and the post has
// been served from that row ever since.
//
// It is done directly against the repository rather than by running a consumer
// because this test is about what happens when there is NO consumer. The create
// being indexed and the delete not being indexed is not a contradiction — it is
// the ordinary shape of the bug: the author's PDS was on a feed when they
// posted, and by the time they deleted it was not (a feed reconfiguration, a
// migrated repo, a consumer down for an afternoon). The row is the only reason
// the post is visible at all, which is what makes the missing compensation
// observable from outside.
func (f *postFixture) indexTheCreate(t *testing.T, created *posts.CreatePostResponse, title, content string) {
	t.Helper()
	ctx := context.Background()

	// The posts row carries a foreign key to users, so the author has to be
	// indexed before their post can be.
	_, err := postgres.NewUserRepository(f.db).Create(ctx, &users.User{
		DID:    f.author.DID,
		Handle: f.author.Handle,
		PDSURL: f.pds.URL(),
	})
	require.NoErrorf(t, err, "indexing the author %s", f.author.DID)

	require.NoErrorf(t, postgres.NewPostRepository(f.db).Create(ctx, &posts.Post{
		URI:          created.URI,
		CID:          created.CID,
		RKey:         rkeyOf(t, created.URI),
		AuthorDID:    f.author.DID,
		CommunityDID: f.community.DID,
		Title:        stringPtr(title),
		Content:      stringPtr(content),
		CreatedAt:    time.Now().UTC(),
	}), "indexing the created post %s", created.URI)
}

// rawIndexedRow reads the post row the way the removal path does — ungated, so
// a soft-deleted row is still visible to the test that is asserting it IS
// soft-deleted.
func (f *postFixture) rawIndexedRow(t *testing.T, uri string) *posts.Post {
	t.Helper()

	row, err := postgres.NewPostRepository(f.db).GetRawIndexedRow(context.Background(), uri)
	require.NoErrorf(t, err, "reading the indexed row of %s", uri)
	return row
}

// getPost is post.get for one URI, read anonymously — the shape every permalink
// and cold load goes through.
func (f *postFixture) getPost(t *testing.T, uri string) *posts.PostResult {
	t.Helper()

	results, err := f.service.GetPosts(context.Background(), posts.GetPostsRequest{URIs: []string{uri}})
	require.NoErrorf(t, err, "post.get for %s", uri)
	require.Lenf(t, results, 1, "post.get must answer one result per requested URI")
	return results[0]
}

func stringPtr(s string) *string { return &s }

// derefOrNil renders an optional string for a failure message without panicking
// on the nil the assertion is hoping for.
func derefOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
