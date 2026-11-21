//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every community moderation index change the firehose applies advances the
// subject's community version by exactly one, in the transaction that made the
// change, and postgres.NewCommunitySubjectStateReader reads the version together
// with the subject's state. These tests drive the real consumers and read back
// only through the reader.

// markHosted marks a community as hosted by this instance (a stored PDS
// refresh token).
func markHosted(t *testing.T, db *sql.DB, communityDID string) {
	t.Helper()
	_, err := db.Exec(`UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`,
		communityDID, []byte("hosted"))
	require.NoError(t, err)
}

// readSubjectState reads one subject's community state through the reader and
// checks the identity fields it echoes.
func readSubjectState(t *testing.T, db *sql.DB, communityDID, subject string, kind moderation.CommunitySubjectKind) *moderation.CommunityState {
	t.Helper()
	state, err := postgres.NewCommunitySubjectStateReader(db).ReadCommunitySubjectState(context.Background(), communityDID, subject, kind)
	require.NoError(t, err)
	require.NotNil(t, state, "the reader must return the %s state of %s in %s", kind, subject, communityDID)
	assert.Equal(t, communityDID, state.CommunityDID)
	assert.Equal(t, subject, state.Subject)
	assert.Equal(t, kind, state.Kind)
	return state
}

// B1: a firehose acceptance, then a firehose removal of the same post, then
// the removal record deleted, a fresh acceptance, and that acceptance record
// deleted. The fresh acceptance is needed because a removal clears the stored
// acceptance rkey, and an acceptance delete resolves its subject only from it.
func TestCommunitySubjectState_PostAcceptanceThenRemoval(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	f := newAccFixture(t, db)
	markHosted(t, db, accCommunity)
	ctx := context.Background()
	base := time.Now().UnixMicro()
	revs := increasingTIDs(t, 5)

	const cid = "bafyreicssacceptremove"
	uri := f.indexPV2(t, "cssacceptremove", cid, base)

	require.NoError(t, f.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, uri, cid, revs[0], base+1_000_000)))
	accepted := readSubjectState(t, db, accCommunity, uri, "post")
	assert.Equal(t, "accepted", accepted.AdmissionStatus)
	require.GreaterOrEqual(t, accepted.Version, int64(1), "the acceptance must advance the post's version")

	require.NoError(t, f.consumer.HandleEvent(ctx, removalEvent(
		accCommunity, uri, cid, string(posts.DecisionRuleViolation), revs[1], base+2_000_000)))
	removed := readSubjectState(t, db, accCommunity, uri, "post")
	assert.Equal(t, "removed", removed.AdmissionStatus)
	assert.Equal(t, accepted.Version+1, removed.Version, "the removal must advance the post's version by exactly one")

	require.NoError(t, f.consumer.HandleEvent(ctx, removalDeleteEvent(accCommunity, uri, revs[2], base+3_000_000)))
	reopened := readSubjectState(t, db, accCommunity, uri, "post")
	assert.Equal(t, "pending", reopened.AdmissionStatus, "deleting the removal record must reopen the post")
	assert.Equal(t, removed.Version+1, reopened.Version, "the removal delete must advance the post's version by exactly one")

	require.NoError(t, f.consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, uri, cid, revs[3], base+4_000_000)))
	reaccepted := readSubjectState(t, db, accCommunity, uri, "post")
	require.Equal(t, "accepted", reaccepted.AdmissionStatus, "fixture: the fresh acceptance must stand")

	require.NoError(t, f.consumer.HandleEvent(ctx, revCommitEvent(accCommunity, posts.AcceptanceCollection, "delete",
		posts.SubjectRkey(uri), revs[4], "", base+5_000_000, nil)))
	withdrawn := readSubjectState(t, db, accCommunity, uri, "post")
	assert.Equal(t, "pending", withdrawn.AdmissionStatus, "deleting the acceptance record must withdraw the acceptance")
	assert.Equal(t, reaccepted.Version+1, withdrawn.Version, "the acceptance delete must advance the post's version by exactly one")
}

