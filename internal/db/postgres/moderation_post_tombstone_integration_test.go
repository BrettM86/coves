//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func moderationPostGetService(db *sql.DB, repo posts.Repository) posts.Service {
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo, testkit.Endpoints().PDS.BaseURL, fixtures.InstanceDID(), "", nil, nil, nil,
		communities.PrivateHostOptions(true)...,
	)
	return posts.NewPostService(repo, communityService, nil, nil, nil, nil, testkit.Endpoints().PDS.BaseURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
		posts.WithSyncAcceptance(postgres.NewAdmissionRepository(db), nil),
	)
}

func moderationPostTombstoneActors(t *testing.T, db *sql.DB) (communityDID, authorDID string) {
	t.Helper()
	communityDID, _ = moderationPostActors(t, db)
	authorDID = "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"
	fixtures.User(t, db, "posttombstoneauthor.test", authorDID)
	return communityDID, authorDID
}

func restoreModerationPost(t *testing.T, service moderation.Service, subject moderation.StrongRef, removed *moderation.MutationResult) {
	t.Helper()
	restored, err := service.RestoreContent(t.Context(), fixtures.DID("postrestoreadmin"), moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &subject, ExpectedVersion: removed.State.Version,
		IdempotencyKey: "restore-post", Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
}

func TestModerationPostActiveRemovalsByURIs(t *testing.T) {
	db := testkit.DB(t)
	repo := postgres.NewPostRepository(db)
	communityDID, authorDID := moderationPostActors(t, db)
	active := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "active decision", "")
	restored := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "restored decision", "")
	control := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "never removed", "")
	labelled := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "labelled only", "")
	service := newPostgresModerationService(db)
	removeIndexedModerationPost(t, service, active)
	labelPost(t, service, fixtures.DID("postlabeladmin"), labelled, "v0", "label-only-post")
	previous, err := service.RemoveContent(t.Context(), fixtures.DID("postremovaladminb"), moderation.RemoveContentRequest{
		Subject: restored, ExpectedVersion: "v0", IdempotencyKey: "remove-restored-post",
		Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.NotNil(t, previous)
	require.NotNil(t, previous.Action)
	restoreModerationPost(t, service, restored, previous)

	got, err := repo.ActiveRemovalsByURIs(t.Context(), []string{control.URI, active.URI, restored.URI, labelled.URI, active.URI})
	require.NoError(t, err)
	assert.NotContains(t, got, labelled.URI, "an active label is not a removal")
	assert.Equal(t, map[string][]posts.RemovalSource{
		active.URI: {{AuthorityDID: fixtures.InstanceDID(), ScopeKind: "instance"}},
	}, got, "only active decisions may appear, even in a batch with restored and unremoved URIs")
	empty, err := repo.ActiveRemovalsByURIs(t.Context(), nil)
	require.NoError(t, err)
	assert.NotNil(t, empty, "an empty lookup must return an empty map")
	assert.Empty(t, empty)
}

func TestModerationPostGetTombstone(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		communityRemoved bool
		deleted          bool
	}{
		{name: "accepted post"},
		{name: "instance removal overrides community removal", communityRemoved: true},
		{name: "author deletion overrides instance removal", deleted: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := testkit.DB(t)
			communityDID, authorDID := moderationPostTombstoneActors(t, db)
			subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "private after removal", "")
			if scenario.communityRemoved {
				_, err := db.ExecContext(t.Context(), `
					UPDATE community_post_admissions SET status = 'removed', accepted_cid = NULL,
						decision_code = 'rule-violation', decision_at = NOW(), updated_at = NOW()
					WHERE community_did = $1 AND post_uri = $2
				`, communityDID, subject.URI)
				require.NoError(t, err)
			}
			if scenario.deleted {
				_, err := db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, subject.URI)
				require.NoError(t, err)
			}
			moderationService := newPostgresModerationService(db)
			postService := moderationPostGetService(db, postgres.NewPostRepository(db))
			if scenario.communityRemoved {
				before, err := postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}})
				require.NoError(t, err)
				require.Len(t, before, 1)
				require.NotNil(t, before[0].Removed, "the community-removal fixture must serve #removedPost before instance removal")
			}
			removed := removeIndexedModerationPost(t, moderationService, subject)
			for _, viewer := range []struct{ name, did string }{{"anonymous", ""}, {"author", authorDID}} {
				t.Run(viewer.name, func(t *testing.T) {
					results, err := postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}, ViewerDID: viewer.did})
					require.NoError(t, err)
					require.Len(t, results, 1)
					if scenario.deleted {
						require.NotNil(t, results[0].NotFound, "author deletion takes precedence over instance moderation")
						assert.Nil(t, results[0].Moderated)
						return
					}
					require.NotNil(t, results[0].Moderated, "instance removal must serve #moderatedPost instead of #notFoundPost or #removedPost")
					assert.Nil(t, results[0].Post)
					assert.Nil(t, results[0].Removed)
					assert.Nil(t, results[0].NotFound)
					assert.Nil(t, results[0].Blocked)
					member, ok := results[0].Member()
					assert.True(t, ok)
					assert.Same(t, results[0].Moderated, member)
					tombstone := results[0].Moderated
					assert.Equal(t, subject.URI, tombstone.URI)
					assert.Equal(t, authorDID, tombstone.AuthorDID)
					require.NotNil(t, tombstone.Moderation)
					assert.Equal(t, "removed", tombstone.Moderation.State)
					require.Len(t, tombstone.Moderation.Sources, 1)
					assert.Equal(t, fixtures.InstanceDID(), tombstone.Moderation.Sources[0].AuthorityDID)
					assert.Equal(t, "instance", tombstone.Moderation.Sources[0].Scope.Kind)
					require.NotNil(t, tombstone.Community)
					assert.Equal(t, communityDID, tombstone.Community.DID)
					var name, handle string
					require.NoError(t, db.QueryRowContext(t.Context(), `SELECT name, handle FROM communities WHERE did = $1`, communityDID).Scan(&name, &handle))
					assert.Equal(t, name, tombstone.Community.Name)
					assert.Equal(t, handle, tombstone.Community.Handle)
					assert.Nil(t, tombstone.Community.Avatar)
					assert.Nil(t, tombstone.Community.Origin)
					assert.Empty(t, tombstone.Community.PDSURL)
				})
			}
			if scenario.communityRemoved {
				restoreModerationPost(t, moderationService, subject, removed)
				after, err := postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}})
				require.NoError(t, err)
				require.Len(t, after, 1)
				require.NotNil(t, after[0].Removed, "restoring the instance removal must reveal the community's #removedPost again")
				assert.Equal(t, "rule-violation", after[0].Removed.Code)
				assert.Nil(t, after[0].Moderated)
			}
		})
	}
}

