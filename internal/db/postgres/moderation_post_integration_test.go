//go:build integration

package postgres_test

import (
	"database/sql"
	"fmt"
	"testing"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const moderationPostCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"

func indexedModerationPost(t *testing.T, db *sql.DB, collection, communityDID, authorDID, title, embed string) moderation.StrongRef {
	t.Helper()
	rkey := testkit.TID()
	authority := authorDID
	if collection == moderation.LegacyPostCollection {
		authority = communityDID
	}
	subject := moderation.StrongRef{URI: "at://" + authority + "/" + collection + "/" + rkey, CID: moderationPostCID}
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, embed, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'indexed post content', NULLIF($7, '')::jsonb, NOW())
	`, subject.URI, subject.CID, rkey, authorDID, communityDID, title, embed)
	require.NoError(t, err)
	if collection == moderation.PostV2Collection {
		_, err = db.ExecContext(t.Context(), `
			INSERT INTO community_post_admissions
				(community_did, post_uri, status, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
			VALUES ($1, $2, 'accepted', $3, $3, '3lqqqqqqqqqq2', 1, NOW(), NOW())
		`, communityDID, subject.URI, subject.CID)
		require.NoError(t, err)
	}
	return subject
}

func moderationPostActors(t *testing.T, db *sql.DB) (communityDID, authorDID string) {
	t.Helper()
	authorName := testkit.UniqueIDWithPrefix(t, "postowner")
	authorDID = fixtures.DID(authorName)
	fixtures.User(t, db, authorName+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "postplace")
	var err error
	communityDID, err = fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)
	return communityDID, authorDID
}

func removeIndexedModerationPost(t *testing.T, service moderation.Service, subject moderation.StrongRef) *moderation.MutationResult {
	t.Helper()
	result, err := service.RemoveContent(t.Context(), fixtures.DID("postremovaladmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-post",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err, "an indexed post must be readable inside the moderation transaction")
	require.NotNil(t, result)
	require.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	return result
}

func TestModerationPostRepositoryRemovalPersistence(t *testing.T) {
	for _, scenario := range []struct {
		name, collection, embed string
		softDeleted             bool
		requestedCID            string
		wantCIDs                []string
		communityOwned          bool
	}{
		{name: "postv2 two images", collection: moderation.PostV2Collection, embed: moderationTwoImageEmbed(), wantCIDs: []string{moderationImageCIDOne, moderationImageCIDTwo}},
		{name: "legacy image belongs to community", collection: moderation.LegacyPostCollection, embed: fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"ref":{"$link":"%s"}}}]}`, moderationImageCIDOne), wantCIDs: []string{moderationImageCIDOne}, communityOwned: true},
		{name: "postv2 without media", collection: moderation.PostV2Collection},
		{name: "author deleted postv2 with stale requested CID", collection: moderation.PostV2Collection, softDeleted: true, requestedCID: moderationImageCIDTwo},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := testkit.DB(t)
			communityDID, authorDID := moderationPostActors(t, db)
			subject := indexedModerationPost(t, db, scenario.collection, communityDID, authorDID, "post to moderate", scenario.embed)
			if scenario.softDeleted {
				_, err := db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, subject.URI)
				require.NoError(t, err)
			}
			requested := subject
			if scenario.requestedCID != "" {
				requested.CID = scenario.requestedCID
			}
			service := newPostgresModerationService(db)
			removed := removeIndexedModerationPost(t, service, requested)
			expectedState := moderation.RecordStatePresent
			if scenario.softDeleted {
				expectedState = moderation.RecordStateDeleted
			}
			assert.Equal(t, expectedState, removed.State.RecordState)
			var collection, associatedCommunity, observedCID string
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT subject_collection, subject_community_did, observed_cid
				FROM moderation_actions WHERE id = $1 AND subject_uri = $2
			`, removed.Action.ID, subject.URI).Scan(&collection, &associatedCommunity, &observedCID))
			assert.Equal(t, scenario.collection, collection)
			assert.Equal(t, communityDID, associatedCommunity)
			assert.Equal(t, subject.CID, observedCID, "the indexed CID, even when the deleted subject was requested with a different CID")
			var decisionCount int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_decisions WHERE subject_uri = $1 AND active`, subject.URI).Scan(&decisionCount))
			assert.Equal(t, 1, decisionCount)
			var activeAction string
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT active_action_id FROM moderation_decisions
				WHERE subject_uri = $1 AND authority_did = $2 AND kind = 'removal' AND active
			`, subject.URI, fixtures.InstanceDID()).Scan(&activeAction))
			assert.Equal(t, removed.Action.ID, activeAction)

			rows, err := db.QueryContext(t.Context(), `SELECT owner_did, blob_cid FROM moderation_media_blocks WHERE action_id = $1 AND active`, removed.Action.ID)
			require.NoError(t, err)
			blocks := map[string][]string{}
			for rows.Next() {
				var owner sql.NullString
				var cid string
				require.NoError(t, rows.Scan(&owner, &cid))
				require.True(t, owner.Valid, "spam must not create ownerless media blocks")
				blocks[owner.String] = append(blocks[owner.String], cid)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			owner := authorDID
			if scenario.communityOwned {
				owner = communityDID
			}
			if len(scenario.wantCIDs) == 0 {
				assert.Empty(t, blocks)
			} else {
				require.Len(t, blocks, 1, "only the correct blob owner may be blocked")
				assert.ElementsMatch(t, scenario.wantCIDs, blocks[owner])
			}
			var originalContent, storedCID string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT content, cid FROM posts WHERE uri = $1`, subject.URI).Scan(&originalContent, &storedCID))
			assert.Equal(t, "indexed post content", originalContent, "instance removal is an overlay")
			assert.Equal(t, subject.CID, storedCID)

			state, err := service.GetSubjectState(t.Context(), subject.URI)
			require.NoError(t, err)
			assert.Equal(t, expectedState, state.RecordState)
			assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)
			require.NotNil(t, state.LocalRemoval)
			assert.Equal(t, removed.Action.ID, state.LocalRemoval.ActionID)
		})
	}
}

