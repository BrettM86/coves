package moderation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	accountBanAdminDID     = "did:plc:firstadmin"
	accountBanInstanceDID  = "did:web:moderation.test"
	accountBanAccountDID   = "did:plc:bannedaccount"
	accountBanSubjectURI   = "at://did:plc:bannedaccount"
	accountBanCommunityDID = "did:plc:hostedcommunity"
	accountBanSpam         = "social.coves.moderation.defs#reasonSpam"
	accountBanDoxing       = "social.coves.moderation.defs#reasonDoxing"
	accountBanIllegal      = "social.coves.moderation.defs#reasonIllegalContent"
	// accountBanBridgePDSURL is the PDS the service's bridge trust trusts;
	// accountBanCommunityPDSURL is an untrusted community PDS.
	accountBanBridgePDSURL    = "https://bridge.test"
	accountBanCommunityPDSURL = "https://pds.community.test"
	// accountBanBridgeHandle is the one community handle the bridge trust
	// trusts, and only on accountBanBridgePDSURL; accountBanOutsideHandle is a
	// handle outside the bridge's domain.
	accountBanBridgeHandle  = "comics.bridge.test"
	accountBanOutsideHandle = "comics.elsewhere.test"
)

// accountBanBridgeCommunity is a (handle, PDS URL) pair the bridge trust trusts.
type accountBanBridgeCommunity struct {
	handle string
	pdsURL string
}

// accountBanBridgeTrust trusts exactly the PDS URLs and the (handle, PDS URL)
// community pairs it holds. TrustsPDS answers true for the bridge PDS whatever
// the handle, so a service still deciding on the PDS alone is caught.
type accountBanBridgeTrust struct {
	trusted     map[string]bool
	communities map[accountBanBridgeCommunity]bool
}

func (trust accountBanBridgeTrust) TrustsPDS(pdsURL string) bool {
	return trust.trusted[pdsURL]
}

func (trust accountBanBridgeTrust) TrustsCommunity(handle, pdsURL string) bool {
	return trust.communities[accountBanBridgeCommunity{handle: handle, pdsURL: pdsURL}]
}

// failingAccountBanCommunities fails every community lookup.
type failingAccountBanCommunities struct{}

func (failingAccountBanCommunities) ExistsByDID(context.Context, string) (bool, error) {
	return false, errors.New("community lookup failed")
}

// accountBanCommunities reports the DIDs it holds as indexed communities.
type accountBanCommunities struct {
	dids map[string]bool
}

func (communities accountBanCommunities) ExistsByDID(_ context.Context, did string) (bool, error) {
	return communities.dids[did], nil
}

func accountBanClock() time.Time {
	return time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
}

func newAccountBanService(store *inMemoryModerationStore) moderation.Service {
	return newAccountBanServiceWithCommunities(store, accountBanCommunities{dids: map[string]bool{accountBanCommunityDID: true}})
}

func newAccountBanServiceWithCommunities(store *inMemoryModerationStore, communities moderation.CommunityChecker) moderation.Service {
	return moderation.NewService(&fakeSubjectReader{}, store, moderation.Config{
		InstanceDID: accountBanInstanceDID, IdempotencyRetention: 24 * time.Hour,
		MaxLiveIdempotencyKeys: 1000, Now: accountBanClock,
		Communities: communities,
		BridgeTrust: accountBanBridgeTrust{
			trusted: map[string]bool{accountBanBridgePDSURL: true},
			communities: map[accountBanBridgeCommunity]bool{
				{handle: accountBanBridgeHandle, pdsURL: accountBanBridgePDSURL}: true,
			},
		},
	})
}

func accountBanTime(t *testing.T, value string) *time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return &parsed
}

