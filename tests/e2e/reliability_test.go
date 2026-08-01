//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The pipeline-reliability suite: docs/TEST_ARCHITECTURE.md §3.4c.
//
// # WHAT THIS TIER-WITHIN-A-TIER IS FOR
//
// The ingestion contracts (§3.4a) all describe a pipeline that is UP. They
// write a record, it arrives, they assert it arrived. Everything the ingestion
// path has for the case where it is NOT up — the persisted cursor, the rewind
// on reconnect, the rev gate's tombstones, the dead letter queue and its
// redriver, the per-feed consumer naming that makes two overlapping feeds safe
// — is invisible to them, because none of it does anything on a healthy stream.
// That machinery is also the machinery whose failure modes are silent: a cursor
// that does not persist loses exactly the records written while the process was
// restarting, and every steady-state test still passes.
//
// So each scenario here BREAKS something on purpose and then asserts what the
// system is supposed to guarantee across the break.
//
// # HOW THE BREAKING HAPPENS: THE STACK CONTROL CHANNEL
//
// These are host actions — stopping a container, recreating it with a second
// feed configured — and the tier runs inside the stack (§3.5), in a container
// with no Docker socket and no route off the internal network. It asks the host
// instead, over a file-based channel with a closed set of five argument-less
// verbs. The transport is stack_control_test.go; the host half, including why
// this shape was chosen over host-orchestrated test phases and over a
// docker.sock sidecar, is scripts/lib/ci-stack.sh.
//
// The consequence to know before reading a scenario: `make ci` and
// `make test-e2e` both serve that channel, and `make test-e2e-dev` cannot
// (there is no container to restart — it grades a host-run AppView), so that
// target excludes this suite by name.
//
// # ONE DELIBERATE DEPARTURE FROM THE SPEC'S WORDING
//
// §3.4c asks for "write during a Jetstream outage and verify replay indexes
// exactly once". The verb set stops the APPVIEW, not Jetstream, and the
// difference is worth stating because the two are not interchangeable.
//
// Stopping Jetstream would prove the connector survives its source vanishing.
// Stopping the AppView proves the property the spec is actually after — that
// events committed while nothing was consuming are indexed afterwards, from a
// cursor that persisted across a process death — and it proves it against the
// harder case, because a restarted process also has to reload that cursor from
// Postgres rather than keep it in memory. It is also the only one of the two
// that a dead-letter redrive or a topology change can be staged on top of.
//
// The Jetstream-side disconnect is covered at T1, where it is cheap and exact:
// internal/atproto/jetstream/connector_test.go drives reconnection against a
// test server that refuses dials and one that drops established connections,
// including the cursor-advance-on-reconnect path. Doing it at T2 would mean
// adding a jetstream-stop verb to the control channel to re-prove what a unit
// test already pins.
//
// # RE-RUNNABILITY
//
// Every scenario here is re-runnable against a kept stack (COVES_CI_KEEP_STACK,
// §3.4 rule 4). Nothing asserts an absolute count: identities are run-scoped,
// dead-letter claims are deltas across a window this suite opens and closes,
// and the AppView is left running single-feed however a scenario ends. The one
// state a re-run inherits is dead letter ROWS from previous runs, which is why
// no assertion here reads a backlog total.
//
// # WHAT A RESTART DOES TO THE REST OF THE TIER
//
// A restart mid-tier is not isolated to the test that asks for it: it resets
// the AppView's in-memory rate-limit buckets and its connectors' counters, and
// every later contract observes through the same process. That is safe, and
// mildly useful — the contracts that run after this file are additional
// evidence that a restarted AppView serves correctly — but it is why every
// scenario restores a healthy, single-feed AppView through t.Cleanup rather
// than trusting its own happy path to do it.
//
// # THE DEAD-LETTER LEDGER THIS SUITE ADDS TO
//
// Task 15 measured the tier's steady state after a full run: users 1,
// communities 1, posts 1, comments 1, aggregators 2, votes 0. Two scenarios
// below add an aggregator dead letter each and then consume it by redriving,
// so this file's net contribution to that ledger is zero — which is itself
// asserted, because "the redriver retired the row" is the only observable that
// distinguishes a redrive from a cursor replay.

// reliabilityConsumers are the connector names these scenarios measure. Named
// rather than spelled inline because p.counters matches both the bare form and
// the "<consumer>@<feed>" form, and getting the base name wrong turns every
// delta assertion into a comparison of two zeroes (see counters' own doc).
const (
	votesConsumer       = "votes"
	aggregatorsConsumer = "aggregators"
)

