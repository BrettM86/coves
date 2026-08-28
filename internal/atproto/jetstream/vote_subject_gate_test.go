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

	// Fixtures for the bridged-subject branch. The author DID is never inserted
	// anywhere: a bridged Bluesky account has no row in this database, which is
	// the whole point of the branch.
	bridgedSubjectAuthor = subjectGatePrefix + "bridgedauthor"
	bridgedSubjectVoter  = subjectGatePrefix + "bridgedvoter"

	// Fixtures for the erased-subject branch. erasedSubjectAuthor gets a
	// migration-036 marker and nothing else — the erasure flow hard-deletes the
	// rows, so the marker is the only trace an erased account leaves.
	erasedSubjectAuthor  = subjectGatePrefix + "erasedauthor"
	erasedSubjectUnknown = subjectGatePrefix + "unknownerasure"
	erasedSubjectVoter   = subjectGatePrefix + "erasedvoter"

	// Fixtures for the withdrawal-through-a-deleted-window arc.
	withdrawCommunity = subjectGatePrefix + "wdcommunity"
	withdrawAuthor    = subjectGatePrefix + "wdauthor"
	withdrawVoter     = subjectGatePrefix + "wdvoter"
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

// TestVoteConsumer_MalformedSubjectURI_IsRejectedPermanently covers the input
// the ordering gate mistakes for an ordering problem.
//
// A subject URI that is not a well-formed at:// record URI names nothing this
// AppView could ever index, so the subject lookup finds no row and the gate
// answers what it answers for a late subject: refuse transiently, wait for the
// subject to arrive. It never arrives. The event burns its whole redrive budget
// and settles in the dead letter queue permanently, where it is
// indistinguishable from a genuine backlog — the same noise the erased-account
// drop exists to avoid, arriving through a malformed payload instead.
//
// The distinction the consumer has to make is not "is there a row" but "could
// there ever be one", and for a malformed URI the answer is decidable from the
// immutable payload alone. That is the definition of ErrPermanentEvent, and it
// is what every other payload-shaped rejection in validateVoteEvent already
// returns.
//
// The three cases are the three ways parseRecordURI (authorpost.go) refuses,
// because a strict parse is what the routing downstream already assumes: the
// collection is read out of the URI's second segment to pick a table, so a URI
// with the segments in the wrong places routes on a value that was never a
// collection. Note that a BRIDGED subject — at://did/app.bsky.feed.post/rkey —
// parses cleanly and must keep doing so; what is rejected here is malformed
// structure, never an unfamiliar collection.
func TestVoteConsumer_MalformedSubjectURI_IsRejectedPermanently(t *testing.T) {
	t.Parallel()

	for _, malformed := range []struct {
		name string
		uri  string
	}{
		{
			// The countable collection sits in segment position, so a parser
			// that split on "/" without demanding the scheme would route this
			// to `posts` and look up a row keyed on a string no record has.
			name: "no at:// scheme",
			uri:  subjectGateHost + "/social.coves.community.post/malformedsubject",
		},
		{
			name: "no rkey",
			uri:  "at://" + subjectGateHost + "/social.coves.community.post",
		},
		{
			name: "an extra path segment",
			uri:  "at://" + subjectGateHost + "/social.coves.community.post/malformedsubject/extra",
		},
	} {
		t.Run(malformed.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx := context.Background()

			consumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)

			voteURI := "at://" + subjectGateVoter + "/social.coves.feed.vote/malformedvote"
			err := consumer.HandleEvent(ctx, &JetstreamEvent{
				Kind:   "commit",
				Did:    subjectGateVoter,
				TimeUS: time.Now().UnixMicro(),
				Commit: &CommitEvent{
					Rev:        "3lgatemalformedaa2a",
					Operation:  "create",
					Collection: "social.coves.feed.vote",
					RKey:       "malformedvote",
					CID:        "bafgatemalformedvote",
					Record: map[string]interface{}{
						"$type":     "social.coves.feed.vote",
						"subject":   map[string]interface{}{"uri": malformed.uri, "cid": "bafgatesubject"},
						"direction": "up",
						"createdAt": "2026-03-01T01:00:00Z",
					},
				},
			})

			require.Error(t, err, "a vote whose subject URI is not a record URI must be refused")
			assert.True(t, errors.Is(err, ErrPermanentEvent),
				"the refusal must be PERMANENT: %q can never name an indexed record, so the transient "+
					"refusal the absent-subject gate returns buys ten redrives and a permanent dead "+
					"letter for a verdict the payload alone already settles. Got: %v", malformed.uri, err)

			exists, _ := voteRowState(t, db, voteURI)
			assert.False(t, exists,
				"a rejected vote must leave no row at %s: an indexed row naming an unparseable subject "+
					"is viewer state no query can key on and a decrement no count can absorb", voteURI)
			requireNoCountsAnywhere(t, db)
		})
	}
}

