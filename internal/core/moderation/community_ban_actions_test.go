package moderation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// banSubject holds no role in rosteralpha.
	banSubject   = rosterOutsider
	banBystander = "did:plc:banbystander1"

	// BanRecordKey(banSubject) and BanRecordKey(rosterOwner), computed once
	// by hand.
	banSubjectKey = "udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq"
	banOwnerKey   = "5jbtoxiw4eatw5k4ht7uq6zz37en4i73i5b6yu6ctid6v23oimya"
	banSubjectURI = "at://did:plc:rosteralpha/social.coves.moderation.ban/udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq"

	// The standing ban record's CID (indexed and in the repo), a CID the repo
	// holds that the index does not, and the CID the fake repo gives a write.
	banStandingCID = "bafyreiabanstandingaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	banDriftedCID  = "bafyreiabandriftedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	banWrittenCID  = "bafyreiabanwrittenaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	banStartVersion    = 4
	banReason          = "Spamming invite links"
	banStandingCreated = "2026-11-10T09:00:00Z"
)

// banExpiry is 2026-11-20T12:00:00.250Z given in UTC-8: the record, the index
// and the fingerprint carry it as 2026-11-20T12:00:00Z.
var banExpiry = time.Date(2026, 11, 20, 4, 0, 0, 250_000_000, time.FixedZone("UTC-8", -8*3600))

// banExpiryUTC is banExpiry as the index stores it.
var banExpiryUTC = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func banVersionKey(communityDID, subjectDID string) string { return communityDID + " " + subjectDID }

// CommunityAccountBan serves the store's standing ban row (in force or
// lapsed) and the (community, subject) version.
func (tx *fakeCommunityActionTx) CommunityAccountBan(_ context.Context, communityDID, subjectDID string) (moderation.CommunityAccountBanState, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	state := moderation.CommunityAccountBanState{Version: tx.store.banVersions[banVersionKey(communityDID, subjectDID)]}
	if ban, ok := tx.store.bans[communityDID][subjectDID]; ok {
		copied := ban
		state.Ban = &copied
	}
	return state, nil
}

// RecentCommunityBans serves the counts the test seeded.
func (tx *fakeCommunityActionTx) RecentCommunityBans(_ context.Context, communityDID, actorDID string, _ time.Time) (int, int, error) {
	tx.store.mu.Lock()
	defer tx.store.mu.Unlock()
	return tx.store.recentBansByActor[actorDID], tx.store.recentBansCommunity[communityDID], nil
}

func (s *fakeCommunityActionStore) setRecentBans(communityDID, actorDID string, byActor, byCommunity int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recentBansByActor[actorDID] = byActor
	s.recentBansCommunity[communityDID] = byCommunity
}

// isFakeBanIntent reports whether the intent writes a community ban record.
func isFakeBanIntent(intent moderation.CommunityIntent) bool {
	return strings.Contains(intent.TargetURI, "/"+communities.BanCollection+"/")
}

