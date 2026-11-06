//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Votes by a banned author: a vote A indexes on a subject in C while ban(C, A)
// is in force is recorded hidden and never enters the subject's tallies — not
// when the ban is lifted, not when it expires, not when the vote event is
// redelivered. Deleting or replacing a hidden vote never decrements a counter
// it never incremented, and a replacement is classified by its own index order.
//
// Every row runs against a post subject in C and a comment subject in C (B's
// comment on the indexed root P). The subject is pre-seeded by three other
// voters to upvote 2 / downvote 1 / score 1, so a wrong decrement is visible
// despite the counters' GREATEST(0, ...) floor.

const (
	bvSeedUpRkey1  = "bvseedup1"
	bvSeedUpRkey2  = "bvseedup2"
	bvSeedDownRkey = "bvseeddown"
)

// bvStep names one event (or clock move) delivered after the subject is seeded.
type bvStep int

const (
	bvStepBan                        bvStep = iota // ban(C, A) create, expiresAt = scenario.banExpiresAt
	bvStepBanOtherCommunity                        // ban(C2, A) create
	bvStepBanDelete                                // C deletes ban(C, A)
	bvStepAuthorUpvote                             // A upvotes the subject (rkey bhAuthorVoteRkey)
	bvStepAuthorDownvote                           // A downvotes the subject (rkey bhAuthorVoteRkey)
	bvStepAuthorUpvoteReplay                       // the earlier bvStepAuthorUpvote event redelivered unchanged (same rev)
	bvStepAuthorUpvoteReplayNewerRev               // A's upvote record (same rkey, same content) at a newer rev
	bvStepAuthorUpvoteDelete                       // A deletes the vote at bhAuthorVoteRkey
	bvStepAuthorReplacementDownvote                // A downvotes the subject under a NEW rkey (bhAuthorReplacementVoteRkey)
	bvStepAdvanceClock                             // the comment and vote clocks move to bhClockAdvanced
)

// bvWant is checked right after the step it is attached to.
type bvWant struct {
	counts subjectCounts
	// hides are the hide_reason values of A's vote rows at this point.
	hides map[bhRecord]sql.NullString
}

type bvAt struct {
	step bvStep
	want *bvWant
}

type bvScenario struct {
	steps        []bvAt
	banExpiresAt string // "" = no expiry
}

// bvSubject is the record A votes on: a post in C, or B's comment on that post.
type bvSubject struct {
	name        string
	uri         string
	cid         string
	countsQuery string
	isComment   bool
}

func bvSubjects() []bvSubject {
	return []bvSubject{
		{
			name:        "post in C",
			uri:         accPostURI(bhRootRkey),
			cid:         bhRootCID,
			countsQuery: `SELECT upvote_count, downvote_count, score FROM posts WHERE uri = $1`,
		},
		{
			name:        "comment in C",
			uri:         bhCommentURI(bhOther, bhOtherCommentRkey),
			cid:         bhOtherCommentCID,
			countsQuery: `SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`,
			isComment:   true,
		},
	}
}

