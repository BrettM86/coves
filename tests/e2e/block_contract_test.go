//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The blocking domain's pipeline contracts: the ingestion proofs for
// social.coves.actor.block (user blocks user) and social.coves.community.block
// (user blocks community), plus the auth boundary of the block endpoints.
//
// # BOTH BLOCK COLLECTIONS ARE INVISIBLE TO AN UNAUTHENTICATED READER, BY DESIGN
//
// Every other contract in this package watches a record appear on a serving
// endpoint. Neither of these can, and the reason is not an oversight in the
// tests — it is what a block IS. A block is private state about one viewer, so
// every surface that reveals one is scoped to that viewer's session:
//
//   - social.coves.actor.getBlockedUsers is behind RequireAuth
//     (internal/api/routes/userblock.go:25).
//   - actor.getProfile hydrates viewer.blocking only when a session names the
//     viewer (internal/api/routes/user.go:161-174).
//   - The feed, discover, timeline and comment queries filter blocked authors
//     only when the request carries a ViewerDID, which the handlers take from
//     the session (feed_repo.go:52, discover_repo.go:50, timeline_repo.go:55,
//     comment_repo.go:659). With no viewer they deliberately return everything.
//   - social.coves.community.post.get replaces a blocked author's post with a
//     blockedPost union member, again only for a viewer
//     (internal/core/posts/service.go:741).
//   - community.block has NO read surface at all: there is no
//     getBlockedCommunities route, and nothing in the product filters on the
//     community_blocks table (see the note at the end of this comment).
//
// §3.4b's standing limitation then closes the door: nothing outside the browser
// OAuth callback mints a session RequireAuth accepts, so this tier cannot hold
// one. A contract here therefore cannot ask "is the row there?" of any
// endpoint. That was verified by spike rather than assumed — an anonymous
// post.get on a post whose author the caller has blocked serves the post in
// full, exactly as it should.
//
// # WHAT THESE CONTRACTS PROVE INSTEAD, AND WHY IT IS STILL PIPELINE PROOF
//
// The AppView reports on its own consumers at /health/consumers — connection
// state, cursor, events processed and events dead-lettered, per consumer
// (cmd/server/health.go). That endpoint is public, and §3.4c already treats it
// as an observation surface for the reliability suite. Two facts about a block
// commit are visible through it, and together they are worth more than they
// look:
//
//  1. THE COMMIT REACHED THE CONSUMER THAT OWNS THE COLLECTION, AND DID NOT
//     FAIL. Bounded the way post's spoof negative is bounded: a second record
//     is written INTO THE SAME REPO right after the block, and that second
//     record's effect IS anonymously visible (a profile display name, a
//     subscriber count). A repo's commits cannot overtake each other anywhere
//     on the path — the PDS sequencer orders them, Jetstream serialises per
//     repo, the connector hands them to the consumer one at a time — so when
//     the later record is being served, the block commit has already been
//     through the same handler. A zero delta on that consumer's dead-letter
//     counter then says it did not fail on the way through.
//
//  2. THE HANDLER REALLY PARSES BLOCK RECORDS. Claim 1 alone has a hole big
//     enough to matter: a consumer that ignored the collection entirely would
//     also show "processed, no failures". So each contract also writes a
//     MALFORMED block — one with no subject — and requires the dead-letter
//     counter to move by exactly one. That can only happen inside the block
//     handler's own validation (user_consumer.go:488-491,
//     community_consumer.go:791-794). For actor.block it proves something
//     further, which is the part most likely to rot: handleUserBlock returns
//     early, silently, when userBlockRepo is nil (user_consumer.go:448-453) —
//     BEFORE parsing. A dead letter is therefore proof that cmd/server actually
//     wired the block repository into the consumer, which is the exact kind of
//     wiring §3.4's whole "the AppView container consumes" design exists to
//     catch, and which no handler test can see.
//
// What remains unproven here is the last hop: that the row lands in
// user_blocks / community_blocks with the right columns. That is proven at T1,
// against real SQL, in internal/atproto/jetstream (consumer → repo) and
// internal/db/postgres (repo → rows), and the enforcement those rows drive is
// proven at T1/T0 in internal/db/postgres and internal/core/posts. The split is
// stated here rather than left implicit, because a marker is a claim and this
// one is narrower than its siblings'.
//
// # WHY THE MARKERS ARE STILL THE RIGHT CALL
//
// The alternative was to leave both collections in
// tests/ci/pending_contracts.txt forever, which would have meant the manifest
// check never covers them and nobody is told when the ingestion path for a
// block breaks. What these contracts do catch is the whole class the manifest
// exists for: a collection dropped from consumerWantedCollections (which
// happened to community.block once already — feeds.go:51-55 records that its
// handler worked for months while no subscribe URL ever asked for the
// collection), a consumer that stops being wired into cmd/server, a repository
// left nil, a parse that starts rejecting valid records. Every one of those is
// invisible to the T1 tests, which build their own consumer.
//
// # KNOWN PRODUCT GAP, FOUND WRITING THIS
//
// community.block indexes a row that nothing ever reads. The table is written
// by the consumer, read back by its own repository methods, and cascade-deleted
// when an account is deleted — and that is all: no feed, discover, timeline,
// comment or post query mentions community_blocks, and communities.Service's
// GetBlockedCommunities and IsBlocked have no HTTP route. Blocking a community
// today hides nothing from anybody. Filed as
// 2026-07-29-community-blocks-indexed-but-never-enforced, and deliberately NOT
// asserted here: a test pinning "a blocked community's posts are still served"
// would be pinning the bug. What this contract asserts is the half that works —
// the record is indexed faithfully — which is also what the fix will build on.
const (
	actorBlockCollection     = "social.coves.actor.block"
	communityBlockCollection = "social.coves.community.block"
)

