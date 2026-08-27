//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A vote and its subject always live in DIFFERENT repos — the vote in the
// voter's, a post in the community's, a comment in its author's — and Jetstream
// parallelises across repos. "The subject is already indexed" is therefore
// topology luck, not a protocol guarantee, and a vote whose subject has no row
// yet is an ordinary delivery the consumer must survive rather than a malformed
// one.
//
// The consumer cannot count such a vote when it arrives: there is no row to
// count it on. Its only correct move is to refuse the event TRANSIENTLY and
// persist nothing, so the dead letter redriver replays it once the subject
// lands — the shape post_consumer.go already uses for a post whose community it
// has not seen, and createSubscription for a subscription to an unknown
// community.
//
// # WHY THE REFUSAL MUST BE TRANSIENT AND NOT PERMANENT
//
// ErrPermanentEvent means "a replay would fail identically", and it retires the
// event. That is true of every other rejection on this path — a malformed
// record, a bad direction, a non-DID repo owner all depend only on the
// immutable payload. A missing subject is the opposite: it is a statement about
// the AppView's state at one instant, and the very next minute it is false. A
// permanent refusal here would discard the vote as surely as counting it on
// nothing does.
//
// # WHY "PERSIST NOTHING" IS PART OF THE CONTRACT AND NOT AN IMPLEMENTATION
// DETAIL
//
// An indexed-but-uncounted vote row is worse than no row. deleteVote decrements
// the subject from whatever the stored row says, and it cannot tell an
// increment that was applied from one that was skipped — so the row's eventual
// withdrawal subtracts a vote the consumer never added, taking a real voter's
// vote with it, floored at zero by GREATEST(0, …) so the drift never even looks
// wrong in the data.

const (
	subjectGatePrefix = "did:plc:jsgate"
	subjectGateVoter  = subjectGatePrefix + "voter"

	// The repo that would own the subject if the subject existed. Nothing is
	// ever inserted for it: an AT-URI is a DID, a collection and an rkey, so it
	// is fully constructible before the record it names has been indexed —
	// which is exactly how a vote comes to name a subject the AppView has never
	// seen.
	subjectGateHost = subjectGatePrefix + "host"
)

// seedActiveVote writes an active vote row directly, bypassing the consumer.
//
// Direct SQL rather than a second HandleEvent call ON PURPOSE: the behaviour
// under test is what the consumer does on the error path, and a fixture built
// by the consumer would inherit whatever that path currently does. This states
// the starting state as a fact instead.
func seedActiveVote(t *testing.T, db *sql.DB, uri, voterDID, subjectURI, direction string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())`,
		uri, "bafgateseed", "gateseed", voterDID, subjectURI, "bafgatesubject", direction,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err, "seeding the voter's pre-existing active vote")
}

// voteRowState reports whether a vote row exists at uri at all, and whether it
// is still active.
//
// Existence is asked WITHOUT a deleted_at filter deliberately. "No active row"
// would be satisfied by a consumer that inserted the vote and soft-deleted it,
// which is not the contract — the contract is that the transaction leaves no
// trace, so that the redriven replay is the one and only insert.
func voteRowState(t *testing.T, db *sql.DB, uri string) (exists, active bool) {
	t.Helper()
	var deletedAt sql.NullTime
	err := db.QueryRow(`SELECT deleted_at FROM votes WHERE uri = $1`, uri).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false
	}
	require.NoError(t, err)
	return true, !deletedAt.Valid
}

// TestVoteConsumer_SubjectNotIndexed_RefusesTransientlyAndPersistsNothing is the
// unit-level statement of the ordering gate that
// tests/e2e/vote_contract_test.go's TestVoteBeforeSubjectIsCountedOnceSubjectIndexed
// observes end-to-end.
//
// Both subject kinds the vote consumer routes counts for are covered, because
// they are separate branches of the same switch (posts.IsPostCollection versus
// CommentCollection) and a gate written into one of them would leave the other
// exactly as broken as it is today.
func TestVoteConsumer_SubjectNotIndexed_RefusesTransientlyAndPersistsNothing(t *testing.T) {
	t.Parallel()

	for _, subject := range []struct {
		kind       string
		collection string
		table      string
	}{
		{"a post that has not been indexed", "social.coves.community.post", "posts"},
		{"a comment that has not been indexed", CommentCollection, "comments"},
	} {
		t.Run(subject.kind, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx := context.Background()

			subjectURI := "at://" + subjectGateHost + "/" + subject.collection + "/gatesubject"

			// The subject genuinely has no row. Asserted rather than assumed:
			// if a fixture elsewhere ever put one there, every assertion below
			// would pass for the wrong reason.
			var subjectRows int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM `+subject.table+` WHERE uri = $1`, subjectURI).Scan(&subjectRows))
			require.Zero(t, subjectRows, "the subject must not be indexed for this test to mean anything")

			// The voter already holds an ACTIVE vote on this same missing
			// subject under a different rkey — the state a client produces when
			// it re-taps, since votes.voteService always writes a fresh TID
			// rather than reusing one. The consumer's stale-vote cleanup keys on
			// (voter_did, subject_uri) and runs BEFORE the count update, so this
			// row is what distinguishes a gate placed at the top of the
			// transaction from one bolted on at the count step.
			seededURI := "at://" + subjectGateVoter + "/social.coves.feed.vote/gateseed"
			seedActiveVote(t, db, seededURI, subjectGateVoter, subjectURI, "up")

			consumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)

			incomingURI := "at://" + subjectGateVoter + "/social.coves.feed.vote/gatenew"
			err := consumer.HandleEvent(ctx, &JetstreamEvent{
				Kind:   "commit",
				Did:    subjectGateVoter,
				TimeUS: time.Now().UnixMicro(),
				Commit: &CommitEvent{
					Rev:        "3lgatetestaa2a",
					Operation:  "create",
					Collection: "social.coves.feed.vote",
					RKey:       "gatenew",
					CID:        "bafgatenewvote",
					Record: map[string]interface{}{
						"$type":     "social.coves.feed.vote",
						"subject":   map[string]interface{}{"uri": subjectURI, "cid": "bafgatesubject"},
						"direction": "up",
						"createdAt": "2026-03-01T01:00:00Z",
					},
				},
			})

			require.Error(t, err,
				"a vote naming a subject with no row must be REFUSED, not accepted: accepting it "+
					"indexes a vote whose count update matches zero rows, and nothing ever revisits it")
			assert.False(t, errors.Is(err, ErrPermanentEvent),
				"the refusal must be TRANSIENT so the dead letter redriver replays it once the "+
					"subject is indexed — ErrPermanentEvent retires the event and loses the vote as "+
					"surely as counting it on nothing does. Got: %v", err)

			exists, _ := voteRowState(t, db, incomingURI)
			assert.False(t, exists,
				"the refused vote must leave NO row at %s, soft-deleted or otherwise: a row the "+
					"consumer never counted is one deleteVote will later decrement anyway, "+
					"subtracting a vote it never added", incomingURI)

			_, seededActive := voteRowState(t, db, seededURI)
			assert.True(t, seededActive,
				"the voter's pre-existing vote must still be ACTIVE at %s: the stale-vote cleanup "+
					"soft-deletes it and decrements its direction, so a gate placed after that step "+
					"withdraws a counted vote on an event that was refused anyway", seededURI)

			var activeRows int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM votes WHERE voter_did = $1 AND deleted_at IS NULL`,
				subjectGateVoter).Scan(&activeRows))
			assert.Equal(t, 1, activeRows,
				"the refused event must leave the voter's vote rows exactly as it found them")
		})
	}
}
