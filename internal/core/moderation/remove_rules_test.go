package moderation_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	removeRulesAuthorDID    = "did:plc:commentauthor"
	removeRulesAdminDID     = "did:plc:firstadmin"
	removeRulesInstanceDID  = "did:web:moderation.test"
	removeRulesCommunityDID = "did:plc:community"
	removeRulesURI          = "at://did:plc:commentauthor/social.coves.community.comment/3kabc"
	removeRulesCID          = "bafyreicommentversion"
	removeRulesFirstImage   = "bafyreifirstimage"
	removeRulesSecondImage  = "bafyreisecondimage"
	removeRulesSpam         = "social.coves.moderation.defs#reasonSpam"
	removeRulesIllegal      = "social.coves.moderation.defs#reasonIllegalContent"
)

type removeRulesOwnerPurge struct {
	ownerDID string
	blobCID  string
}

type removeRulesPurger struct {
	ownerPurges []removeRulesOwnerPurge
	blobPurges  []string
	onPurge     func()
}

func (purger *removeRulesPurger) PurgeOwnerBlob(ownerDID, blobCID string) error {
	if purger.onPurge != nil {
		purger.onPurge()
	}
	purger.ownerPurges = append(purger.ownerPurges, removeRulesOwnerPurge{ownerDID, blobCID})
	return nil
}

func (purger *removeRulesPurger) PurgeBlob(blobCID string) error {
	if purger.onPurge != nil {
		purger.onPurge()
	}
	purger.blobPurges = append(purger.blobPurges, blobCID)
	return nil
}

type removeRulesScenario struct {
	store   *inMemoryModerationStore
	reader  *fakeSubjectReader
	purger  *removeRulesPurger
	service moderation.Service
	request moderation.RemoveContentRequest
}

func newRemoveRulesScenario() removeRulesScenario {
	clock := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	store := newInMemoryModerationStore(clock)
	store.state.indexedComments[removeRulesURI] = moderation.IndexedComment{
		URI: removeRulesURI, CID: removeRulesCID, OwnerDID: removeRulesAuthorDID,
		CommunityDID: removeRulesCommunityDID, ImageCIDs: []string{removeRulesFirstImage, removeRulesSecondImage},
	}
	reader := &fakeSubjectReader{record: &moderation.IndexedRecord{URI: removeRulesURI, CID: removeRulesCID}}
	purger := &removeRulesPurger{}
	service := moderation.NewService(reader, store, moderation.Config{
		InstanceDID: removeRulesInstanceDID, IdempotencyRetention: 24 * time.Hour,
		MaxLiveIdempotencyKeys: 1000, Now: func() time.Time { return clock }, Purger: purger,
	})
	return removeRulesScenario{
		store: store, reader: reader, purger: purger, service: service,
		request: moderation.RemoveContentRequest{
			Subject:         moderation.StrongRef{URI: removeRulesURI, CID: removeRulesCID},
			ExpectedVersion: "v0", IdempotencyKey: "first-removal", Reason: removeRulesSpam, PrivateNote: "n",
		},
	}
}

func (scenario *removeRulesScenario) withCDNPurger(store moderation.Store, cdn moderation.CDNPurger) {
	scenario.service = moderation.NewService(scenario.reader, store, moderation.Config{
		InstanceDID: removeRulesInstanceDID, IdempotencyRetention: 24 * time.Hour,
		MaxLiveIdempotencyKeys: 1000, Now: func() time.Time { return scenario.store.now },
		Purger: scenario.purger, CDNPurger: cdn,
	})
}

func assertRemoveRulesNoWrites(t *testing.T, scenario removeRulesScenario) {
	t.Helper()
	assert.Empty(t, scenario.store.writeCalls)
	assert.Empty(t, scenario.store.state.actions)
	assert.Empty(t, scenario.store.state.activeRemovals)
	assert.Empty(t, scenario.store.state.versions)
	assert.Empty(t, scenario.store.state.mediaBlocks)
	assert.Empty(t, scenario.store.state.idempotency)
	assert.Empty(t, scenario.purger.ownerPurges)
	assert.Empty(t, scenario.purger.blobPurges)
}