// B2: a community removal of a comment, applied directly or held until the
// comment is indexed, and its retraction.
func TestCommunitySubjectState_CommentRemoval(t *testing.T) {
	t.Parallel()

	// csStep is one delivery; check asks for the comment's state after it,
	// with the version exactly delta above the previous read.
	type csStep struct {
		step        ccrStep
		check       bool
		delta       int64
		wantRemoved bool
	}
	tests := []struct {
		name  string
		steps []csStep
	}{
		{
			name: "removal of an indexed comment, then the removal record deleted",
			steps: []csStep{
				{step: stepRoot},
				{step: stepComment, check: true},
				{step: stepRemovalC, check: true, delta: 1, wantRemoved: true},
				{step: stepRemovalDeleteC, check: true, delta: 1, wantRemoved: false},
			},
		},
		{
			name: "removal held until its comment is indexed",
			steps: []csStep{
				{step: stepRoot},
				{step: stepRemovalC, check: true},
				{step: stepComment, check: true, delta: 1, wantRemoved: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCCRFixture(t)
			markHosted(t, f.db, accCommunity)
			ctx := context.Background()
			base := time.Now().UnixMicro()
			revs := increasingTIDs(t, len(tt.steps))

			const (
				rootRkey    = "cssroot"
				rootCID     = "bafyreicssroot"
				commentRkey = "csscomment"
				commentCID  = "bafyreicsscomment"
			)
			rootURI := accPostURI(rootRkey)
			commentURI := ccrCommentURI(commentRkey)

			var last *moderation.CommunityState
			for i, s := range tt.steps {
				rev, timeUS := revs[i], base+int64(i+1)*1_000_000
				switch s.step {
				case stepRoot:
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(
						accAuthor, "create", rootRkey, rev, rootCID, timeUS,
						pv2Record(accCommunity, "thread root", "root body"))), "root post")
				case stepComment:
					require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
						ccrCommenter, CommentCollection, "create", commentRkey, rev, commentCID, timeUS,
						revCommentRecord("the comment", rootURI, rootCID, rootURI, rootCID))), "comment")
				case stepRemovalC:
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
						accCommunity, commentURI, commentCID, string(posts.DecisionRuleViolation), rev, timeUS)), "removal from C")
				case stepRemovalDeleteC:
					require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalDeleteEvent(
						accCommunity, commentURI, rev, timeUS)), "removal delete from C")
				default:
					t.Fatalf("step %d: unsupported step %d", i, s.step)
				}
				if !s.check {
					continue
				}
				state := readSubjectState(t, f.db, accCommunity, commentURI, "comment")
				if last != nil {
					assert.Equalf(t, last.Version+s.delta, state.Version, "step %d: the comment's version must advance by exactly %d", i, s.delta)
					assert.Equalf(t, s.wantRemoved, state.CommentRemoved, "step %d: the comment's removed state", i)
				}
				last = state
			}
		})
	}
}

// B3: a ban record written outside Coves, its deletion, and in-force reads.
func TestCommunitySubjectState_AccountBan(t *testing.T) {
	t.Parallel()

	newBanFixture := func(t *testing.T) (*sql.DB, accFixture) {
		t.Helper()
		db := testkit.DB(t)
		f := newAccFixture(t, db, WithPostCommunityBans(postgres.NewCommunityBanRepository(db)))
		markHosted(t, db, accCommunity)
		return db, f
	}

	t.Run("indexed ban, then the ban record deleted", func(t *testing.T) {
		t.Parallel()
		db, f := newBanFixture(t)
		ctx := context.Background()
		base := time.Now().UnixMicro()
		revs := increasingTIDs(t, 2)
		const subject = "did:plc:csbansubject"

		before := readSubjectState(t, db, accCommunity, subject, "account")
		assert.Nil(t, before.Ban, "no ban is indexed yet")

		require.NoError(t, f.consumer.HandleEvent(ctx, banCommitEvent(banStep{
			repo: accCommunity, op: "create", subject: subject, rev: revs[0],
			cid: "bafyreicsban", reason: "spam", expiresAt: "2099-01-01T00:00:00Z",
		}, base)))
		banned := readSubjectState(t, db, accCommunity, subject, "account")
		require.NotNil(t, banned.Ban, "the indexed ban must be in force")
		assert.Equal(t, "spam", banned.Ban.Reason)
		require.NotNil(t, banned.Ban.ExpiresAt)
		assert.True(t, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Equal(*banned.Ban.ExpiresAt),
			"expiresAt = %v, want 2099-01-01T00:00:00Z", *banned.Ban.ExpiresAt)
		assert.Greater(t, banned.Version, before.Version, "indexing the ban must advance the account's version")

		require.NoError(t, f.consumer.HandleEvent(ctx, banCommitEvent(banStep{
			repo: accCommunity, op: "delete", subject: subject, rev: revs[1],
		}, base+1_000_000)))
		unbanned := readSubjectState(t, db, accCommunity, subject, "account")
		assert.Nil(t, unbanned.Ban, "a deleted ban is not in force")
		assert.Greater(t, unbanned.Version, banned.Version, "deleting the ban must advance the account's version")
	})

	tests := []struct {
		name      string
		expiresAt string
		inForce   bool
	}{
		{name: "a ban that expired in 2001 is not in force but keeps its version", expiresAt: "2001-01-01T00:00:00Z"},
		{name: "a ban without an expiry is in force", inForce: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db, f := newBanFixture(t)
			const subject = "did:plc:csbanexpiry"

			require.NoError(t, f.consumer.HandleEvent(context.Background(), banCommitEvent(banStep{
				repo: accCommunity, op: "create", subject: subject, rev: testkit.TID(),
				cid: "bafyreicsbanexpiry", reason: "trolling", expiresAt: tt.expiresAt,
			}, time.Now().UnixMicro())))

			state := readSubjectState(t, db, accCommunity, subject, "account")
			assert.Positive(t, state.Version, "an indexed ban gives the account a version")
			if !tt.inForce {
				assert.Nil(t, state.Ban, "an expired ban is not in force")
				return
			}
			require.NotNil(t, state.Ban, "a ban without an expiry is in force")
			assert.Equal(t, "trolling", state.Ban.Reason)
			assert.Nil(t, state.Ban.ExpiresAt)
		})
	}
}