// ---------------------------------------------------------------------------
// Scenario 1: cursor resume
// ---------------------------------------------------------------------------

// TestReliabilityCursorResumeIndexesWritesMadeWhileDown proves the AppView
// resumes consumption from where it stopped rather than from the live tail.
//
// It carries NO ingestion marker: social.coves.community.post is proven by
// TestPostIngestion. What is under test here is the connector's cursor, and a
// post is merely the cheapest record whose arrival is observable.
//
// # THE SHAPE, AND WHY IT CANNOT FALSE-PASS
//
//	write a post          → indexed        (the pipeline is live before the break)
//	STOP the AppView      → assert it is refusing connections
//	write a second post   → nobody is consuming; the PDS commits it regardless
//	START the AppView     → healthy again
//	the second post       → indexed
//
// The last step is the whole test. The AppView was not running when that record
// was committed, so a connector that live-tailed on boot — which is exactly
// what it does when GetCursor returns 0 (connector.go's loadCursorOnce) — would
// subscribe from "now", never see the event, and the record would stay absent
// forever. Nothing else can index a post: unlike actor.profile there is no
// backfill path that reads the PDS on its own (see the package doc's
// RECONCILIATION hazard), so firehose replay from a persisted cursor is the
// only explanation for its appearance.
//
// The FIRST post is not decoration either. It fails the test early, and for the
// right reason, if the pipeline was already broken before anything was stopped
// — otherwise a stack whose posts consumer was dead would fail at the last step
// and read as a cursor bug.
func TestReliabilityCursorResumeIndexesWritesMadeWhileDown(t *testing.T) {
	p := newPipeline(t)
	control := requireStackControl(t)

	author := p.IndexedAccount(t, "cr")
	community := indexedCommunity(t, p, "cr", author.DID)

	beforeRKey := testkit.TID()
	community.PutRecord(t, postCollection, beforeRKey,
		postRecord(community.DID, author.DID, "before the outage", "written while the AppView was up"))
	beforeURI := postURI(community.DID, beforeRKey)

	p.Await(t, "the post written BEFORE the outage to be indexed", func() (bool, error) {
		view, err := p.Post(context.Background(), beforeURI)
		if err != nil {
			return false, err
		}
		return !view.NotFound, nil
	})

	duringRKey := testkit.TID()
	duringURI := postURI(community.DID, duringRKey)

	control.outage(t, p, func() {
		// The PDS and Jetstream are untouched by the outage — only the AppView
		// is down — so this commit succeeds and sits in Jetstream's store
		// waiting for a subscriber that can name a cursor old enough to see it.
		community.PutRecord(t, postCollection, duringRKey,
			postRecord(community.DID, author.DID, "during the outage",
				"committed to the PDS while the AppView was stopped"))
	})

	p.Await(t, "the post written DURING the outage to be indexed after the AppView resumed",
		func() (bool, error) {
			view, err := p.Post(context.Background(), duringURI)
			if err != nil {
				// PendingIfUnavailable rather than the strict form: the AppView
				// has just come back and its serving path may still answer a
				// gateway status for a moment (testkit's own note at that
				// function says this is the case it exists for).
				return testkit.PendingIfUnavailable(err)
			}
			return !view.NotFound, nil
		})

	// Read back rather than assumed: an indexed row with the wrong content
	// would mean the replay delivered something other than the record we wrote,
	// which "it exists" alone cannot rule out.
	view, err := p.Post(context.Background(), duringURI)
	require.NoError(t, err)
	require.Equal(t, "during the outage", view.Record["title"],
		"the record indexed after the resume is not the one written during the outage")
}

// ---------------------------------------------------------------------------
// Scenario 2: replay exactly once
// ---------------------------------------------------------------------------