func TestBanAccountRefusals(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *moderation.BanAccountRequest)
		want   error
	}{
		{name: "subject is not a DID", change: func(_ *testing.T, request *moderation.BanAccountRequest) {
			request.Subject = "not-a-did"
		}, want: moderation.ErrInvalidRequest},
		{name: "subject is an AT-URI rather than a DID", change: func(_ *testing.T, request *moderation.BanAccountRequest) {
			request.Subject = "at://did:plc:bannedaccount"
		}, want: moderation.ErrInvalidRequest},
		{name: "subject is a handle", change: func(_ *testing.T, request *moderation.BanAccountRequest) {
			request.Subject = "bannedaccount.coves.social"
		}, want: moderation.ErrInvalidRequest},
		{name: "subject is an indexed community with no account activity", change: func(_ *testing.T, request *moderation.BanAccountRequest) {
			request.Subject = accountBanCommunityDID
		}, want: moderation.ErrInvalidSubject},
		{name: "expiresAt equal to the server clock", change: func(t *testing.T, request *moderation.BanAccountRequest) {
			request.ExpiresAt = accountBanTime(t, "2026-10-22T12:00:00Z")
		}, want: moderation.ErrInvalidRequest},
		{name: "expiresAt before the server clock", change: func(t *testing.T, request *moderation.BanAccountRequest) {
			request.ExpiresAt = accountBanTime(t, "2026-10-22T11:59:59.999Z")
		}, want: moderation.ErrInvalidRequest},
		{name: "missing reason", change: func(_ *testing.T, request *moderation.BanAccountRequest) {
			request.Reason = ""
		}, want: moderation.ErrInvalidRequest},
		{name: "unknown reason", change: func(_ *testing.T, request *moderation.BanAccountRequest) {
			request.Reason = "social.coves.moderation.defs#reasonCsam"
		}, want: moderation.ErrUnsupportedReason},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{accountBanCommunityDID: {IsCommunity: true}}
			before := store.state.copy()
			request := moderation.BanAccountRequest{
				Subject: accountBanAccountDID, ExpectedVersion: "v0", IdempotencyKey: "ban-refusal",
				Reason: accountBanSpam, PrivateNote: "n",
			}
			test.change(t, &request)

			result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, request)
			require.ErrorIs(t, err, test.want)
			assert.Nil(t, result)
			assert.Equal(t, before, store.state, "a refused ban must leave actions, decisions, versions and idempotency records unchanged")
			assert.Empty(t, store.writeCalls, "a refused ban must not attempt a write")
		})
	}
}

func TestBanAccountAccepts(t *testing.T) {
	for _, test := range []struct {
		name          string
		reason        string
		expiresAt     string
		wantExpiresAt string
	}{
		{
			name: "expiresAt one millisecond after the server clock", reason: accountBanSpam,
			expiresAt: "2026-10-22T12:00:00.001Z", wantExpiresAt: "2026-10-22T12:00:00.001Z",
		},
		{name: "doxing reason", reason: accountBanDoxing},
		{name: "illegal-content reason", reason: accountBanIllegal},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			request := moderation.BanAccountRequest{
				Subject: accountBanAccountDID, ExpectedVersion: "v0", IdempotencyKey: "ban-accepted",
				Reason: test.reason, PrivateNote: "n",
			}
			if test.expiresAt != "" {
				request.ExpiresAt = accountBanTime(t, test.expiresAt)
			}

			result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, request)
			require.NoError(t, err)
			require.NotNil(t, result, "an accepted ban returns its mutation result")
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, moderation.ActionRemove, result.Action.Action)
			assert.Equal(t, accountBanSubjectURI, result.Action.SubjectURI)
			assert.Empty(t, result.Action.SubjectCollection)
			assert.Equal(t, test.reason, result.Action.Reason)
			if test.wantExpiresAt == "" {
				assert.Nil(t, result.Action.ExpiresAt, "a ban without expiresAt is permanent")
			} else {
				require.NotNil(t, result.Action.ExpiresAt)
				assert.Equal(t, test.wantExpiresAt, result.Action.ExpiresAt.UTC().Format(time.RFC3339Nano))
			}
			assert.Equal(t, "v1", result.State.Version)
			assert.Equal(t, map[string]int64{accountBanSubjectURI: 1}, store.state.versions)
			assert.Len(t, store.state.actions, 1)
			assert.Len(t, store.state.activeRemovals, 1)
			assert.Len(t, store.state.idempotency, 1)
		})
	}
}