// A label is a decision row too, but never a removal: a community-removed post
// carrying only an NSFW label stays the community's #removedPost.
func TestModerationPostGetCommunityRemovedLabelledPost(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostTombstoneActors(t, db)
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "community removed, labelled", "")
	_, err := db.ExecContext(t.Context(), `
		UPDATE community_post_admissions SET status = 'removed', accepted_cid = NULL,
			decision_code = 'rule-violation', decision_at = NOW(), updated_at = NOW()
		WHERE community_did = $1 AND post_uri = $2
	`, communityDID, subject.URI)
	require.NoError(t, err)
	labelPost(t, newPostgresModerationService(db), fixtures.DID("tombstonelabeladmin"), subject, "v0", "community-removed-label")
	results, err := moderationPostGetService(db, postgres.NewPostRepository(db)).GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NotNil(t, results[0].Removed, "with no instance removal, the community's #removedPost must stand")
	assert.Equal(t, "rule-violation", results[0].Removed.Code)
	assert.Nil(t, results[0].Moderated)
}

func TestModerationPostGetBatchPreservesOrder(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostTombstoneActors(t, db)
	uris := make([]string, 25)
	var subject moderation.StrongRef
	for i := range uris {
		post := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, fmt.Sprintf("post %d", i), "")
		uris[i] = post.URI
		if i == 12 {
			subject = post
		}
	}
	removeIndexedModerationPost(t, newPostgresModerationService(db), subject)
	results, err := moderationPostGetService(db, postgres.NewPostRepository(db)).GetPosts(t.Context(), posts.GetPostsRequest{URIs: uris})
	require.NoError(t, err)
	require.Len(t, results, len(uris))
	for i, result := range results {
		if i == 12 {
			require.NotNil(t, result.Moderated, "the removed post must occupy its original slot in the 25-URI batch")
			assert.Equal(t, uris[i], result.Moderated.URI)
		} else {
			require.NotNil(t, result.Post, "the remaining 24 posts must still be #postView at index %d", i)
			assert.Equal(t, uris[i], result.Post.URI)
		}
	}
}