// TestReliabilityRewindReplaysExactlyOnce proves the deliberate re-delivery a
// reconnect causes cannot double-apply an event.
//
// # WHY THERE IS A REWIND AT ALL
//
// A connector does not resume from its cursor: it resumes from its cursor MINUS
// cursorRewind, five seconds (connector.go's dialURL). Jetstream's own
// reconnection guidance asks for that overlap, because a cursor is only written
// after an event is handled and the alternative to replaying a few seconds is
// losing whichever events were in flight. The overlap is only safe if handlers
// are idempotent, and this is the test of that claim end to end.
//
// # WHY A VOTE
//
// Rows are the easy half: every consumer's write is an upsert keyed on the
// record URI, so a replayed create rewrites the same row. COUNTS are where
// double-application shows up, because an increment applied twice is simply
// wrong and nothing about the row it is derived from looks unusual afterwards.
// A vote is the cheapest counter in the product: one record, one increment,
// visible on the post's stats.
//
// # WHY THE REPLAY IS GUARANTEED TO INCLUDE THE VOTE
//
// The rewind is computed from the cursor's VALUE — the time_us of the last
// handled event — and not from the wall clock, so how long the container takes
// to stop and start does not enter into it. The vote is the last event the
// consumer handled before shutdown, so the cursor is at the vote's own
// timestamp and the connector dials from five seconds before it. A slow
// shutdown that lost the final cursor flush would only rewind FURTHER. Both
// directions replay the vote, which is what makes this deterministic rather
// than a race the test usually wins.
//
// The replay is also asserted rather than assumed: the votes consumer's
// processed counter is reset by the restart, so any movement afterwards, with
// nothing being written, is re-delivered history. Without that check a
// connector that came back live-tailing would replay nothing, count nothing
// twice, and pass this test while having lost the guarantee it is about.
func TestReliabilityRewindReplaysExactlyOnce(t *testing.T) {
	p := newPipeline(t)
	control := requireStackControl(t)

	voter := p.IndexedAccount(t, "rw")
	community := indexedCommunity(t, p, "rw", voter.DID)
	post := indexedPost(t, p, community, voter.DID, "a post to vote on across a reconnect")

	voter.CreateRecord(t, voteCollection, voteRecord(post, "up"))
	awaitStats(t, p, post.URI, "the vote to be counted once, before the reconnect",
		func(s postStats) bool { return s.Upvotes == 1 })

	// An empty outage: the point is the reconnect, not a gap to write into.
	control.outage(t, p, func() {})

	p.Await(t, "the votes consumer to have replayed rewound events after the restart",
		func() (bool, error) {
			report, err := p.AppView.ConsumerHealth(context.Background())
			if err != nil {
				return testkit.PendingIfUnavailable(err)
			}
			for _, consumer := range report.Consumers {
				base, _, _ := strings.Cut(consumer.Name, "@")
				if base == votesConsumer && consumer.EventsProcessed > 0 {
					return true, nil
				}
			}
			return false, nil
		})

	holdStats(t, p, post.URI,
		"the replayed vote must not be counted a second time: the connector re-delivers up to five "+
			"seconds of already-handled events on every reconnect, so a count that moves here is a "+
			"count that moves in production on every network blip",
		postStats{Upvotes: 1, Downvotes: 0, Score: 1, CommentCount: 0})
}

// ---------------------------------------------------------------------------
// Scenarios 3 and 4: the dead letter queue, and the tombstone that outlives it
// ---------------------------------------------------------------------------

// The pair below share one setup, one failure mode and one delivery mechanism,
// and they are the two halves of the same guarantee: a captured event is
// replayed until it can succeed, and "can succeed" is still decided by the rev
// gate.
//
// # THE FAILURE MODE, WHICH IS A REAL ONE
//
// An authorization record names an aggregator and lives in the COMMUNITY's
// repo. The two records are in different repos, so nothing orders them: a
// moderator can authorize an aggregator whose service declaration the AppView
// has not indexed yet. The insert's foreign key fails, the consumer wraps the
// error without ErrPermanentEvent, and the connector — after its in-line
// retries — writes the raw event to the dead letter queue with its redrive
// budget intact (aggregator_consumer.go, connector.go's processMessage).
// TestAggregatorAuthorizationArrivingBeforeItsAggregator asserts the capture
// and stops there, because the recovery is on a five-minute timer.
//
// # HOW A REDRIVE IS TRIGGERED WITHOUT WAITING FIVE MINUTES
//
// By restarting the AppView. DeadLetterRedriver.Run does a pass IMMEDIATELY at
// boot before starting its ticker (redrive.go), precisely so a backlog that
// accumulated while the process was down is not held hostage to the first full
// interval. So the restart these scenarios already have machinery for is also
// the redrive trigger, and it is a production code path rather than a test
// affordance.
//
// The alternative was a configurable interval — a REDRIVE_INTERVAL knob set
// short in .env.ci. It was rejected for two reasons. It adds production
// configuration whose only consumer is a test, and it would change the
// behaviour of every OTHER contract's leftovers: at a ten-second interval the
// permanently-failing dead letters the tier leaves behind would burn their
// ten-attempt budget during a run instead of over fifty minutes, so the dead
// letter table the block contracts measure against would be quietly different
// depending on how long the suite took.
//
// # WHAT PROVES IT WAS THE REDRIVER
//
// The dead letter ROW. Only DeadLetterRedriver.redriveConsumer deletes one, and
// only after the handler it replayed returned nil, so a backlog that drops by
// one is a redrive that succeeded — there is no other writer of that outcome.
// The restart's cursor rewind can also re-deliver a recent event through the
// normal path, and in scenario 4 that would index the same record; it cannot
// delete the row, so the assertion stays honest either way. Both scenarios
// therefore assert the backlog delta, not just the visible effect.