func TestUnbanAccountRefusesRemovalOnlyReasons(t *testing.T) {
	for _, test := range []struct {
		name    string
		subject string
		reason  string
		want    error
	}{
		{name: "doxing reason", reason: accountBanDoxing, want: moderation.ErrUnsupportedReason},
		{name: "illegal-content reason", reason: accountBanIllegal, want: moderation.ErrUnsupportedReason},
		{name: "missing reason", reason: "", want: moderation.ErrInvalidRequest},
		// A community removal has the account-ban row shape on the community's
		// DID; unbanAccount must not reverse it under account-unban semantics.
		{name: "subject is an indexed community", subject: accountBanCommunityDID, reason: accountBanSpam, want: moderation.ErrInvalidSubject},
	} {
		t.Run(test.name, func(t *testing.T) {
			subject := accountBanAccountDID
			if test.subject != "" {
				subject = test.subject
			}
			subjectURI := "at://" + subject
			store := newInMemoryModerationStore(accountBanClock())
			store.state.actions["action-ban"] = moderation.Action{
				ID: "action-ban", ActorDID: accountBanAdminDID, AuthorityDID: accountBanInstanceDID,
				ScopeKind: moderation.ScopeInstance, SubjectURI: subjectURI,
				Action: moderation.ActionRemove, Reason: accountBanSpam, Origin: moderation.OriginLocal,
				CreatedAt: time.Date(2026, time.October, 21, 12, 0, 0, 0, time.UTC),
			}
			store.state.activeRemovals[inMemoryModerationDecisionKey{accountBanInstanceDID, subjectURI}] = "action-ban"
			store.state.versions[subjectURI] = 1
			before := store.state.copy()

			result, err := newAccountBanService(store).UnbanAccount(t.Context(), accountBanAdminDID, moderation.UnbanAccountRequest{
				Subject: subject, ExpectedVersion: "v1", IdempotencyKey: "unban-refusal",
				Reason: test.reason, PrivateNote: "n",
			})
			require.ErrorIs(t, err, test.want)
			assert.Nil(t, result)
			assert.Equal(t, before, store.state, "a refused unban must leave the ban, its version and idempotency records unchanged")
			assert.Empty(t, store.writeCalls, "a refused unban must not attempt a write")
		})
	}
}

// A store failure inside an account mutation reaches the caller wrapped in
// ErrModerationUnavailable once, not once per layer.
func TestBanAccountStoreFailureIsWrappedOnce(t *testing.T) {
	store := newInMemoryModerationStore(accountBanClock())
	store.failInsertAction = errors.New("insert action failed")
	request := moderation.BanAccountRequest{
		Subject: accountBanAccountDID, ExpectedVersion: "v0", IdempotencyKey: "ban-store-failure",
		Reason: accountBanSpam, PrivateNote: "n",
	}

	result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, request)
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	assert.Nil(t, result)
	assert.Equal(t, 1, strings.Count(err.Error(), moderation.ErrModerationUnavailable.Error()),
		"the unavailable sentinel appears once in %q", err.Error())
}

