//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	postModerationCIDOne    = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	postModerationCIDTwo    = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
	postModerationRecordCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
)

type postModerationPurgeCall struct {
	ownerDID, blobCID string
	committed         bool
}

type postModerationPurger struct {
	db    *sql.DB
	calls []postModerationPurgeCall
}

func (p *postModerationPurger) purge(ownerDID, blobCID string) error {
	call := postModerationPurgeCall{ownerDID: ownerDID, blobCID: blobCID}
	err := p.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM moderation_media_blocks
		               WHERE owner_did IS NOT DISTINCT FROM NULLIF($1, '')
		                 AND blob_cid = $2 AND active)
	`, ownerDID, blobCID).Scan(&call.committed)
	p.calls = append(p.calls, call)
	return err
}

func (p *postModerationPurger) PurgeOwnerBlob(ownerDID, blobCID string) error {
	return p.purge(ownerDID, blobCID)
}

func (p *postModerationPurger) PurgeBlob(blobCID string) error {
	return p.purge("", blobCID)
}

func postModerationRecord(imageCIDs ...string) map[string]interface{} {
	record := pv2Record(pv2Community, "shared image post", "same authored body")
	images := make([]interface{}, 0, len(imageCIDs))
	for _, imageCID := range imageCIDs {
		images = append(images, map[string]interface{}{
			"alt": "indexed image",
			"image": map[string]interface{}{
				"$type": "blob", "ref": map[string]interface{}{"$link": imageCID},
				"mimeType": "image/png", "size": 10,
			},
		})
	}
	record["embed"] = map[string]interface{}{"$type": "social.coves.embed.images", "images": images}
	return record
}

type postModerationConsumerFixture struct {
	pv2Fixture
	postRepository posts.Repository
	postService    posts.Service
	moderator      moderation.Service
	purger         *postModerationPurger
	uri, rkey      string
	createdAt      int64
	revs           []string
	create         *JetstreamEvent
}

func newPostModerationConsumerFixture(t *testing.T, cdnPurgers ...moderation.CDNPurger) postModerationConsumerFixture {
	t.Helper()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	postRepository := postgres.NewPostRepository(db)
	moderationRepository := postgres.NewModerationRepository(db)
	purger := &postModerationPurger{db: db}
	var options []moderation.MediaReconcilerOption
	if len(cdnPurgers) > 0 {
		options = append(options, moderation.WithCDNPurger(cdnPurgers[0]))
	}
	f.consumer = NewPostEventConsumer(
		postRepository, postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), f.users, db,
		WithAdmissions(f.admissions), WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostMediaReconciler(moderation.NewMediaReconciler(moderationRepository, fixtures.InstanceDID(), purger, options...)),
	)
	moderator := moderation.NewService(
		moderation.NewRepositorySubjectReader(postRepository, postgres.NewCommentRepository(db)),
		moderationRepository,
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	postService := posts.NewPostService(postRepository, nil, nil, nil, nil, nil, testkit.Endpoints().PDS.BaseURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
		posts.WithSyncAcceptance(f.admissions, nil))
	rkey := testkit.TID()
	uri := pv2URI(pv2Author, rkey)
	revs := increasingTIDs(t, 4)
	createdAt := time.Now().Add(-time.Minute).UnixMicro()
	create := pv2Event(pv2Author, "create", rkey, revs[0], postModerationRecordCID, createdAt,
		postModerationRecord(postModerationCIDOne))
	require.NoError(t, f.consumer.HandleEvent(t.Context(), create))
	indexed, err := postRepository.GetRawIndexedRow(t.Context(), uri)
	require.NoError(t, err)
	require.Equal(t, postModerationRecordCID, indexed.CID)
	acceptanceRkey := testkit.TID()
	accepted, err := f.admissions.ApplyAcceptance(t.Context(), posts.ApplyAcceptanceCommand{
		CommunityDID: pv2Community, PostURI: uri,
		AcceptanceURI:  "at://" + pv2Community + "/" + posts.AcceptanceCollection + "/" + acceptanceRkey,
		AcceptanceRkey: acceptanceRkey, PinnedCID: postModerationRecordCID,
		Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome)
	return postModerationConsumerFixture{
		pv2Fixture: f, postRepository: postRepository, postService: postService,
		moderator: moderator, purger: purger, uri: uri, rkey: rkey,
		createdAt: createdAt, revs: revs, create: create,
	}
}

func TestModerationPostConsumerCDNPurgeAfterEditCommit(t *testing.T) {
	const token = "consumer-cdn-token-SENTINEL"
	const base = "https://img.example.test"
	endpoint := testkit.NewCloudflarePurgeEndpoint(t)
	cdn, err := imageproxy.NewCloudflarePurger(imageproxy.CloudflarePurgerConfig{
		APIBase: endpoint.URL(), ZoneID: "zone-abc", APIToken: token,
		BaseURLs: []string{base}, Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	require.NotNil(t, cdn)
	f := newPostModerationConsumerFixture(t, cdn)
	removed := f.remove(t, "social.coves.moderation.defs#reasonSpam")
	assert.Empty(t, endpoint.Requests(), "the moderator has no CDN purger")
	observed := make(chan struct {
		blocked bool
		err     error
	}, 1)
	endpoint.SetHook(func([]string) {
		blocked, err := postgres.NewModerationRepository(f.db).IsBlocked(t.Context(), pv2Author, postModerationCIDTwo)
		// Non-blocking: an unexpected second request must not wedge the handler.
		select {
		case observed <- struct {
			blocked bool
			err     error
		}{blocked, err}:
		default:
		}
	})
	update := pv2Event(pv2Author, "update", f.rkey, f.revs[1], postModerationCIDOne,
		f.createdAt+1_000_000, postModerationRecord(postModerationCIDOne, postModerationCIDTwo))
	require.NoError(t, f.consumer.HandleEvent(t.Context(), update))
	assert.Equal(t, 1, countRows(t, f.db, `
		SELECT count(*) FROM moderation_media_blocks
		WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active
	`, removed.Action.ID, pv2Author, postModerationCIDTwo))
	requests := endpoint.Requests()
	require.Len(t, requests, 1)
	request := requests[0]
	assert.NoError(t, request.DecodeError)
	assert.Equal(t, "POST", request.Method)
	assert.Equal(t, "/zones/zone-abc/purge_cache", request.Path)
	assert.Equal(t, "Bearer "+token, request.Authorization)
	var expected []string
	for _, preset := range []string{"avatar", "avatar_small", "banner", "content_preview", "content_full", "embed_thumbnail"} {
		expected = append(expected, blobs.HydrateImageProxyURL(base, preset, pv2Author, postModerationCIDTwo))
	}
	assert.ElementsMatch(t, expected, request.Files)
	select {
	case committed := <-observed:
		require.NoError(t, committed.err)
		assert.True(t, committed.blocked, "the endpoint must see the edited image block on a separate connection")
	default:
		t.Fatal("Cloudflare request hook did not check the committed edited image block")
	}
}

func (f postModerationConsumerFixture) remove(t *testing.T, reason string) *moderation.MutationResult {
	t.Helper()
	removed, err := f.moderator.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "postmediaadmin")), moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: f.uri, CID: postModerationRecordCID},
		ExpectedVersion: "v0", IdempotencyKey: "remove-post-media", Reason: reason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	require.NotNil(t, removed.Action)
	var originalBlocks int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM moderation_media_blocks
		WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active
	`, removed.Action.ID, pv2Author, postModerationCIDOne).Scan(&originalBlocks))
	require.Equal(t, 1, originalBlocks, "the original image must be blocked before testing edit reconciliation")
	assert.Empty(t, f.purger.calls, "removal service has no purger; calls must come from the consumer")
	return removed
}