// runBanHideVoteScenario indexes the root P in C (and B's comment on it for a
// comment subject), seeds the subject to 2/1 through the real vote consumer,
// then delivers every scenario step, checking each attached bvWant.
func runBanHideVoteScenario(t *testing.T, f ccrFixture, clock *bhClock, subject bvSubject, s bvScenario) {
	t.Helper()
	ctx := context.Background()
	const setupEvents = 5 // root, B's comment, three seed votes
	revs := increasingTIDs(t, setupEvents+len(s.steps))
	base := time.Now().UnixMicro()
	next := 0
	nextEvent := func() (string, int64) {
		rev := revs[next]
		next++
		return rev, base + int64(next)*1_000_000
	}

	rootURI := accPostURI(bhRootRkey)
	rev, timeUS := nextEvent()
	require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(
		accAuthor, "create", bhRootRkey, rev, bhRootCID, timeUS,
		pv2Record(accCommunity, "thread root", "root body"))), "root post in C")
	rev, timeUS = nextEvent()
	if subject.isComment {
		require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, bhOtherCommentRkey, bhOtherCommentCID,
			rev, timeUS, "", rootURI, bhRootCID, rootURI, bhRootCID)), "B's comment on P")
	}
	for _, seed := range []struct{ voter, rkey, direction string }{
		{bhOther, bvSeedUpRkey1, "up"},
		{ccrUser, bvSeedUpRkey2, "up"},
		{ccrCommenter, bvSeedDownRkey, "down"},
	} {
		rev, timeUS = nextEvent()
		require.NoError(t, f.votes.HandleEvent(ctx, bhVoteEvent(seed.voter, seed.rkey, rev, timeUS,
			seed.direction, subject.uri, subject.cid)), "seed vote %s by %s", seed.direction, seed.voter)
	}
	require.Equal(t, subjectCounts{Upvotes: 2, Downvotes: 1, Score: 1},
		readSubjectCounts(t, f.db, subject.countsQuery, subject.uri), "seeded subject counts")

	ban := func(repo, op, rev, expiresAt string, timeUS int64) *JetstreamEvent {
		return banCommitEvent(banStep{
			repo: repo, op: op, subject: bhAuthor, rev: rev,
			cid: "bafyreibvban", reason: "spam", expiresAt: expiresAt,
		}, timeUS)
	}
	authorVote := func(rkey, direction, rev string, timeUS int64) *JetstreamEvent {
		return bhVoteEvent(bhAuthor, rkey, rev, timeUS, direction, subject.uri, subject.cid)
	}

	var upvoteEvent *JetstreamEvent
	for i, at := range s.steps {
		rev, timeUS := nextEvent()
		switch at.step {
		case bvStepBan:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx,
				ban(accCommunity, "create", rev, s.banExpiresAt, timeUS)), "ban(C, A)")
		case bvStepBanOtherCommunity:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx,
				ban(ccrOtherCommunity, "create", rev, "", timeUS)), "ban(C2, A)")
		case bvStepBanDelete:
			require.NoError(t, f.posts.consumer.HandleEvent(ctx,
				ban(accCommunity, "delete", rev, "", timeUS)), "ban(C, A) delete")
		case bvStepAuthorUpvote:
			upvoteEvent = authorVote(bhAuthorVoteRkey, "up", rev, timeUS)
			require.NoError(t, f.votes.HandleEvent(ctx, upvoteEvent), "A's upvote")
		case bvStepAuthorDownvote:
			require.NoError(t, f.votes.HandleEvent(ctx, authorVote(bhAuthorVoteRkey, "down", rev, timeUS)), "A's downvote")
		case bvStepAuthorUpvoteReplay:
			require.NotNil(t, upvoteEvent, "a replay needs an earlier bvStepAuthorUpvote")
			require.NoError(t, f.votes.HandleEvent(ctx, upvoteEvent), "A's upvote redelivered")
		case bvStepAuthorUpvoteReplayNewerRev:
			require.NoError(t, f.votes.HandleEvent(ctx, authorVote(bhAuthorVoteRkey, "up", rev, timeUS)),
				"A's upvote at a newer rev")
		case bvStepAuthorUpvoteDelete:
			require.NoError(t, f.votes.HandleEvent(ctx, revCommitEvent(bhAuthor, "social.coves.feed.vote",
				"delete", bhAuthorVoteRkey, rev, "", timeUS, nil)), "A deletes the vote")
		case bvStepAuthorReplacementDownvote:
			require.NoError(t, f.votes.HandleEvent(ctx, authorVote(bhAuthorReplacementVoteRkey, "down", rev, timeUS)),
				"A's replacement downvote under a new rkey")
		case bvStepAdvanceClock:
			clock.now = bhClockAdvanced
		default:
			t.Fatalf("unknown step %d", at.step)
		}

		if at.want == nil {
			continue
		}
		assert.Equal(t, at.want.counts, readSubjectCounts(t, f.db, subject.countsQuery, subject.uri),
			"counts of %s after step %d", subject.uri, i)
		for rec, wantReason := range at.want.hides {
			_, uri := rec.tableAndURI()
			assert.Equal(t, wantReason, readHideReason(t, f.db, rec), "hide_reason of %s after step %d", uri, i)
		}
	}
}