// TestBanAccountRefusesIndexedCommunityWithoutBannableAccount covers indexed
// community DIDs that stay unbannable as accounts: one with no account
// activity, and a trusted-bridge community whose bridged content it authors as
// itself. The refusal wins over a stale expected version.
func TestBanAccountRefusesIndexedCommunityWithoutBannableAccount(t *testing.T) {
	for _, test := range []struct {
		name            string
		facts           moderation.AccountSubjectFacts
		expectedVersion string
	}{
		{
			name:            "no account activity on an untrusted PDS",
			facts:           moderation.AccountSubjectFacts{IsCommunity: true, HasAccountActivity: false, CommunityPDSURL: accountBanCommunityPDSURL},
			expectedVersion: "v0",
		},
		{
			name:            "account activity on the trusted bridge PDS",
			facts:           moderation.AccountSubjectFacts{IsCommunity: true, HasAccountActivity: true, CommunityPDSURL: accountBanBridgePDSURL, CommunityHandle: accountBanBridgeHandle},
			expectedVersion: "v0",
		},
		{
			name:            "account activity on the trusted bridge PDS with a stale version",
			facts:           moderation.AccountSubjectFacts{IsCommunity: true, HasAccountActivity: true, CommunityPDSURL: accountBanBridgePDSURL, CommunityHandle: accountBanBridgeHandle},
			expectedVersion: "v7",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{accountBanCommunityDID: test.facts}
			before := store.state.copy()

			result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
				Subject: accountBanCommunityDID, ExpectedVersion: test.expectedVersion, IdempotencyKey: "ban-community-refused",
				Reason: accountBanSpam, PrivateNote: "n",
			})
			require.ErrorIs(t, err, moderation.ErrInvalidSubject)
			assert.Nil(t, result)
			assert.Equal(t, before, store.state, "a refused ban must leave actions, decisions, versions and idempotency records unchanged")
			assert.Empty(t, store.writeCalls, "a refused ban must not attempt a write")
		})
	}
}

// accountBanRacedCommunityDID is indexed as a community after the pre-lock
// community lookup ran: Config.Communities does not report it, and only the
// facts read under the subject lock show its community row.
const accountBanRacedCommunityDID = "did:plc:racedcommunity"

// TestBanAccountUsesLockedCommunityFacts covers a community indexed between
// the pre-lock community lookup and LockSubject: the facts read under the lock
// decide, so an unbannable community is refused and one with account activity
// on an untrusted PDS is banned as an account.
func TestBanAccountUsesLockedCommunityFacts(t *testing.T) {
	for _, test := range []struct {
		name  string
		facts moderation.AccountSubjectFacts
		want  error
	}{
		{
			name:  "no account activity on an untrusted PDS is refused",
			facts: moderation.AccountSubjectFacts{IsCommunity: true, HasAccountActivity: false, CommunityPDSURL: accountBanCommunityPDSURL},
			want:  moderation.ErrInvalidSubject,
		},
		{
			name:  "account activity on the trusted bridge PDS is refused",
			facts: moderation.AccountSubjectFacts{IsCommunity: true, HasAccountActivity: true, CommunityPDSURL: accountBanBridgePDSURL, CommunityHandle: accountBanBridgeHandle},
			want:  moderation.ErrInvalidSubject,
		},
		{
			name:  "account activity on an untrusted PDS is banned as an account",
			facts: moderation.AccountSubjectFacts{IsCommunity: true, HasAccountActivity: true, CommunityPDSURL: accountBanCommunityPDSURL},
			want:  nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{accountBanRacedCommunityDID: test.facts}
			before := store.state.copy()

			result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
				Subject: accountBanRacedCommunityDID, ExpectedVersion: "v0", IdempotencyKey: "ban-raced-community",
				Reason: accountBanSpam, PrivateNote: "n",
			})
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
				assert.Nil(t, result)
				assert.Equal(t, before, store.state, "a refused ban must leave actions, decisions, versions and idempotency records unchanged")
				assert.Empty(t, store.writeCalls, "a refused ban must not attempt a write")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			stored, exists := store.state.actions[result.Action.ID]
			require.True(t, exists, "the ban action is stored")
			assert.Equal(t, moderation.SubjectKind("account"), stored.SubjectKind, "the stored ban records the account kind")
		})
	}
}