// deadLetterBacklog sums one consumer's dead letter rows across its connectors.
//
// A DATABASE count, unlike the EventsProcessed/EventsDeadLettered counters
// beside it in the same document, which live in the connector's memory and
// reset when the process does. That difference is the reason these scenarios
// can measure across a restart at all.
func deadLetterBacklog(t *testing.T, p *pipeline, consumer string) int64 {
	t.Helper()
	backlog, err := readDeadLetterBacklog(p, consumer)
	require.NoError(t, err)
	return backlog
}

// readDeadLetterBacklog is the error-returning form, so a wait can poll it
// under the Probe contract (§3.3: a non-nil error is terminal) instead of
// calling require from inside a probe, where a failed assertion would abort the
// wait without the wait's own diagnostics ever being rendered.
func readDeadLetterBacklog(p *pipeline, consumer string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	report, err := p.AppView.ConsumerHealth(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading /health/consumers: %w", err)
	}
	if report.DeadLetterBacklogUnknown {
		return 0, fmt.Errorf(
			"the AppView could not count dead letters, so every backlog assertion here would be "+
				"measuring zero against zero. Health document was:\n%s", report)
	}

	var total int64
	var matched int
	for _, state := range report.Consumers {
		base, _, hasFeed := strings.Cut(state.Name, "@")
		if state.Name == consumer || (hasFeed && base == consumer) {
			total += state.DeadLetterBacklog
			matched++
		}
	}
	if matched == 0 {
		return 0, fmt.Errorf(
			"no connector on /health/consumers is named %q or %q@<feed>, so this backlog would read "+
				"zero no matter what the queue holds. Health document was:\n%s", consumer, consumer, report)
	}
	return total, nil
}

// orphanedAuthorization is the fixture both dead-letter scenarios start from: a
// community that has authorized an aggregator nobody has declared, captured in
// the dead letter queue and provably not indexed.
type orphanedAuthorization struct {
	community  provisionedCommunity
	moderator  *testkit.Account
	undeclared *testkit.Account
	known      *testkit.Account
	rkey       string
}

// captureOrphanedAuthorization writes the orphan, bounds it, and asserts it was
// captured rather than dropped.
//
// # THE BOUND
//
// A second authorization — for an aggregator that IS declared, written into the
// SAME repo immediately after — is what makes the measurement race-free.
// Jetstream guarantees per-repo commit order and the connector handles events
// one at a time, so by the time the bounding authorization is visible the
// orphan has already been through its retries and its dead-letter write. A
// bound in a different repo would be ordering luck; a bound that is merely a
// timer would be a sleep.
func captureOrphanedAuthorization(t *testing.T, p *pipeline, prefix string) orphanedAuthorization {
	t.Helper()

	moderator := p.IndexedAccount(t, prefix+"m")
	community := indexedCommunity(t, p, prefix+"c", moderator.DID)
	known, _ := indexedAggregator(t, p, prefix+"k")

	// A real account with a real repo that has simply never declared itself:
	// the only thing wrong with the authorization is the ordering.
	undeclared := provisionAggregatorRepo(t, p, prefix+"u")

	before := p.counters(t, aggregatorsConsumer)

	rkey := testkit.TID()
	community.PutRecord(t, aggregatorAuthorizationCollection, rkey,
		aggregatorAuthorizationRecord(undeclared.DID, community.DID, moderator.DID, true))
	community.PutRecord(t, aggregatorAuthorizationCollection, testkit.TID(),
		aggregatorAuthorizationRecord(known.DID, community.DID, moderator.DID, true))

	awaitAuthorization(t, p, community.DID, known.DID,
		"the authorization for the DECLARED aggregator, which bounds the orphan",
		func(authorizationView) bool { return true })

	after := p.counters(t, aggregatorsConsumer)
	require.Equalf(t, uint64(1), after.DeadLettered-before.DeadLettered,
		"the orphaned authorization must be CAPTURED, not dropped: the aggregators consumer should "+
			"have dead-lettered exactly one event in this window (delta was %d). Everything below "+
			"tests what happens to that captured event, so there is nothing left to test if it was "+
			"never captured",
		after.DeadLettered-before.DeadLettered)

	granted, err := p.CommunityAggregators(context.Background(), community.DID, false)
	require.NoError(t, err)
	require.Len(t, granted, 1, "only the declared aggregator's authorization may be indexed at this point")
	require.Equal(t, known.DID, granted[0].AggregatorDID)

	return orphanedAuthorization{
		community:  community,
		moderator:  moderator,
		undeclared: undeclared,
		known:      known,
		rkey:       rkey,
	}
}