func runBanHideVoteTable(t *testing.T, tests []struct {
	name string
	bvScenario
},
) {
	t.Helper()
	for _, subject := range bvSubjects() {
		for _, tt := range tests {
			t.Run(subject.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				clock := &bhClock{now: bhClockStart}
				f := newBanHideFixture(t, clock)
				runBanHideVoteScenario(t, f, clock, subject, tt.bvScenario)
			})
		}
	}
}

var (
	bvSeeded       = subjectCounts{Upvotes: 2, Downvotes: 1, Score: 1}
	bvWithAuthorUp = subjectCounts{Upvotes: 3, Downvotes: 1, Score: 2}
)

func bvDo(s bvStep) bvAt { return bvAt{step: s} }

func bvThen(s bvStep, counts subjectCounts, hides map[bhRecord]sql.NullString) bvAt {
	return bvAt{step: s, want: &bvWant{counts: counts, hides: hides}}
}

func bvAuthorVote(reason sql.NullString) map[bhRecord]sql.NullString {
	return map[bhRecord]sql.NullString{bhRecAuthorVote: reason}
}

// B4: hidden votes never enter the subject's tallies; votes outside a ban do.
func TestCommunityBanHide_VoteTallies(t *testing.T) {
	t.Parallel()

	runBanHideVoteTable(t, []struct {
		name string
		bvScenario
	}{
		{
			name: "a ban, upvote, ban deleted, same-rev replay",
			bvScenario: bvScenario{steps: []bvAt{
				bvDo(bvStepBan),
				bvThen(bvStepAuthorUpvote, bvSeeded, bvAuthorVote(bhHidden)),
				bvThen(bvStepBanDelete, bvSeeded, bvAuthorVote(bhHidden)),
				bvThen(bvStepAuthorUpvoteReplay, bvSeeded, bvAuthorVote(bhHidden)),
			}},
		},
		{
			name: "a2 ban, upvote, ban deleted, newer-rev replay",
			bvScenario: bvScenario{steps: []bvAt{
				bvDo(bvStepBan),
				bvThen(bvStepAuthorUpvote, bvSeeded, bvAuthorVote(bhHidden)),
				bvDo(bvStepBanDelete),
				bvThen(bvStepAuthorUpvoteReplayNewerRev, bvSeeded, bvAuthorVote(bhHidden)),
			}},
		},
		{
			name: "b ban expiring after the vote clock, upvote, clock passes expiry, newer-rev replay",
			bvScenario: bvScenario{
				banExpiresAt: bhExpiresAfterClock,
				steps: []bvAt{
					bvDo(bvStepBan),
					bvThen(bvStepAuthorUpvote, bvSeeded, bvAuthorVote(bhHidden)),
					bvDo(bvStepAdvanceClock),
					bvThen(bvStepAuthorUpvoteReplayNewerRev, bvSeeded, bvAuthorVote(bhHidden)),
				},
			},
		},
		{
			name: "e ban expired before the vote clock, upvote counts",
			bvScenario: bvScenario{
				banExpiresAt: bhExpiresBeforeClock,
				steps: []bvAt{
					bvDo(bvStepBan),
					bvThen(bvStepAuthorUpvote, bvWithAuthorUp, bvAuthorVote(bhNotHidden)),
				},
			},
		},
		{
			name: "f ban, downvote",
			bvScenario: bvScenario{steps: []bvAt{
				bvDo(bvStepBan),
				bvThen(bvStepAuthorDownvote, bvSeeded, bvAuthorVote(bhHidden)),
			}},
		},
		{
			name: "c upvote before the ban still counts",
			bvScenario: bvScenario{steps: []bvAt{
				bvThen(bvStepAuthorUpvote, bvWithAuthorUp, bvAuthorVote(bhNotHidden)),
				bvThen(bvStepBan, bvWithAuthorUp, bvAuthorVote(bhNotHidden)),
			}},
		},
		{
			name: "d ban in C2, upvote on the C subject counts",
			bvScenario: bvScenario{steps: []bvAt{
				bvDo(bvStepBanOtherCommunity),
				bvThen(bvStepAuthorUpvote, bvWithAuthorUp, bvAuthorVote(bhNotHidden)),
			}},
		},
	})
}