// B4: role records created, rewritten and deleted through the communities
// consumer each advance the roster version by exactly one.
func TestCommunitySubjectState_RosterVersion(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	consumer, _ := newRosterConsumer(t, db)
	revs := increasingTIDs(t, 5)

	tests := []struct {
		step           rosterStep
		wantOwner      string
		wantModerators []string
	}{
		{
			step: rosterStep{
				repo: rosterCommunityA, op: "create", subject: rosterSubject1, rkey: rosterSubject1Key,
				role: "moderator", createdAt: rosterTime1, rev: revs[0], cid: "bafyreicsroster1",
			},
			wantModerators: []string{"did:plc:rostersubject1"},
		},
		{
			step: rosterStep{
				repo: rosterCommunityA, op: "create", subject: rosterSubject2, rkey: rosterSubject2Key,
				role: "owner", createdAt: rosterTime1, rev: revs[1], cid: "bafyreicsroster2",
			},
			wantOwner:      "did:plc:rostersubject2",
			wantModerators: []string{"did:plc:rostersubject1"},
		},
		{
			// A rewrite that changes only the record CID.
			step: rosterStep{
				repo: rosterCommunityA, op: "update", subject: rosterSubject1, rkey: rosterSubject1Key,
				role: "moderator", createdAt: rosterTime1, rev: revs[2], cid: "bafyreicsroster1b",
			},
			wantOwner:      "did:plc:rostersubject2",
			wantModerators: []string{"did:plc:rostersubject1"},
		},
		{
			// A rewrite that changes the role.
			step: rosterStep{
				repo: rosterCommunityA, op: "update", subject: rosterSubject2, rkey: rosterSubject2Key,
				role: "moderator", createdAt: rosterTime2, rev: revs[3], cid: "bafyreicsroster2b",
			},
			wantModerators: []string{"did:plc:rostersubject1", "did:plc:rostersubject2"},
		},
		{
			step:           rosterStep{repo: rosterCommunityA, op: "delete", rkey: rosterSubject1Key, rev: revs[4]},
			wantModerators: []string{"did:plc:rostersubject2"},
		},
	}

	last := readSubjectState(t, db, rosterCommunityA, rosterCommunityA, "roster")
	for i, tt := range tests {
		deliverRosterSteps(t, consumer, []rosterStep{tt.step})
		state := readSubjectState(t, db, rosterCommunityA, rosterCommunityA, "roster")
		assert.Equalf(t, last.Version+1, state.Version, "step %d (%s %s): the roster version must advance by exactly one", i, tt.step.op, tt.step.rkey)
		require.NotNilf(t, state.Roster, "step %d: the roster kind carries the roster", i)
		var owner string
		if state.Roster.Owner != nil {
			owner = state.Roster.Owner.SubjectDID
		}
		assert.Equalf(t, tt.wantOwner, owner, "step %d: the roster owner", i)
		moderators := make([]string, 0, len(state.Roster.Moderators))
		for _, moderator := range state.Roster.Moderators {
			moderators = append(moderators, moderator.SubjectDID)
		}
		assert.ElementsMatchf(t, tt.wantModerators, moderators, "step %d: the roster moderators", i)
		last = state
	}
}
