//go:build integration

package postgres

import (
	"context"
	"math/rand"
	"sync"
	"testing"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The §5.2 rule under contention, which is the only condition it actually has
// to hold under.
//
// Every other test in this package applies events one at a time, and a
// single-threaded test passes against an implementation that reads the current
// watermark, decides, and then writes — the classic lost update. Production
// does not deliver events one at a time: the AppView drains several overlapping
// Jetstream feeds carrying the same community repos, and two consumers can be
// inside the same subject's transition simultaneously. Whether the tuple
// comparison survives that is decided entirely by whether the comparison
// happens INSIDE the writing statement, and only concurrency can tell the
// difference.
//
// The claim under test is convergence, not serialization: whatever order the
// scheduler picks, the row must end up as though the events had arrived in
// tuple order. Every event carries a distinct tuple, so "tuple order" is total
// and the expected final state is unambiguous — it is whatever the highest
// tuple asks for.
//
// Run under -race. This is also where a duplicate INSERT would show up: the
// first event for a subject takes the INSERT branch, and N goroutines racing to
// take it must produce one row, arbitrated by the primary key rather than by a
// prior SELECT.

func TestAdmissionRepo_ConcurrentCommunityEventsConverge(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	subject := newAdmissionSubject(t, db)

	const eventCount = 8
	const finalDecisionCode = "highest_tuple_removal"

	revs := increasingRevs(t, eventCount)

	// One event per rev, in tuple order. The LAST one is a removal, so the
	// expected final state is a single unambiguous row rather than something
	// this test has to compute from the mix.
	type communityEvent struct {
		describe string
		apply    func() (posts.AdmissionResult, error)
	}

	events := make([]communityEvent, eventCount)
	for i, rev := range revs {
		rev := rev
		switch {
		case i == eventCount-1:
			events[i] = communityEvent{"removal (highest tuple)", func() (posts.AdmissionResult, error) {
				return repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
					CommunityDID: subject.CommunityDID,
					PostURI:      subject.PostURI,
					DecisionCode: finalDecisionCode,
					Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
				})
			}}
		case i%3 == 0:
			events[i] = communityEvent{"acceptance", func() (posts.AdmissionResult, error) {
				acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
				return repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
					CommunityDID:   subject.CommunityDID,
					PostURI:        subject.PostURI,
					AcceptanceURI:  acceptanceURI,
					AcceptanceRkey: acceptanceRkey,
					PinnedCID:      contentCID(t, "race"),
					Watermark:      posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
				})
			}}
		case i%3 == 1:
			events[i] = communityEvent{"acceptance deletion", func() (posts.AdmissionResult, error) {
				return repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
					CommunityDID: subject.CommunityDID,
					PostURI:      subject.PostURI,
					Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpDelete},
				})
			}}
		default:
			events[i] = communityEvent{"intermediate removal", func() (posts.AdmissionResult, error) {
				return repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
					CommunityDID: subject.CommunityDID,
					PostURI:      subject.PostURI,
					DecisionCode: "intermediate",
					Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
				})
			}}
		}
	}

	// Launch order is shuffled from a fixed seed so a failure is reproducible.
	// It only removes a launch-order bias — the order the statements actually
	// reach Postgres is the scheduler's to decide, and that is the point.
	shuffled := make([]int, eventCount)
	for i := range shuffled {
		shuffled[i] = i
	}
	rand.New(rand.NewSource(20260807)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	outcomes := make([]posts.AdmissionOutcome, eventCount)
	failures := make([]error, eventCount)

	var waitGroup sync.WaitGroup
	waitGroup.Add(eventCount)
	for _, index := range shuffled {
		go func(index int) {
			defer waitGroup.Done()
			result, err := events[index].apply()
			outcomes[index], failures[index] = result.Outcome, err
		}(index)
	}
	waitGroup.Wait()

	applied := 0
	for i, err := range failures {
		require.NoErrorf(t, err, "%s at tuple %d: contention is not an error condition", events[i].describe, i)
		switch outcomes[i] {
		case posts.AdmissionApplied:
			applied++
		case posts.AdmissionSkippedStale:
		default:
			assert.Failf(t, "unexpected outcome under contention",
				"%s at tuple %d returned %q; a community event either applies or loses the tuple comparison",
				events[i].describe, i, outcomes[i])
		}
	}
	assert.GreaterOrEqual(t, applied, 1, "every event was refused, so nothing was ever written")

	final, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)

	assert.Equal(t, posts.AdmissionStatusRemoved, final.Status,
		"the highest tuple is a removal, so it decides the row no matter when it ran")
	assertNullableString(t, finalDecisionCode, final.DecisionCode,
		"decision_code: an intermediate removal's code surviving means a lower tuple overwrote a higher one")
	assertWatermark(t, revs[eventCount-1], posts.CommunityOpPut, final.LastCommunityEvent)

	assert.Nil(t, final.AcceptanceURI, "the winning removal must have cleared the acceptance columns, not half of them")
	assert.Nil(t, final.AcceptanceRkey)
	assert.Nil(t, final.AcceptedCID)

	var rowCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2
	`, subject.CommunityDID, subject.PostURI).Scan(&rowCount))
	assert.Equal(t, 1, rowCount,
		"eight goroutines raced for the INSERT branch; the primary key has to be what arbitrates that, "+
			"not a SELECT that decided the row was absent")
}

func TestAdmissionRepo_SameTupleDuplicateAppliesExactlyOnce(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// The multi-feed overlap case in its purest form: two consumers deliver the
	// SAME event — identical tuple, identical payload — simultaneously, to a
	// subject with no row yet. Exactly one delivery may apply; the other must be
	// the equal-tuple replay, and the row must not betray that it happened twice.
	subject := newAdmissionSubject(t, db)
	rev := testkit.TID()
	const duplicates = 2

	results := make([]posts.AdmissionResult, duplicates)
	failures := make([]error, duplicates)

	var waitGroup sync.WaitGroup
	waitGroup.Add(duplicates)
	for i := 0; i < duplicates; i++ {
		go func(i int) {
			defer waitGroup.Done()
			results[i], failures[i] = repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
				CommunityDID: subject.CommunityDID,
				PostURI:      subject.PostURI,
				DecisionCode: "duplicate_delivery",
				Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
			})
		}(i)
	}
	waitGroup.Wait()

	applied, skipped := 0, 0
	for i := 0; i < duplicates; i++ {
		require.NoErrorf(t, failures[i], "delivery %d: a duplicate is the system working, never an error", i)
		switch results[i].Outcome {
		case posts.AdmissionApplied:
			applied++
		case posts.AdmissionSkippedStale:
			skipped++
		default:
			assert.Failf(t, "unexpected duplicate outcome", "delivery %d returned %q", i, results[i].Outcome)
		}
	}
	assert.Equal(t, 1, applied, "exactly one of two identical deliveries may apply")
	assert.Equal(t, 1, skipped, "the other must be refused as the equal-tuple replay it is")

	final, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)
	for i := 0; i < duplicates; i++ {
		if results[i].Outcome == posts.AdmissionApplied {
			assert.Equal(t, results[i].Admission, final,
				"the row must be byte-stable: the refused duplicate re-stamped nothing, updated_at included")
		} else {
			assert.Equal(t, results[i].Admission, final,
				"the refused delivery must have been shown the applied row — the same row the guard was evaluated against")
		}
	}

	var rowCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2
	`, subject.CommunityDID, subject.PostURI).Scan(&rowCount))
	assert.Equal(t, 1, rowCount, "two goroutines raced the INSERT branch; the primary key arbitrates, one row results")
}