// B5: given A's ban-hidden upvote on a subject at 2/1, a delete or a new-rkey
// replacement never decrements what the hidden vote never added, and the
// replacement is classified by its own index order.
func TestCommunityBanHide_HiddenVoteDeleteAndReplacement(t *testing.T) {
	t.Parallel()

	runBanHideVoteTable(t, []struct {
		name string
		bvScenario
	}{
		{
			name: "a delete of the hidden upvote",
			bvScenario: bvScenario{steps: []bvAt{
				bvDo(bvStepBan),
				bvThen(bvStepAuthorUpvote, bvSeeded, bvAuthorVote(bhHidden)),
				bvThen(bvStepAuthorUpvoteDelete, bvSeeded, bvAuthorVote(bhHidden)),
			}},
		},
		{
			name: "b replacement downvote while the ban is in force",
			bvScenario: bvScenario{steps: []bvAt{
				bvDo(bvStepBan),
				bvThen(bvStepAuthorUpvote, bvSeeded, bvAuthorVote(bhHidden)),
				bvThen(bvStepAuthorReplacementDownvote, bvSeeded, map[bhRecord]sql.NullString{
					bhRecAuthorVote:            bhHidden,
					bhRecAuthorReplacementVote: bhHidden,
				}),
			}},
		},
		{
			name: "c ban deleted, then replacement downvote counts",
			bvScenario: bvScenario{steps: []bvAt{
				bvDo(bvStepBan),
				bvThen(bvStepAuthorUpvote, bvSeeded, bvAuthorVote(bhHidden)),
				bvDo(bvStepBanDelete),
				bvThen(bvStepAuthorReplacementDownvote, subjectCounts{Upvotes: 2, Downvotes: 2, Score: 0},
					map[bhRecord]sql.NullString{
						bhRecAuthorVote:            bhHidden,
						bhRecAuthorReplacementVote: bhNotHidden,
					}),
			}},
		},
	})
}

// waitForLockWaitOrDone waits until done has a result or another session of
// this test database waits on a lock of the given kind (advisory, or any other
// such as a row lock). observer is a pool or an open transaction: the test
// pool holds three connections, so a test that already uses them all observes
// through one of its own transactions. The statistics snapshot is cleared on
// every read, since a transaction otherwise keeps its first one.
func waitForLockWaitOrDone(t *testing.T, observer lockWaitObserver, done chan error, advisory bool, description string) {
	t.Helper()
	waitForLockWaitersOrDone(t, observer, done, advisory, 1, description)
}

type lockWaitObserver interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// waitForLockWaitersOrDone is waitForLockWaitOrDone for at least minimum
// sessions waiting on a lock of the given kind.
func waitForLockWaitersOrDone(t *testing.T, observer lockWaitObserver, done chan error, advisory bool, minimum int, description string) {
	t.Helper()
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		if len(done) > 0 {
			return true, nil
		}
		var waiting int
		err := observer.QueryRowContext(t.Context(), `
			WITH cleared AS (SELECT pg_stat_clear_snapshot())
			SELECT count(*) FROM cleared, pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND (wait_event = 'advisory') = $1
		`, advisory).Scan(&waiting)
		return waiting >= minimum, err
	}, testkit.WithDescription("%s", description))
}

