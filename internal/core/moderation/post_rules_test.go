package moderation_test

import (
	"context"
	"database/sql"
	"testing"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	postRulesAuthorDID    = "did:plc:postauthor"
	postRulesCommunityDID = "did:plc:postcommunity"
	postRulesCID          = "bafyreipostversion"
	postRulesURI          = "at://did:plc:postauthor/social.coves.community.postv2/3kabc"
	legacyPostRulesURI    = "at://did:plc:postcommunity/social.coves.community.post/3kabc"
)

func newPostRulesScenario(uri, ownerDID string, blobCIDs []string) removeRulesScenario {
	scenario := newRemoveRulesScenario()
	scenario.store.state.indexedPosts[uri] = moderation.IndexedPost{
		URI: uri, CID: postRulesCID, OwnerDID: ownerDID,
		CommunityDID: postRulesCommunityDID, BlobCIDs: blobCIDs,
	}
	scenario.reader.record = &moderation.IndexedRecord{URI: uri, CID: postRulesCID}
	scenario.request.Subject = moderation.StrongRef{URI: uri, CID: postRulesCID}
	return scenario
}

func TestRemovePostContentRecordsCollectionAndBlocksMedia(t *testing.T) {
	for _, test := range []struct {
		name, uri, collection, ownerDID, reason string
		blobCIDs                                []string
		wantBlocks                              []moderation.MediaBlock
		wantOwnerPurges                         []removeRulesOwnerPurge
		wantBlobPurges                          []string
	}{
		{
			name: "author-owned postv2", uri: postRulesURI, collection: moderation.PostV2Collection,
			ownerDID: postRulesAuthorDID, reason: removeRulesSpam,
			blobCIDs: []string{removeRulesFirstImage, removeRulesSecondImage},
			wantBlocks: []moderation.MediaBlock{
				{OwnerDID: postRulesAuthorDID, BlobCID: removeRulesFirstImage},
				{OwnerDID: postRulesAuthorDID, BlobCID: removeRulesSecondImage},
			},
			wantOwnerPurges: []removeRulesOwnerPurge{
				{postRulesAuthorDID, removeRulesFirstImage}, {postRulesAuthorDID, removeRulesSecondImage},
			},
		},
		{
			name: "community-owned legacy post", uri: legacyPostRulesURI, collection: moderation.LegacyPostCollection,
			ownerDID: postRulesCommunityDID, reason: removeRulesSpam,
			blobCIDs: []string{removeRulesFirstImage, removeRulesSecondImage},
			wantBlocks: []moderation.MediaBlock{
				{OwnerDID: postRulesCommunityDID, BlobCID: removeRulesFirstImage},
				{OwnerDID: postRulesCommunityDID, BlobCID: removeRulesSecondImage},
			},
			wantOwnerPurges: []removeRulesOwnerPurge{
				{postRulesCommunityDID, removeRulesFirstImage}, {postRulesCommunityDID, removeRulesSecondImage},
			},
		},
		{
			name: "illegal content also blocks every owner", uri: postRulesURI, collection: moderation.PostV2Collection,
			ownerDID: postRulesAuthorDID, reason: removeRulesIllegal,
			blobCIDs: []string{removeRulesFirstImage, removeRulesSecondImage},
			wantBlocks: []moderation.MediaBlock{
				{OwnerDID: postRulesAuthorDID, BlobCID: removeRulesFirstImage},
				{BlobCID: removeRulesFirstImage},
				{OwnerDID: postRulesAuthorDID, BlobCID: removeRulesSecondImage},
				{BlobCID: removeRulesSecondImage},
			},
			wantOwnerPurges: []removeRulesOwnerPurge{
				{postRulesAuthorDID, removeRulesFirstImage}, {postRulesAuthorDID, removeRulesSecondImage},
			},
			wantBlobPurges: []string{removeRulesFirstImage, removeRulesSecondImage},
		},
		{
			name: "post with no blobs", uri: postRulesURI, collection: moderation.PostV2Collection,
			ownerDID: postRulesAuthorDID, reason: removeRulesSpam,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newPostRulesScenario(test.uri, test.ownerDID, test.blobCIDs)
			scenario.request.Reason = test.reason
			scenario.purger.onPurge = func() {
				require.NotEmpty(t, scenario.store.state.activeRemovals, "purge must follow the committed decision")
				require.NotEmpty(t, scenario.store.state.mediaBlocks, "purge must follow committed block insertion")
			}
			result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, test.collection, result.Action.SubjectCollection)
			assert.Equal(t, postRulesCommunityDID, result.Action.SubjectCommunityDID)
			assert.Equal(t, *result.Action, scenario.store.state.actions[result.Action.ID])
			assert.Equal(t, result.Action.ID, scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, test.uri}])
			wantBlocks := make(map[moderation.MediaBlock]bool)
			for _, block := range test.wantBlocks {
				block.ActionID = result.Action.ID
				wantBlocks[block] = true
			}
			assert.Equal(t, wantBlocks, scenario.store.state.mediaBlocks)
			assert.Equal(t, test.wantOwnerPurges, scenario.purger.ownerPurges)
			assert.Equal(t, test.wantBlobPurges, scenario.purger.blobPurges)
			assert.Equal(t, len(test.blobCIDs) > 0, containsModerationWrite(scenario.store.writeCalls, "InsertMediaBlocks"))
		})
	}
}

