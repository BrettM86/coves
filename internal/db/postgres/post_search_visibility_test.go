//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const searchVisibilityTerm = "zirconium"

func TestSearchPosts_PublicViewerSeesAcceptedOnly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "srchVisPublic")
	author := "did:plc:srchvispublicauthor"
	createTestUser(t, db, "srchvispublicauthor.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	accepted := seedVisibilityPost(t, db, community, author, "srchvisaccepted", "Zirconium accepted", base.Add(8*time.Hour))
	pending := seedVisibilityPost(t, db, community, author, "srchvispending", "Zirconium pending", base.Add(7*time.Hour))
	rejected := seedVisibilityPost(t, db, community, author, "srchvisrejected", "Zirconium rejected", base.Add(6*time.Hour))
	removed := seedVisibilityPost(t, db, community, author, "srchvisremoved", "Zirconium removed", base.Add(5*time.Hour))
	reaccepting := seedVisibilityPost(t, db, community, author, "srchvisreaccepting", "Zirconium awaiting reacceptance", base.Add(4*time.Hour))
	drifted := seedVisibilityPost(t, db, community, author, "srchvisdrifted", "Zirconium drifted acceptance", base.Add(3*time.Hour))
	unpinned := seedVisibilityPost(t, db, community, author, "srchvisunpinned", "Zirconium unpinned acceptance", base.Add(2*time.Hour))
	deleted := seedVisibilityPost(t, db, community, author, "srchvisdeleted", "Zirconium deleted", base.Add(time.Hour))

	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2srchvisaccepted", "")
	seedVisibilityAdmission(t, db, community, pending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, rejected, posts.AdmissionStatusRejected, "", "spam")
	seedVisibilityAdmission(t, db, community, removed, posts.AdmissionStatusRemoved, "", "rule-violation")
	seedVisibilityAdmission(t, db, community, reaccepting, posts.AdmissionStatusPendingReacceptance, "", "")
	seedVisibilityAdmissionDriftedCID(t, db, community, drifted)
	seedVisibilityAdmissionUnpinned(t, db, community, unpinned)
	seedVisibilityAdmission(t, db, community, deleted, posts.AdmissionStatusAccepted, "bafypostv2srchvisdeleted", "")
	_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, deleted)
	require.NoError(t, err, "soft-deleting the accepted search fixture")

	assert.ElementsMatch(t, []string{accepted}, searchVisibilityURIs(t, db, community, ""),
		"public search must return the one accepted post whose pinned CID still matches and nothing else. A pending, rejected, removed, or pending_reacceptance result is speech the community has not agreed to carry in its current state; a drifted or unpinned acceptance attests to no current content; and a deleted post has been withdrawn")
}

func TestSearchPosts_AuthorSeesOwnPendingPost(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	community := visibilityCommunity(t, db, "srchVisAuthor")
	authorX := "did:plc:srchvisauthorx"
	authorY := "did:plc:srchvisauthory"
	createTestUser(t, db, "srchvisauthorx.test", authorX)
	createTestUser(t, db, "srchvisauthory.test", authorY)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	accepted := seedVisibilityPost(t, db, community, authorX, "srchvisauthoraccepted", "Zirconium accepted", base.Add(3*time.Hour))
	ownedPending := seedVisibilityPost(t, db, community, authorX, "srchvisauthorpending", "Zirconium own pending", base.Add(2*time.Hour))
	otherPending := seedVisibilityPost(t, db, community, authorY, "srchvisotherpending", "Zirconium other pending", base.Add(time.Hour))
	seedVisibilityAdmission(t, db, community, accepted, posts.AdmissionStatusAccepted, "bafypostv2srchvisauthoraccepted", "")
	seedVisibilityAdmission(t, db, community, ownedPending, posts.AdmissionStatusPending, "", "")
	seedVisibilityAdmission(t, db, community, otherPending, posts.AdmissionStatusPending, "", "")

	assert.ElementsMatch(t, []string{accepted, ownedPending}, searchVisibilityURIs(t, db, community, authorX),
		"an author search must include their own pending post so the client can render its review state, but it must not widen visibility for another author's pending post. Returning the other pending URI publishes speech the community has not agreed to carry to a non-author")
}