func TestModerationPostRepositoryNeverIndexed(t *testing.T) {
	db := testkit.DB(t)
	service := newPostgresModerationService(db)
	missing := moderation.StrongRef{URI: "at://" + fixtures.DID("neverindexedpost") + "/" + moderation.PostV2Collection + "/" + testkit.TID(), CID: moderationPostCID}
	result, err := service.RemoveContent(t.Context(), fixtures.DID("postremovaladmin"), moderation.RemoveContentRequest{
		Subject: missing, ExpectedVersion: "v0", IdempotencyKey: "missing-post",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	assert.ErrorIs(t, err, moderation.ErrSubjectNotFound, "a post URI never indexed must not be moderated")
	assert.Nil(t, result)
	for _, table := range []string{"moderation_actions", "moderation_decisions", "moderation_media_blocks", "moderation_idempotency_keys"} {
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count))
		assert.Zero(t, count, table)
	}
}

func TestModerationPostRepositoryReconcilesEditedImagesInTransaction(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "edited moderated post", moderationTwoImageEmbed())
	removed := removeIndexedModerationPost(t, newPostgresModerationService(db), subject)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	digest, err := multihash.Sum([]byte("moderation edited post third image"), multihash.SHA2_256, -1)
	require.NoError(t, err)
	thirdCID := cid.NewCidV1(cid.Raw, digest).String()
	thirdImage := fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"ref":{"$link":"%s"}}},{"image":{"ref":{"$link":"%s"}}},{"image":{"ref":{"$link":"%s"}}}]}`, moderationImageCIDOne, moderationImageCIDTwo, thirdCID)
	_, err = tx.ExecContext(t.Context(), `UPDATE posts SET embed = $1::jsonb WHERE uri = $2`, thirdImage, subject.URI)
	require.NoError(t, err)
	reconciler := moderation.NewMediaReconciler(postgres.NewModerationRepository(db), fixtures.InstanceDID(), nil)
	blocks, err := reconciler.ReconcileTx(t.Context(), tx, subject.URI)
	require.NoError(t, err, "the consumer's post media seam must read the edited post inside its transaction")
	require.Len(t, blocks, 1)
	assert.Equal(t, authorDID, blocks[0].OwnerDID)
	assert.Equal(t, thirdCID, blocks[0].BlobCID)
	require.NoError(t, tx.Commit())
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM moderation_media_blocks
		WHERE action_id = $1 AND owner_did = $2 AND blob_cid = $3 AND active
	`, removed.Action.ID, authorDID, thirdCID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestModerationPostRepositoryRemovalVisibilityAndRestore(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	viewerName := testkit.UniqueIDWithPrefix(t, "postviewer")
	viewerDID := fixtures.DID(viewerName)
	fixtures.User(t, db, viewerName+".test", viewerDID)
	_, err := db.ExecContext(t.Context(), `INSERT INTO community_subscriptions (user_did, community_did, subscribed_at) VALUES ($1, $2, NOW())`, viewerDID, communityDID)
	require.NoError(t, err)
	title := "moderation visibility signal"
	removed := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, title, "")
	control := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, title, "")
	postRepo := postgres.NewPostRepository(db)
	feedRepo := postgres.NewCommunityFeedRepository(db, "moderation-post-visibility-secret")
	discoverRepo := postgres.NewDiscoverRepository(db, "moderation-post-visibility-secret")
	timelineRepo := postgres.NewTimelineRepository(db, "moderation-post-visibility-secret")
	service := newPostgresModerationService(db)

	check := func(stage string, want []string, count int) {
		t.Helper()
		checks := []struct {
			name string
			read func(*testing.T) []string
		}{
			{"post.get anonymous", func(t *testing.T) []string {
				views, err := postRepo.GetViewsByURIs(t.Context(), []string{removed.URI, control.URI}, "")
				require.NoError(t, err)
				return moderationPostViewURIs(views)
			}},
			{"post.get author", func(t *testing.T) []string {
				views, err := postRepo.GetViewsByURIs(t.Context(), []string{removed.URI, control.URI}, authorDID)
				require.NoError(t, err)
				return moderationPostViewURIs(views)
			}},
			{"comment header anonymous", func(t *testing.T) []string { return moderationPostHeaderURI(t, postRepo, removed.URI, control.URI, "") }},
			{"comment header author", func(t *testing.T) []string {
				return moderationPostHeaderURI(t, postRepo, removed.URI, control.URI, authorDID)
			}},
			{"actor.getPosts anonymous", func(t *testing.T) []string {
				views, _, err := postRepo.GetByAuthor(t.Context(), posts.GetAuthorPostsRequest{ActorDID: authorDID, Limit: 50})
				require.NoError(t, err)
				var uris []string
				for _, view := range views {
					uris = append(uris, view.URI)
				}
				return uris
			}},
			{"actor.getPosts author", func(t *testing.T) []string {
				views, _, err := postRepo.GetByAuthor(t.Context(), posts.GetAuthorPostsRequest{ActorDID: authorDID, ViewerDID: authorDID, Limit: 50})
				require.NoError(t, err)
				var uris []string
				for _, view := range views {
					uris = append(uris, view.URI)
				}
				return uris
			}},
			{"timeline subscriber", func(t *testing.T) []string {
				feed, _, err := timelineRepo.GetTimeline(t.Context(), timeline.GetTimelineRequest{UserDID: viewerDID, Sort: "new", Limit: 50})
				require.NoError(t, err)
				var uris []string
				for _, item := range feed {
					uris = append(uris, item.Post.URI)
				}
				return uris
			}},
			{"search matching title", func(t *testing.T) []string {
				feed, _, err := feedRepo.SearchPosts(t.Context(), communityFeeds.SearchPostsRequest{Query: "moderation visibility", Community: communityDID, Sort: "relevance", Timeframe: "all", Limit: 50})
				require.NoError(t, err)
				var uris []string
				for _, item := range feed {
					uris = append(uris, item.Post.URI)
				}
				return uris
			}},
		}
		for _, sort := range []string{"hot", "new", "top"} {
			checks = append(checks, struct {
				name string
				read func(*testing.T) []string
			}{"community feed " + sort, func(t *testing.T) []string {
				feed, _, err := feedRepo.GetCommunityFeed(t.Context(), communityFeeds.GetCommunityFeedRequest{Community: communityDID, Sort: sort, Timeframe: "all", Limit: 50})
				require.NoError(t, err)
				var uris []string
				for _, item := range feed {
					uris = append(uris, item.Post.URI)
				}
				return uris
			}})
		}
		for _, sort := range []string{"new", "top"} {
			checks = append(checks, struct {
				name string
				read func(*testing.T) []string
			}{"discover " + sort, func(t *testing.T) []string {
				feed, _, err := discoverRepo.GetDiscover(t.Context(), discover.GetDiscoverRequest{Sort: sort, Timeframe: "all", Limit: 50})
				require.NoError(t, err)
				var uris []string
				for _, item := range feed {
					uris = append(uris, item.Post.URI)
				}
				return uris
			}})
		}
		for _, check := range checks {
			t.Run(stage+"/"+check.name, func(t *testing.T) {
				assert.ElementsMatch(t, want, check.read(t), "instance removal must hide the post from this read path, including its author")
			})
		}
		t.Run(stage+"/community postCount", func(t *testing.T) {
			community, err := postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).GetByDID(t.Context(), communityDID)
			require.NoError(t, err)
			assert.Equal(t, count, community.PostCount)
		})
		t.Run(stage+"/profile post count", func(t *testing.T) {
			stats, err := postgres.NewUserRepository(db).GetProfileStats(t.Context(), authorDID)
			require.NoError(t, err)
			assert.Equal(t, count, stats.PostCount)
		})
	}
	check("before removal", []string{removed.URI, control.URI}, 2)
	result := removeIndexedModerationPost(t, service, removed)
	check("while removed", []string{control.URI}, 1)
	restored, err := service.RestoreContent(t.Context(), fixtures.DID("postrestoreadmin"), moderation.RestoreContentRequest{
		ActionID: result.Action.ID, ReviewedSubject: &removed, ExpectedVersion: result.State.Version,
		IdempotencyKey: "restore-post", Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	check("after restore", []string{removed.URI, control.URI}, 2)
}

func moderationPostViewURIs(views map[string]*posts.PostView) []string {
	var uris []string
	for uri := range views {
		uris = append(uris, uri)
	}
	return uris
}

func moderationPostHeaderURI(t *testing.T, repo *postgres.PostRepository, removed, control, viewer string) []string {
	t.Helper()
	var uris []string
	for _, uri := range []string{removed, control} {
		view, err := repo.VisibleHeaderView(t.Context(), uri, viewer)
		require.NoError(t, err)
		if view != nil {
			uris = append(uris, view.URI)
		}
	}
	return uris
}