// completeBanIntentLocked applies a ban intent's local effect on Commit: the
// ban index follows the written record, the (community, subject) version
// advances once, one action is logged, the intent completes and the result is
// stored. The caller holds the store's lock.
func (tx *fakeCommunityActionTx) completeBanIntentLocked(index int, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	stored := tx.store.intents[index]
	rkey := stored.TargetURI[strings.LastIndex(stored.TargetURI, "/")+1:]
	var ban *communities.CommunityBan
	if stored.Op != string(pds.WriteOpDelete) {
		if commit.RecordCID == "" {
			return moderation.CommunityActionResult{}, errors.New("fake store: completing a ban write without its record CID")
		}
		var record struct {
			Subject   string `json:"subject"`
			Reason    string `json:"reason"`
			ExpiresAt string `json:"expiresAt"`
		}
		if err := json.Unmarshal(stored.Record, &record); err != nil {
			return moderation.CommunityActionResult{}, fmt.Errorf("fake store: decoding the ban intent's record: %w", err)
		}
		ban = &communities.CommunityBan{
			CommunityDID: stored.CommunityDID, SubjectDID: record.Subject, RecordKey: rkey,
			RecordCID: commit.RecordCID, Reason: record.Reason, IndexedAt: tx.store.now(),
		}
		if record.ExpiresAt != "" {
			expires, err := time.Parse(time.RFC3339, record.ExpiresAt)
			if err != nil {
				return moderation.CommunityActionResult{}, fmt.Errorf("fake store: the ban intent's expiresAt: %w", err)
			}
			ban.ExpiresAt = &expires
		}
	}
	versionKey := banVersionKey(stored.CommunityDID, stored.SubjectDID)
	actionID := fmt.Sprintf("community-action-%d", len(tx.store.actions)+1)
	result := moderation.CommunityActionResult{
		CommunityDID: stored.CommunityDID,
		SubjectDID:   stored.SubjectDID,
		RecordURI:    stored.TargetURI,
		ActionID:     actionID,
		Version:      tx.store.banVersions[versionKey] + 1,
	}
	tx.pending = append(tx.pending, func() {
		if ban != nil {
			if tx.store.bans[stored.CommunityDID] == nil {
				tx.store.bans[stored.CommunityDID] = map[string]communities.CommunityBan{}
			}
			tx.store.bans[stored.CommunityDID][ban.SubjectDID] = *ban
		} else {
			delete(tx.store.bans[stored.CommunityDID], stored.SubjectDID)
		}
		tx.store.banVersions[versionKey] = result.Version
		tx.store.actions = append(tx.store.actions, communityActionRecorded{
			ActorDID: stored.ActorDID, AdminActing: stored.AdminActing,
			CommunityDID: stored.CommunityDID, SubjectDID: stored.SubjectDID,
		})
		tx.store.actionLog = append(tx.store.actionLog, communityActionLogged{
			Action: stored.Action, ActorDID: stored.ActorDID, AdminActing: stored.AdminActing,
			CommunityDID: stored.CommunityDID, SubjectDID: stored.SubjectDID,
		})
		completed := tx.store.intents[index]
		completed.State = moderation.CommunityIntentCompleted
		completed.CommitRev = commit.Rev
		completed.RecordCID = commit.RecordCID
		completed.ActionID = actionID
		tx.store.intents[index] = completed
		tx.store.idempotency[communityActionIdempotencyKey{stored.ActorDID, stored.CommunityDID, stored.IdempotencyKey}] = communityActionStoredRecord{
			record:    moderation.CommunityIdempotencyRecord{Fingerprint: stored.Fingerprint, Result: result},
			expiresAt: tx.store.now().Add(idempotencyTTL),
		}
	})
	return result, nil
}

type communityBanFixture struct {
	*communityPostFixture
	authorizes *recordingRoleAuthorizer
}

// newCommunityBanFixture extends the post fixture for ban actions: ban limits
// are 60 per moderator and 300 per community per hour, and every account of
// rosteralpha's roster and banSubject is at (community, subject) version
// banStartVersion with no ban standing.
func newCommunityBanFixture(t *testing.T) *communityBanFixture {
	t.Helper()
	f := newCommunityPostFixture(t)
	authorizes := &recordingRoleAuthorizer{RoleAuthorizer: f.authorizer}
	cfg := f.config
	cfg.Authorizer = authorizes
	cfg.BanLimits = moderation.CommunityRemovalLimits{PerModerator: 60, PerCommunity: 300, Window: time.Hour}
	f.config = cfg
	f.service = moderation.NewCommunityActionService(cfg)
	f.repo.writeCIDs[communities.BanCollection+"/"+banSubjectKey] = banWrittenCID
	f.repo.writeCIDs[communities.BanCollection+"/"+banOwnerKey] = banWrittenCID
	for _, subject := range []string{banSubject, rosterOwner, rosterModerator, rosterSecondModerator} {
		f.store.banVersions[banVersionKey(rosterCommunity, subject)] = banStartVersion
	}
	return &communityBanFixture{communityPostFixture: f, authorizes: authorizes}
}