// awaitAuthorization waits for a community to be serving an authorization for
// aggregatorDID that satisfies accept.
func awaitAuthorization(t *testing.T, p *pipeline, communityDID, aggregatorDID, description string, accept func(authorizationView) bool) {
	t.Helper()
	p.Await(t, description, func() (bool, error) {
		granted, err := p.CommunityAggregators(context.Background(), communityDID, false)
		if err != nil {
			return testkit.PendingIfUnavailable(err)
		}
		for _, view := range granted {
			if view.AggregatorDID == aggregatorDID {
				return accept(view), nil
			}
		}
		return false, nil
	})
}

// declareAggregator declares the service record the orphaned authorization has
// been waiting for, and waits for it to be indexed — so that a redrive after
// this point can only fail for reasons the scenario is about.
func declareAggregator(t *testing.T, p *pipeline, aggregator *testkit.Account) {
	t.Helper()

	aggregator.PutRecord(t, aggregatorServiceCollection, "self",
		aggregatorServiceRecord(aggregator.DID, "late "+testkit.UniqueID(t), "declared after it was authorized"))

	p.Await(t, "the late aggregator declaration to be indexed", func() (bool, error) {
		_, found, err := p.Service(context.Background(), aggregator.DID, false)
		if err != nil {
			return testkit.PendingIfUnavailable(err)
		}
		return found, nil
	})
}

// TestReliabilityDeadLetterRedriveRecovers proves a captured event is replayed
// once the condition that failed it clears.
//
// This is §3.4c's dead-letter case, and the property is self-healing: an event
// the pipeline could not apply when it arrived is not lost, and nobody has to
// notice or intervene for it to land.
//
//	authorize an undeclared aggregator → captured (attempts 0, transient)
//	declare the aggregator             → the failure's cause is now gone
//	restart                            → the redriver's boot pass replays it
//	the authorization                  → served, and its dead letter row is gone
//
// Both halves of that last line are asserted, and the second one is the
// mechanism claim: see the note above this pair for why a backlog that dropped
// by one can only be the redriver.
func TestReliabilityDeadLetterRedriveRecovers(t *testing.T) {
	p := newPipeline(t)
	control := requireStackControl(t)

	orphan := captureOrphanedAuthorization(t, p, "d")
	declareAggregator(t, p, orphan.undeclared)

	backlogBefore := deadLetterBacklog(t, p, aggregatorsConsumer)

	// Empty outage: the restart IS the trigger (redrive.go's boot pass).
	control.outage(t, p, func() {})

	awaitAuthorization(t, p, orphan.community.DID, orphan.undeclared.DID,
		"the redriven authorization to be served after the aggregator declared itself",
		func(view authorizationView) bool { return view.Enabled })

	p.Await(t, "the redriver to have retired the dead letter row it replayed", func() (bool, error) {
		backlog, err := readDeadLetterBacklog(p, aggregatorsConsumer)
		if err != nil {
			return false, err
		}
		return backlog <= backlogBefore-1, nil
	})

	// EXACTLY one, and that exactness rests on a dependency worth naming: this
	// is the aggregators consumer's WHOLE backlog, not this scenario's row. The
	// tier leaves one other aggregators dead letter behind (task 15's baseline —
	// an invalid record whose redrive budget is exhausted at birth), and the
	// equality holds only because that row can never succeed and so can never be
	// retired by the same boot pass. If a future contract leaves a RETRYABLE
	// aggregators dead letter, this drops by two and fails here rather than
	// anywhere useful; scope the delta to the row by URI at that point.
	after := deadLetterBacklog(t, p, aggregatorsConsumer)
	require.Equalf(t, backlogBefore-1, after,
		"the aggregators dead letter backlog should have dropped by exactly one — the orphan this "+
			"scenario captured, replayed successfully and deleted (redrive.go's redriveConsumer is the "+
			"only code that deletes a dead letter, and only after its handler returned nil). It went "+
			"from %d to %d. A backlog that did not move means the record was indexed by the restart's "+
			"cursor rewind rather than by the redriver, and the redrive path is not covered by anything "+
			"else in this suite",
		backlogBefore, after)
}