func containsModerationWrite(writes []string, wanted string) bool {
	for _, write := range writes {
		if write == wanted {
			return true
		}
	}
	return false
}

func TestRemovePostContentChecksIndexedRecord(t *testing.T) {
	for _, test := range []struct {
		name, requestedCID string
		deleted, missing   bool
		wantError          error
	}{
		{name: "author-deleted post ignores stale strongRef", requestedCID: "bafyreistalepostversion", deleted: true},
		{name: "present post refuses stale CID", requestedCID: "bafyreistalepostversion", wantError: moderation.ErrContentChanged},
		{name: "never-indexed post", requestedCID: postRulesCID, missing: true, wantError: moderation.ErrSubjectNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newPostRulesScenario(postRulesURI, postRulesAuthorDID, nil)
			scenario.request.Subject.CID = test.requestedCID
			post := scenario.store.state.indexedPosts[postRulesURI]
			post.AuthorDeleted = test.deleted
			scenario.store.state.indexedPosts[postRulesURI] = post
			scenario.reader.record.Deleted = test.deleted
			if test.missing {
				delete(scenario.store.state.indexedPosts, postRulesURI)
				scenario.reader.record = nil
				scenario.reader.err = moderation.ErrSubjectNotIndexed
			}
			result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
			if test.wantError != nil {
				require.ErrorIs(t, err, test.wantError)
				assert.Nil(t, result)
				assertRemoveRulesNoWrites(t, scenario)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			assert.Equal(t, postRulesCID, result.Action.ObservedCID)
			assert.Equal(t, moderation.RecordStateDeleted, result.State.RecordState)
			assert.Nil(t, result.State.CurrentSubject)
		})
	}
}

func TestRestorePostContentReviewsCurrentPostAndDeactivatesBlocks(t *testing.T) {
	for _, test := range []struct {
		name      string
		noReview  bool
		deleted   bool
		staleCID  bool
		wantError error
	}{
		{name: "present post requires review", noReview: true, wantError: moderation.ErrInvalidRequest},
		{name: "present post refuses stale review", staleCID: true, wantError: moderation.ErrContentChanged},
		{name: "present post accepts current review"},
		{name: "author-deleted post needs no review", deleted: true, noReview: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newPostRulesScenario(postRulesURI, postRulesAuthorDID, []string{removeRulesFirstImage})
			post := scenario.store.state.indexedPosts[postRulesURI]
			post.AuthorDeleted = test.deleted
			scenario.store.state.indexedPosts[postRulesURI] = post
			scenario.reader.record.Deleted = test.deleted
			removal := moderation.Action{
				ID: "post-removal", Action: moderation.ActionRemove, ActorDID: removeRulesAdminDID,
				AuthorityDID: removeRulesInstanceDID, ScopeKind: moderation.ScopeInstance,
				SubjectURI: postRulesURI, SubjectCollection: moderation.PostV2Collection,
				SubjectCommunityDID: postRulesCommunityDID, ObservedCID: postRulesCID,
			}
			scenario.store.state.actions[removal.ID] = removal
			scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, postRulesURI}] = removal.ID
			scenario.store.state.versions[postRulesURI] = 1
			block := moderation.MediaBlock{OwnerDID: postRulesAuthorDID, BlobCID: removeRulesFirstImage, ActionID: removal.ID}
			scenario.store.state.mediaBlocks[block] = true
			request := moderation.RestoreContentRequest{
				ActionID: removal.ID, ExpectedVersion: "v1", IdempotencyKey: "restore-post",
				Reason: removeRulesSpam, ReviewedSubject: &moderation.StrongRef{URI: postRulesURI, CID: postRulesCID},
			}
			if test.noReview {
				request.ReviewedSubject = nil
			}
			if test.staleCID {
				request.ReviewedSubject.CID = "bafyreistalepostversion"
			}
			before := scenario.store.state.copy()
			result, err := scenario.service.RestoreContent(t.Context(), restoreRulesAdminDID, request)
			if test.wantError != nil {
				require.ErrorIs(t, err, test.wantError)
				assert.Nil(t, result)
				assert.Equal(t, before, scenario.store.state)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, moderation.ActionRestore, result.Action.Action)
			assert.Equal(t, removal.ID, result.Action.ReversesActionID)
			assert.Equal(t, moderation.PostV2Collection, result.Action.SubjectCollection)
			assert.Equal(t, postRulesCommunityDID, result.Action.SubjectCommunityDID)
			assert.Equal(t, "v2", result.State.Version)
			assert.Equal(t, moderation.ModerationStateClear, result.State.Moderation.State)
			assert.Empty(t, scenario.store.state.activeRemovals)
			assert.False(t, scenario.store.state.mediaBlocks[block], "restore deactivates its removal's blob block")
			if test.deleted {
				assert.Equal(t, moderation.RecordStateDeleted, result.State.RecordState)
				assert.Nil(t, result.State.CurrentSubject)
			} else {
				assert.Equal(t, moderation.RecordStatePresent, result.State.RecordState)
				assert.Equal(t, &moderation.StrongRef{URI: postRulesURI, CID: postRulesCID}, result.State.CurrentSubject)
			}
		})
	}
}