// seedBan: banSubject is banned in rosteralpha by a record at cid, indexed
// and standing in the repo.
func (f *communityBanFixture) seedBan(reason string, expiresAt *time.Time, cid string) {
	f.store.mu.Lock()
	if f.store.bans[rosterCommunity] == nil {
		f.store.bans[rosterCommunity] = map[string]communities.CommunityBan{}
	}
	f.store.bans[rosterCommunity][banSubject] = communities.CommunityBan{
		CommunityDID: rosterCommunity, SubjectDID: banSubject, RecordKey: banSubjectKey,
		RecordCID: cid, Reason: reason, ExpiresAt: expiresAt, IndexedAt: rosterClockStart.Add(-72 * time.Hour),
	}
	f.store.mu.Unlock()
	f.setRepoBan(reason, expiresAt, cid)
}

// setRepoBan puts banSubject's ban record at cid into the community repo only.
func (f *communityBanFixture) setRepoBan(reason string, expiresAt *time.Time, cid string) {
	body := map[string]any{
		"$type":     communities.BanCollection,
		"subject":   banSubject,
		"createdAt": banStandingCreated,
	}
	if reason != "" {
		body["reason"] = reason
	}
	if expiresAt != nil {
		body["expiresAt"] = expiresAt.UTC().Format(time.RFC3339)
	}
	f.repo.setPostRecord(communities.BanCollection, banSubjectKey, cid, body)
}