// TestReliabilityRevGateRefusesResurrectionAfterDelete proves a stale event
// cannot bring back a record that has since been deleted.
//
// This is §3.4c's no-resurrection case, and it is the negative of the scenario
// above: the same captured event, the same redrive, the opposite outcome,
// because in between the record was deleted.
//
//	authorize an undeclared aggregator → captured, unindexed
//	DELETE the authorization record    → the rev gate writes its tombstone
//	declare the aggregator             → a replay would now succeed on its merits
//	restart → redrive                  → the replay is refused, and STAYS refused
//
// # WHY THIS IS THE HONEST WAY TO STAGE IT
//
// The obvious staging — let a reconnect's rewind replay a create after its
// delete was processed — cannot actually be observed from outside. A rewind
// replays the create AND the delete, in that order, so the end state is
// "deleted" whether the gate works or not, and the window in which a broken
// gate would show a resurrected record is the few milliseconds between the two
// replayed events. An eventually-check cannot see it and a Holds would have to
// be extraordinarily lucky.
//
// The dead letter queue removes the ordering constraint that made it
// unobservable. The create is held out of the stream entirely, the delete is
// processed on its own, and the create is then delivered — minutes later if we
// like — with a rev that is now older than the tombstone's. The record must
// never appear, at any point, and that IS observable.
//
// # WHAT THE TOMBSTONE IS
//
// jetstream_record_revs keeps the last applied rev per record URI and survives
// hard deletes on purpose (rev_gate.go's header, and
// WithAggregatorRevGate's own note). The delete's rev is written even though
// the row it deletes was never indexed — applyGated claims the gate row FIRST
// and the repository's delete is a documented idempotent no-op for a record it
// cannot find — which is precisely the case this scenario is in.
func TestReliabilityRevGateRefusesResurrectionAfterDelete(t *testing.T) {
	p := newPipeline(t)
	control := requireStackControl(t)

	orphan := captureOrphanedAuthorization(t, p, "n")

	// The delete, bounded by a LATER commit in the SAME repo whose effect is
	// visible: re-authorizing the declared aggregator with enabled=false flips
	// a served field (the authorizations table upserts on
	// (aggregator_did, community_did), aggregator_repo.go:358). When that flip
	// is visible the delete ahead of it in the same repo's commit order has
	// been handled, so the tombstone is in place — without a sleep, and without
	// relying on ordering between repos, which Jetstream does not promise.
	orphan.community.DeleteExistingRecord(t, aggregatorAuthorizationCollection, orphan.rkey)
	orphan.community.PutRecord(t, aggregatorAuthorizationCollection, testkit.TID(),
		aggregatorAuthorizationRecord(orphan.known.DID, orphan.community.DID, orphan.moderator.DID, false))

	awaitAuthorization(t, p, orphan.community.DID, orphan.known.DID,
		"the disabling re-authorization, which bounds the delete", func(view authorizationView) bool {
			return !view.Enabled
		})

	declareAggregator(t, p, orphan.undeclared)

	backlogBefore := deadLetterBacklog(t, p, aggregatorsConsumer)

	control.outage(t, p, func() {})

	// The redrive is what makes the assertion below meaningful: without it, the
	// authorization's absence would only prove that nothing tried to add it.
	p.Await(t, "the redriver to have consumed the captured authorization", func() (bool, error) {
		backlog, err := readDeadLetterBacklog(p, aggregatorsConsumer)
		if err != nil {
			return false, err
		}
		return backlog <= backlogBefore-1, nil
	})

	p.Holds(t,
		"the deleted authorization must NOT be resurrected by its own replayed create: the delete's "+
			"rev is newer, so the gate row rejects it (rev_gate.go). If this fails, a stale copy of any "+
			"record — from a lagging second feed in production, or from any dead letter older than a "+
			"delete — can undo a deletion",
		func() (bool, error) {
			granted, err := p.CommunityAggregators(context.Background(), orphan.community.DID, false)
			if err != nil {
				return false, err
			}
			for _, view := range granted {
				if view.AggregatorDID == orphan.undeclared.DID {
					return false, nil
				}
			}
			return true, nil
		})
}

// ---------------------------------------------------------------------------
// Scenario 5: two overlapping feeds
// ---------------------------------------------------------------------------