func TestAdmissionRepo_AuthorEditsRacingCommunityEvents(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	repo := NewAdmissionRepository(db)
	ctx := context.Background()

	// Production's actual mix: the author edits (fast path AND firehose carry
	// the same events) while the community's decisions stream in from
	// overlapping feeds. Author events carry no watermark, so they slot into
	// the community-event race at arbitrary points — and whatever interleaving
	// the scheduler picks, the highest community tuple is a removal and removal
	// is terminal against author events, so the final STATUS is not negotiable.
	subject := newAdmissionSubject(t, db)

	const communityEventCount = 6
	const authorEditCount = 4
	const finalDecisionCode = "highest_tuple_removal"

	revs := increasingRevs(t, communityEventCount)

	type racedEvent struct {
		describe string
		isAuthor bool
		apply    func() (posts.AdmissionResult, error)
	}

	var events []racedEvent
	for i, rev := range revs {
		rev := rev
		switch {
		case i == communityEventCount-1:
			events = append(events, racedEvent{"removal (highest tuple)", false, func() (posts.AdmissionResult, error) {
				return repo.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
					CommunityDID: subject.CommunityDID,
					PostURI:      subject.PostURI,
					DecisionCode: finalDecisionCode,
					Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
				})
			}})
		case i%2 == 0:
			events = append(events, racedEvent{"acceptance", false, func() (posts.AdmissionResult, error) {
				acceptanceURI, acceptanceRkey := acceptanceRecord(t, subject.CommunityDID)
				return repo.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
					CommunityDID:   subject.CommunityDID,
					PostURI:        subject.PostURI,
					AcceptanceURI:  acceptanceURI,
					AcceptanceRkey: acceptanceRkey,
					PinnedCID:      contentCID(t, "race"),
					Watermark:      posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
				})
			}})
		default:
			events = append(events, racedEvent{"acceptance deletion", false, func() (posts.AdmissionResult, error) {
				return repo.ApplyAcceptanceDelete(ctx, posts.CommunityDeleteCommand{
					CommunityDID: subject.CommunityDID,
					PostURI:      subject.PostURI,
					Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpDelete},
				})
			}})
		}
	}
	for i := 0; i < authorEditCount; i++ {
		cid := contentCID(t, "edit")
		events = append(events, racedEvent{"author edit", true, func() (posts.AdmissionResult, error) {
			return repo.UpsertPending(ctx, posts.UpsertPendingCommand{
				CommunityDID: subject.CommunityDID,
				PostURI:      subject.PostURI,
				EvaluatedCID: cid,
			})
		}})
	}

	shuffled := make([]int, len(events))
	for i := range shuffled {
		shuffled[i] = i
	}
	rand.New(rand.NewSource(20260807)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	outcomes := make([]posts.AdmissionOutcome, len(events))
	failures := make([]error, len(events))

	var waitGroup sync.WaitGroup
	waitGroup.Add(len(events))
	for _, index := range shuffled {
		go func(index int) {
			defer waitGroup.Done()
			result, err := events[index].apply()
			outcomes[index], failures[index] = result.Outcome, err
		}(index)
	}
	waitGroup.Wait()

	for i, err := range failures {
		require.NoErrorf(t, err, "%s (event %d): contention is not an error condition", events[i].describe, i)
		switch outcomes[i] {
		case posts.AdmissionApplied, posts.AdmissionSkippedStale:
		case posts.AdmissionSkippedTerminal:
			assert.Truef(t, events[i].isAuthor,
				"%s (event %d): only an author event may be refused as terminal here — every community tuple is distinct",
				events[i].describe, i)
		default:
			assert.Failf(t, "unexpected outcome under contention", "%s (event %d) returned %q",
				events[i].describe, i, outcomes[i])
		}
	}

	final, err := repo.Get(ctx, subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)

	assert.Equal(t, posts.AdmissionStatusRemoved, final.Status,
		"the highest community tuple is a removal and removal is terminal against author events, whatever the interleaving")
	assertNullableString(t, finalDecisionCode, final.DecisionCode, "decision_code")
	assertWatermark(t, revs[communityEventCount-1], posts.CommunityOpPut, final.LastCommunityEvent)
	assert.Nil(t, final.AcceptanceURI, "the winning removal must have cleared the acceptance columns")
	assert.Nil(t, final.AcceptanceRkey)
	assert.Nil(t, final.AcceptedCID)

	var rowCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM community_post_admissions WHERE community_did = $1 AND post_uri = $2
	`, subject.CommunityDID, subject.PostURI).Scan(&rowCount))
	assert.Equal(t, 1, rowCount, "however the race lands, there is exactly one row per subject")
}
