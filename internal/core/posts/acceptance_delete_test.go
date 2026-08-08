//go:build integration

package posts_test

import (
	"context"
	"testing"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DeleteAcceptance: the writer's fifth method, and the one the author's own
// deletion needs (docs/PRD_AUTHOR_OWNED_POSTS.md §5.3).
//
// # WHY THE OTHER FOUR CANNOT DO THIS
//
// When an author tombstones their post, the community's acceptance is left
// pointing at a record nobody can fetch. The community repo is the curated index
// its whole portability argument rests on, so leaving the acceptance standing
// means the CAR permanently cites content the author withdrew, and any peer
// replaying it shows a post that no longer exists.
//
// WriteRemoval would clear it — and would also publish a signed moderation
// event, with a reason code, that never happened. That record is portable and
// permanent: a peer reading the community's repo would see the community
// declaring it removed this post, when in fact the author deleted it. The
// distinction is the difference between an accurate public record and a
// defamatory one, which is why this is its own method rather than a removal with
// a special code.
//
// # AGAINST A REAL PDS, FOR ONE REASON
//
// The PDS answers a delete of a missing record with a 500 (task 4's locked
// decisions: the writers are STATE-SHAPED because the PDS refuses the wrong
// shape). A fake would happily accept the delete and this file would prove
// nothing about the case that actually happens — a redelivered tombstone event
// meeting an acceptance an earlier pass already withdrew.

func TestDeleteAcceptance_WithdrawsAStandingAcceptance(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx := context.Background()

	post := f.publishPost(t, "a post its author will delete")
	f.seedPending(t, post.URI, post.CID)
	outcome, err := f.process(t, post.URI)
	require.NoError(t, err)
	require.Equal(t, posts.EngineAccepted, outcome, "fixture: the acceptance must stand before it can be withdrawn")

	rkey := posts.SubjectRkey(post.URI)
	standing := f.acceptanceOf(t, post.URI)
	require.NotEmpty(t, standing.CID, "fixture: the acceptance record must exist")

	result, err := f.writer.DeleteAcceptance(ctx, posts.CommunityAcceptanceDeleteCommand{
		CommunityDID: f.community.DID,
		PostURI:      post.URI,
	})
	require.NoError(t, err)
	assert.False(t, result.Skipped, "an acceptance that was actually there is a delete, not a skip")
	assert.NotEmptyf(t, result.Rev,
		"the delete must report the rev it committed in: that is the §5.2 watermark the firehose copy of this same deletion will be compared against, "+
			"and without it the AppView cannot stamp its own event")

	f.assertRecordAbsent(t, posts.AcceptanceCollection, rkey, "the withdrawn acceptance")

	// And NOTHING was published in its place. A removal record here would put a
	// moderation act on the public firehose that no moderator performed.
	f.assertRecordAbsent(t, posts.RemovalCollection, rkey,
		"a removal record — the author deleted their own post, which is not the community removing it")
}

func TestDeleteAcceptance_IsASkipWhenThereIsNothingToWithdraw(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx := context.Background()

	post := f.publishPost(t, "a post that was never accepted")

	// No acceptance was ever written. This is the ordinary case, not an edge
	// one: the sweep runs on every tombstone event, and most posts a community
	// sees were never accepted by it — plus every tombstone event is redelivered
	// at least once by the connector's cursor rewind.
	result, err := f.writer.DeleteAcceptance(ctx, posts.CommunityAcceptanceDeleteCommand{
		CommunityDID: f.community.DID,
		PostURI:      post.URI,
	})

	require.NoError(t, err,
		"deleting an acceptance that is not there must be a no-op, not an error: the PDS answers a delete of a missing record with a 500, "+
			"so an unshaped delete would dead-letter every redelivered tombstone")
	assert.True(t, result.Skipped, "nothing was written, and the result must say so")
}

func TestDeleteAcceptance_IsIdempotentUnderRedelivery(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)
	ctx := context.Background()

	post := f.publishPost(t, "a post whose tombstone arrives twice")
	f.seedPending(t, post.URI, post.CID)
	_, err := f.process(t, post.URI)
	require.NoError(t, err)

	cmd := posts.CommunityAcceptanceDeleteCommand{CommunityDID: f.community.DID, PostURI: post.URI}

	first, err := f.writer.DeleteAcceptance(ctx, cmd)
	require.NoError(t, err)
	require.False(t, first.Skipped)

	// The connector rewinds its cursor five seconds after every reconnect, so
	// the identical tombstone commit is guaranteed to be redelivered.
	second, err := f.writer.DeleteAcceptance(ctx, cmd)
	require.NoError(t, err, "a redelivered tombstone must not fail the sweep")
	assert.True(t, second.Skipped, "the second attempt found nothing to withdraw")

	f.assertRecordAbsent(t, posts.AcceptanceCollection, posts.SubjectRkey(post.URI), "the withdrawn acceptance")
}

func TestDeleteAcceptance_RefusesASubjectItCannotKey(t *testing.T) {
	t.Parallel()

	f := newEngineFixture(t)

	// The rkey is derived from the subject URI, so an empty or malformed subject
	// hashes to a perfectly valid-looking key pointing at nothing — and a delete
	// aimed at the wrong rkey in a community's own repo is a write, not a read.
	// Every other writer validates its command; this one must too.
	for _, badURI := range []string{"", "not-an-at-uri", "at://"} {
		_, err := f.writer.DeleteAcceptance(context.Background(), posts.CommunityAcceptanceDeleteCommand{
			CommunityDID: f.community.DID,
			PostURI:      badURI,
		})
		assert.Errorf(t, err, "a subject URI of %q must be refused rather than hashed into a key", badURI)
	}

	_, err := f.writer.DeleteAcceptance(context.Background(), posts.CommunityAcceptanceDeleteCommand{
		CommunityDID: "",
		PostURI:      "at://" + testkit.UniqueID(t) + "/social.coves.community.postv2/x",
	})
	assert.Error(t, err, "a delete with no community names no repo to delete from")
}