// TestReliabilityOverlappingFeedsDoNotDoubleIndex proves the production feed
// topology — several Jetstreams carrying the same repos — indexes each event
// once.
//
// # WHY CI'S USUAL TOPOLOGY CANNOT COVER THIS
//
// .env.ci configures ONE feed (self). Production configures two (bsky + self):
// the public Jetstream, kept for third-party PDS records, plus our own
// relay+Jetstream pair, and both carry our own PDSes' repos. Every event
// therefore arrives twice, at two connectors with independent cursors, and what
// makes that safe is the rev gate — the same mechanism scenario 3 tests from
// the staleness side, here tested from the duplication side. A gate regression
// would be invisible to every other test in this suite and would double every
// count in production.
//
// # HOW THE TOPOLOGY IS APPLIED
//
// The AppView is recreated with a compose overlay that sets two feed keys
// pointing at the same Jetstream (docker-compose.ci.two-feed.yml, which
// explains why it is a file and not an environment variable), and restored to
// the single-feed configuration by a cleanup that runs however this test ends.
//
// BOTH keys are new — neither is jetstream.PrimaryFeedKey and, more to the
// point, neither is the key the base stack runs (.env.ci uses `self`). A
// connector's name comes from its feed key and its persisted cursor is stored
// under that name, so reusing the base key would give one of the two
// connectors the cursor the base topology has been writing all run: it would
// resume five seconds behind that cursor and replay history while its partner
// live-tailed. The "both connectors processed it" assertion below would then be
// satisfied by replayed history rather than by the vote, and the overlap would
// go unproven while the test passed. An earlier version had exactly that hole,
// which is why twoFeedConnectorNames asserts the names rather than merely
// counting them.
//
// Both connectors therefore start cursor-less and live-tail, which is why the
// scenario waits for BOTH to report connected before writing anything: a write
// that landed before the second connector dialled would be seen once, and the
// test would pass without exercising an overlap.
//
// # WHAT IT ASSERTS
//
//	two connectors per consumer     the topology really is two feeds, not one
//	both of them processing         both are consuming, so the overlap is real
//	one upvote, and it stays one    the duplicate was gated, not applied
//
// The middle assertion is the one that keeps the others honest. The rev gate's
// loser still counts the event as processed (applyGated returns nil after
// skipping, and the connector counts a nil return), so "both connectors
// processed events" is exactly the observable that separates "the duplicate was
// rejected" from "the duplicate never arrived".
//
// This scenario is also a standing check on the boot rule in
// cmd/server/consumers.go: with more than one feed, the AppView REFUSES to
// start unless every consumer is rev-gated. An ungated consumer added later
// makes the recreate below fail its health wait.
func TestReliabilityOverlappingFeedsDoNotDoubleIndex(t *testing.T) {
	p := newPipeline(t)
	control := requireStackControl(t)

	control.twoFeedAppView(t, p)

	awaitFeedConnectors(t, p, votesConsumer, twoFeedConnectorNames(votesConsumer))

	voter := p.IndexedAccount(t, "of")
	community := indexedCommunity(t, p, "of", voter.DID)
	post := indexedPost(t, p, community, voter.DID, "a post voted on under two feeds")

	before := feedConnectorStates(t, p, votesConsumer)

	voter.CreateRecord(t, voteCollection, voteRecord(post, "up"))
	awaitStats(t, p, post.URI, "the vote to be counted", func(s postStats) bool { return s.Upvotes == 1 })

	// Both connectors must move, or nothing was overlapping.
	p.Await(t, "both feeds' votes connectors to have processed the vote", func() (bool, error) {
		now := feedConnectorStates(t, p, votesConsumer)
		if len(now) != len(before) {
			return false, fmt.Errorf(
				"the number of votes connectors changed mid-scenario, from %d to %d — the AppView "+
					"restarted underneath this test", len(before), len(now))
		}
		for name, state := range now {
			if state.EventsProcessed <= before[name].EventsProcessed {
				return false, nil
			}
		}
		return true, nil
	})

	holdStats(t, p, post.URI,
		"one vote delivered by two overlapping feeds must be counted ONCE: both connectors handled "+
			"it and the rev gate let exactly one of them apply it. A count of two here is what "+
			"production would show on every vote, comment and post",
		postStats{Upvotes: 1, Downvotes: 0, Score: 1, CommentCount: 0})
}