// A delete or new-rkey replacement of a pending vote that races its root's
// arrival decides the decrement from the row it soft-deletes: the root
// arrival is held uncommitted after counting the vote, the delete or
// replacement starts and waits on the vote row, and the root arrival commits.
func TestCommunityBanHide_PendingVoteRemovalRacingRootArrival(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		remove func(f ccrFixture, rev string, timeUS int64) error
		want   subjectCounts
	}{
		{
			name: "delete",
			remove: func(f ccrFixture, rev string, timeUS int64) error {
				return f.votes.HandleEvent(context.Background(), revCommitEvent(bhOther, "social.coves.feed.vote",
					"delete", bhOtherVoteRkey, rev, "", timeUS, nil))
			},
			want: subjectCounts{Upvotes: 0, Downvotes: 0, Score: 0},
		},
		{
			name: "new-rkey replacement downvote",
			remove: func(f ccrFixture, rev string, timeUS int64) error {
				return f.votes.HandleEvent(context.Background(), bhVoteEvent(bhOther, "bhothervote2", rev, timeUS,
					"down", bhCommentURI(bhAuthor, bhCommentRkey), bhCommentCID))
			},
			want: subjectCounts{Upvotes: 0, Downvotes: 1, Score: -1},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			clock := &bhClock{now: bhClockStart}
			f := newBanHideFixture(t, clock)
			runBanHideScenario(t, f, clock, bhScenario{steps: []bhStep{bhStepComment, bhStepOtherUpvote}})
			require.Equal(t, sql.NullString{String: hideReasonPendingBanCheck, Valid: true},
				readHideReason(t, f.db, bhRecOtherVote), "fixture: the vote waits for its subject's root")

			rootArrival, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			require.NoError(t, err)
			t.Cleanup(func() { _ = rootArrival.Rollback() })
			require.NoError(t, classifyPendingBelow(ctx, rootArrival, accPostURI(bhRootRkey), accCommunity, countedVoteNotifier{}))

			done := make(chan error, 1)
			rev := testkit.TID()
			go func() { done <- tt.remove(f, rev, time.Now().UnixMicro()) }()
			waitForLockWaitOrDone(t, f.db, done, false, "the vote removal waits on the vote row or finished")
			require.NoError(t, rootArrival.Commit())
			require.NoError(t, <-done)

			assert.Equal(t, tt.want, readSubjectCounts(t, f.db,
				`SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`, bhCommentURI(bhAuthor, bhCommentRkey)),
				"the removed vote, counted by the root arrival, is decremented")
		})
	}
}

// A new-rkey replacement of a pending vote that starts after its root's
// arrival has locked the subject comment, but before the arrival reaches the
// pending votes, queues on the comment instead of first taking the old vote
// row: root classification locks comments before votes, and so does the
// replacement. The arrival is held after taking the comment row (the lock its
// first statement takes), the replacement starts and waits, then the arrival
// classifies and commits.
func TestCommunityBanHide_ReplacementVoteQueuesBehindRootArrivalCommentLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &bhClock{now: bhClockStart}
	f := newBanHideFixture(t, clock)
	runBanHideScenario(t, f, clock, bhScenario{steps: []bhStep{bhStepComment, bhStepOtherUpvote}})
	require.Equal(t, sql.NullString{String: hideReasonPendingBanCheck, Valid: true},
		readHideReason(t, f.db, bhRecOtherVote), "fixture: the vote waits for its subject's root")
	subjectURI := bhCommentURI(bhAuthor, bhCommentRkey)

	rootArrival, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rootArrival.Rollback() })
	_, err = rootArrival.ExecContext(ctx, `SELECT 1 FROM comments WHERE uri = $1 FOR NO KEY UPDATE`, subjectURI)
	require.NoError(t, err)

	done := make(chan error, 1)
	rev := testkit.TID()
	go func() {
		done <- f.votes.HandleEvent(ctx, bhVoteEvent(bhOther, bhOtherReplacementVoteRkey, rev, time.Now().UnixMicro(),
			"down", subjectURI, bhCommentCID))
	}()
	waitForLockWaitOrDone(t, f.db, done, false, "the replacement waits on a row lock or finished")

	require.NoError(t, classifyPendingBelow(ctx, rootArrival, accPostURI(bhRootRkey), accCommunity, countedVoteNotifier{}),
		"the root arrival classifies the pending comment and vote")
	require.NoError(t, rootArrival.Commit())
	require.NoError(t, <-done, "the replacement downvote")

	assert.Equal(t, subjectCounts{Upvotes: 0, Downvotes: 1, Score: -1}, readSubjectCounts(t, f.db,
		`SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`, subjectURI),
		"the upvote counted by the root arrival is replaced by the counted downvote")
}