func (f *communityBanFixture) ban(actorDID, communityDID, subjectDID, reason string, expiresAt *time.Time, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.BanCommunityAccount(context.Background(), moderation.BanCommunityAccountRequest{
		ActorDID: actorDID, CommunityDID: communityDID, SubjectDID: subjectDID,
		Reason: reason, ExpiresAt: expiresAt, ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

func (f *communityBanFixture) unban(actorDID, communityDID, subjectDID string, expectedVersion int64, key string) (*moderation.CommunityActionResult, error) {
	return f.service.UnbanCommunityAccount(context.Background(), moderation.UnbanCommunityAccountRequest{
		ActorDID: actorDID, CommunityDID: communityDID, SubjectDID: subjectDID,
		ExpectedVersion: expectedVersion, IdempotencyKey: key,
	})
}

// assertBanNothingWritten: no batch reached the repo, no intent was stored
// and no action was logged.
func (f *communityBanFixture) assertBanNothingWritten(t *testing.T) {
	t.Helper()
	assert.Empty(t, f.repo.batchesSnapshot(), "no batch may reach the community repo")
	assert.Empty(t, f.store.intentsSnapshot(), "no intent may be stored")
	assert.Empty(t, f.store.actionLogSnapshot(), "no action may be logged")
}

// assertNothingRead: nothing past the request itself was read — no
// authorization, no action transaction, no community repo.
func (f *communityBanFixture) assertNothingRead(t *testing.T) {
	t.Helper()
	assert.Empty(t, f.authorizes.callsSnapshot(), "no authorization for an invalid request")
	assert.Empty(t, f.store.locksSnapshot(), "no action transaction may open")
	assert.Empty(t, f.factory.openedSnapshot(), "the community repo may not be opened")
	f.assertBanNothingWritten(t)
}

func expiryPtr(at time.Time) *time.Time { return &at }

// U1: a ban of an unbanned account is ONE batch with ONE op, a create of the
// ban record at the subject's derived rkey, its body carrying the subject,
// the reason, the expiry normalized to whole UTC seconds and the intent's
// createdAt; with no reason and no expiry the body carries neither. A standing
// ban with another reason is ONE update op; an unban is ONE delete op. Each
// batch arrives with its open intent stored and is guarded by the head read.
func TestCommunityBanActions_CommitShape(t *testing.T) {
	cases := []struct {
		name       string
		standing   bool
		send       func(f *communityBanFixture) (*moderation.CommunityActionResult, error)
		want       []postBatchWrite
		wantAction string
	}{
		{
			name: "ban of an unbanned account",
			send: func(f *communityBanFixture) (*moderation.CommunityActionResult, error) {
				return f.ban(rosterModerator, rosterCommunity, banSubject, banReason, expiryPtr(banExpiry), banStartVersion, "ban-1")
			},
			want:       []postBatchWrite{{Op: "create", Collection: "social.coves.moderation.ban", RKey: "udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq", Record: `{"$type":"social.coves.moderation.ban","createdAt":"2026-11-13T12:00:00Z","expiresAt":"2026-11-20T12:00:00Z","reason":"Spamming invite links","subject":"did:plc:rosteroutsider"}`}},
			wantAction: "remove",
		},
		{
			name: "ban with no reason and no expiry",
			send: func(f *communityBanFixture) (*moderation.CommunityActionResult, error) {
				return f.ban(rosterModerator, rosterCommunity, banSubject, "", nil, banStartVersion, "ban-1")
			},
			want:       []postBatchWrite{{Op: "create", Collection: "social.coves.moderation.ban", RKey: "udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq", Record: `{"$type":"social.coves.moderation.ban","createdAt":"2026-11-13T12:00:00Z","subject":"did:plc:rosteroutsider"}`}},
			wantAction: "remove",
		},
		{
			name:     "standing ban rewritten with another reason",
			standing: true,
			send: func(f *communityBanFixture) (*moderation.CommunityActionResult, error) {
				return f.ban(rosterModerator, rosterCommunity, banSubject, "Harassing members", nil, banStartVersion, "ban-1")
			},
			want:       []postBatchWrite{{Op: "update", Collection: "social.coves.moderation.ban", RKey: "udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq", Record: `{"$type":"social.coves.moderation.ban","createdAt":"2026-11-13T12:00:00Z","reason":"Harassing members","subject":"did:plc:rosteroutsider"}`}},
			wantAction: "remove",
		},
		{
			name:     "unban",
			standing: true,
			send: func(f *communityBanFixture) (*moderation.CommunityActionResult, error) {
				return f.unban(rosterModerator, rosterCommunity, banSubject, banStartVersion, "unban-1")
			},
			want:       []postBatchWrite{{Op: "delete", Collection: "social.coves.moderation.ban", RKey: "udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq"}},
			wantAction: "restore",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityBanFixture(t)
			if tc.standing {
				f.seedBan(banReason, nil, banStandingCID)
			}
			// The clock moves on after the intent is stored: the record must
			// carry the intent's time, not the time of the write.
			f.withHooks(moderation.CommunityActionHooks{BeforeWrite: func(context.Context) {
				f.clock.set(rosterClockStart.Add(5 * time.Minute))
			}})

			result, err := tc.send(f)

			batches := f.repo.batchesSnapshot()
			require.Len(t, batches, 1, "a ban action is exactly one ApplyWrites batch")
			assert.Equal(t, rosterHeadZeroCID, batches[0].swapCommit, "the batch is guarded by the head read before the write")
			assert.Equal(t, tc.want, postBatchWrites(t, batches[0]), "one op on the ban record")
			assert.Empty(t, f.repo.violationsSnapshot(), "the open intent must exist when the batch arrives")
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, "at://did:plc:rosteralpha/social.coves.moderation.ban/udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq", result.RecordURI)
			assert.False(t, result.AlreadyApplied)
			assert.EqualValues(t, 5, result.Version)
			assert.NotEmpty(t, result.ActionID)
			assert.Equal(t, []communityActionLogged{
				{Action: tc.wantAction, ActorDID: rosterModerator, CommunityDID: rosterCommunity, SubjectDID: banSubject},
			}, f.store.actionLogSnapshot())
		})
	}
}

// U2: a caller the authorizer refuses — no role, a role in another
// community, a community this instance does not host, an instance-removed
// community — is Forbidden with authorization the only read: no action
// transaction, no community repo, no intent. A role revoked after the first
// authorization is Forbidden under the lock, with nothing written.
func TestCommunityBanActions_RefusedBeforeAnyRead(t *testing.T) {
	cases := []struct {
		name, actor, community string
	}{
		{"caller holds no role", banBystander, rosterCommunity},
		{"moderator of another community", postBetaModerator, rosterCommunity},
		{"community not hosted here", rosterModerator, rosterRemote},
		{"instance-removed community", rosterModerator, rosterRemoved},
	}
	for _, unban := range []bool{false, true} {
		operation := "ban"
		if unban {
			operation = "unban"
		}
		for _, tc := range cases {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				f := newCommunityBanFixture(t)

				var err error
				if unban {
					_, err = f.unban(tc.actor, tc.community, banSubject, banStartVersion, "unban-1")
				} else {
					_, err = f.ban(tc.actor, tc.community, banSubject, banReason, nil, banStartVersion, "ban-1")
				}

				require.ErrorIs(t, err, moderation.ErrForbidden)
				assert.Equal(t, []string{tc.community}, f.authorizes.callsSnapshot(), "authorization is the one read")
				assert.Empty(t, f.store.locksSnapshot(), "no action transaction may open")
				assert.Empty(t, f.factory.openedSnapshot(), "the community repo may not be opened")
				f.assertBanNothingWritten(t)
			})
		}
	}

	t.Run("role revoked after the first authorization", func(t *testing.T) {
		f := newCommunityBanFixture(t)
		f.withHooks(moderation.CommunityActionHooks{AfterAuthorize: func(context.Context) {
			f.store.dropRole(rosterCommunity, rosterModeratorKey)
		}})

		_, err := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, nil, banStartVersion, "ban-1")

		require.ErrorIs(t, err, moderation.ErrForbidden)
		assert.Empty(t, f.factory.openedSnapshot(), "the community repo may not be opened")
		f.assertBanNothingWritten(t)
	})
}