func (f postModerationConsumerFixture) assertReadPaths(t *testing.T, viewerDID string, wantModerated, wantNotFound bool) {
	t.Helper()
	results, err := f.postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{f.uri}, ViewerDID: viewerDID})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if wantModerated {
		require.NotNil(t, results[0].Moderated, "a present post under an active removal must be a moderatedPost")
		assert.Nil(t, results[0].Post)
	}
	if wantNotFound {
		require.NotNil(t, results[0].NotFound, "a deleted post, or one this viewer could not see before the removal, must be notFound")
		assert.Nil(t, results[0].Moderated)
	}
	feed, _, err := postgres.NewCommunityFeedRepository(f.db, "post-moderation-consumer-cursor").GetCommunityFeed(t.Context(),
		communityFeeds.GetCommunityFeedRequest{Community: pv2Community, Sort: "new", Timeframe: "all", Limit: 10})
	require.NoError(t, err)
	for _, item := range feed {
		assert.NotEqual(t, f.uri, item.Post.URI, "removed or deleted post must not appear in the community feed")
	}
	state, err := f.moderator.GetSubjectState(t.Context(), f.uri)
	require.NoError(t, err)
	assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
	require.NotNil(t, state.LocalRemoval)
}

func TestModerationPostConsumerReplayAndDuplicateKeepRemoval(t *testing.T) {
	f := newPostModerationConsumerFixture(t)
	removed := f.remove(t, "social.coves.moderation.defs#reasonSpam")
	for _, delivery := range []string{"replay", "duplicate"} {
		t.Run(delivery, func(t *testing.T) {
			require.NoError(t, f.consumer.HandleEvent(t.Context(), f.create))
			f.assertReadPaths(t, "", true, false)
			state, err := f.moderator.GetSubjectState(t.Context(), f.uri)
			require.NoError(t, err)
			assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)
			assert.Empty(t, f.purger.calls)
		})
	}
}