type failingPostRemovalRepository struct {
	*postgres.PostRepository
	err error
}

func (r failingPostRemovalRepository) ActiveRemovalsByURIs(context.Context, []string) (map[string][]posts.RemovalSource, error) {
	return nil, r.err
}

func TestModerationPostGetRemovalLookupFailure(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostTombstoneActors(t, db)
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "removed subject", "")
	removeIndexedModerationPost(t, newPostgresModerationService(db), subject)
	lookupFailure := errors.New("removal lookup failed")
	repo := failingPostRemovalRepository{PostRepository: postgres.NewPostRepository(db), err: lookupFailure}
	results, err := moderationPostGetService(db, repo).GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}})
	assert.ErrorIs(t, err, lookupFailure, "a failed instance-removal lookup must fail post.get, never return #notFoundPost")
	assert.Nil(t, results)
}

// An instance removal must not widen access: a post the viewer could not see
// before the removal (pending, rejected, or a postv2 its community never
// admitted) stays #notFoundPost for that viewer afterwards, instead of becoming
// a #moderatedPost that discloses its author and ties it to the community it
// names. The author, who could see it before, gets the tombstone.
func TestModerationPostGetTombstoneDoesNotWidenAccess(t *testing.T) {
	for _, scenario := range []struct {
		name, admissionSQL string
	}{
		{name: "pending postv2", admissionSQL: `
			UPDATE community_post_admissions SET status = 'pending', accepted_cid = NULL, updated_at = NOW()
			WHERE community_did = $1 AND post_uri = $2`},
		{name: "rejected postv2", admissionSQL: `
			UPDATE community_post_admissions SET status = 'rejected', accepted_cid = NULL,
				decision_code = 'off-topic', decision_at = NOW(), updated_at = NOW()
			WHERE community_did = $1 AND post_uri = $2`},
		{name: "unadmitted postv2 naming the community", admissionSQL: `
			DELETE FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := testkit.DB(t)
			communityDID, authorDID := moderationPostTombstoneActors(t, db)
			otherViewerDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "otherviewer"))
			subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "never public", "")
			_, err := db.ExecContext(t.Context(), scenario.admissionSQL, communityDID, subject.URI)
			require.NoError(t, err)
			postService := moderationPostGetService(db, postgres.NewPostRepository(db))
			before, err := postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}})
			require.NoError(t, err)
			require.Len(t, before, 1)
			require.NotNil(t, before[0].NotFound, "fixture: the post must be #notFoundPost to the public before the removal")

			removeIndexedModerationPost(t, newPostgresModerationService(db), subject)

			for _, viewer := range []struct{ name, did string }{{"anonymous", ""}, {"other viewer", otherViewerDID}} {
				t.Run(viewer.name, func(t *testing.T) {
					results, err := postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}, ViewerDID: viewer.did})
					require.NoError(t, err)
					require.Len(t, results, 1)
					assert.Nil(t, results[0].Moderated, "an instance removal must not disclose a post this viewer could not see before it")
					require.NotNil(t, results[0].NotFound)
					assert.Equal(t, subject.URI, results[0].NotFound.URI)
				})
			}
			t.Run("author", func(t *testing.T) {
				results, err := postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{subject.URI}, ViewerDID: authorDID})
				require.NoError(t, err)
				require.Len(t, results, 1)
				require.NotNil(t, results[0].Moderated, "the author could see their own post before the removal, so they get the tombstone")
				assert.Equal(t, subject.URI, results[0].Moderated.URI)
			})
		})
	}
}