// blockRecord builds a block record in the shape both block services write it
// (userblocks/service.go:114, communities/service.go:942): a subject and a
// timestamp, nothing else. Both collections share the shape, which is why one
// builder serves both.
func blockRecord(collection, subjectDID string) map[string]any {
	return map[string]any{
		"$type":     collection,
		"subject":   subjectDID,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// malformedBlockRecord builds a block record with NO subject: the field both
// consumers require and reject the event over.
//
// It is a deliberate, minimal malformation. The record is otherwise valid, so
// the only thing that can reject it is the block handler's own check — which is
// the entire point of writing it (see the file's opening note, claim 2).
func malformedBlockRecord(collection string) map[string]any {
	return map[string]any{
		"$type":     collection,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// consumerCounters is one consumer's event tally, summed over every feed that
// carries it.
//
// Summed rather than read per connector because the AppView runs one connector
// per consumer PER FEED (jetstream.ParseFeeds), and how many feeds there are is
// deployment configuration: the CI stack has one ("self"), production has two.
// A contract that reached for a single connector by name would be asserting
// against .env.ci rather than against the AppView.
type consumerCounters struct {
	Processed    uint64
	DeadLettered uint64
	Connectors   int
}

// counters reads one consumer's tally from /health/consumers.
//
// The consumer name is the canonical one — "users", "communities" — and matches
// both naming forms the AppView uses: the primary feed's consumers keep the
// bare name, every other feed's are "<consumer>@<feedKey>" (feeds.go's
// PrimaryFeedKey note). In the CI stack the only feed key is "self", so the
// names on the wire are all suffixed; hard-coding that would break the day the
// stack grew a second feed.
//
// A name that matches NOTHING is fatal rather than an empty tally, and that is
// the most important line in this helper: every assertion built on these
// counters is a delta, and two zero readings subtract to a delta of zero, so a
// renamed consumer would turn "no event failed" into a test that passes without
// looking at anything.
func (p *pipeline) counters(t *testing.T, consumer string) consumerCounters {
	t.Helper()
	total, err := p.countersOrErr(consumer)
	require.NoError(t, err, "reading /health/consumers, which these contracts observe through")
	return total
}

// countersOrErr is counters for a caller that has no *testing.T to fail: a wait
// probe, which turns the error into its own failure (testkit.WaitFor treats a
// probe error as fatal) with the wait's description attached.
//
// The unmatched-name case is an error here rather than a require, and stays
// just as fatal for exactly the reason the require gives.
func (p *pipeline) countersOrErr(consumer string) (consumerCounters, error) {
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	report, err := p.AppView.ConsumerHealth(ctx)
	if err != nil {
		return consumerCounters{}, err
	}

	var total consumerCounters
	for _, c := range report.Consumers {
		base, _, hasFeed := strings.Cut(c.Name, "@")
		if !(c.Name == consumer || (hasFeed && base == consumer)) {
			continue
		}
		total.Processed += c.EventsProcessed
		total.DeadLettered += c.EventsDeadLettered
		total.Connectors++
	}
	if total.Connectors == 0 {
		return consumerCounters{}, fmt.Errorf(
			"no connector on /health/consumers is named %q or %q@<feed> — the AppView's consumer "+
				"naming changed, and every delta this contract measures would silently read zero. "+
				"Health document was:\n%s", consumer, consumer, report)
	}
	return total, nil
}

// blockDelivery is one measured window: a block commit, bounded by a later
// commit in the same repo whose effect is visible, plus the consumer's tally
// before and after.
//
// WHY THE BOUND HAS TO BE A LATER EVENT, not the block's own effect: the
// connector increments these counters AFTER the handler returns
// (connector.go's handleWithRetry call site), so a reading taken the instant a
// record became visible could still be missing that record's own increment.
// Bounding on a SUBSEQUENT commit in the same repo removes the race entirely —
// the block was counted before the bounding record's handler was even entered.
type blockDelivery struct {
	before consumerCounters
	after  consumerCounters
}

// requireAccepted asserts the block commit went through the consumer without
// failing: the bound has been observed by the caller, so the commit is behind
// us, and nothing was dead-lettered while it happened.
func (d blockDelivery) requireAccepted(t *testing.T, what string) {
	t.Helper()
	require.Equalf(t, d.before.DeadLettered, d.after.DeadLettered,
		"%s: the consumer dead-lettered %d event(s) while the block commit and its bounding "+
			"record went through. Nothing else writes to this stack during a serial contract, so "+
			"the failure was one of them",
		what, d.after.DeadLettered-d.before.DeadLettered)
	require.Greaterf(t, d.after.Processed, d.before.Processed,
		"%s: the consumer's processed counter did not move at all, so it saw neither the block "+
			"commit nor the record that bounds it — which means this window measured nothing",
		what)
}

// requireRejected asserts exactly one event was dead-lettered in the window:
// the malformed block, rejected inside the block handler's own validation.
func (d blockDelivery) requireRejected(t *testing.T, what string) {
	t.Helper()
	require.Equalf(t, uint64(1), d.after.DeadLettered-d.before.DeadLettered,
		"%s: expected exactly one dead letter — the malformed block record. A delta of ZERO means "+
			"the handler never looked at the record (the collection is not being consumed, or the "+
			"handler short-circuited before parsing, which for actor.block means the block "+
			"repository was not wired into the consumer). A delta above one means something else "+
			"failed in the same window — with ONE known-innocent exception: if the log shows this "+
			"consumer reconnecting within ~5s of the write, the cursor rewind (connector.go's "+
			"cursorRewind) replayed the malformed event, and while the dead letter ROW dedups "+
			"(state_store.go's ON CONFLICT DO NOTHING) the in-memory counter increments anyway — so "+
			"a delta of 2 alongside a reconnect is the rewind, not a second failure",
		what)
}

// TestActorBlockIngestion is the pipeline proof for user-to-user blocks.
//
// coves:ingestion-contract social.coves.actor.block
//
// Every record is written straight into the BLOCKER's own repo — a block lives
// with the person who made it, never with its subject — and each step is
// observed through the AppView's consumer health, bounded by a profile write
// into the same repo whose display name IS served back. The file's opening note
// explains why that is the observation surface here and what it does and does
// not prove.
//
//	block            → the users consumer takes the commit without failing
//	malformed block  → the users consumer REJECTS it, proving it parses these
//	                   records and has the block repository wired
//	unblock (delete) → the delete commit goes through the same way
func TestActorBlockIngestion(t *testing.T) {
	p := newPipeline(t)

	blocker := p.IndexedAccount(t, "bk")
	blocked := p.IndexedAccount(t, "bd")

	// ARM. users.maybeBackfillProfile fetches actor.profile/self straight from
	// the PDS when a freshly indexed user's profile row is COMPLETELY empty
	// (contracts_test.go's reconciliation hazard). This contract's bound is a
	// profile display name being served, so a backfill that raced could serve
	// one with the firehose dead and the bound would prove nothing. Backfill
	// re-checks emptiness immediately before writing, so one non-empty write
	// disarms it for good — and every measurement below is on a LATER write.
	arming := "arming " + testkit.UniqueID(t)
	blocker.PutRecord(t, profileCollection, "self", map[string]any{
		"$type": profileCollection, "displayName": arming,
	})
	awaitDisplayName(t, p, blocker.DID, arming, "the arming profile write to disarm backfill")

	// measure runs one window: snapshot the users consumer, write whatever the
	// step is writing, then write a profile marker INTO THE SAME REPO and wait
	// for it to be served. When the marker is visible the earlier commit has
	// necessarily already been through the same consumer (same repo ⇒ ordered
	// end to end), so the tally taken afterwards covers it.
	measure := func(what string, write func()) blockDelivery {
		t.Helper()
		before := p.counters(t, "users")
		write()
		marker := what + " " + testkit.UniqueID(t)
		blocker.PutRecord(t, profileCollection, "self", map[string]any{
			"$type": profileCollection, "displayName": marker,
		})
		awaitDisplayName(t, p, blocker.DID, marker,
			fmt.Sprintf("the profile written straight after %s to be served, which bounds it", what))
		return blockDelivery{before: before, after: p.counters(t, "users")}
	}

	// ---- block ---------------------------------------------------------------
	blockRkey := testkit.TID()
	measure("the block", func() {
		blocker.PutRecord(t, actorBlockCollection, blockRkey,
			blockRecord(actorBlockCollection, blocked.DID))
	}).requireAccepted(t, "blocking a user")

	// ---- a block record the handler must refuse ------------------------------
	// The step that turns "the consumer saw something" into "the consumer parses
	// THIS collection with a live repository behind it". See the file's opening
	// note, claim 2.
	measure("the malformed block", func() {
		blocker.PutRecord(t, actorBlockCollection, testkit.TID(),
			malformedBlockRecord(actorBlockCollection))
	}).requireRejected(t, "a block record with no subject")

	// ---- unblock -------------------------------------------------------------
	// A delete commit carries no record body, so the consumer resolves the
	// block by the URI it reconstructs from repo DID, collection and rkey
	// (user_consumer.go:539). A mismatch there would make every unblock a
	// silent no-op — the handler treats "no such block" as success — so what
	// this step can see from outside is only that the commit was taken. The row
	// really disappearing is asserted at T1, where the rows are visible.
	measure("the unblock", func() {
		blocker.DeleteExistingRecord(t, actorBlockCollection, blockRkey)
	}).requireAccepted(t, "unblocking a user")

	// ---- the auth boundary ---------------------------------------------------
	// Every NSID RegisterUserBlockRoutes puts behind RequireAuth
	// (internal/api/routes/userblock.go), listed together for the reason
	// TestCommunityAPIContract gives: a handler test cannot see a route that was
	// registered without its middleware, and for THIS domain that mistake would
	// publish one user's private block list to anyone who asked.
	//
	// The read endpoint is here too, unlike in the other contracts' matrices,
	// because getBlockedUsers being authenticated is not incidental — it is the
	// reason this whole file observes through consumer health.
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	err := p.AppView.Procedure(ctx, "social.coves.actor.blockUser",
		map[string]any{"subject": blocked.DID}, nil)
	require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
		"social.coves.actor.blockUser must answer 401 to a client with no session, answered: %v", err)

	err = p.AppView.Procedure(ctx, "social.coves.actor.unblockUser",
		map[string]any{"subject": blocked.DID}, nil)
	require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
		"social.coves.actor.unblockUser must answer 401 to a client with no session, answered: %v", err)

	err = p.AppView.Query(ctx, "social.coves.actor.getBlockedUsers", nil, nil)
	require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
		"social.coves.actor.getBlockedUsers must answer 401 to a client with no session — it serves "+
			"one user's private block list and there is no public form of it. Answered: %v", err)
}

// awaitDisplayName waits until getProfile serves displayName for actor.
//
// The bounding observation of TestActorBlockIngestion, and the only thing it
// uses profiles for. Kept out of the user contract's file because it is this
// contract's mechanism rather than that one's subject.
func awaitDisplayName(t *testing.T, p *pipeline, actorDID, displayName, description string) {
	t.Helper()
	p.Await(t, description, func() (bool, error) {
		view, err := p.Profile(context.Background(), actorDID)
		if err != nil {
			return false, err
		}
		return view.DisplayName == displayName, nil
	})
}

// TestCommunityBlockIngestion is the pipeline proof for user-blocks-community.
//
// coves:ingestion-contract social.coves.community.block
//
// The record lives in the USER's repo and names a community as its subject
// (communities/service.go:942) — it is a reader hiding a place, not a moderator
// banning a person, and the direction is worth stating because the collection's
// name suggests the opposite. It is consumed by the COMMUNITIES consumer, which
// is why this contract measures a different counter from its sibling above and
// bounds its windows with a different visible record: a subscription, whose
// subscriber count community.get serves.
//
//	block            → the communities consumer takes the commit without failing
//	malformed block  → the communities consumer REJECTS it, proving it parses
//	                   this collection rather than ignoring it
//	unblock (delete) → the delete commit goes through the same way
//
// Choosing subscriptions as the bound has a bonus: they are the OTHER
// collection this consumer takes from a user's repo, so the two are proven to
// interleave correctly in one repo's commit order.
func TestCommunityBlockIngestion(t *testing.T) {
	p := newPipeline(t)

	creator := p.IndexedAccount(t, "cb")
	blocker := p.IndexedAccount(t, "cr")
	community := indexedCommunity(t, p, "cx", creator.DID)

	awaitSubscribers := func(want int, description string) {
		t.Helper()
		p.Await(t, description, func() (bool, error) {
			view, err := p.Community(context.Background(), community.DID)
			if err != nil {
				return false, err
			}
			return view.SubscriberCount == want, nil
		})
	}

	// Same shape as the actor contract's window, with the subscriber count as
	// the visible bound. Every bounding record is written into the BLOCKER's
	// repo — the same repo the block goes into — because per-repo commit order
	// is the whole guarantee.
	measure := func(what string, write func(), bound func()) blockDelivery {
		t.Helper()
		before := p.counters(t, "communities")
		write()
		bound()
		return blockDelivery{before: before, after: p.counters(t, "communities")}
	}

	subscription := testkit.TID()

	// ---- block ---------------------------------------------------------------
	blockRkey := testkit.TID()
	measure("the community block",
		func() {
			blocker.PutRecord(t, communityBlockCollection, blockRkey,
				blockRecord(communityBlockCollection, community.DID))
		},
		func() {
			blocker.PutRecord(t, subscriptionCollection, subscription,
				subscriptionRecord(community.DID, 3))
			awaitSubscribers(1, "the subscription written straight after the block, which bounds it")
		}).requireAccepted(t, "blocking a community")

	// ---- a block record the handler must refuse ------------------------------
	measure("the malformed community block",
		func() {
			blocker.PutRecord(t, communityBlockCollection, testkit.TID(),
				malformedBlockRecord(communityBlockCollection))
		},
		func() {
			blocker.DeleteExistingRecord(t, subscriptionCollection, subscription)
			awaitSubscribers(0, "the unsubscribe written straight after the malformed block, which bounds it")
		}).requireRejected(t, "a community block record with no subject")

	// ---- unblock -------------------------------------------------------------
	measure("the community unblock",
		func() {
			blocker.DeleteExistingRecord(t, communityBlockCollection, blockRkey)
		},
		func() {
			blocker.PutRecord(t, subscriptionCollection, testkit.TID(),
				subscriptionRecord(community.DID, 3))
			awaitSubscribers(1, "the re-subscribe written straight after the unblock, which bounds it")
		}).requireAccepted(t, "unblocking a community")

	// The two community block NSIDs (blockCommunity, unblockCommunity) are not
	// re-asserted here: TestCommunityAPIContract's boundary matrix already
	// enumerates every NSID RegisterCommunityRoutes puts behind RequireAuth, and
	// splitting that list would make its completeness — the property that
	// matters — harder to see.
}