// TestVoteConsumer_ErasureGatedReportsItsWiring pins the erasure gate the way
// RevGated pins the rev gate: as a property boot can read off the consumer.
//
// The rev gate is hardwired — RevGated returns a constant true because c.db is
// always present — but the erasure gate arrives through an option, and an option
// that is not passed produces a consumer that is perfectly functional and
// quietly wrong. subjectWasErased returns (false, nil) with no lookup installed,
// so every vote naming an erased account's post is treated as an ordinary late
// subject and indexed the moment a row for it happens to exist. Nothing logs,
// nothing fails, and the erasure leaks back through the votes table.
//
// A wiring mistake is only expensive when it is invisible, so the consumer has
// to be able to answer the question at boot, where cmd/server can refuse to
// start rather than serve a gate-less consumer for a week.
func TestVoteConsumer_ErasureGatedReportsItsWiring(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	gated := NewVoteEventConsumer(
		postgres.NewVoteRepository(db), newMockUserService(), db,
		WithVoteDeletedAccounts(postgres.NewDeletedAccountRepository(db)))
	assert.True(t, gated.ErasureGated(),
		"a consumer built WITH WithVoteDeletedAccounts must report itself gated; if this reports false "+
			"then a boot check can never tell a correctly wired consumer from one silently missing "+
			"the lookup, which is the whole point of asking")

	ungated := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
	assert.False(t, ungated.ErasureGated(),
		"a consumer built WITHOUT the option must report itself ungated: a check that answered true "+
			"unconditionally would pass at boot and leave the erasure gate absent in production")
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
// # WHY THE REV GATE MUST COMMIT HERE AND ROLL BACK FOR AN ABSENT SUBJECT
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

// requireNoCountsAnywhere asserts that no row in either subject table carries a
// vote count.
//
// This is what "no counts changed anywhere" can honestly mean for a subject the
// AppView has no table for. A bystander fixture would prove nothing: every
// count statement on this path is keyed `WHERE uri = $1` on the vote's own
// subject URI, so it structurally cannot reach another row, and a green
// assertion about one would be theatre. A table-wide sweep is a different
// claim, and a real one — it fails if ANY count moved, including one written
// through a query this test never anticipated.
//
// It is affordable because testkit.DB hands each test a fresh clone: both
// tables start genuinely empty, so the sweep is over exactly the rows this test
// created, which is none.
func requireNoCountsAnywhere(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, sweep := range []struct {
		what  string
		query string
	}{
		{"posts", `SELECT COUNT(*) FROM posts WHERE upvote_count <> 0 OR downvote_count <> 0 OR score <> 0`},
		{"comments", `SELECT COUNT(*) FROM comments WHERE upvote_count <> 0 OR downvote_count <> 0 OR score <> 0`},
	} {
		var rows int
		require.NoError(t, db.QueryRow(sweep.query).Scan(&rows))
		assert.Zero(t, rows,
			"a vote on a subject with no table of its own must not move a count in %s — not on "+
				"its own subject, which has no row there, and not on anything else", sweep.what)
	}
}

// TestVoteConsumer_UnsupportedSubjectCollection_IsIndexedWithoutCounting pins
// the branch the ordering gate must NOT apply to.
//
// A vote names its subject by AT-URI and nothing else, so the URI's collection
// segment is the only thing that says which table the count belongs in. Two
// collections route to `posts` and one to `comments`; everything else routes
// nowhere, and the realistic everything-else is a BRIDGED Bluesky subject — a
// Coves user upvoting an app.bsky.feed.post they are seeing through the bridge.
//
// # WHY THE GATE MUST NOT REACH THIS BRANCH
//
// The gate's two outcomes both presuppose a table. "Refuse until the subject is
// indexed" is a promise that indexing will eventually happen, and no consumer
// will ever index an app.bsky.feed.post into this database — so a refusal here
// is not a deferral, it is ten redrives and a dead letter per vote, forever,
// for a condition that is permanent by construction. "Drop it" is equally wrong
// in the other direction: it would silently discard the vote.
//
// # WHY THE ROW IS KEPT EVEN THOUGH NOTHING COUNTS IT
//
// This is the case where an indexed-but-uncounted vote row is CORRECT rather
// than a time bomb, and the difference is worth stating because it is the exact
// shape the other two branches forbid — the absent subject, which must persist
// nothing at all, and the soft-deleted subject, whose vote is dropped outright.
//
// The row is not a pending count. It is VIEWER STATE: it is how the AppView
// answers "did I vote on this, and which way" for bridged content, which is the
// only thing it can usefully answer about a subject it does not host. And it
// cannot become a phantom decrement the way an orphaned row on a real post
// does, because the decrement is keyed on the same collection routing that
// declined to increment — deleteVote's switch has no branch for this collection
// either, so the withdrawal is as countless as the vote was.
//
// That symmetry is load-bearing, and it is why migration 038's orphan sweep
// must be restricted to KNOWN collections: a sweep that deleted every
// uncounted vote row would delete exactly this, and take the bridge's viewer
// state with it.
func TestVoteConsumer_UnsupportedSubjectCollection_IsIndexedWithoutCounting(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	// A bridged Bluesky post: a subject that genuinely exists in the world, is
	// genuinely votable from a Coves client, and will never have a row here.
	subjectURI := "at://" + bridgedSubjectAuthor + "/app.bsky.feed.post/3lbridgedpost"
	voteURI := "at://" + bridgedSubjectVoter + "/social.coves.feed.vote/bridgedvote"

	consumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)

	require.NoError(t, consumer.HandleEvent(ctx, &JetstreamEvent{
		Kind:   "commit",
		Did:    bridgedSubjectVoter,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Rev:        "3lgatebridgedaa2a",
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       "bridgedvote",
			CID:        "bafgatebridgedvote",
			Record: map[string]interface{}{
				"$type":     "social.coves.feed.vote",
				"subject":   map[string]interface{}{"uri": subjectURI, "cid": "bafbridgedsubject"},
				"direction": "up",
				"createdAt": "2026-03-01T01:00:00Z",
			},
		},
	}), "a vote on a collection with no table must be accepted: neither of the gate's outcomes "+
		"fits a subject that will never be indexed — refusing burns the redrive budget forever, "+
		"and dropping discards the vote")

	exists, active := voteRowState(t, db, voteURI)
	assert.True(t, exists, "the vote must be indexed at %s", voteURI)
	assert.True(t, active,
		"and it must be ACTIVE: the row is viewer state for bridged content — how the AppView "+
			"answers 'did I vote on this' for a subject it does not host — not a count waiting to "+
			"be applied")

	// The row has to be USABLE as viewer state, which means it has to carry the
	// subject and direction a viewer query reads back. An indexed row with the
	// wrong subject would satisfy the assertions above and answer every viewer
	// question wrongly.
	var storedSubject, storedDirection string
	require.NoError(t, db.QueryRow(
		`SELECT subject_uri, direction FROM votes WHERE uri = $1`, voteURI,
	).Scan(&storedSubject, &storedDirection))
	assert.Equal(t, subjectURI, storedSubject, "the stored row must name the subject that was voted on")
	assert.Equal(t, "up", storedDirection, "the stored row must carry the direction that was cast")

	requireNoCountsAnywhere(t, db)
}