// twoFeedAppView recreates the AppView consuming two overlapping feeds, and
// restores the single-feed configuration when the test ends.
//
// The restore is registered BEFORE the change, so a failure anywhere below —
// including a t.Fatal inside the health wait — cannot leave the rest of the
// tier grading a stack this test reconfigured.
func (s *stackControl) twoFeedAppView(t *testing.T, p *pipeline) {
	t.Helper()

	t.Cleanup(func() {
		if err := s.send(t, "appview-single-feed"); err != nil {
			t.Errorf("could not restore the single-feed AppView: %v\n"+
				"Every later contract in this tier runs against it, and the stack is now in the "+
				"two-feed configuration this scenario installed", err)
			return
		}
		p.AppView.WaitHealthy(t, appViewRestartBudget)
	})

	s.do(t, "appview-two-feed")
	p.AppView.WaitHealthy(t, appViewRestartBudget)
}

// feedConnectorStates returns one consumer's connectors, keyed by their full
// per-feed name ("votes@overlap-a", "votes@overlap-b" under the two-feed
// overlay; "votes@self" under the base stack).
//
// Per connector rather than summed — which is what p.counters does — because
// the question this scenario asks is precisely about the difference between
// them: a sum cannot distinguish two connectors each processing an event from
// one connector processing two.
func feedConnectorStates(t *testing.T, p *pipeline, consumer string) map[string]testkit.ConsumerState {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	report, err := p.AppView.ConsumerHealth(ctx)
	require.NoError(t, err, "reading /health/consumers")

	states := make(map[string]testkit.ConsumerState)
	for _, state := range report.Consumers {
		base, _, hasFeed := strings.Cut(state.Name, "@")
		if state.Name == consumer || (hasFeed && base == consumer) {
			states[state.Name] = state
		}
	}
	return states
}

// twoFeedFeedKeys are the feed keys docker-compose.ci.two-feed.yml configures.
//
// Mirrored here rather than derived because the assertion they support is
// precisely that the overlay did not drift: both must differ from the base
// stack's key (.env.ci's `self`) or one connector inherits a persisted cursor
// and replays history instead of live-tailing. The overlay's header explains
// what that would cost.
var twoFeedFeedKeys = []string{"overlap-a", "overlap-b"}

// twoFeedConnectorNames returns the connector names one consumer must present
// under the two-feed overlay.
func twoFeedConnectorNames(consumer string) []string {
	names := make([]string, 0, len(twoFeedFeedKeys))
	for _, key := range twoFeedFeedKeys {
		names = append(names, consumer+"@"+key)
	}
	return names
}

// awaitFeedConnectors waits until a consumer presents exactly the named
// connectors and every one of them is connected.
//
// It asserts NAMES, not a count, and that is the guard against the subtle
// version of this scenario passing for the wrong reason. A count of two is
// satisfied by a topology that reuses the base stack's feed key, which hands
// one connector an existing cursor to rewind and replay from; the names are
// what pin both feeds as new, and therefore both connectors as live-tailing.
//
// Connectedness matters separately and becomes true later: a connector exists
// as soon as the process has wired it, and becomes connected only once its
// WebSocket dial succeeds. A live-tailing connector cannot see a record
// committed between those two moments, so a scenario that wrote before this
// returned would be measuring one delivery and calling it two.
func awaitFeedConnectors(t *testing.T, p *pipeline, consumer string, want []string) {
	t.Helper()

	var last map[string]testkit.ConsumerState
	testkit.WaitFor(t, appViewRestartBudget, func() (bool, error) {
		last = feedConnectorStates(t, p, consumer)
		if len(last) != len(want) {
			return false, nil
		}
		for _, name := range want {
			state, present := last[name]
			if !present || !state.Connected {
				return false, nil
			}
		}
		return true, nil
	},
		testkit.WithPollInterval(contractPollInterval),
		testkit.WithDescription("connected connectors %s (one per feed the overlay configures)",
			strings.Join(want, ", ")),
		testkit.WithDiagnostics(func() string {
			if len(last) == 0 {
				return fmt.Sprintf("no connector is named %q or %q@<feed>", consumer, consumer)
			}
			var b strings.Builder
			for name, state := range last {
				fmt.Fprintf(&b, "\n  %s: connected=%t cursor=%d processed=%d",
					name, state.Connected, state.CursorTimeUS, state.EventsProcessed)
			}
			return "connectors seen (a name that is not in the wanted list means " +
				"docker-compose.ci.two-feed.yml's feed keys drifted; if one of them is the base " +
				"stack's key, that connector is replaying a persisted cursor rather than " +
				"live-tailing and the overlap this scenario claims to prove is unproven):" + b.String()
		}),
		testkit.WithConsumerHealth(p.AppView))
}