type postRulesMediaTransaction struct {
	post   moderation.IndexedPost
	action moderation.Action
	calls  []string
	blocks []moderation.MediaBlock
}

func (transaction *postRulesMediaTransaction) ActiveRemoval(context.Context, string, string) (*moderation.Action, error) {
	transaction.calls = append(transaction.calls, "ActiveRemoval")
	return &transaction.action, nil
}

func (transaction *postRulesMediaTransaction) ReadIndexedComment(context.Context, string) (*moderation.IndexedComment, error) {
	transaction.calls = append(transaction.calls, "ReadIndexedComment")
	return &moderation.IndexedComment{}, nil
}

func (transaction *postRulesMediaTransaction) ReadIndexedPost(context.Context, string) (*moderation.IndexedPost, error) {
	transaction.calls = append(transaction.calls, "ReadIndexedPost")
	return &transaction.post, nil
}

func (transaction *postRulesMediaTransaction) InsertNewMediaBlocks(_ context.Context, blocks []moderation.MediaBlock) ([]moderation.MediaBlock, error) {
	transaction.calls = append(transaction.calls, "InsertNewMediaBlocks")
	transaction.blocks = append(transaction.blocks, blocks...)
	return blocks, nil
}

type postRulesMediaBinder struct{ bound *postRulesMediaTransaction }

func (binder postRulesMediaBinder) BindTransaction(*sql.Tx) moderation.MediaTransaction {
	return binder.bound
}

func TestReconcileRemovedPostMediaReadsIndexedPost(t *testing.T) {
	bound := &postRulesMediaTransaction{
		post: moderation.IndexedPost{
			URI: postRulesURI, CID: postRulesCID, OwnerDID: postRulesAuthorDID,
			BlobCIDs: []string{removeRulesFirstImage, removeRulesSecondImage},
		},
		action: moderation.Action{ID: "post-removal", Reason: removeRulesSpam},
	}
	reconciler := moderation.NewMediaReconciler(postRulesMediaBinder{bound}, removeRulesInstanceDID, nil)
	blocks, err := reconciler.ReconcileTx(t.Context(), nil, postRulesURI)
	require.Equal(t, []string{"ActiveRemoval", "ReadIndexedPost", "InsertNewMediaBlocks"}, bound.calls)
	require.NoError(t, err)
	want := []moderation.MediaBlock{
		{OwnerDID: postRulesAuthorDID, BlobCID: removeRulesFirstImage, ActionID: bound.action.ID},
		{OwnerDID: postRulesAuthorDID, BlobCID: removeRulesSecondImage, ActionID: bound.action.ID},
	}
	assert.Equal(t, want, bound.blocks)
	assert.Equal(t, want, blocks)
}

var _ moderation.MediaTransaction = (*postRulesMediaTransaction)(nil)