// erasedSubjectVoteEvent builds the vote-create commit both erasure subtests
// send. The subject's repo DID is a parameter because that DID — not the
// voter's — is what the erasure gate must consult: the vote is the erased
// person's CONTENT only in the sense that it points at it.
func erasedSubjectVoteEvent(voterDID, rkey, rev, subjectURI string) *JetstreamEvent {
	return &JetstreamEvent{
		Kind:   "commit",
		Did:    voterDID,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Rev:        rev,
			Operation:  "create",
			Collection: "social.coves.feed.vote",
			RKey:       rkey,
			CID:        "bafgateerasedvote",
			Record: map[string]interface{}{
				"$type":     "social.coves.feed.vote",
				"subject":   map[string]interface{}{"uri": subjectURI, "cid": "baferasedsubject"},
				"direction": "up",
				"createdAt": "2026-03-01T01:00:00Z",
			},
		},
	}
}

// TestVoteConsumer_SubjectInErasedAccount covers the third reason a subject can
// be permanently absent, and the one the ordering gate gets exactly backwards.
//
// # WHY AN ERASED SUBJECT LOOKS IDENTICAL TO A LATE ONE, AND MUST NOT BE
// TREATED LIKE ONE
//
// Account erasure — the migration-036 marker flow, whose deletes live in
// user_repo.DeleteUser — HARD-deletes the account's posts, comments and votes:
// no tombstone row, no deleted_at, nothing. From inside the vote
// consumer's transaction the result is a subject URI with no row, which is
// pixel-for-pixel what a vote arriving ahead of its subject looks like.
//
// The ordering gate's answer to "no row" is to refuse transiently and wait for
// the subject to arrive. For an erased account the subject is never arriving:
// the post consumer refuses to re-index an erased account's posts precisely so
// that it cannot (WithDeletedAccounts, authorpost.go's authorWasErased). So the
// vote burns its whole redrive budget, ten replays of an event whose outcome
// was decided the moment the account was deleted, and then sits in the dead
// letter queue forever — where it is indistinguishable from a real backlog and
// buries the signal the queue exists to carry.
//
// erasure_integrity_test.go states the principle for the neighbouring case:
// such an event "must be DROPPED, not refused". This is the vote-shaped
// instance of it.
//
// # THE DID THE GATE MUST ASK ABOUT IS THE SUBJECT'S, NOT THE VOTER'S
//
// Worth stating because the vote consumer's every other identity check reads
// the voter's DID off the commit's repo. The erased party here is the person
// whose post was voted on, and their DID is reachable only by taking the repo
// segment out of the SUBJECT URI. A gate that checked the voter would answer
// the wrong question and let the event through.
func TestVoteConsumer_SubjectInErasedAccount(t *testing.T) {
	t.Parallel()

	t.Run("a vote on an erased account's post is dropped, not refused", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		ctx := context.Background()

		// The erasure swept the post itself, so the ONLY trace of this DID
		// anywhere in the database is the marker. That is the whole difficulty:
		// there is nothing else left to distinguish it from a subject in flight.
		erased := erasedSubjectAuthor
		markAccountDeleted(t, db, erased)
		subjectURI := "at://" + erased + "/" + PostV2Collection + "/erasedsubject"
		require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, subjectURI),
			"the erasure hard-deletes the post, leaving no row — asserted so this test cannot pass "+
				"against a fixture that quietly left one behind")

		consumer := NewVoteEventConsumer(
			postgres.NewVoteRepository(db), newMockUserService(), db,
			WithVoteDeletedAccounts(postgres.NewDeletedAccountRepository(db)))

		const voteRev = "3lgateerasedaa2a"
		voteURI := "at://" + erasedSubjectVoter + "/social.coves.feed.vote/erasedvote"

		require.NoError(t,
			consumer.HandleEvent(ctx, erasedSubjectVoteEvent(erasedSubjectVoter, "erasedvote", voteRev, subjectURI)),
			"a vote naming an erased account's post must be DROPPED, not refused. The subject is "+
				"never arriving — the post consumer's own erasure gate guarantees it — so a "+
				"transient refusal buys ten redrives and a permanent dead letter for a condition "+
				"that was decided when the account was deleted")

		exists, _ := voteRowState(t, db, voteURI)
		assert.False(t, exists,
			"no row may land at %s: indexing a vote that points into an erasure is the resurrection "+
				"the migration-036 marker flow exists to prevent, arriving through the votes table", voteURI)
		requireNoCountsAnywhere(t, db)

		// Same shape as the soft-deleted drop: a permanent condition is a
		// DECISION, so the gate advance commits as a tombstone and a duplicate
		// delivery is answered by the gate rather than by re-deciding.
		stale, err := NewRevGate(db).IsStale(ctx, voteURI, voteRev)
		require.NoError(t, err)
		assert.True(t, stale, "the dropped vote's rev advance must COMMIT, as it does for a deleted subject")
	})

	t.Run("an unreadable marker table fails closed", func(t *testing.T) {
		t.Parallel()
		db := testkit.DB(t)
		ctx := context.Background()

		// "I could not read the marker" and "there is no marker" must never be
		// the same answer. Failing OPEN here means a database blip becomes a
		// window in which votes pointing into erased accounts are indexed —
		// content coming back that somebody asked to have removed — with
		// nothing recording that it happened. Failing closed costs a redrive.
		lookupErr := errors.New("deleted_accounts is unreachable")
		consumer := NewVoteEventConsumer(
			postgres.NewVoteRepository(db), newMockUserService(), db,
			WithVoteDeletedAccounts(failingErasureLookup{err: lookupErr}))

		const voteRev = "3lgateerasedaa2b"
		subjectURI := "at://" + erasedSubjectUnknown + "/" + PostV2Collection + "/unknownerasure"
		voteURI := "at://" + erasedSubjectVoter + "/social.coves.feed.vote/unknownerasurevote"

		err := consumer.HandleEvent(ctx,
			erasedSubjectVoteEvent(erasedSubjectVoter, "unknownerasurevote", voteRev, subjectURI))

		require.Error(t, err, "a failed erasure lookup must not be treated as 'not erased'")

		// The assertion that makes this subtest MEAN something today. With the
		// subject absent, a consumer that never consulted the lookup at all
		// refuses transiently anyway — for ordering reasons — and would satisfy
		// every other assertion here vacuously. Requiring the returned error to
		// carry the lookup's own error is what proves the gate was consulted
		// and that its failure, not the missing row, is what stopped the event.
		// The post consumer already wraps this way (authorWasErased).
		assert.ErrorIs(t, err, lookupErr,
			"the refusal must be attributable to the erasure lookup: an error that merely says the "+
				"subject is not indexed is what this consumer already returns without consulting "+
				"any lookup, and it would leave a fail-open gate undetected")
		assert.NotErrorIs(t, err, ErrPermanentEvent,
			"an unreachable table is transient: the redrive is what makes failing closed cheap "+
				"rather than lossy")

		exists, _ := voteRowState(t, db, voteURI)
		assert.False(t, exists, "nothing may be indexed while the gate cannot be consulted")
		requireNoCountsAnywhere(t, db)

		// Unlike the drop, this one must NOT leave a tombstone: the condition is
		// an infrastructure failure that will pass, so the replay has to be able
		// to win the gate. Same rollback the ordering refusal relies on.
		stale, err := NewRevGate(db).IsStale(ctx, voteURI, voteRev)
		require.NoError(t, err)
		assert.False(t, stale,
			"a fail-closed refusal must roll the rev advance back, or the redrive that makes "+
				"failing closed affordable would itself be rejected as stale")
	})
}