func TestSearchPosts_BlockedAuthorHiddenFromBlockerOnly(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	community := visibilityCommunity(t, db, "srchVisBlock")
	authorP := "did:plc:srchvisauthorp"
	authorQ := "did:plc:srchvisauthorq"
	viewerV := "did:plc:srchvisviewerv"
	createTestUser(t, db, "srchvisauthorp.test", authorP)
	createTestUser(t, db, "srchvisauthorq.test", authorQ)
	createTestUser(t, db, "srchvisviewerv.test", viewerV)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	postP := seedVisibilityPost(t, db, community, authorP, "srchvisblockvisible", "Zirconium visible author", base.Add(2*time.Hour))
	postQ := seedVisibilityPost(t, db, community, authorQ, "srchvisblockhidden", "Zirconium blocked author", base.Add(time.Hour))
	seedVisibilityAdmission(t, db, community, postP, posts.AdmissionStatusAccepted, "bafypostv2srchvisblockvisible", "")
	seedVisibilityAdmission(t, db, community, postQ, posts.AdmissionStatusAccepted, "bafypostv2srchvisblockhidden", "")
	insertUserBlock(t, db, viewerV, authorQ)

	assert.ElementsMatch(t, []string{postP}, searchVisibilityURIs(t, db, community, viewerV),
		"search must hide the blocked author's accepted post from the blocker while preserving an unblocked author's result; dropping both would make the block assertion pass vacuously")
	assert.ElementsMatch(t, []string{postP, postQ}, searchVisibilityURIs(t, db, community, ""),
		"a user block is a viewer preference, not a public takedown. Hiding the blocked author's accepted post from an unauthenticated search would incorrectly apply one viewer's block to everyone")
}

func TestSearchPosts_CrossCommunitySearchHonoursCommunityBlocks(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	communityA := visibilityCommunity(t, db, "srchVisCommunityBlockA")
	communityB := visibilityCommunity(t, db, "srchVisCommunityBlockB")
	authorA := "did:plc:srchviscommunityblockauthora"
	authorB := "did:plc:srchviscommunityblockauthorb"
	viewerV := "did:plc:srchviscommunityblockviewerv"
	otherViewer := "did:plc:srchviscommunityblockother"
	createTestUser(t, db, "srchviscommunityblockauthora.test", authorA)
	createTestUser(t, db, "srchviscommunityblockauthorb.test", authorB)
	createTestUser(t, db, "srchviscommunityblockviewerv.test", viewerV)
	createTestUser(t, db, "srchviscommunityblockother.test", otherViewer)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	postA := seedVisibilityPost(t, db, communityA, authorA, "srchviscommunityblocka", "Zirconium in blocked community", base.Add(2*time.Hour))
	postB := seedVisibilityPost(t, db, communityB, authorB, "srchviscommunityblockb", "Zirconium in visible community", base.Add(time.Hour))
	seedVisibilityAdmission(t, db, communityA, postA, posts.AdmissionStatusAccepted, "bafypostv2srchviscommunityblocka", "")
	seedVisibilityAdmission(t, db, communityB, postB, posts.AdmissionStatusAccepted, "bafypostv2srchviscommunityblockb", "")
	insertCommunityBlock(t, db, viewerV, communityA)

	assert.ElementsMatch(t, []string{postB}, searchVisibilityURIs(t, db, "", viewerV),
		"cross-community search is an aggregate surface and must mute accepted posts from a community the viewer blocked while preserving matches from unblocked communities; dropping both would make the block assertion pass vacuously")
	assert.ElementsMatch(t, []string{postA}, searchVisibilityURIs(t, db, communityA, viewerV),
		"a community block must not hide results when the viewer explicitly scopes search to that community; an explicit request for the community's own content overrides its aggregate-surface mute")
	assert.ElementsMatch(t, []string{postA, postB}, searchVisibilityURIs(t, db, "", ""),
		"a community block is one viewer's aggregate-feed preference, not a public takedown; anonymous cross-community search must still return both accepted matches")
	assert.ElementsMatch(t, []string{postA, postB}, searchVisibilityURIs(t, db, "", otherViewer),
		"one viewer's community block must not mute that community for a different authenticated viewer searching across communities")
}

func TestSearchPosts_LegacyCommunityPostRowsStayVisible(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	community := visibilityCommunity(t, db, "srchVisLegacy")
	author := "did:plc:srchvislegacyauthor"
	createTestUser(t, db, "srchvislegacyauthor.test", author)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	legacy := seedFilterablePost(t, db, community, author, "srchvislegacy", base.Add(2*time.Hour))
	_, err := db.ExecContext(ctx, `UPDATE posts SET title = $2 WHERE uri = $1`, legacy, "Zirconium legacy post")
	require.NoError(t, err, "giving the legacy post the shared search term")
	seedVisibilityPost(t, db, community, author, "srchvisunadmittedv2", "Zirconium admission-less postv2", base.Add(time.Hour))

	assert.ElementsMatch(t, []string{legacy}, searchVisibilityURIs(t, db, community, ""),
		"a legacy social.coves.community.post row with no admission is accepted by construction and must remain searchable, while an admission-less postv2 means its pending seed failed and must fail closed. Returning the postv2 publishes unadmitted speech; omitting the legacy row retroactively hides pre-admission content")
}

func searchVisibilityURIs(t *testing.T, db *sql.DB, communityDID, viewerDID string) []string {
	t.Helper()

	feed, _, err := NewCommunityFeedRepository(db, "test-secret").SearchPosts(context.Background(), communityFeeds.SearchPostsRequest{
		Query:     searchVisibilityTerm,
		Community: communityDID,
		ViewerDID: viewerDID,
		Sort:      "relevance",
		Timeframe: "all",
		Limit:     50,
	})
	require.NoErrorf(t, err, "searching visible posts as viewer %q", viewerDID)
	return feedURIs(feed)
}