func TestModerationPostConsumerEditReconcilesNewImages(t *testing.T) {
	for _, scenario := range []struct {
		name, reason   string
		illegalContent bool
	}{
		{name: "spam owner block", reason: "social.coves.moderation.defs#reasonSpam"},
		{name: "illegal content owner-scoped block", reason: "social.coves.moderation.defs#reasonIllegalContent", illegalContent: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newPostModerationConsumerFixture(t)
			removed := f.remove(t, scenario.reason)
			update := pv2Event(pv2Author, "update", f.rkey, f.revs[1], postModerationCIDOne,
				f.createdAt+1_000_000, postModerationRecord(postModerationCIDOne, postModerationCIDTwo))
			require.NoError(t, f.consumer.HandleEvent(t.Context(), update))
			indexed, err := f.postRepository.GetRawIndexedRow(t.Context(), f.uri)
			require.NoError(t, err)
			assert.Equal(t, postModerationCIDOne, indexed.CID, "the edited record must be indexed before media reconciliation")
			require.NotNil(t, indexed.Embed)
			assert.Contains(t, *indexed.Embed, postModerationCIDTwo, "the new image must be present in the indexed embed")
			// The edit moves the admission to pending_reacceptance, which only the
			// author could see before the removal, so the tombstone is the
			// author's; everyone else gets notFound (#moderatedPost never widens
			// access).
			f.assertReadPaths(t, pv2Author, true, false)
			f.assertReadPaths(t, "", false, true)
			state, err := f.moderator.GetSubjectState(t.Context(), f.uri)
			require.NoError(t, err)
			require.NotNil(t, state.CurrentSubject)
			assert.Equal(t, postModerationCIDOne, state.CurrentSubject.CID)
			assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)
			var ownedBlocks int
			require.NoError(t, f.db.QueryRowContext(t.Context(), `
				SELECT count(*) FROM moderation_media_blocks
				WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active
			`, removed.Action.ID, pv2Author, postModerationCIDTwo).Scan(&ownedBlocks))
			assert.Equal(t, 1, ownedBlocks, "the edited image needs an active author-owned block on the original action")
			assert.Contains(t, f.purger.calls, postModerationPurgeCall{ownerDID: pv2Author, blobCID: postModerationCIDTwo, committed: true},
				"the consumer must purge the author-owned image after the block commits")
			if scenario.illegalContent {
				var ownerlessBlocks int
				require.NoError(t, f.db.QueryRowContext(t.Context(), `
					SELECT count(*) FROM moderation_media_blocks
					WHERE action_id = $1 AND owner_did IS NULL AND blob_cid = $2 AND active
				`, removed.Action.ID, postModerationCIDTwo).Scan(&ownerlessBlocks))
				assert.Zero(t, ownerlessBlocks, "an edit must not add an ownerless image block")
				assert.Equal(t, []postModerationPurgeCall{{ownerDID: pv2Author, blobCID: postModerationCIDTwo, committed: true}}, f.purger.calls,
					"only the author's image cache is purged after commit")
				blocked, checkErr := postgres.NewModerationRepository(f.db).IsBlocked(t.Context(), pv2Other, postModerationCIDTwo)
				require.NoError(t, checkErr)
				assert.False(t, blocked, "another owner's new image must remain available")
			}
		})
	}
}