// TestBanAccountBridgeTrustUsesStoredCommunityHandleAndPDS covers the
// trusted-bridge refusal at ban time: it is decided on the community handle and
// PDS URL read under the subject lock, and needs both. A community on the
// bridge PDS whose stored handle is outside the bridge's domain is banned as an
// account, as is one on an untrusted PDS whose handle is under it.
func TestBanAccountBridgeTrustUsesStoredCommunityHandleAndPDS(t *testing.T) {
	for _, test := range []struct {
		name  string
		facts moderation.AccountSubjectFacts
		want  error
	}{
		{
			name: "bridge PDS and a handle under the bridge domain is refused",
			facts: moderation.AccountSubjectFacts{
				IsCommunity: true, HasAccountActivity: true,
				CommunityPDSURL: accountBanBridgePDSURL, CommunityHandle: accountBanBridgeHandle,
			},
			want: moderation.ErrInvalidSubject,
		},
		{
			name: "bridge PDS and a handle outside the bridge domain is banned as an account",
			facts: moderation.AccountSubjectFacts{
				IsCommunity: true, HasAccountActivity: true,
				CommunityPDSURL: accountBanBridgePDSURL, CommunityHandle: accountBanOutsideHandle,
			},
			want: nil,
		},
		{
			name: "untrusted PDS and a handle under the bridge domain is banned as an account",
			facts: moderation.AccountSubjectFacts{
				IsCommunity: true, HasAccountActivity: true,
				CommunityPDSURL: accountBanCommunityPDSURL, CommunityHandle: accountBanBridgeHandle,
			},
			want: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{accountBanCommunityDID: test.facts}
			before := store.state.copy()

			result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
				Subject: accountBanCommunityDID, ExpectedVersion: "v0", IdempotencyKey: "ban-bridge-community",
				Reason: accountBanSpam, PrivateNote: "n",
			})
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
				assert.Nil(t, result)
				assert.Equal(t, before, store.state, "a refused ban must leave actions, decisions, versions and idempotency records unchanged")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, moderation.SubjectKind("account"), result.Action.SubjectKind)
		})
	}
}

// TestBanAccountDecidesOnLockedFactsOnly covers BanAccount ignoring any
// community lookup made before the subject lock: the facts read under the lock
// decide whether the DID is a community, so a stale or failed earlier lookup
// neither refuses nor fails a ban on a DID the locked facts show is not one.
func TestBanAccountDecidesOnLockedFactsOnly(t *testing.T) {
	for _, test := range []struct {
		name        string
		communities moderation.CommunityChecker
	}{
		{
			name:        "the pre-lock lookup reports a community",
			communities: accountBanCommunities{dids: map[string]bool{accountBanCommunityDID: true}},
		},
		{
			name:        "the pre-lock lookup fails",
			communities: failingAccountBanCommunities{},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newInMemoryModerationStore(accountBanClock())
			store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{
				accountBanCommunityDID: {IsCommunity: false, HasAccountActivity: false},
			}

			result, err := newAccountBanServiceWithCommunities(store, test.communities).BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
				Subject: accountBanCommunityDID, ExpectedVersion: "v0", IdempotencyKey: "ban-locked-facts",
				Reason: accountBanSpam, PrivateNote: "n",
			})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
			require.NotNil(t, result.Action)
			assert.Equal(t, "at://"+accountBanCommunityDID, result.Action.SubjectURI)
			assert.Equal(t, moderation.SubjectKind("account"), result.Action.SubjectKind)
		})
	}
}