// Two vote transactions on the same comment, each classifying the vote and then
// taking the comment row for its count update, queue on the row rather than
// deadlock: the first holds its classification read open while the second
// starts, then counts and commits.
func TestCommunityBanHide_ConcurrentVotesOnOneCommentDoNotDeadlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &bhClock{now: bhClockStart}
	f := newBanHideFixture(t, clock)
	runBanHideScenario(t, f, clock, bhScenario{steps: []bhStep{bhStepRoot, bhStepOtherComment}})
	subjectURI := bhCommentURI(bhOther, bhOtherCommentRkey)
	const countUpdate = `UPDATE comments SET upvote_count = upvote_count + 1 WHERE uri = $1`

	first, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Rollback() })
	reason, err := classifyIndexedVote(ctx, first, subjectURI, ccrUser, "", 1, bhClockStart)
	require.NoError(t, err)
	require.False(t, reason.Valid, "fixture: the first vote is not hidden")

	done := make(chan error, 1)
	go func() {
		done <- func() error {
			second, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
			if err != nil {
				return err
			}
			defer func() { _ = second.Rollback() }()
			if _, err := classifyIndexedVote(ctx, second, subjectURI, ccrCommenter, "", 2, bhClockStart); err != nil {
				return err
			}
			if _, err := second.ExecContext(ctx, countUpdate, subjectURI); err != nil {
				return err
			}
			return second.Commit()
		}()
	}()
	waitForLockWaitOrDone(t, f.db, done, false, "the second vote waits on the comment row or finished")

	_, err = first.ExecContext(ctx, countUpdate, subjectURI)
	require.NoError(t, err, "the first vote's count update")
	require.NoError(t, first.Commit())
	require.NoError(t, <-done, "the second vote")
	assert.Equal(t, 2, countRows(t, f.db, `SELECT upvote_count FROM comments WHERE uri = $1`, subjectURI),
		"both votes are counted")
}