// U3: a subject that holds a role in the community — the owner, another
// moderator, the acting moderator itself, a moderator the owner bans — is
// SubjectHoldsRole with no intent and no write. Once the subject's role is
// gone from the roster, the same ban is written.
func TestCommunityBanActions_SubjectHoldsRole(t *testing.T) {
	cases := []struct {
		name, actor, subject string
	}{
		{"moderator bans the owner", rosterModerator, rosterOwner},
		{"moderator bans another moderator", rosterModerator, rosterSecondModerator},
		{"moderator bans itself", rosterModerator, rosterModerator},
		{"owner bans a moderator", rosterOwner, rosterModerator},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityBanFixture(t)

			_, err := f.ban(tc.actor, rosterCommunity, tc.subject, banReason, nil, banStartVersion, "ban-1")

			require.ErrorIs(t, err, moderation.ErrSubjectHoldsRole)
			f.assertBanNothingWritten(t)
		})
	}

	t.Run("owner's role gone from the roster", func(t *testing.T) {
		f := newCommunityBanFixture(t)
		f.store.dropRole(rosterCommunity, rosterOwnerKey)

		result, err := f.ban(rosterModerator, rosterCommunity, rosterOwner, banReason, nil, banStartVersion, "ban-1")

		batches := f.repo.batchesSnapshot()
		require.Len(t, batches, 1, "the ban is written")
		assert.Equal(t, []postBatchWrite{{
			Op: "create", Collection: "social.coves.moderation.ban", RKey: "5jbtoxiw4eatw5k4ht7uq6zz37en4i73i5b6yu6ctid6v23oimya",
			Record: `{"$type":"social.coves.moderation.ban","createdAt":"2026-11-13T12:00:00Z","reason":"Spamming invite links","subject":"did:plc:rosterowner"}`,
		}}, postBatchWrites(t, batches[0]))
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, "at://did:plc:rosteralpha/social.coves.moderation.ban/5jbtoxiw4eatw5k4ht7uq6zz37en4i73i5b6yu6ctid6v23oimya", result.RecordURI)
	})
}

