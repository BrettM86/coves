package moderation_test

import (
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCommunityRemovalService is the account-ban service shape; communities
// overrides Config.Communities, and nil leaves it unset.
func newCommunityRemovalService(store *inMemoryModerationStore, communities moderation.CommunityChecker) moderation.Service {
	return moderation.NewService(&fakeSubjectReader{}, store, moderation.Config{
		InstanceDID: accountBanInstanceDID, IdempotencyRetention: 24 * time.Hour,
		MaxLiveIdempotencyKeys: 1000, Now: accountBanClock, Communities: communities,
	})
}

func TestCommunityRemovalRefusals(t *testing.T) {
	indexedCommunities := accountBanCommunities{dids: map[string]bool{accountBanCommunityDID: true}}
	for _, test := range []struct {
		name        string
		restore     bool
		subject     string
		reason      string
		communities moderation.CommunityChecker
		want        error
	}{
		{
			name: "remove: a user DID is not a community", subject: accountBanAccountDID, reason: accountBanSpam,
			communities: indexedCommunities, want: moderation.ErrInvalidSubject,
		},
		{
			name: "restore: a user DID is not a community", restore: true, subject: accountBanAccountDID, reason: accountBanSpam,
			communities: indexedCommunities, want: moderation.ErrInvalidSubject,
		},
		{
			name: "remove: malformed DID", subject: "not-a-did", reason: accountBanSpam,
			communities: indexedCommunities, want: moderation.ErrInvalidRequest,
		},
		{
			name: "restore: malformed DID", restore: true, subject: "not-a-did", reason: accountBanSpam,
			communities: indexedCommunities, want: moderation.ErrInvalidRequest,
		},
		{
			name: "remove: AT-URI rather than a DID", subject: "at://did:plc:hostedcommunity", reason: accountBanSpam,
			communities: indexedCommunities, want: moderation.ErrInvalidRequest,
		},
		{
			name: "remove: community lookup not configured", subject: accountBanCommunityDID, reason: accountBanSpam,
			want: moderation.ErrModerationUnavailable,
		},
		{
			name: "restore: community lookup not configured", restore: true, subject: accountBanCommunityDID, reason: accountBanSpam,
			want: moderation.ErrModerationUnavailable,
		},
		{
			name: "restore: illegal-content reason", restore: true, subject: accountBanCommunityDID, reason: accountBanIllegal,
			communities: indexedCommunities, want: moderation.ErrUnsupportedReason,
		},
		{
			name: "restore: doxing reason", restore: true, subject: accountBanCommunityDID, reason: accountBanDoxing,
			communities: indexedCommunities, want: moderation.ErrUnsupportedReason,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			before := store.state.copy()
			service := newCommunityRemovalService(store, test.communities)

			var result *moderation.MutationResult
			var err error
			if test.restore {
				result, err = service.RestoreCommunity(t.Context(), accountBanAdminDID, moderation.RestoreCommunityRequest{
					Subject: test.subject, ExpectedVersion: "v1", IdempotencyKey: "community-restore-refusal",
					Reason: test.reason, PrivateNote: "n",
				})
			} else {
				result, err = service.RemoveCommunity(t.Context(), accountBanAdminDID, moderation.RemoveCommunityRequest{
					Subject: test.subject, ExpectedVersion: "v0", IdempotencyKey: "community-remove-refusal",
					Reason: test.reason, PrivateNote: "n",
				})
			}
			require.ErrorIs(t, err, test.want)
			assert.Nil(t, result)
			assert.Equal(t, before, store.state, "a refused community mutation must leave actions, decisions, versions and idempotency records unchanged")
			assert.Empty(t, store.writeCalls, "a refused community mutation must not attempt a write")
		})
	}
}