// A pending upvote that comes out counted when its subject's community becomes
// known bumps the subject author's upvote group, as an upvote counted at index
// time does: on root arrival by its own event, by the acceptance direct fetch,
// and on the arrival of a missing parent comment.
func TestCommunityBanHide_PendingUpvoteCountedLaterBumpsUpvoteGroup(t *testing.T) {
	t.Parallel()

	rootURI := accPostURI(bhRootRkey)
	authorCommentURI := bhCommentURI(bhAuthor, bhCommentRkey)
	authorReplyURI := bhCommentURI(bhAuthor, bhReplyRkey)
	otherCommentURI := bhCommentURI(bhOther, bhOtherCommentRkey)
	rootEvent := func(rev string) *JetstreamEvent {
		return pv2Event(accAuthor, "create", bhRootRkey, rev, bhRootCID, time.Now().UnixMicro(),
			pv2Record(accCommunity, "thread root", "root body"))
	}
	otherUpvote := func(subjectURI, subjectCID, createdAt, rev string) *JetstreamEvent {
		return revCommitEvent(bhOther, "social.coves.feed.vote", "create", bhOtherVoteRkey, rev,
			"bafyreivote"+bhOtherVoteRkey, time.Now().UnixMicro(), map[string]interface{}{
				"$type":     "social.coves.feed.vote",
				"subject":   map[string]interface{}{"uri": subjectURI, "cid": subjectCID},
				"direction": "up",
				"createdAt": createdAt,
			})
	}

	for _, tt := range []struct {
		name    string
		subject string
		steps   func(t *testing.T, f ccrFixture, createdAt string, revs []string)
	}{
		{
			name:    "root arrives by its own event",
			subject: authorCommentURI,
			steps: func(t *testing.T, f ccrFixture, createdAt string, revs []string) {
				ctx := context.Background()
				require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID,
					revs[0], time.Now().UnixMicro(), createdAt, rootURI, bhRootCID, rootURI, bhRootCID)))
				require.NoError(t, f.votes.HandleEvent(ctx, otherUpvote(authorCommentURI, bhCommentCID, createdAt, revs[1])))
				require.NoError(t, f.posts.consumer.HandleEvent(ctx, rootEvent(revs[2])))
			},
		},
		{
			name:    "root indexed by the acceptance direct fetch",
			subject: authorCommentURI,
			steps: func(t *testing.T, f ccrFixture, createdAt string, revs []string) {
				ctx := context.Background()
				require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhCommentRkey, bhCommentCID,
					revs[0], time.Now().UnixMicro(), createdAt, rootURI, bhRootCID, rootURI, bhRootCID)))
				require.NoError(t, f.votes.HandleEvent(ctx, otherUpvote(authorCommentURI, bhCommentCID, createdAt, revs[1])))
				f.rootFetcher.served = &FetchedPost{
					URI: rootURI, CID: bhRootCID,
					Record: pv2Record(accCommunity, "thread root", "root body"),
				}
				require.NoError(t, f.posts.consumer.HandleEvent(ctx, acceptanceEvent(
					accCommunity, rootURI, bhRootCID, revs[2], time.Now().UnixMicro())))
			},
		},
		{
			name:    "missing parent comment arrives",
			subject: authorReplyURI,
			steps: func(t *testing.T, f ccrFixture, createdAt string, revs []string) {
				ctx := context.Background()
				require.NoError(t, f.posts.consumer.HandleEvent(ctx, rootEvent(revs[0])))
				require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhAuthor, bhReplyRkey, bhReplyCID,
					revs[1], time.Now().UnixMicro(), createdAt, rootURI, bhRootCID, otherCommentURI, bhOtherCommentCID)))
				require.NoError(t, f.votes.HandleEvent(ctx, otherUpvote(authorReplyURI, bhReplyCID, createdAt, revs[2])))
				require.NoError(t, f.comments.HandleEvent(ctx, bhCommentEvent(bhOther, bhOtherCommentRkey, bhOtherCommentCID,
					revs[3], time.Now().UnixMicro(), createdAt, rootURI, bhRootCID, rootURI, bhRootCID)))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := &bhClock{now: bhClockStart}
			f := newCCRFixture(t,
				withCCRCommunityBans(),
				func(db *sql.DB, o *ccrConsumerOptions) {
					o.posts = append(o.posts, WithPostNotifications(postgres.NewNotificationRepository(db)))
				},
				withCCRCommentOptions(WithCommentClock(clock.Now)),
				func(db *sql.DB, o *ccrConsumerOptions) {
					o.votes = append(o.votes, WithVoteClock(clock.Now),
						WithVoteNotifications(postgres.NewNotificationRepository(db)))
				})
			// On a native PDS: the comment consumer trusts the test bridge host,
			// whose accounts get no notifications.
			insertBridgedUserOnPDS(t, f.db, bhAuthor, "bhauthor.test", bridgedTestNativePDS)
			insertBridgedUserOnPDS(t, f.db, bhOther, "bhother.test", bridgedTestNativePDS)
			createdAt := activatedCommentNotificationTime(t, f.db, context.Background())

			tt.steps(t, f, createdAt, increasingTIDs(t, 4))

			var reason sql.NullString
			require.NoError(t, f.db.QueryRow(`SELECT hide_reason FROM votes WHERE uri = $1`,
				bhVoteURI(bhOther, bhOtherVoteRkey)).Scan(&reason))
			require.False(t, reason.Valid, "fixture: B's upvote is counted, not pending or hidden")
			assert.Equal(t, subjectCounts{Upvotes: 1, Downvotes: 0, Score: 1}, readSubjectCounts(t, f.db,
				`SELECT upvote_count, downvote_count, score FROM comments WHERE uri = $1`, tt.subject))
			assert.Equal(t, 1, groupCount(t, f.db, bhAuthor, tt.subject),
				"A gets an upvote group for B's upvote once it is counted")
		})
	}
}