// U4: a subject that is not a DID, a subject equal to the community or a
// reason over 2000 graphemes is InvalidRequest before anything is read. An
// expiry at or before the service clock — including one in the same second
// once both are truncated to whole seconds — is InvalidRequest with nothing
// written.
func TestCommunityBanActions_RequestValidation(t *testing.T) {
	malformed := []struct {
		name string
		send func(f *communityBanFixture) error
	}{
		{"ban subject not a DID", func(f *communityBanFixture) error {
			_, err := f.ban(rosterModerator, rosterCommunity, "rosteroutsider.test", banReason, nil, banStartVersion, "ban-1")
			return err
		}},
		{"unban subject not a DID", func(f *communityBanFixture) error {
			_, err := f.unban(rosterModerator, rosterCommunity, "rosteroutsider.test", banStartVersion, "unban-1")
			return err
		}},
		{"subject equal to the community", func(f *communityBanFixture) error {
			_, err := f.ban(rosterModerator, rosterCommunity, rosterCommunity, banReason, nil, banStartVersion, "ban-1")
			return err
		}},
		{"reason of 2001 graphemes", func(f *communityBanFixture) error {
			_, err := f.ban(rosterModerator, rosterCommunity, banSubject, strings.Repeat("a", 2001), nil, banStartVersion, "ban-1")
			return err
		}},
		{"expiry past year 9999 in UTC", func(f *communityBanFixture) error {
			// JSON decoding accepts this offset; in UTC it is
			// 10000-01-01T22:00:00Z, which no RFC 3339 reader parses back.
			var expiresAt time.Time
			if err := json.Unmarshal([]byte(`"9999-12-31T23:00:00-23:00"`), &expiresAt); err != nil {
				return err
			}
			_, err := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, &expiresAt, banStartVersion, "ban-1")
			return err
		}},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityBanFixture(t)

			err := tc.send(f)

			require.ErrorIs(t, err, moderation.ErrInvalidRequest)
			f.assertNothingRead(t)
		})
	}

	expiries := []struct {
		name      string
		clock     time.Time
		expiresAt time.Time
	}{
		{"expiry equal to the clock", rosterClockStart, rosterClockStart},
		{"expiry before the clock", rosterClockStart, rosterClockStart.Add(-time.Minute)},
		{"expiry later in the clock's second", rosterClockStart.Add(500 * time.Millisecond), rosterClockStart.Add(900 * time.Millisecond)},
	}
	for _, tc := range expiries {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityBanFixture(t)
			f.clock.set(tc.clock)

			_, err := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, expiryPtr(tc.expiresAt), banStartVersion, "ban-1")

			require.ErrorIs(t, err, moderation.ErrInvalidRequest)
			f.assertBanNothingWritten(t)
		})
	}
}