func TestRemoveContentRules(t *testing.T) {
	t.Run("applied removal records action, decision, version, two owned blocks and purges", func(t *testing.T) {
		scenario := newRemoveRulesScenario()
		result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, moderation.OutcomeApplied, result.Outcome)
		require.NotNil(t, result.Action)
		action := result.Action
		require.NotEmpty(t, action.ID)
		assert.Equal(t, scenario.store.now, action.CreatedAt)
		assert.Equal(t, moderation.ActionRemove, action.Action)
		assert.Equal(t, removeRulesAdminDID, action.ActorDID)
		assert.Equal(t, removeRulesInstanceDID, action.AuthorityDID)
		assert.Equal(t, moderation.ScopeInstance, action.ScopeKind)
		assert.Equal(t, removeRulesURI, action.SubjectURI)
		assert.Equal(t, moderation.CommentCollection, action.SubjectCollection)
		assert.Equal(t, removeRulesCommunityDID, action.SubjectCommunityDID)
		assert.Equal(t, removeRulesCID, action.ObservedCID)
		assert.Equal(t, removeRulesSpam, action.Reason)
		assert.Equal(t, "n", action.PrivateNote)
		assert.Equal(t, moderation.OriginLocal, action.Origin)
		assert.NotEqual(t, "v0", result.State.Version)
		assert.Equal(t, moderation.ModerationStateRemoved, result.State.Moderation.State)
		assert.Equal(t, &moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: action.ID}, result.State.LocalRemoval)

		require.Len(t, scenario.store.state.actions, 1)
		assert.Equal(t, *action, scenario.store.state.actions[action.ID])
		require.Len(t, scenario.store.state.activeRemovals, 1)
		assert.Equal(t, action.ID, scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, removeRulesURI}])
		assert.Equal(t, []string{"InsertAction", "SetRemovalDecision", "SetSubjectVersion", "InsertMediaBlocks", "SaveIdempotencyRecord"}, scenario.store.writeCalls)
		stored, err := scenario.store.SubjectModeration(t.Context(), removeRulesInstanceDID, removeRulesURI)
		require.NoError(t, err)
		assert.Equal(t, "v"+strconv.FormatInt(stored.Version, 10), result.State.Version)
		assert.Equal(t, action.ID, stored.ActiveRemoval.ID)
		assert.Equal(t, map[moderation.MediaBlock]bool{
			{OwnerDID: removeRulesAuthorDID, BlobCID: removeRulesFirstImage, ActionID: action.ID}:  true,
			{OwnerDID: removeRulesAuthorDID, BlobCID: removeRulesSecondImage, ActionID: action.ID}: true,
		}, scenario.store.state.mediaBlocks)
		assert.Equal(t, []removeRulesOwnerPurge{
			{removeRulesAuthorDID, removeRulesFirstImage}, {removeRulesAuthorDID, removeRulesSecondImage},
		}, scenario.purger.ownerPurges)
		assert.Empty(t, scenario.purger.blobPurges)

		following, err := scenario.service.GetSubjectState(t.Context(), removeRulesURI)
		require.NoError(t, err)
		require.NotNil(t, following)
		assert.Equal(t, result.State.Version, following.Version, "the next state read must reflect the committed version")
		assert.Equal(t, moderation.ModerationStateRemoved, following.Moderation.State)
		assert.Equal(t, result.State.LocalRemoval, following.LocalRemoval)
	})

	t.Run("illegal content adds one owner-less block and purge per image", func(t *testing.T) {
		scenario := newRemoveRulesScenario()
		scenario.request.Reason = removeRulesIllegal
		result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Action)
		assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
		assert.Equal(t, map[moderation.MediaBlock]bool{
			{OwnerDID: removeRulesAuthorDID, BlobCID: removeRulesFirstImage, ActionID: result.Action.ID}:  true,
			{OwnerDID: removeRulesAuthorDID, BlobCID: removeRulesSecondImage, ActionID: result.Action.ID}: true,
			{BlobCID: removeRulesFirstImage, ActionID: result.Action.ID}:                                  true,
			{BlobCID: removeRulesSecondImage, ActionID: result.Action.ID}:                                 true,
		}, scenario.store.state.mediaBlocks)
		assert.Equal(t, []removeRulesOwnerPurge{
			{removeRulesAuthorDID, removeRulesFirstImage}, {removeRulesAuthorDID, removeRulesSecondImage},
		}, scenario.purger.ownerPurges)
		assert.Equal(t, []string{removeRulesFirstImage, removeRulesSecondImage}, scenario.purger.blobPurges)
	})

	for _, test := range []struct {
		name   string
		change func(*removeRulesScenario)
		want   error
	}{
		{name: "stale version v7", change: func(s *removeRulesScenario) { s.request.ExpectedVersion = "v7" }, want: moderation.ErrStateConflict},
		{name: "unparseable version banana", change: func(s *removeRulesScenario) { s.request.ExpectedVersion = "banana" }, want: moderation.ErrStateConflict},
		{name: "changed indexed CID", change: func(s *removeRulesScenario) { s.request.Subject.CID = "bafyreistalecommentcid" }, want: moderation.ErrContentChanged},
		{name: "never indexed subject", change: func(s *removeRulesScenario) {
			delete(s.store.state.indexedComments, removeRulesURI)
			s.reader.record = nil
			s.reader.err = moderation.ErrSubjectNotIndexed
		}, want: moderation.ErrSubjectNotFound},
	} {
		t.Run(test.name+" changes nothing and never purges", func(t *testing.T) {
			scenario := newRemoveRulesScenario()
			test.change(&scenario)
			result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
			require.ErrorIs(t, err, test.want)
			assert.Nil(t, result)
			assertRemoveRulesNoWrites(t, scenario)
		})
	}

	t.Run("author-deleted comment removes despite changed strongRef CID", func(t *testing.T) {
		scenario := newRemoveRulesScenario()
		comment := scenario.store.state.indexedComments[removeRulesURI]
		comment.AuthorDeleted = true
		scenario.store.state.indexedComments[removeRulesURI] = comment
		scenario.reader.record.Deleted = true
		scenario.request.Subject.CID = "bafyreidifferentcomment"
		result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, moderation.OutcomeApplied, result.Outcome)
		require.NotNil(t, result.Action)
		assert.Equal(t, removeRulesCID, result.Action.ObservedCID)
		assert.Equal(t, moderation.RecordStateDeleted, result.State.RecordState)
		assert.Nil(t, result.State.CurrentSubject)
		require.Len(t, scenario.store.state.actions, 1)
	})

	t.Run("redundant removal by another admin keeps action and version", func(t *testing.T) {
		scenario := newRemoveRulesScenario()
		prior := moderation.Action{
			ID: "earlier-removal", Action: moderation.ActionRemove, AuthorityDID: removeRulesInstanceDID,
			SubjectURI: removeRulesURI, SubjectCollection: moderation.CommentCollection,
		}
		scenario.store.state.actions[prior.ID] = prior
		scenario.store.state.activeRemovals[inMemoryModerationDecisionKey{removeRulesInstanceDID, removeRulesURI}] = prior.ID
		scenario.store.state.versions[removeRulesURI] = 1
		scenario.request.ExpectedVersion = "v1"
		result, err := scenario.service.RemoveContent(t.Context(), "did:plc:secondadmin", scenario.request)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, moderation.OutcomeUnchanged, result.Outcome)
		assert.Nil(t, result.Action)
		assert.Equal(t, "v1", result.State.Version)
		assert.Equal(t, moderation.ModerationStateRemoved, result.State.Moderation.State)
		assert.Equal(t, &moderation.ActionRef{ServiceDID: removeRulesInstanceDID, ActionID: prior.ID}, result.State.LocalRemoval)
		assert.Equal(t, map[string]moderation.Action{prior.ID: prior}, scenario.store.state.actions)
		assert.Equal(t, map[string]int64{removeRulesURI: 1}, scenario.store.state.versions)
		assert.Equal(t, []string{"SaveIdempotencyRecord"}, scenario.store.writeCalls)
		assert.Empty(t, scenario.store.state.mediaBlocks)
		assert.Empty(t, scenario.purger.ownerPurges)
		assert.Empty(t, scenario.purger.blobPurges)
	})

	for _, test := range []struct {
		name      string
		inject    func(*inMemoryModerationStore, error)
		wantCalls []string
	}{
		{name: "InsertAction failure rolls back", inject: func(store *inMemoryModerationStore, err error) {
			store.failInsertAction = err
		}, wantCalls: []string{"InsertAction"}},
		{name: "SetRemovalDecision failure rolls back inserted action", inject: func(store *inMemoryModerationStore, err error) {
			store.failSetRemovalDecision = err
		}, wantCalls: []string{"InsertAction", "SetRemovalDecision"}},
	} {
		t.Run(test.name+" and does not purge", func(t *testing.T) {
			scenario := newRemoveRulesScenario()
			storageError := errors.New("injected store failure")
			test.inject(scenario.store, storageError)
			result, err := scenario.service.RemoveContent(t.Context(), removeRulesAdminDID, scenario.request)
			require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			assert.ErrorIs(t, err, storageError)
			assert.Nil(t, result)
			assert.Equal(t, test.wantCalls, scenario.store.writeCalls)
			assert.Empty(t, scenario.store.state.actions)
			assert.Empty(t, scenario.store.state.activeRemovals)
			assert.Empty(t, scenario.store.state.versions)
			assert.Empty(t, scenario.store.state.idempotency)
			assert.Empty(t, scenario.store.state.mediaBlocks)
			assert.Empty(t, scenario.purger.ownerPurges)
			assert.Empty(t, scenario.purger.blobPurges)
		})
	}
}