// countSubject describes one of the two subject tables for the withdrawal
// tests. Every statement is carried whole rather than built from a table name,
// so no SQL here is assembled by concatenation.
type countSubject struct {
	kind        string
	repoDID     string
	collection  string
	seedLive    func(t *testing.T, db *sql.DB, uri string, counts subjectCounts)
	softDelete  string
	countsQuery string
}

// countSubjects is the post/comment pair, seeded LIVE with counts the caller
// supplies: the vote row each test creates is one of them, and the rest are
// numbers with no rows behind them. That is deliberate — the tests below assert
// on the stored count, and inventing more vote rows would add fixture without
// adding an assertion — but the counts have to be a parameter rather than a
// constant, because a withdrawal in one direction says nothing about the
// statement that handles the other.
var countSubjects = []countSubject{
	{
		kind:       "a post",
		repoDID:    withdrawCommunity,
		collection: "social.coves.community.post",
		seedLive: func(t *testing.T, db *sql.DB, uri string, counts subjectCounts) {
			t.Helper()
			insertBridgedUser(t, db, withdrawAuthor, "withdrawauthor.test")
			insertBridgedCommunity(t, db, withdrawCommunity, "withdrawcommunity.test", withdrawAuthor)
			_, err := db.Exec(`
				INSERT INTO posts (uri, cid, rkey, author_did, community_did, title,
				                   created_at, upvote_count, downvote_count, score)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				uri, "bafwithdrawsubject", "withdrawsubject", withdrawAuthor, withdrawCommunity,
				"the subject being deleted under a live vote",
				time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
				counts.Upvotes, counts.Downvotes, counts.Score)
			require.NoError(t, err)
		},
		softDelete:  `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`,
		countsQuery: `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`,
	},
	{
		kind:       "a comment",
		repoDID:    withdrawAuthor,
		collection: CommentCollection,
		seedLive: func(t *testing.T, db *sql.DB, uri string, counts subjectCounts) {
			t.Helper()
			root := "at://" + withdrawCommunity + "/social.coves.community.post/withdrawroot"
			_, err := db.Exec(`
				INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid,
				                      parent_uri, parent_cid, content, created_at,
				                      upvote_count, downvote_count, score)
				VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, $8, $9, $10, $11)`,
				uri, "bafwithdrawsubject", "withdrawsubject", withdrawAuthor,
				root, "bafwithdrawroot", "the subject being deleted under a live vote",
				time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
				counts.Upvotes, counts.Downvotes, counts.Score)
			require.NoError(t, err)
		},
		softDelete:  `UPDATE comments SET deleted_at = NOW() WHERE uri = $1`,
		countsQuery: `SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`,
	},
}

// withdrawUnderDeletedSubject runs the shared arc: a live subject carrying a
// counted vote in the given direction, the subject soft-deleted, then the vote
// withdrawn. It returns the subject URI so a caller can carry the arc further.
func withdrawUnderDeletedSubject(t *testing.T, db *sql.DB, subject countSubject, rev, direction string, before subjectCounts) string {
	t.Helper()
	ctx := context.Background()

	subjectURI := "at://" + subject.repoDID + "/" + subject.collection + "/withdrawsubject"
	subject.seedLive(t, db, subjectURI, before)

	// The vote is seeded directly and counted by construction: it is one of the
	// votes the subject already carries. Going through the consumer instead
	// would put the create path's own behaviour between the setup and the
	// measurement, and after the ordering gate landed that path has its own
	// opinions about deleted subjects.
	voteURI := "at://" + withdrawVoter + "/social.coves.feed.vote/withdrawvote"
	seedActiveVote(t, db, voteURI, withdrawVoter, subjectURI, direction)

	// THE DELETED WINDOW OPENS. Nothing about the vote changes here — it is
	// still live, still counted, still one of the subject's own.
	_, err := db.Exec(subject.softDelete, subjectURI)
	require.NoError(t, err, "soft-deleting the subject out from under a live vote")

	consumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
	require.NoError(t, consumer.HandleEvent(ctx, &JetstreamEvent{
		Kind:   "commit",
		Did:    withdrawVoter,
		TimeUS: time.Now().UnixMicro(),
		Commit: &CommitEvent{
			Rev:        rev,
			Operation:  "delete",
			Collection: "social.coves.feed.vote",
			RKey:       "withdrawvote",
		},
	}), "withdrawing a vote must succeed whatever state its subject is in")

	exists, active := voteRowState(t, db, voteURI)
	require.True(t, exists, "the withdrawn vote's row must still be there, soft-deleted")
	require.False(t, active, "the withdrawal must soft-delete the vote row")

	return subjectURI
}

// TestVoteConsumer_WithdrawalDecrementsThroughADeletedSubject is the
// count-integrity half of this run, and the one that outlives every gate above
// it.
//
// # THE INVARIANT
//
// Every LIVE vote row on a post or comment must be included in that subject's
// stored counts. Not "while the subject is visible" — always. The gates above
// keep uncounted rows from being created; this keeps counted rows from
// silently becoming uncounted.
//
// # WHY A deleted_at FILTER ON THE SUBJECT BREAKS IT
//
// Before this branch, every count mutation in vote_consumer.go filtered the
// SUBJECT row on `deleted_at IS NULL`. It reads as harmless — why touch a
// deleted row? — and is not, because deletion is not the end of the story:
//
//	the vote is cast and counted        subject live, counts 3
//	the subject is soft-deleted         counts still 3, vote still live
//	the voter withdraws                 decrement matches ZERO rows, suppressed
//	the subject is RESURRECTED          counts 3, live votes 2
//
// The discrepancy is permanent and unattributable. Nothing recounts on
// resurrection — comment_consumer.go's re-create path preserves native counts
// deliberately — and the vote row that would explain the extra point is
// soft-deleted, so no query over live votes can ever find it. The count is
// simply wrong forever, by an amount that grows with every withdrawal that
// happens to land inside a deleted window.
//
// # WHY THE WITHDRAWAL IS THE ONE SITE WORTH TESTING
//
// Three mutating sites — twelve statements across the two subject tables and
// both directions — carried the filter, but only this one is reachable now. The
// create-path increment can no longer meet a deleted subject at all: the
// ordering gate drops that event before any count runs, and the stale-cleanup
// decrement operates on rows the create path will no longer produce for a
// deleted subject. Removing their filters is consistency, not behaviour, and a
// test that forced those paths would be asserting against code the gates make
// unreachable. This one is plain: a live counted vote, its subject deleted, the
// voter changes their mind.
//
// Both DIRECTIONS are exercised because the decrement is not one statement: the
// consumer picks a column by the stored row's direction, so an upvote
// withdrawal proves nothing about the downvote statement standing beside it,
// and the score arithmetic differs in sign between them.
func TestVoteConsumer_WithdrawalDecrementsThroughADeletedSubject(t *testing.T) {
	t.Parallel()

	for _, cast := range []struct {
		direction string
		rev       string
		before    subjectCounts
		after     subjectCounts
	}{
		{
			direction: "up",
			rev:       "3lgatewithdrawaa2a",
			before:    subjectCounts{Upvotes: 3, Downvotes: 0, Score: 3},
			after:     subjectCounts{Upvotes: 2, Downvotes: 0, Score: 2},
		},
		{
			// The subject carries a downvote among its counts, so withdrawing
			// it moves downvote_count DOWN and the score UP — the opposite
			// direction from the upvote case in both columns, which is what
			// makes a decrement wired to the wrong column visible.
			direction: "down",
			rev:       "3lgatewithdrawaa2d",
			before:    subjectCounts{Upvotes: 3, Downvotes: 1, Score: 2},
			after:     subjectCounts{Upvotes: 3, Downvotes: 0, Score: 3},
		},
	} {
		for _, subject := range countSubjects {
			t.Run(subject.kind+" that was deleted after a "+cast.direction+"vote was counted", func(t *testing.T) {
				t.Parallel()
				db := testkit.DB(t)

				subjectURI := withdrawUnderDeletedSubject(t, db, subject, cast.rev, cast.direction, cast.before)

				assert.Equal(t, cast.after,
					readSubjectCounts(t, db, subject.countsQuery, subjectURI),
					"the withdrawal must decrement THROUGH the deleted window. Counts still reading "+
						"%v means the decrement's `AND deleted_at IS NULL` on the subject "+
						"suppressed it, and the subject now claims a vote that no longer exists — "+
						"unattributably, because the row that would explain it is soft-deleted",
					cast.before)
			})
		}
	}
}

// TestVoteConsumer_ResurrectedCommentCarriesTheDecrementedCount is the
// user-visible statement of the same invariant, and the reason it is worth
// fixing rather than filing.
//
// A soft-deleted comment is not a terminal state, it is a pause:
// comment_consumer.go's re-create path clears deleted_at and preserves the
// native vote counts on purpose, so whatever the counts say when the comment
// comes back is what readers see. If a withdrawal was suppressed while it was
// away, the resurrected comment displays a score one higher than the votes it
// actually holds, permanently, with nothing left to point at.
//
// Posts have no equivalent re-create path today, which is why this arc is
// asserted for comments only — the invariant is the same for both, but this is
// where a reader can actually observe it being violated.
//
// # WHY THE WHOLE ARC IS DRIVEN THROUGH THE REAL CONSUMERS
//
// The deletion and the resurrection here are genuine Jetstream commits handled
// by CommentEventConsumer, not UPDATE statements this test writes. The
// invariant's premise is a claim about the comment consumer's behaviour —
// "re-create clears deleted_at and PRESERVES the native counts" — and a test
// that performed the resurrection itself would assert that claim against its
// own UPDATE, staying green for as long as the test kept agreeing with itself
// and going silent the day the re-create path started zeroing counts or
// recounting them. Driving the real event binds the two behaviours together:
// if the comment consumer ever stops preserving native counts, this fails, and
// it should, because the vote consumer's decrement-through-a-deleted-window is
// only load-bearing while something downstream carries the corrected number
// back into view.
//
// The re-create carries a HIGHER rev than the delete, which is what makes it a
// genuine atProto recreate-same-rkey rather than a stale replay the rev gate is
// supposed to reject (rev_gate_test.go pins both sides of that).
func TestVoteConsumer_ResurrectedCommentCarriesTheDecrementedCount(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	_, postURI, postCID := setupRevFixtures(t, db)
	commentConsumer := NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	baseTime := time.Now().UnixMicro()

	const commentRKey = "resurrectedsubject"
	commentURI := "at://" + revTestCommenter + "/" + CommentCollection + "/" + commentRKey
	commentRecord := revCommentRecord("the comment that goes away and comes back", postURI, postCID, postURI, postCID)

	// ---- first life -----------------------------------------------------------
	require.NoError(t, commentConsumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentRKey, revA, "bafresurrect1", baseTime, commentRecord)))

	// Three upvotes, of which the seeded row is one. The other two are numbers
	// with no rows behind them, the same shape countSubjects uses: what is
	// asserted is the stored count, and two more vote rows would add fixture
	// without adding an assertion.
	_, err := db.Exec(
		`UPDATE comments SET upvote_count = 3, score = 3 WHERE uri = $1`, commentURI)
	require.NoError(t, err, "putting the votes the comment collected in its first life on the row")

	voteURI := "at://" + revTestVoter + "/social.coves.feed.vote/resurrectvote"
	seedActiveVote(t, db, voteURI, revTestVoter, commentURI, "up")

	// ---- the comment is deleted, through its own consumer ---------------------
	require.NoError(t, commentConsumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "delete", commentRKey, revB, "", baseTime+1_000_000, nil)))

	var deletedAt *time.Time
	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM comments WHERE uri = $1`, commentURI).Scan(&deletedAt))
	require.NotNil(t, deletedAt, "the comment consumer's delete must soft-delete the row, or there is no deleted window to withdraw inside")

	// ---- the voter withdraws while it is away ---------------------------------
	voteConsumer := NewVoteEventConsumer(postgres.NewVoteRepository(db), newMockUserService(), db)
	require.NoError(t, voteConsumer.HandleEvent(ctx, &JetstreamEvent{
		Kind:   "commit",
		Did:    revTestVoter,
		TimeUS: baseTime + 1_500_000,
		Commit: &CommitEvent{
			Rev:        "3lgateresurrectaa2a",
			Operation:  "delete",
			Collection: "social.coves.feed.vote",
			RKey:       "resurrectvote",
		},
	}), "withdrawing a vote must succeed whatever state its subject is in")

	// ---- the comment comes back, through its own consumer ---------------------
	require.NoError(t, commentConsumer.HandleEvent(ctx, revCommitEvent(
		revTestCommenter, CommentCollection, "create", commentRKey, revC, "bafresurrect2", baseTime+2_000_000, commentRecord)))

	require.NoError(t, db.QueryRow(`SELECT deleted_at FROM comments WHERE uri = $1`, commentURI).Scan(&deletedAt))
	require.Nil(t, deletedAt, "the genuine re-create (higher rev) must resurrect the comment, or the arc measures nothing")

	assert.Equal(t, subjectCounts{Upvotes: 2, Score: 2},
		readSubjectCounts(t, db, `SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`, commentURI),
		"the comment came back claiming a vote that was withdrawn while it was deleted. This is "+
			"what a suppressed decrement looks like to a reader: a score that cannot be "+
			"reconciled against any live vote row, and that no recount will ever correct because "+
			"the re-create path preserves native counts by design")
}