// U5: bans have their own limits, 60 per moderator and 300 per community in
// the window: a moderator or a community at its limit is RateLimited with no
// intent; an instance admin acting is exempt, an unban is not limited, and an
// exhausted removal budget does not limit bans.
func TestCommunityBanActions_RateLimits(t *testing.T) {
	cases := []struct {
		name                       string
		actor                      string
		byActor, byCommunity       int
		removalsByActor, removalsC int
		unban                      bool
		limited                    bool
	}{
		{name: "moderator at the per-moderator limit", actor: rosterModerator, byActor: 60, limited: true},
		{name: "community at the per-community limit", actor: rosterModerator, byCommunity: 300, limited: true},
		{name: "instance admin acting at both limits", actor: rosterAdmin, byActor: 60, byCommunity: 300},
		{name: "unban at both limits", actor: rosterModerator, byActor: 60, byCommunity: 300, unban: true},
		{name: "removal budget exhausted, ban budget free", actor: rosterModerator, removalsByActor: 60, removalsC: 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityBanFixture(t)
			f.store.setRecentBans(rosterCommunity, tc.actor, tc.byActor, tc.byCommunity)
			f.store.setRecentRemovals(rosterCommunity, tc.actor, tc.removalsByActor, tc.removalsC)

			var err error
			if tc.unban {
				f.seedBan(banReason, nil, banStandingCID)
				_, err = f.unban(tc.actor, rosterCommunity, banSubject, banStartVersion, "unban-1")
			} else {
				_, err = f.ban(tc.actor, rosterCommunity, banSubject, banReason, nil, banStartVersion, "ban-1")
			}

			if tc.limited {
				require.ErrorIs(t, err, moderation.ErrRateLimited)
				f.assertBanNothingWritten(t)
				return
			}
			require.NoError(t, err)
			assert.Len(t, f.repo.batchesSnapshot(), 1, "the action is written")
		})
	}
}

// U6: expectedVersion is the (community, subject) version; a stale one is
// StateConflict with nothing written. A ban record in the community repo that
// is not the indexed one (another CID, or present where nothing is indexed) is
// StateConflict before any write.
func TestCommunityBanActions_StateConflicts(t *testing.T) {
	cases := []struct {
		name string
		seed func(f *communityBanFixture)
		send func(f *communityBanFixture) error
	}{
		{
			name: "ban at a stale version",
			send: func(f *communityBanFixture) error {
				_, err := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, nil, banStartVersion-1, "ban-1")
				return err
			},
		},
		{
			name: "unban at a stale version",
			seed: func(f *communityBanFixture) { f.seedBan(banReason, nil, banStandingCID) },
			send: func(f *communityBanFixture) error {
				_, err := f.unban(rosterModerator, rosterCommunity, banSubject, banStartVersion-1, "unban-1")
				return err
			},
		},
		{
			name: "ban record in the repo differs from the indexed one",
			seed: func(f *communityBanFixture) {
				f.seedBan(banReason, nil, banStandingCID)
				f.setRepoBan("Harassing members", nil, banDriftedCID)
			},
			send: func(f *communityBanFixture) error {
				_, err := f.ban(rosterModerator, rosterCommunity, banSubject, "Posting malware", nil, banStartVersion, "ban-1")
				return err
			},
		},
		{
			name: "ban record in the repo with none indexed",
			seed: func(f *communityBanFixture) { f.setRepoBan(banReason, nil, banDriftedCID) },
			send: func(f *communityBanFixture) error {
				_, err := f.ban(rosterModerator, rosterCommunity, banSubject, "Posting malware", nil, banStartVersion, "ban-1")
				return err
			},
		},
		{
			name: "unban with the repo's ban record differing from the indexed one",
			seed: func(f *communityBanFixture) {
				f.seedBan(banReason, nil, banStandingCID)
				f.setRepoBan("Harassing members", nil, banDriftedCID)
			},
			send: func(f *communityBanFixture) error {
				_, err := f.unban(rosterModerator, rosterCommunity, banSubject, banStartVersion, "unban-1")
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCommunityBanFixture(t)
			if tc.seed != nil {
				tc.seed(f)
			}

			err := tc.send(f)

			require.ErrorIs(t, err, moderation.ErrStateConflict)
			f.assertBanNothingWritten(t)
		})
	}
}