func TestModerationPostConsumerDeleteThenCreateRemainsNotFound(t *testing.T) {
	for _, scenario := range []struct {
		name, reason   string
		illegalContent bool
	}{
		{name: "spam owner block", reason: "social.coves.moderation.defs#reasonSpam"},
		{name: "illegal content owner-scoped block", reason: "social.coves.moderation.defs#reasonIllegalContent", illegalContent: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newPostModerationConsumerFixture(t)
			removed := f.remove(t, scenario.reason)
			require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(pv2Author, "delete", f.rkey, f.revs[1], "", f.createdAt+1_000_000, nil)))
			require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(pv2Author, "create", f.rkey, f.revs[2], postModerationCIDOne,
				f.createdAt+2_000_000, postModerationRecord(postModerationCIDOne, postModerationCIDTwo))))
			indexed, err := f.postRepository.GetRawIndexedRow(t.Context(), f.uri)
			require.NoError(t, err)
			require.NotNil(t, indexed.DeletedAt, "a newer create must not resurrect a soft-deleted postv2")
			f.assertReadPaths(t, "", false, true)
			state, err := f.moderator.GetSubjectState(t.Context(), f.uri)
			require.NoError(t, err)
			assert.Equal(t, moderation.RecordStateDeleted, state.RecordState)
			assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)
			// The recreated record's new image is served from the author's repo
			// even though the AppView keeps the tombstone, so it must be blocked.
			assert.Equal(t, 1, countRows(t, f.db, `
				SELECT count(*) FROM moderation_media_blocks
				WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active
			`, removed.Action.ID, pv2Author, postModerationCIDTwo), "the recreated image needs an active author-owned block on the original action")
			assert.Contains(t, f.purger.calls, postModerationPurgeCall{ownerDID: pv2Author, blobCID: postModerationCIDTwo, committed: true},
				"the consumer must purge the recreated image after the block commits")
			if scenario.illegalContent {
				assert.Zero(t, countRows(t, f.db, `
					SELECT count(*) FROM moderation_media_blocks
					WHERE action_id = $1 AND owner_did IS NULL AND blob_cid = $2 AND active
				`, removed.Action.ID, postModerationCIDTwo), "a recreate must not add an ownerless image block")
				assert.Equal(t, []postModerationPurgeCall{{ownerDID: pv2Author, blobCID: postModerationCIDTwo, committed: true}}, f.purger.calls,
					"only the author's image cache is purged after commit")
				blocked, checkErr := postgres.NewModerationRepository(f.db).IsBlocked(t.Context(), pv2Other, postModerationCIDTwo)
				require.NoError(t, checkErr)
				assert.False(t, blocked, "another owner's recreated image must remain available")
			}
		})
	}
}

