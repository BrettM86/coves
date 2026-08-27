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

	// Fixtures for the deleted-subject branch. A post lives in its community's
	// repo and a comment in its author's, so the two variants build their
	// subject URI from different DIDs — the URI's repo segment is not what the
	// vote consumer routes on, but a fixture that spelled it wrong would model a
	// record atProto cannot produce.
	deletedSubjectCommunity = subjectGatePrefix + "delcommunity"
	deletedSubjectAuthor    = subjectGatePrefix + "delauthor"
	deletedSubjectVoter     = subjectGatePrefix + "delvoter"
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

// seedIndexedPost inserts a post row directly, which is what "the subject
// finally arrived" looks like from the vote consumer's side.
//
// Direct SQL rather than a PostEventConsumer round-trip because this test is
// about the VOTE consumer's second attempt, and routing the fixture through
// another consumer would put that consumer's behaviour — its own rev gate, its
// community checks — between the setup and the thing being measured. The FK
// rows are seeded because posts.author_did and posts.community_did reference
// users and communities; nothing else about them matters here.
func seedIndexedPost(t *testing.T, db *sql.DB, uri, communityDID, authorDID, rkey string) {
	t.Helper()
	insertBridgedUser(t, db, authorDID, "redriveauthor.test")
	insertBridgedCommunity(t, db, communityDID, "redrivecommunity.test", authorDID)
	_, err := db.Exec(`
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uri, "bafredrivesubject", rkey, authorDID, communityDID, "the late subject",
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err, "seeding the subject that arrives after the vote")
}

// TestVoteConsumer_SubjectIndexedAfterRefusal_IdenticalReplayIsCounted is the
// other half of the ordering gate, and the half that is easy to build wrong.
//
// The gate's refusal is only useful if the SAME event can succeed later, and
// "the same event" is meant literally: the dead letter redriver stores the
// event as it arrived and re-invokes HandleEvent with it. Same URI, same CID,
// same rev, same everything. The replay is therefore indistinguishable from a
// duplicate delivery — and the vote consumer has a machine whose entire job is
// to reject those, the per-record rev gate, where an incoming rev that is not
// strictly greater than the stored one loses.
//
// So the refusal must roll the gate's advance back along with the rest of the
// transaction. If it ever did not — the check moved outside the transaction, an
// early commit before the error return, a gate advance written through a
// different connection — the redriven replay would be rejected as stale and the
// vote lost PERMANENTLY, and the only trace would be a routine-looking
//
//	rev-gate: votes skipped stale create for at://… (incoming rev "…" <= stored)
//
// which is a line the logs are full of for genuinely stale events. No count
// goes wrong, no error is raised, and no dead letter is left behind: the
// failure is a vote that silently never existed. This test is the only thing
// that would notice, which is why it asserts on the SECOND call's success
// rather than on the gate's internals.
func TestVoteConsumer_SubjectIndexedAfterRefusal_IdenticalReplayIsCounted(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	const (
		redriveCommunity = subjectGatePrefix + "redrivecommunity"
		redriveAuthor    = subjectGatePrefix + "redriveauthor"
		redriveVoter     = subjectGatePrefix + "redrivevoter"
		redriveRKey      = "redrivesubject"
	)
	subjectURI := "at://" + redriveCommunity + "/social.coves.community.post/" + redriveRKey
	voteURI := "at://" + redriveVoter + "/social.coves.feed.vote/redrivevote"

	consumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)

	// ONE event value, handled twice. Built once and reused rather than built
	// twice from the same literals, so that "identical" is guaranteed by the
	// code rather than by two spellings happening to agree — the rev in
	// particular is what the second call's success turns on.
	event := &JetstreamEvent{
		Kind:   "commit",
		Did:    redriveVoter,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Rev:        "3lgateredriveaa2a",
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       "redrivevote",
			CID:        "bafgateredrivevote",
			Record: map[string]interface{}{
				"$type":     "social.coves.feed.vote",
				"subject":   map[string]interface{}{"uri": subjectURI, "cid": "bafredrivesubject"},
				"direction": "up",
				"createdAt": "2026-03-01T01:00:00Z",
			},
		},
	}

	// ---- the first delivery, before the subject exists ----------------------
	err := consumer.HandleEvent(ctx, event)
	require.Error(t, err, "the vote must be refused while its subject has no row")
	require.False(t, errors.Is(err, ErrPermanentEvent),
		"the refusal must be transient or the redriver would never replay it at all, and the "+
			"rest of this test would be measuring nothing. Got: %v", err)

	// ---- the subject arrives -------------------------------------------------
	seedIndexedPost(t, db, subjectURI, redriveCommunity, redriveAuthor, redriveRKey)

	// ---- the redriver replays the stored event verbatim ----------------------
	require.NoError(t, consumer.HandleEvent(ctx, event),
		"the identical event must succeed once its subject is indexed. A rev-gate rejection here "+
			"means the refused attempt left its rev advance committed, so every redrive of a "+
			"deferred vote is silently discarded as stale")

	exists, active := voteRowState(t, db, voteURI)
	assert.True(t, exists, "the replayed vote must be indexed at %s", voteURI)
	assert.True(t, active, "the replayed vote must be ACTIVE, not soft-deleted")

	upvotes, downvotes, score, _ := readDupPostCounts(t, db, subjectURI)
	assert.Equal(t, 1, upvotes, "the deferred vote must be counted on the subject that finally arrived")
	assert.Equal(t, 0, downvotes)
	assert.Equal(t, 1, score, "score must reflect the one recovered upvote")
}

// subjectCounts is the (upvotes, downvotes, score) triple both subject tables
// carry, read through a query the caller supplies so that no table name is ever
// concatenated into SQL.
type subjectCounts struct {
	Upvotes   int
	Downvotes int
	Score     int
}

func readSubjectCounts(t *testing.T, db *sql.DB, query, uri string) subjectCounts {
	t.Helper()
	var counts subjectCounts
	require.NoError(t, db.QueryRow(query, uri).Scan(&counts.Upvotes, &counts.Downvotes, &counts.Score))
	return counts
}

// TestVoteConsumer_SubjectSoftDeleted_DropsTheVoteEntirely covers the branch the
// ordering gate deliberately lets through, and which needs the OPPOSITE
// treatment from an absent subject.
//
// # WHY THIS IS A DROP AND NOT A TRANSIENT REFUSAL
//
// The two cases look alike at the count update — both leave nothing to
// increment — but they differ in what the future holds. An absent subject is a
// statement about ordering that time will falsify, so refusing transiently and
// letting the redriver replay is exactly right. A DELETED subject is not going
// to un-delete because a redriver asked again ten times; refusing it would turn
// every vote on every deleted post into ten pointless replays and a dead letter,
// burying the dead letter queue's real signal in noise.
//
// # WHY IT IS A DROP AND NOT TODAY'S INDEX-WITHOUT-COUNTING
//
// This is the part that makes the test worth writing rather than a matter of
// taste, and it turns on a fact about the neighbouring domain: COMMENTS CAN BE
// RESURRECTED. comment_consumer.go's re-create path clears deleted_at and
// deliberately preserves the comment's native vote counts, so a deleted comment
// is not a terminal state — it is a pause.
//
// An indexed-but-uncounted vote on a deleted subject is therefore a time bomb
// with a fuse of unknown length. The subject comes back carrying the counts it
// had before; the uncounted vote is still sitting there, live and undeleted;
// and when it is eventually withdrawn, deleteVote decrements the resurrected
// subject for an increment that was never applied. That is precisely the drift
// the ordering gate was built to kill, surviving inside the one branch the gate
// waves through — and it takes a real voter's vote with it, floored at zero by
// GREATEST(0, …) so it never looks wrong in the data.
//
// Dropping the vote is what makes the deleted branch safe under resurrection:
// no row, nothing to withdraw, nothing to subtract.
//
// # WHY THE REV GATE MUST COMMIT HERE AND ROLL BACK IN B1
//
// The drop is a decision, not a deferral, so it leaves a tombstone: the gate
// advance commits, and a duplicate delivery of the same commit is answered by
// the cheap gate check instead of re-running the subject lookup. That is the
// same shape deleteVote's not-found branch already has, and asserting it is what
// distinguishes this path from
// TestVoteConsumer_SubjectNotIndexed_RefusesTransientlyAndPersistsNothing, where
// the advance must NOT survive.
func TestVoteConsumer_SubjectSoftDeleted_DropsTheVoteEntirely(t *testing.T) {
	t.Parallel()

	for _, subject := range []struct {
		kind        string
		repoDID     string
		collection  string
		seed        func(t *testing.T, db *sql.DB, uri string)
		countsQuery string
	}{
		{
			kind:       "a post that has been deleted",
			repoDID:    deletedSubjectCommunity,
			collection: "social.coves.community.post",
			seed: func(t *testing.T, db *sql.DB, uri string) {
				t.Helper()
				insertBridgedUser(t, db, deletedSubjectAuthor, "deletedauthor.test")
				insertBridgedCommunity(t, db, deletedSubjectCommunity, "deletedcommunity.test", deletedSubjectAuthor)
				_, err := db.Exec(`
					INSERT INTO posts (uri, cid, rkey, author_did, community_did, title,
					                   created_at, upvote_count, downvote_count, score, deleted_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, 3, 0, 3, NOW())`,
					uri, "bafdeletedsubject", "deletedsubject", deletedSubjectAuthor,
					deletedSubjectCommunity, "the deleted subject",
					time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
				require.NoError(t, err)
			},
			countsQuery: `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`,
		},
		{
			kind:       "a comment that has been deleted",
			repoDID:    deletedSubjectAuthor,
			collection: CommentCollection,
			seed: func(t *testing.T, db *sql.DB, uri string) {
				t.Helper()
				root := "at://" + deletedSubjectCommunity + "/social.coves.community.post/deletedroot"
				_, err := db.Exec(`
					INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid,
					                      parent_uri, parent_cid, content, created_at,
					                      upvote_count, downvote_count, score, deleted_at)
					VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, $8, 3, 0, 3, NOW())`,
					uri, "bafdeletedsubject", "deletedsubject", deletedSubjectAuthor,
					root, "bafdeletedroot", "the deleted subject",
					time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
				require.NoError(t, err)
			},
			countsQuery: `SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`,
		},
	} {
		t.Run(subject.kind, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx := context.Background()

			subjectURI := "at://" + subject.repoDID + "/" + subject.collection + "/deletedsubject"
			subject.seed(t, db, subjectURI)

			// The subject is seeded with counts ALREADY ON IT — the votes it
			// collected while it was alive. Nonzero on purpose: "the counts did
			// not change" is a claim that says nothing against a row of zeroes,
			// and these are exactly the counts a resurrection would revive.
			before := readSubjectCounts(t, db, subject.countsQuery, subjectURI)
			require.Equal(t, subjectCounts{Upvotes: 3, Score: 3}, before,
				"the fixture must start with counts a spurious mutation could disturb")

			consumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)

			const voteRev = "3lgatedeletedaa2a"
			voteURI := "at://" + deletedSubjectVoter + "/social.coves.feed.vote/deletedvote"
			event := &JetstreamEvent{
				Kind:   "commit",
				Did:    deletedSubjectVoter,
				TimeUS: time.Now().UnixMicro(),
				Commit: &CommitEvent{
					Rev:        voteRev,
					Operation:  "create",
					Collection: "social.coves.feed.vote",
					RKey:       "deletedvote",
					CID:        "bafgatedeletedvote",
					Record: map[string]interface{}{
						"$type":     "social.coves.feed.vote",
						"subject":   map[string]interface{}{"uri": subjectURI, "cid": "bafdeletedsubject"},
						"direction": "up",
						"createdAt": "2026-03-01T01:00:00Z",
					},
				},
			}

			require.NoError(t, consumer.HandleEvent(ctx, event),
				"a vote on a deleted subject must be DROPPED, not refused: a deleted subject will "+
					"not un-delete because a redriver asked again, so a transient error here buys "+
					"ten replays and a dead letter for every vote on every deleted post")

			exists, _ := voteRowState(t, db, voteURI)
			assert.False(t, exists,
				"no row may land at %s, active or soft-deleted. A deleted COMMENT can be "+
					"resurrected — comment_consumer.go's re-create path clears deleted_at and keeps "+
					"the native counts — so an indexed-but-uncounted vote here is a decrement waiting "+
					"for the subject to come back", voteURI)

			assert.Equal(t, before, readSubjectCounts(t, db, subject.countsQuery, subjectURI),
				"the deleted subject's counts must be exactly as they were: they are what a "+
					"resurrection restores, and this vote was never counted among them")

			// ---- the same commit, delivered twice ---------------------------
			// A rewind duplicate or a redrive of some other event in the same
			// batch. The drop must be as final the second time as the first.
			require.NoError(t, consumer.HandleEvent(ctx, event),
				"a duplicate delivery of a dropped vote must also be a no-op")

			exists, _ = voteRowState(t, db, voteURI)
			assert.False(t, exists, "the duplicate delivery must not resurrect the dropped vote as a row")
			assert.Equal(t, before, readSubjectCounts(t, db, subject.countsQuery, subjectURI),
				"the duplicate delivery must not move the deleted subject's counts either")

			// The drop is a DECISION and leaves a tombstone, unlike the ordering
			// refusal, which must leave the gate untouched so its replay can win.
			// Equal rev reads as stale precisely when the advance committed.
			stale, err := NewRevGate(db).IsStale(ctx, voteURI, voteRev)
			require.NoError(t, err)
			assert.True(t, stale,
				"the dropped vote's rev advance must COMMIT, so a duplicate delivery is answered by "+
					"the gate rather than by re-deciding the drop — the opposite of the ordering "+
					"refusal, whose advance must roll back or its own redrive would be rejected")
		})
	}
}