// An indexed community DID with account activity on an untrusted PDS is banned
// as an account, and the ban is the subject's decision: a following
// removeCommunity at the ban's version is a state conflict.
func TestBanAccountAppliesToIndexedCommunityWithAccountActivity(t *testing.T) {
	store := newInMemoryModerationStore(accountBanClock())
	store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{
		accountBanCommunityDID: {IsCommunity: true, HasAccountActivity: true, CommunityPDSURL: accountBanCommunityPDSURL},
	}
	service := newAccountBanService(store)
	subjectURI := "at://" + accountBanCommunityDID

	result, err := service.BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
		Subject: accountBanCommunityDID, ExpectedVersion: "v0", IdempotencyKey: "ban-community-with-activity",
		Reason: accountBanSpam, PrivateNote: "n",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, moderation.OutcomeApplied, result.Outcome)
	require.NotNil(t, result.Action)
	assert.Equal(t, moderation.ActionRemove, result.Action.Action)
	assert.Equal(t, moderation.SubjectKind("account"), result.Action.SubjectKind)
	assert.Equal(t, "v1", result.State.Version)
	stored, exists := store.state.actions[result.Action.ID]
	require.True(t, exists, "the ban action is stored")
	assert.Equal(t, moderation.SubjectKind("account"), stored.SubjectKind, "the stored ban records the account kind")
	assert.Equal(t, result.Action.ID, store.state.activeRemovals[inMemoryModerationDecisionKey{accountBanInstanceDID, subjectURI}],
		"the ban is the subject's active removal decision")

	before := store.state.copy()
	removal, err := service.RemoveCommunity(t.Context(), accountBanAdminDID, moderation.RemoveCommunityRequest{
		Subject: accountBanCommunityDID, ExpectedVersion: result.State.Version, IdempotencyKey: "remove-community-after-ban",
		Reason: accountBanSpam, PrivateNote: "n",
	})
	require.ErrorIs(t, err, moderation.ErrStateConflict, "removeCommunity refuses a community under an in-force account ban")
	assert.Nil(t, removal)
	assert.Equal(t, before, store.state, "the refused removal leaves the ban in place")
}

// A ban never replaces an active community removal, even on a community DID
// with account activity.
func TestBanAccountRefusesIndexedCommunityUnderCommunityRemoval(t *testing.T) {
	store := newInMemoryModerationStore(accountBanClock())
	store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{
		accountBanCommunityDID: {IsCommunity: true, HasAccountActivity: true, CommunityPDSURL: accountBanCommunityPDSURL},
	}
	seedSubjectKindDecision(store, accountBanCommunityDID, moderation.SubjectKindCommunity, nil)
	before := store.state.copy()

	result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
		Subject: accountBanCommunityDID, ExpectedVersion: "v1", IdempotencyKey: "ban-removed-community",
		Reason: accountBanSpam, PrivateNote: "n",
	})
	require.ErrorIs(t, err, moderation.ErrStateConflict)
	assert.Nil(t, result)
	assert.Equal(t, before, store.state, "the refused ban leaves the community removal, version and idempotency records unchanged")
	assert.Empty(t, store.writeCalls, "a refused ban must not attempt a write")
}

// A failed read of the account subject facts fails the ban as unavailable
// without a mutation.
func TestBanAccountAccountSubjectFactsFailureIsUnavailable(t *testing.T) {
	store := newInMemoryModerationStore(accountBanClock())
	store.accountSubjectFacts = map[string]moderation.AccountSubjectFacts{
		accountBanCommunityDID: {IsCommunity: true, HasAccountActivity: true, CommunityPDSURL: accountBanCommunityPDSURL},
	}
	store.failAccountSubjectFacts = errors.New("account subject facts read failed")
	before := store.state.copy()

	result, err := newAccountBanService(store).BanAccount(t.Context(), accountBanAdminDID, moderation.BanAccountRequest{
		Subject: accountBanCommunityDID, ExpectedVersion: "v0", IdempotencyKey: "ban-facts-failure",
		Reason: accountBanSpam, PrivateNote: "n",
	})
	require.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	assert.Nil(t, result)
	assert.Equal(t, before, store.state, "a failed facts read leaves the store unchanged")
	assert.Empty(t, store.writeCalls, "a failed facts read must not attempt a write")
}
