//go:build integration

package moderation_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Firehose convergence: the production role consumer (CommunityEventConsumer
// with WithCommunityRoster and the real rev gate) over the same database as the
// community action service. Outcomes are read back from the database.

const (
	rosterBeta = "did:plc:rosterbeta"

	rosterHeadThreeRev = "3krosterrev03"
	rosterHeadFiveRev  = "3krosterrev05"
	rosterHeadSixRev   = "3krosterrev06"
	// rosterBetweenHeadsRev sorts after the fixture's gate (head zero) and
	// before the removal's commit (head one).
	rosterBetweenHeadsRev = "3krosterrev005"

	rosterModeratorRegrantCID = "bafyreiamoderatorregrantaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterModeratorUpdatedCID = "bafyreiamoderatorupdatedaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterOutsideGrantCID     = "bafyreiaoutsidegrantaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rosterBetaOwnerCID        = "bafyreiabetaowneraaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	rosterRecordCreatedAt = "2026-11-02T00:00:00Z"
)

// firehose is the production role consumer over the fixture's database.
func (f *communityActionDBFixture) firehose() *jetstream.CommunityEventConsumer {
	return firehoseOn(f.db)
}

// firehoseOn is the production role consumer over db, a handle on the
// fixture's database (such as a dedicated pool).
func firehoseOn(db *sql.DB) *jetstream.CommunityEventConsumer {
	return jetstream.NewCommunityEventConsumer(postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		rosterInstanceDID, true, nil,
		jetstream.WithCommunityRevGate(jetstream.NewRevGate(db)),
		jetstream.WithCommunityRoster(postgres.NewCommunityRoleRepository(db)))
}

// deliver hands one role commit to the consumer.
func (f *communityActionDBFixture) deliver(event *jetstream.JetstreamEvent) error {
	return f.firehose().HandleEvent(context.Background(), event)
}

// loseRemovalResponse has O remove M while the PDS commits the delete at head
// one and the response is lost.
func (f *communityActionDBFixture) loseRemovalResponse(t *testing.T) {
	t.Helper()
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.remove(context.Background(), rosterOwner, "remove-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")
}

func (f *communityActionDBFixture) roleIndexed(t *testing.T, communityDID, subjectDID string) bool {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM community_roles WHERE community_did = $1 AND subject_did = $2
	`, communityDID, subjectDID).Scan(&count))
	return count == 1
}

func (f *communityActionDBFixture) actionIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT id FROM moderation_actions WHERE scope_community_did = $1 ORDER BY created_at, id`, rosterCommunity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// communityActionRolesIn is the role column of the moderation_actions rows
// scoped to communityDID, oldest first; a NULL role reads as "<null>".
func communityActionRolesIn(t *testing.T, db *sql.DB, communityDID string) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT COALESCE(role, '<null>') FROM moderation_actions WHERE scope_community_did = $1 ORDER BY created_at, id
	`, communityDID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var role string
		require.NoError(t, rows.Scan(&role))
		out = append(out, role)
	}
	require.NoError(t, rows.Err())
	return out
}

func roleDeleteEvent(communityDID, rkey, rev string) *jetstream.JetstreamEvent {
	return &jetstream.JetstreamEvent{
		Kind: "commit", Did: communityDID, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{Rev: rev, Operation: "delete", Collection: communities.RoleCollection, RKey: rkey},
	}
}

func roleWriteEvent(communityDID, op, rkey, rev, cid string, record map[string]any) *jetstream.JetstreamEvent {
	return &jetstream.JetstreamEvent{
		Kind: "commit", Did: communityDID, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: rev, Operation: op, Collection: communities.RoleCollection, RKey: rkey, CID: cid, Record: record,
		},
	}
}

func roleRecord(subjectDID, role string) map[string]any {
	return map[string]any{
		"$type":     communities.RoleCollection,
		"subject":   subjectDID,
		"role":      role,
		"createdAt": rosterRecordCreatedAt,
	}
}

// insertRosterCommunity indexes a community; hosted stores a PDS refresh
// token, the hosted fact.
func insertRosterCommunity(t *testing.T, db *sql.DB, did, handle string, hosted bool) {
	t.Helper()
	var token []byte
	if hosted {
		token = []byte("hosted")
	}
	_, err := db.Exec(`
		INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at, pds_refresh_token_encrypted)
		VALUES ($1, $2, 'Roster Community', $1, $1, $3, NOW(), $4)
	`, did, handle, rosterInstanceDID, token)
	require.NoError(t, err)
}

// actorlessRoleAction is the one action a role change from outside Coves writes.
func actorlessRoleAction(action, communityDID, subjectDID string) storedAction {
	return storedAction{
		Action: action, ActorDID: "", AdminActing: false,
		AuthorityDID: communityDID, ScopeKind: "community", ScopeCommunityDID: communityDID,
		SubjectURI: "at://" + subjectDID, Origin: "inherited",
	}
}

// modlogServiceOn is the moderation service's action log over db: the real
// moderation repository, subject reader and community resolver.
func modlogServiceOn(db *sql.DB) moderation.Service {
	return moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{
			InstanceDID: rosterInstanceDID, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
			CursorSecret: "community-modlog-cursor-secret",
			CommunityResolver: communities.NewCommunityService(
				postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), "http://pds.invalid",
				"", "coves.social", nil, nil, nil, nil),
		},
	)
}

// publicModlog is the served JSON of each public listActions entry, newest
// first.
func publicModlog(t *testing.T, db *sql.DB, params moderation.ListActionsParams) []map[string]any {
	t.Helper()
	page, err := modlogServiceOn(db).ListActions(context.Background(), params)
	require.NoError(t, err)
	object, _ := modlogJSON(t, page)
	return modlogEntries(t, object)
}

// adminModlog is the served JSON of each listAdminActions entry, newest first.
func adminModlog(t *testing.T, db *sql.DB, params moderation.ListAdminActionsParams) []map[string]any {
	t.Helper()
	page, err := modlogServiceOn(db).ListAdminActions(context.Background(), params)
	require.NoError(t, err)
	object, _ := modlogJSON(t, page)
	return modlogEntries(t, object)
}

func modlogEntries(t *testing.T, page map[string]any) []map[string]any {
	t.Helper()
	raw, ok := page["actions"].([]any)
	require.Truef(t, ok, "actions is a list: %#v", page["actions"])
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		out = append(out, modlogObject(t, entry))
	}
	return out
}

// installActionInsertFailure makes every moderation_actions insert scoped to
// rosteralpha raise.
func installActionInsertFailure(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		CREATE FUNCTION fail_roster_firehose_action_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected moderation_actions insert failure';
		END
		$$
	`)
	require.NoError(t, err)
	_, err = db.Exec(`
		CREATE TRIGGER fail_roster_firehose_action_insert BEFORE INSERT ON moderation_actions
		FOR EACH ROW WHEN (NEW.scope_community_did = 'did:plc:rosteralpha')
		EXECUTE FUNCTION fail_roster_firehose_action_insert()
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS fail_roster_firehose_action_insert ON moderation_actions`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS fail_roster_firehose_action_insert()`)
	})
}

// B10: the PDS commits O's delete of M at head one and the response is lost.
// The retry with the same key completes it without a second delete; the
// firehose copy of that delete then changes nothing.
func TestCommunityActionFirehose_LostResponseRetryFirst(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.loseRemovalResponse(t)

	result, err := f.remove(context.Background(), rosterOwner, "remove-1")

	require.NoError(t, err, "the retry completes the removal")
	if assert.NotNil(t, result) {
		assert.EqualValues(t, 8, result.Version)
	}
	assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "one action naming O")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "no second ApplyWrites")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t))

	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadOneRev)))

	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "the firehose copy adds no action")
	assert.EqualValues(t, 8, f.rosterVersion(t), "the firehose copy leaves the roster version")
	assert.False(t, f.moderatorRowIndexed(t), "M's row stays gone")
}

// B11: after the lost response, the firehose copy of the delete arrives first.
// Its handling completes the open intent with O as the actor; the later retry
// replays that result.
func TestCommunityActionFirehose_LostResponseFirehoseFirst(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.loseRemovalResponse(t)

	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadOneRev)))

	assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t),
		"the firehose copy completes the intent at its rev")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t),
		"one action naming O, origin local")
	assert.Equal(t, []string{communities.RoleModerator}, communityActionRolesIn(t, f.db, rosterCommunity),
		"the completed removal records M's role")
	assert.EqualValues(t, 8, f.rosterVersion(t))
	ids := f.actionIDs(t)
	require.Len(t, ids, 1)

	result, err := f.remove(context.Background(), rosterOwner, "remove-1")

	require.NoError(t, err, "the retry replays the firehose completion")
	require.NotNil(t, result)
	assert.Equal(t, ids[0], result.ActionID, "the replayed result names the one action")
	assert.EqualValues(t, 8, result.Version)
	assert.Equal(t, rosterCommunity, result.CommunityDID)
	assert.Equal(t, rosterModerator, result.SubjectDID)
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "the retry adds no action")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no ApplyWrites")
}

// B12: a successful local removal, then its firehose copy delivered twice.
func TestCommunityActionFirehose_LocalRemovalThenItsCopyTwice(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	_, err := f.remove(context.Background(), rosterOwner, "remove-1")
	require.NoError(t, err)

	// A late copy of M's grant, at a rev above the removal's guard (head zero,
	// the fixture's gate) but below its commit (head one): only the gate the
	// removal stamped at head one rejects it. It arrives before the delete's
	// own copy, which would advance the gate to head one by itself.
	require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "create", rosterModeratorKey, rosterBetweenHeadsRev,
		rosterModeratorCID, roleRecord(rosterModerator, communities.RoleModerator))))

	assert.False(t, f.moderatorRowIndexed(t), "the late copy does not re-index M")
	assert.EqualValues(t, 8, f.rosterVersion(t), "the late copy leaves the roster version")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "the late copy adds no action")

	for i := 0; i < 2; i++ {
		require.NoErrorf(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadOneRev)), "delivery %d", i+1)
	}

	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "exactly one action, naming O")
	assert.EqualValues(t, 8, f.rosterVersion(t), "roster version advanced exactly once")
}

// B13: a role record created and deleted outside Coves, in a hosted and in an
// indexed non-hosted community: one actorless grant-role and one actorless
// remove-role action each, and a replay of both adds nothing.
func TestCommunityActionFirehose_RoleRecordChangedOutsideCoves(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		community string
		seed      func(t *testing.T, db *sql.DB)
	}{
		{name: "hosted community", community: rosterCommunity, seed: func(*testing.T, *sql.DB) {}},
		{name: "indexed non-hosted community", community: rosterRemote, seed: func(t *testing.T, db *sql.DB) {
			insertRosterCommunity(t, db, rosterRemote, "rosterremote.test", false)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			test.seed(t, f.db)
			events := []*jetstream.JetstreamEvent{
				roleWriteEvent(test.community, "create", rosterSecondModeratorKey, rosterHeadOneRev, rosterOutsideGrantCID,
					roleRecord(rosterSecondModerator, communities.RoleModerator)),
				roleDeleteEvent(test.community, rosterSecondModeratorKey, rosterHeadTwoRev),
			}

			for _, event := range events {
				require.NoError(t, f.deliver(event))
			}
			want := []storedAction{
				actorlessRoleAction("grant-role", test.community, rosterSecondModerator),
				actorlessRoleAction("remove-role", test.community, rosterSecondModerator),
			}
			assert.Equal(t, want, communityActionsIn(t, f.db, test.community))

			for _, event := range events {
				require.NoError(t, f.deliver(event), "replay")
			}
			assert.Equal(t, want, communityActionsIn(t, f.db, test.community), "the replay adds nothing")
		})
	}
}

// R1-9: an indexed role updated outside Coves with a new CID writes one
// actorless grant-role action and advances the roster version; a replay
// changes neither.
func TestCommunityActionFirehose_ExternalRoleUpdate(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	update := roleWriteEvent(rosterCommunity, "update", rosterModeratorKey, rosterHeadOneRev, rosterModeratorUpdatedCID,
		roleRecord(rosterModerator, communities.RoleModerator))

	require.NoError(t, f.deliver(update))

	want := []storedAction{actorlessRoleAction("grant-role", rosterCommunity, rosterModerator)}
	assert.Equal(t, want, f.actions(t))
	assert.EqualValues(t, 8, f.rosterVersion(t))

	require.NoError(t, f.deliver(update), "replay")

	assert.Equal(t, want, f.actions(t), "the replay adds no action")
	assert.EqualValues(t, 8, f.rosterVersion(t), "the replay leaves the roster version")
}

// Chunk 13 B5: a moderator role record created and then deleted outside Coves
// in the hosted community reads back from the public log, filtered to the
// community, as an actorless remove-role and an actorless grant-role, each
// naming the subject account and the role.
func TestCommunityModlog_ExternalRoleChangesListPublicly(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "create", rosterSecondModeratorKey, rosterHeadOneRev,
		rosterOutsideGrantCID, roleRecord("did:plc:rostermodtwo", "moderator"))))
	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterSecondModeratorKey, rosterHeadTwoRev)))

	entries := publicModlog(t, f.db, moderation.ListActionsParams{Community: "did:plc:rosteralpha"})

	require.Len(t, entries, 2, "both role changes are listed")
	for i, wantAction := range []string{"remove-role", "grant-role"} {
		entry := entries[i]
		assert.Equal(t, wantAction, entry["action"], "entry %d", i)
		assert.Equal(t, "moderator", entry["role"], "%s names the role", wantAction)
		assert.NotContains(t, entry, "actor", "%s has no actor", wantAction)
		assert.NotContains(t, entry, "adminActing", "%s is not admin-acting", wantAction)
		if assert.Contains(t, entry, "subject", "%s names its subject", wantAction) {
			assert.Equal(t, "at://did:plc:rostermodtwo", modlogObject(t, entry["subject"])["uri"], "%s subject", wantAction)
		}
		assert.Equal(t, map[string]any{"kind": "community", "communityDid": "did:plc:rosteralpha"}, entry["scope"],
			"%s scope", wantAction)
	}
}

// R1-5, R2-4, R2-5: an open delete intent for M's record (the PDS failed), then
// a gate-passing firehose event that is not that delete. The index changes with
// one actorless action and the intent stays open. The intent's guard rev is
// head five; M's gate holds head zero. An equal-rev event writes nothing.
func TestCommunityActionFirehose_EventsThatDoNotMatchTheOpenIntent(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		// before runs after the intent is open, before the event.
		before func(t *testing.T, f *communityActionDBFixture)
		event  *jetstream.JetstreamEvent
		// wantPermanent: the consumer refuses the event as permanently invalid.
		wantPermanent bool
		wantActions   []storedAction
		wantVersion   int64
		// wantIndexed is whether each subject's row is indexed afterwards.
		wantIndexed map[string]bool
	}{
		{
			name: "delete at a different record key",
			before: func(t *testing.T, f *communityActionDBFixture) {
				_, err := f.db.Exec(`
					INSERT INTO community_roles (community_did, subject_did, role, record_key, record_cid, record_created_at)
					VALUES ($1, $2, 'moderator', $3, $4, '2026-11-01T02:00:00Z')
				`, rosterCommunity, rosterSecondModerator, rosterSecondModeratorKey, rosterSecondModeratorCID)
				require.NoError(t, err)
				_, err = f.db.Exec(`INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)`,
					rosterSecondModeratorURI, rosterHeadZeroRev)
				require.NoError(t, err)
			},
			event:       roleDeleteEvent(rosterCommunity, rosterSecondModeratorKey, rosterHeadSixRev),
			wantActions: []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterSecondModerator)},
			wantVersion: 8,
			wantIndexed: map[string]bool{rosterModerator: true, rosterSecondModerator: false},
		},
		{
			name: "delete of an indexed row whose CID is not the replaced CID",
			before: func(t *testing.T, f *communityActionDBFixture) {
				_, err := f.db.Exec(`UPDATE community_roles SET record_cid = $1 WHERE community_did = $2 AND record_key = $3`,
					rosterModeratorStaleCID, rosterCommunity, rosterModeratorKey)
				require.NoError(t, err)
			},
			event:       roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadSixRev),
			wantActions: []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterModerator)},
			wantVersion: 8,
			wantIndexed: map[string]bool{rosterModerator: false},
		},
		{
			name:        "delete at a rev above the gate but not above the intent's guard rev",
			event:       roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadThreeRev),
			wantActions: []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterModerator)},
			wantVersion: 8,
			wantIndexed: map[string]bool{rosterModerator: false},
		},
		{
			name: "malformed update at M's record key clears the row",
			event: roleWriteEvent(rosterCommunity, "update", rosterModeratorKey, rosterHeadSixRev, rosterModeratorUpdatedCID,
				roleRecord(rosterModerator, "")),
			wantPermanent: true,
			wantActions:   []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterModerator)},
			wantVersion:   8,
			wantIndexed:   map[string]bool{rosterModerator: false},
		},
		{
			name:        "delete at the gate's own rev is stale and writes nothing",
			event:       roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadZeroRev),
			wantActions: []storedAction{},
			wantVersion: rosterStartVersion,
			wantIndexed: map[string]bool{rosterModerator: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.repo.landCommit(pds.LatestCommit{CID: rosterHeadZeroCID, Rev: rosterHeadFiveRev})
			f.repo.applyErr = errors.New("pds: 502 bad gateway")
			_, err := f.remove(context.Background(), rosterOwner, "remove-1")
			assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
			require.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "fixture: an open delete intent for M's record")
			if test.before != nil {
				test.before(t, f)
			}

			err = f.deliver(test.event)

			if test.wantPermanent {
				assert.ErrorIs(t, err, jetstream.ErrPermanentEvent)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, test.wantActions, f.actions(t), "never attributed to the intent's actor")
			assert.EqualValues(t, test.wantVersion, f.rosterVersion(t))
			assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "the intent stays open")
			for subject, want := range test.wantIndexed {
				assert.Equalf(t, want, f.roleIndexed(t, rosterCommunity, subject), "%s indexed", subject)
			}
		})
	}
}

// R2-1: the PDS commits O's delete at head one and the response is lost; an
// outside re-grant at M's record key lands at head two before the retry. The
// retry completes with O as the actor, advancing M's rev gate only to the
// guard rev (head zero), so the firehose then indexes the re-grant with its
// own actorless action.
func TestCommunityActionFirehose_RegrantBeforeTheRetry(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.loseRemovalResponse(t)
	f.repo.setRecord(communities.RoleCollection, rosterModeratorKey, rosterModeratorRegrantCID)
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})

	_, err := f.remove(context.Background(), rosterOwner, "remove-1")

	require.NoError(t, err, "the retry completes the removal it made")
	assert.False(t, f.moderatorRowIndexed(t), "M's row with the replaced CID is gone")
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "one action naming O")
	assert.Equal(t, rosterHeadZeroRev, f.moderatorGateRev(t), "the retry advances M's rev gate only to the guard rev")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "no second ApplyWrites")

	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadOneRev)))

	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "the delete copy changes nothing")
	assert.EqualValues(t, 8, f.rosterVersion(t))

	require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "create", rosterModeratorKey, rosterHeadTwoRev,
		rosterModeratorRegrantCID, roleRecord(rosterModerator, communities.RoleModerator))))

	assert.True(t, f.moderatorRowIndexed(t), "the re-grant indexes M again")
	assert.Equal(t, []storedAction{
		moderatorRemovalAction(rosterOwner, false),
		actorlessRoleAction("grant-role", rosterCommunity, rosterModerator),
	}, f.actions(t))
	assert.EqualValues(t, 9, f.rosterVersion(t))
}

// R1-3: the consumer handles a role event in one transaction. With the action
// insert failing, the firehose copy that would complete the open intent leaves
// the index row, the roster version, the rev gate and the intent as they were.
func TestCommunityActionFirehose_RoleEventIsOneTransaction(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.loseRemovalResponse(t)
	installActionInsertFailure(t, f.db)

	err := f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadOneRev))

	assert.Error(t, err, "the failed action insert fails the event")
	assert.True(t, f.moderatorRowIndexed(t), "M's row unchanged")
	assert.EqualValues(t, rosterStartVersion, f.rosterVersion(t), "roster version unchanged")
	assert.Equal(t, rosterHeadZeroRev, f.moderatorGateRev(t), "rev gate unchanged")
	assert.Equal(t, []storedIntent{{State: "open"}}, f.intents(t), "the intent stays open")
	assert.Empty(t, f.actions(t), "no action")
}

// R2-3: provisioning indexes the owner role locally at its commit rev and logs
// the grant with the owner as actor; the firehose copy at the same rev adds
// nothing.
func TestCommunityActionFirehose_ProvisioningGrantIsLoggedOnce(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	insertRosterCommunity(t, f.db, rosterBeta, "rosterbeta.test", true)
	_, err := f.db.Exec(`INSERT INTO community_subject_versions (community_did, subject, version) VALUES ($1, $1, 3)`, rosterBeta)
	require.NoError(t, err)

	applied, err := postgres.NewCommunityRoleRepository(f.db).UpsertCommunityRoleAtRev(ctx, communities.CommunityRole{
		CommunityDID: rosterBeta, SubjectDID: rosterOwner, Role: communities.RoleOwner,
		RecordKey: rosterOwnerKey, RecordCID: rosterBetaOwnerCID,
		RecordCreatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
	}, rosterHeadOneRev)
	require.NoError(t, err)
	require.True(t, applied, "fixture: the local index applies")
	require.NoError(t, f.deliver(roleWriteEvent(rosterBeta, "create", rosterOwnerKey, rosterHeadOneRev, rosterBetaOwnerCID,
		roleRecord(rosterOwner, communities.RoleOwner))))

	assert.Equal(t, []storedAction{{
		Action: "grant-role", ActorDID: rosterOwner, AdminActing: false,
		AuthorityDID: rosterBeta, ScopeKind: "community", ScopeCommunityDID: rosterBeta,
		SubjectURI: "at://did:plc:rosterowner", Origin: "local",
	}}, communityActionsIn(t, f.db, rosterBeta), "one grant naming the owner")
	assert.Equal(t, []string{communities.RoleOwner}, communityActionRolesIn(t, f.db, rosterBeta),
		"the grant records the owner role")
	state, err := postgres.NewCommunitySubjectStateReader(f.db).ReadCommunitySubjectState(ctx, rosterBeta, rosterBeta,
		moderation.CommunitySubjectRoster)
	require.NoError(t, err)
	assert.EqualValues(t, 4, state.Version, "roster version advanced once")
}

// R2-3 firehose first: the firehose copy of the owner record indexes it and
// logs an actorless grant before provisioning's local index runs. The local
// index loses the rev gate, and attributes that record's grant to the owner:
// one grant naming the owner, in either delivery order.
func TestCommunityActionFirehose_ProvisioningGrantFirehoseFirst(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	insertRosterCommunity(t, f.db, rosterBeta, "rosterbeta.test", true)
	_, err := f.db.Exec(`INSERT INTO community_subject_versions (community_did, subject, version) VALUES ($1, $1, 3)`, rosterBeta)
	require.NoError(t, err)
	require.NoError(t, f.deliver(roleWriteEvent(rosterBeta, "create", rosterOwnerKey, rosterHeadOneRev, rosterBetaOwnerCID,
		roleRecord(rosterOwner, communities.RoleOwner))))
	require.Equal(t, []storedAction{actorlessRoleAction("grant-role", rosterBeta, rosterOwner)},
		communityActionsIn(t, f.db, rosterBeta), "fixture: the firehose copy logs an actorless grant")

	applied, err := postgres.NewCommunityRoleRepository(f.db).UpsertCommunityRoleAtRev(ctx, communities.CommunityRole{
		CommunityDID: rosterBeta, SubjectDID: rosterOwner, Role: communities.RoleOwner,
		RecordKey: rosterOwnerKey, RecordCID: rosterBetaOwnerCID,
		RecordCreatedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
	}, rosterHeadOneRev)

	require.NoError(t, err)
	assert.False(t, applied, "the local index loses the rev gate")
	assert.Equal(t, []storedAction{{
		Action: "grant-role", ActorDID: rosterOwner, AdminActing: false,
		AuthorityDID: rosterBeta, ScopeKind: "community", ScopeCommunityDID: rosterBeta,
		SubjectURI: "at://did:plc:rosterowner", Origin: "local",
	}}, communityActionsIn(t, f.db, rosterBeta), "one grant naming the owner")
	assert.Equal(t, []string{communities.RoleOwner}, communityActionRolesIn(t, f.db, rosterBeta),
		"the grant records the owner role")
	state, err := postgres.NewCommunitySubjectStateReader(f.db).ReadCommunitySubjectState(ctx, rosterBeta, rosterBeta,
		moderation.CommunitySubjectRoster)
	require.NoError(t, err)
	assert.EqualValues(t, 4, state.Version, "roster version advanced once")
}

// A lost-response removal recovered by the retry or the sweep advances M's rev
// gate to the intent's guard rev (head five), so a delayed role event from at
// or before the guard (head three) cannot restore M, while the gate stays
// below the commit (head six) for a genuine later re-grant.
func TestCommunityActionFirehose_RecoveryRejectsADelayedPreGuardEvent(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		recover func(t *testing.T, f *communityActionDBFixture)
	}{
		{name: "same-key retry", recover: func(t *testing.T, f *communityActionDBFixture) {
			_, err := f.remove(context.Background(), rosterOwner, "remove-1")
			require.NoError(t, err, "the retry completes the removal")
		}},
		{name: "expiry sweep", recover: func(t *testing.T, f *communityActionDBFixture) {
			f.pastIntentExpiry()
			swept, err := f.service.SweepExpiredIntents(context.Background())
			require.NoError(t, err)
			require.Equal(t, 1, swept, "the sweep completes the removal")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newCommunityActionDBFixture(t)
			f.repo.landCommit(pds.LatestCommit{CID: rosterHeadZeroCID, Rev: rosterHeadFiveRev})
			f.repo.commitHead = pds.LatestCommit{CID: rosterHeadOneCID, Rev: rosterHeadSixRev}
			f.loseRemovalResponse(t)

			test.recover(t, f)

			assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
			assert.Equal(t, rosterHeadFiveRev, f.moderatorGateRev(t), "the gate is advanced to the intent's guard rev")

			require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "update", rosterModeratorKey, rosterHeadThreeRev,
				rosterModeratorCID, roleRecord(rosterModerator, communities.RoleModerator))))

			assert.False(t, f.moderatorRowIndexed(t), "the delayed pre-guard event does not restore M")
			assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t), "one action naming O")
			assert.EqualValues(t, 8, f.rosterVersion(t))

			require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadSixRev)))

			assert.Equal(t, []storedAction{moderatorRemovalAction(rosterOwner, false)}, f.actions(t),
				"the delete's own copy adds no action")
			assert.EqualValues(t, 8, f.rosterVersion(t))
		})
	}
}

// R3: an empty-rev firehose delete (no gate, no intent match) removes M's row
// with an actorless action after the lost response. The retry then completes
// the intent against that action: no second action and no second version.
func TestCommunityActionFirehose_EmptyRevDeleteBeforeTheRetry(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.loseRemovalResponse(t)
	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, "")))
	require.Equal(t, []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterModerator)}, f.actions(t),
		"fixture: the empty-rev delete logs one actorless removal")
	ids := f.actionIDs(t)
	require.Len(t, ids, 1)

	result, err := f.remove(context.Background(), rosterOwner, "remove-1")

	require.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Equal(t, ids[0], result.ActionID, "the result names the action that logged the removal")
		assert.EqualValues(t, 8, result.Version)
	}
	assert.Equal(t, []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterModerator)}, f.actions(t),
		"one action for one record change")
	assert.EqualValues(t, 8, f.rosterVersion(t), "the roster version advanced once")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t))
}

// R3: an empty-rev firehose delete logs an actorless removal of M's record
// after the lost response, and a re-grant of M is indexed (a new CID at the
// same record key) before the retry. The retry finds no row at the replaced
// CID and links to the action that logged that record's removal; the re-grant
// being M's newest role action does not make it log a second removal.
func TestCommunityActionFirehose_EmptyRevDeleteThenRegrantBeforeTheRetry(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.loseRemovalResponse(t)
	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, "")))
	f.repo.setRecord(communities.RoleCollection, rosterModeratorKey, rosterModeratorRegrantCID)
	f.repo.landCommit(pds.LatestCommit{CID: rosterHeadTwoCID, Rev: rosterHeadTwoRev})
	require.NoError(t, f.deliver(roleWriteEvent(rosterCommunity, "create", rosterModeratorKey, rosterHeadTwoRev,
		rosterModeratorRegrantCID, roleRecord(rosterModerator, communities.RoleModerator))))
	wantActions := []storedAction{
		actorlessRoleAction("remove-role", rosterCommunity, rosterModerator),
		actorlessRoleAction("grant-role", rosterCommunity, rosterModerator),
	}
	require.Equal(t, wantActions, f.actions(t), "fixture: an actorless removal, then the re-grant")
	ids := f.actionIDs(t)
	require.Len(t, ids, 2)

	result, err := f.remove(context.Background(), rosterOwner, "remove-1")

	require.NoError(t, err)
	if assert.NotNil(t, result) {
		assert.Equal(t, ids[0], result.ActionID, "the result names the action that logged the record's removal")
		assert.EqualValues(t, 9, result.Version)
	}
	assert.Equal(t, wantActions, f.actions(t), "no second removal for the one record change")
	assert.True(t, f.moderatorRowIndexed(t), "the re-grant stays indexed")
	assert.EqualValues(t, 9, f.rosterVersion(t), "the retry advances no version")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadTwoRev}}, f.intents(t))
}

// An outside delete of M's record, at the CID O's intent replaced, arriving
// after that intent expired: the expired intent is not completed by it. The
// delete logs one actorless action and the intent stays expired.
func TestCommunityActionFirehose_DeleteAfterTheIntentExpired(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	ctx := context.Background()
	f.repo.applyErr = errors.New("pds: 502 bad gateway")
	_, err := f.remove(ctx, rosterOwner, "remove-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	f.pastIntentExpiry()
	swept, err := f.service.SweepExpiredIntents(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, swept, "fixture: the sweep reconciles the intent")
	require.Equal(t, []storedIntent{{State: "expired"}}, f.intents(t), "fixture: the intent expired")
	require.True(t, f.moderatorRowIndexed(t), "fixture: M is indexed at the intent's replaced CID")

	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadSixRev)))

	assert.False(t, f.moderatorRowIndexed(t), "the outside delete removes M's row")
	assert.Equal(t, []storedAction{actorlessRoleAction("remove-role", rosterCommunity, rosterModerator)}, f.actions(t),
		"one actorless action, not O's")
	assert.EqualValues(t, 8, f.rosterVersion(t))
	assert.Equal(t, []storedIntent{{State: "expired"}}, f.intents(t), "the intent stays expired")

	result, err := f.remove(ctx, rosterOwner, "remove-1")
	assert.ErrorIs(t, err, moderation.ErrRoleNotFound, "O's key holds no result from the outside delete")
	assert.Nil(t, result)
}

// B11 for an instance admin: the firehose copy completes the admin's open
// intent from the stored row, which keeps the admin-acting mark; the retry
// replays that result.
func TestCommunityActionFirehose_AdminLostResponseFirehoseFirst(t *testing.T) {
	t.Parallel()
	f := newCommunityActionDBFixture(t)
	f.repo.applyThenErr = errors.New("pds: connection reset by peer")
	_, err := f.remove(context.Background(), rosterAdmin, "admin-remove-1")
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable, "the lost response reads as unavailable")

	require.NoError(t, f.deliver(roleDeleteEvent(rosterCommunity, rosterModeratorKey, rosterHeadOneRev)))

	assert.False(t, f.moderatorRowIndexed(t), "M's row is gone")
	assert.Equal(t, []storedIntent{{State: "completed", CommitRev: rosterHeadOneRev}}, f.intents(t))
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterAdmin, true)}, f.actions(t),
		"one action naming the admin, marked admin-acting")

	result, err := f.remove(context.Background(), rosterAdmin, "admin-remove-1")

	require.NoError(t, err, "the retry replays the firehose completion")
	require.NotNil(t, result)
	assert.EqualValues(t, 8, result.Version)
	assert.Equal(t, []storedAction{moderatorRemovalAction(rosterAdmin, true)}, f.actions(t), "the retry adds no action")
	assert.Len(t, f.repo.batchesSnapshot(), 1, "the retry sends no ApplyWrites")
}