// U6: a ban with the identical reason and expiry already standing, or an
// unban with no ban record standing, is AlreadyApplied: nothing is written
// and no action is logged.
func TestCommunityBanActions_AlreadyApplied(t *testing.T) {
	t.Run("identical ban standing", func(t *testing.T) {
		f := newCommunityBanFixture(t)
		f.seedBan(banReason, expiryPtr(banExpiryUTC), banStandingCID)

		// The same expiry, given in another zone with a sub-second part.
		result, err := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, expiryPtr(banExpiry), banStartVersion, "ban-1")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.AlreadyApplied)
		assert.Equal(t, "at://did:plc:rosteralpha/social.coves.moderation.ban/udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq", result.RecordURI)
		assert.EqualValues(t, 4, result.Version)
		assert.Empty(t, result.ActionID, "no action was written")
		f.assertBanNothingWritten(t)
	})

	t.Run("unban with no ban standing", func(t *testing.T) {
		f := newCommunityBanFixture(t)

		result, err := f.unban(rosterModerator, rosterCommunity, banSubject, banStartVersion, "unban-1")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.AlreadyApplied)
		assert.Equal(t, "at://did:plc:rosteralpha/social.coves.moderation.ban/udlvadllmfhyyofx6pehuz3v7i2h7kt6e3bucygcsoxu3ojnymaq", result.RecordURI)
		assert.EqualValues(t, 4, result.Version)
		assert.Empty(t, result.ActionID, "no action was written")
		f.assertBanNothingWritten(t)
	})
}

// U7: the idempotency fingerprint covers the reason and the expiry, against a
// stored result and against the first request's still-open intent.
func TestCommunityBanActions_IdempotencyFingerprint(t *testing.T) {
	variants := []struct {
		name      string
		reason    string
		expiresAt *time.Time
	}{
		{"different reason", "Harassing members", expiryPtr(banExpiry)},
		{"different expiry", banReason, expiryPtr(banExpiry.Add(time.Hour))},
	}
	for _, first := range []string{"completed", "intent open"} {
		for _, variant := range variants {
			t.Run(first+"/"+variant.name, func(t *testing.T) {
				f := newCommunityBanFixture(t)
				if first == "intent open" {
					f.repo.applyErr = errors.New("connection reset by peer")
				}
				_, firstErr := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, expiryPtr(banExpiry), banStartVersion, "ban-1")

				_, err := f.ban(rosterModerator, rosterCommunity, banSubject, variant.reason, variant.expiresAt, banStartVersion, "ban-1")

				require.ErrorIs(t, err, moderation.ErrIdempotencyConflict)
				assert.Len(t, f.repo.batchesSnapshot(), 1, "the conflicting request sends nothing")
				if first == "completed" {
					require.NoError(t, firstErr)
					return
				}
				require.ErrorIs(t, firstErr, moderation.ErrModerationUnavailable)
				open := 0
				for _, intent := range f.store.intentsSnapshot() {
					if intent.State == moderation.CommunityIntentOpen {
						open++
					}
				}
				assert.Equal(t, 1, open, "the first request's intent stays open")
			})
		}
	}
}

// U7: a same-key retry of a completed ban whose expiry has since passed gets
// the stored result, not InvalidRequest, and writes nothing more.
func TestCommunityBanActions_StoredResultAfterExpiryPassed(t *testing.T) {
	f := newCommunityBanFixture(t)
	expiresAt := rosterClockStart.Add(time.Hour)
	first, err := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, &expiresAt, banStartVersion, "ban-1")
	require.NoError(t, err)
	require.NotNil(t, first, "the first ban completes")
	f.clock.set(rosterClockStart.Add(2 * time.Hour))

	replay, err := f.ban(rosterModerator, rosterCommunity, banSubject, banReason, &expiresAt, banStartVersion, "ban-1")

	require.NoError(t, err)
	require.NotNil(t, replay)
	assert.Equal(t, *first, *replay, "the stored result is replayed")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "the replay writes nothing")
	assert.Len(t, f.store.actionLogSnapshot(), 1, "the replay logs nothing")
}