// The create path's ON CONFLICT branch discards incoming content when the row
// already exists (a concurrent insert, or the documented active-row re-create).
// The incoming images are still served from the author's repo, so a removed
// post must block them there too.
func TestModerationPostConsumerAlreadyIndexedCreateBlocksIncomingImages(t *testing.T) {
	f := newPostModerationConsumerFixture(t)
	removed := f.remove(t, "social.coves.moderation.defs#reasonIllegalContent")
	embed, err := json.Marshal(postModerationRecord(postModerationCIDTwo)["embed"])
	require.NoError(t, err)
	embedJSON := string(embed)
	applied, err := f.consumer.indexPostIfRevWins(t.Context(), &posts.Post{
		URI: f.uri, CID: postModerationCIDOne, RKey: f.rkey, AuthorDID: pv2Author, CommunityDID: pv2Community,
		Embed: &embedJSON, CreatedAt: time.Now(), IndexedAt: time.Now(),
	}, f.revs[1])
	require.NoError(t, err)
	assert.False(t, applied, "an existing row must keep its content")
	indexed, err := f.postRepository.GetRawIndexedRow(t.Context(), f.uri)
	require.NoError(t, err)
	assert.Equal(t, postModerationRecordCID, indexed.CID)
	assert.Equal(t, 1, countRows(t, f.db, `
		SELECT count(*) FROM moderation_media_blocks
		WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active
	`, removed.Action.ID, pv2Author, postModerationCIDTwo), "the discarded create's image needs an author-owned block")
	assert.Zero(t, countRows(t, f.db, `
		SELECT count(*) FROM moderation_media_blocks
		WHERE action_id = $1 AND owner_did IS NULL AND blob_cid = $2 AND active
	`, removed.Action.ID, postModerationCIDTwo), "a discarded create must not add an ownerless block")
	assert.ElementsMatch(t, []postModerationPurgeCall{
		{ownerDID: pv2Author, blobCID: postModerationCIDTwo, committed: true},
	}, f.purger.calls)
	blocked, checkErr := postgres.NewModerationRepository(f.db).IsBlocked(t.Context(), pv2Other, postModerationCIDTwo)
	require.NoError(t, checkErr)
	assert.False(t, blocked, "another owner's incoming image must remain available")
}

func TestModerationPostConsumerRemovalIsPerURI(t *testing.T) {
	f := newPostModerationConsumerFixture(t)
	f.remove(t, "social.coves.moderation.defs#reasonSpam")
	otherRkey := testkit.TID()
	otherURI := pv2URI(pv2Author, otherRkey)
	require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(pv2Author, "create", otherRkey, f.revs[1],
		postModerationRecordCID, f.createdAt+1_000_000, postModerationRecord(postModerationCIDOne))))
	acceptanceRkey := testkit.TID()
	accepted, err := f.admissions.ApplyAcceptance(t.Context(), posts.ApplyAcceptanceCommand{
		CommunityDID: pv2Community, PostURI: otherURI,
		AcceptanceURI:  "at://" + pv2Community + "/" + posts.AcceptanceCollection + "/" + acceptanceRkey,
		AcceptanceRkey: acceptanceRkey, PinnedCID: postModerationRecordCID,
		Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome)
	results, err := f.postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{f.uri, otherURI}})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.NotNil(t, results[0].Moderated)
	require.NotNil(t, results[1].Post, "a distinct accepted URI must remain a postView")
	assert.Equal(t, otherURI, results[1].Post.URI)
}

func TestModerationPostConsumerUnremovedEditCreatesNoBlocks(t *testing.T) {
	f := newPostModerationConsumerFixture(t)
	require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(pv2Author, "update", f.rkey, f.revs[1], postModerationCIDOne,
		f.createdAt+1_000_000, postModerationRecord(postModerationCIDOne, postModerationCIDTwo))))
	indexed, err := f.postRepository.GetRawIndexedRow(t.Context(), f.uri)
	require.NoError(t, err)
	assert.Equal(t, postModerationCIDOne, indexed.CID)
	assert.Equal(t, 0, countRows(t, f.db, `SELECT count(*) FROM moderation_media_blocks WHERE blob_cid = $1`, postModerationCIDTwo))
	assert.Empty(t, f.purger.calls)
}

// An out-of-order update that predates the delete is stale by rev; the rev gate
// must reject it before any media is blocked, or an older record's images would
// be blocked for a post whose newest state is the tombstone.
func TestModerationPostConsumerStaleUpdateAfterDeleteAddsNoBlocks(t *testing.T) {
	f := newPostModerationConsumerFixture(t)
	f.remove(t, "social.coves.moderation.defs#reasonIllegalContent")
	require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(pv2Author, "delete", f.rkey, f.revs[2], "", f.createdAt+2_000_000, nil)))
	require.NoError(t, f.consumer.HandleEvent(t.Context(), pv2Event(pv2Author, "update", f.rkey, f.revs[1], postModerationCIDOne,
		f.createdAt+1_000_000, postModerationRecord(postModerationCIDOne, postModerationCIDTwo))))
	assert.Equal(t, 0, countRows(t, f.db, `SELECT count(*) FROM moderation_media_blocks WHERE blob_cid = $1`, postModerationCIDTwo),
		"a stale update must not block its images")
	assert.Empty(t, f.purger.calls)
}
