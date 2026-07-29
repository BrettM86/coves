//go:build e2e

// Package e2e is the pipeline tier: docs/TEST_ARCHITECTURE.md §3.4's contracts.
//
// # WHAT A TEST IN THIS PACKAGE MAY DO
//
// A contract writes a record STRAIGHT TO THE PDS and then watches it appear on
// an AppView serving endpoint. Everything between those two points — the PDS
// firehose, Jetstream, the AppView's own consumers, Postgres — is the system
// under test, and none of it is stood up by the test. The AppView is the
// container the gate built from the working tree, consuming exactly as it does
// in production.
//
// That shape is what makes the tier honest, and it only survives if three rules
// hold (§3.4, and they are not stylistic):
//
//   - NEVER dial a websocket. A test that subscribes to Jetstream itself proves
//     that Jetstream delivers, which nobody doubted; the question is whether the
//     AppView's consumers are wired up, and only the serving endpoint answers it.
//     tests/testkit/firehose.go exists for T1 and for debugging, not for here.
//   - NEVER instantiate a consumer. cmd/server's wiring is precisely what this
//     tier exists to exercise. A test-owned consumer passes with the shipped one
//     dead.
//   - NEVER open a testkit.DB clone to assert on AppView state. The AppView
//     writes coves_dev; testkit.DB hands out clones of a template. Asserting
//     against a clone reads a database nothing under test ever wrote to, so it
//     fails for a reason that has nothing to do with the code. Observe through
//     endpoints. If a contract needs fixture state that no API can create, that
//     is a missing endpoint or a missing T1 test — report it, do not reach into
//     the database.
//
// Two files still in this package predate the tier and break these rules
// (error_recovery_test.go instantiates consumers in-process and clones the test
// database; user_signup_test.go hand-rolls its HTTP). Task 16 rebuilds or
// deletes them. Until then they set this package's infrastructure floor — see
// TestMain — and they are not a precedent.
//
// # HAZARD: RECONCILIATION PATHS (sync indexing's subtler sibling)
//
// docs/TEST_ARCHITECTURE.md §3.4 warns that several domains index
// SYNCHRONOUSLY on the client path, so an endpoint-in/endpoint-out test passes
// with the firehose dead. There is a second, quieter version of the same trap,
// and it survives the direct-PDS write that defeats the first one:
// RECONCILIATION CODE THAT READS THE PDS BY ITSELF.
//
// The known instance is profile backfill. users.maybeBackfillProfile (service.go)
// spawns a DETACHED goroutine at IndexUser time that fetches
// social.coves.actor.profile/self straight from the user's PDS and writes it to
// Postgres. A contract that signs an account up, writes a profile record to the
// PDS, and waits for it to appear can therefore be satisfied by that goroutine
// with every consumer dead — the record really did come from the PDS, just not
// through the firehose.
//
// The guard is the same in every case: reconciliation is conditional, so make
// its condition false before making the assertion. Backfill only touches a
// profile that is COMPLETELY empty (checked at the spawn site AND re-checked
// immediately before the write, precisely so a firehose event cannot be
// clobbered), so a contract must first get the row into a non-empty state and
// only then assert on a second, freshly-written value. TestPipelineSmoke does
// exactly that, and the actor.profile contract (task 14) must too.
//
// Before writing a contract, look for who else reads the PDS: a backfill, a
// hydration path, a lazy repair on read. If one exists for your collection,
// the first observation proves nothing.
//
// # HAZARD: THE RATE LIMITER IS A SHARED, RUN-SPANNING RESOURCE
//
// The AppView rate limits every request at 100/minute per client IP
// (cmd/server/routes.go), in a fixed window, in memory. Two consequences the
// tier has to design around:
//
//   - Every service shares one network namespace, so by default every request
//     from every contract arrives from 127.0.0.1 — ONE bucket for the whole
//     tier. Polling at 100ms, two contracts are enough to exhaust it, and
//     because PendingIfNotFound treats a 429 as terminal the victim fails with
//     "HTTP 429" rather than a timeout, which reads like an application bug.
//   - The buckets outlive the test binary, so a re-run against a kept stack
//     inherits the previous run's spent quota.
//
// So newPipeline gives each contract its OWN bucket, via an X-Real-IP unique to
// this run and this test (testkit.SyntheticClientIP). The arithmetic that
// remains is stated at contractPollInterval — read it before changing either
// number, because the budget and the interval are only safe together.
//
// # DECLARING A CONTRACT
//
// An ingestion contract declares which collection it proves with a comment line
// whose first word is the token cmd/contract-manifest looks for
// (coves:ingestion-contract, followed by the collection NSID). That command
// walks jetstream.ConsumedCollections() and fails the gate when a consumed
// collection has no such marker and no entry in tests/ci/pending_contracts.txt.
// The inventory is therefore generated, never curated: adding a collection to a
// consumer breaks the build until it is proven.
//
// # PINNING A DEFECT INSIDE A MARKED CONTRACT
//
// Contracts keep finding bugs, and a bug this phase is not fixing gets PINNED:
// an assertion of what the shipped code currently does, written so it fails
// loudly the moment somebody fixes it. The question that comes up each time is
// whether a pin may live inside a function carrying an ingestion marker, since
// a marker is a claim that the collection is proven and a pin is a record that
// something about it is broken.
//
// THE RULE: a pin may share a marked contract's arc, provided the contract
// STILL PASSES once the defect is fixed — everything except the pinned step,
// which is expected to fail and is what announces the fix. If a fix would also
// break the surrounding proof, the pin belongs in its own unmarked test
// function beside the contract, because the marker would otherwise be
// advertising a proof that no longer runs.
//
// Both shapes are in the tree as worked examples, and the difference is
// instructive. vote_contract_test.go pins a same-rkey update being dropped
// INSIDE TestVoteIngestion: fixing it changes one step's expected counts and
// leaves create, re-tap, direction change and delete proving exactly what they
// prove today. The out-of-order defect is pinned OUTSIDE, in
// TestVoteOutOfOrderIsLostAndSubtracts, because its whole arc is the defect —
// there is no residual pipeline proof left if the behaviour changes, so it
// carries no marker and claims nothing about the collection.
//
// Two obligations either way. Name the issue file in the assertion message, so
// a red run says which defect got fixed rather than merely which line moved.
// And state the wrong-but-current value with Holds, not only Await, wherever an
// asynchronous fix — a reconciliation pass, a lazy repair on read — could
// satisfy an eventually-check on its way to the right answer and leave the pin
// silently passing against corrected code. A pin that cannot detect its own
// obsolescence is worse than no pin, because it reads as coverage.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"Coves/tests/testkit"
)

// TestMain sets this package's infrastructure floor.
//
// Missing infrastructure is a FAILURE, not a skip (§3.1): invoking -tags e2e is
// asking for the whole stack, so the package says once, up front, which service
// it could not reach — instead of every contract timing out separately and
// blaming a different feature.
//
// RequirePostgres is owed to the two legacy files described in the package doc,
// not to the contracts: a contract observes through endpoints and never opens a
// database. It comes out when task 16 does.
func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m,
		testkit.RequirePostgres,
		testkit.RequirePDS,
		testkit.RequireAppView,
		testkit.RequireJetstream,
	))
}

// contractBudget is how long a contract waits for the pipeline to deliver, and
// it is the ONLY such number in this tier.
//
// One constant, because the alternative is what the pre-refactor suite had:
// per-loop literals that drifted from 5s to 30s with no rationale anywhere, and
// an effective timeout (the gorilla read deadline) that silently undercut every
// advertised one. A single name means a slow CI machine is one edit, and it
// means nobody has to wonder whether this particular wait was tuned or typed.
//
// Sized for the whole chain — PDS commit, firehose fan-out, Jetstream, the
// consumer's own retry-on-transient-error, the AppView's write — on a loaded
// machine, not for the happy path. Waits finish as soon as the probe says so,
// so a generous budget costs nothing when the pipeline is healthy and buys a
// diagnosable failure when it is not.
const contractBudget = 45 * time.Second

// contractHoldWindow is how long a "stays true" assertion watches.
//
// A separate dimension from contractBudget rather than a second opinion about
// the same one: WaitFor asks how long delivery may take, Holds asks how long we
// watch for a delivery that must NOT arrive — a replayed delete resurrecting a
// record, a duplicate inflating a count. Its cost is paid in full on every
// passing run, which is why it is a fraction of the budget rather than equal to
// it.
const contractHoldWindow = 5 * time.Second

// contractPollInterval is how often a T2 wait re-asks the serving endpoint.
//
// Slower than testkit's 100ms default ON PURPOSE, and the reason is the rate
// limiter described in the package doc. Every poll is a request against a
// 100-per-minute budget, so the interval and contractBudget are a pair:
//
//	45s budget ÷ 250ms  =  180 polls  if a wait runs its FULL length
//
// which is over the 100 a bucket allows. That is deliberate rather than
// overlooked, because of what the two cases cost:
//
//   - A wait that SUCCEEDS costs one or two polls. The pipeline delivers in
//     well under a second on this stack, and WaitFor probes before it sleeps,
//     so a healthy contract never approaches the budget. This is every poll the
//     tier issues on a green run.
//   - A wait that FAILS was going to fail anyway. Past roughly 25 seconds it
//     starts collecting 429s instead of "not yet" — so Await translates that
//     status into a message saying so, rather than letting a rate limit
//     masquerade as a broken endpoint.
//
// Buying the difference would mean either a 1s interval (adding half a second
// to every wait in the tier for the benefit of runs that are already red) or
// raising the AppView's limit in .env.ci — which would delete the one signal
// that a polling storm is happening at all. Neither trade is worth it.
const contractPollInterval = 250 * time.Millisecond

// contractHoldPollInterval is how often a Holds re-asks. It is deliberately
// four times slower than contractPollInterval, and the reason is arithmetic the
// comment above does not cover.
//
// contractPollInterval reasons about WaitFor, where a green run costs one or
// two polls because the wait ends the moment the probe succeeds. Holds is the
// opposite: it has no early exit — proving something STAYED true means watching
// the whole window — so it pays its full cost on every PASSING run. At 250ms
// that is 20 requests per Holds, and a contract with two of them spends 40 of
// its 100-request minute before its first assertion. That was affordable while
// contracts were shallow and stopped being affordable at the comment domain,
// whose setup alone stands up an account, a community and two posts, each with
// its own wait; the first draft of TestCommentIngestion exhausted its bucket
// mid-Holds and reported a rate limit where the pipeline was in fact healthy.
//
//	5s window ÷ 1s  =  5 requests per Holds, against 20
//
// Five samples is not a weaker assertion than twenty, because of WHAT a Holds
// in this tier watches: a record resurrected by a replayed create, a delete
// undone, a count inflated by a duplicate. Every one of those is a durable row
// change — once it happens it is still there on the next poll, and on the one
// after that. Sampling frequency would matter for a transient blip, and no
// contract here guards against one. What bounds the assertion is the WINDOW,
// which is unchanged at contractHoldWindow: the tier still watches for the same
// five seconds, it just stops asking four times a second.
const contractHoldPollInterval = time.Second

// withReadCadence slows a wait to a poll rate that fits inside a 20-per-minute
// endpoint's budget, for the ONE endpoint in the product that has one.
//
// # THE TIER HAS TWO RATE LIMITERS, NOT ONE
//
// Everything reachable here is bounded by the global 100/minute-per-IP limiter,
// and contractPollInterval's arithmetic is written against that. But
// social.coves.community.comment.getComments carries a second, much tighter cap
// of its own — commentQueryRateLimit, 20 per minute (cmd/server/routes.go) —
// because a nested tree query fans out across the whole comment tree.
//
// At the default 250ms, poll 21 of a single wait lands about five seconds in.
// So a delivery that is slow but perfectly healthy — a loaded machine, a
// consumer working through a backlog — would collect a 429 five seconds into a
// forty-five second budget and fail as though the endpoint were broken, having
// never spent the budget it was given. The failure would be indistinguishable
// from a real fault and would arrive most often on exactly the slow runs where
// the budget matters.
//
//	45s budget ÷ 2.5s  =  18 polls  for a wait that runs its FULL length
//
// which fits under 20 with room for the reads a contract makes outside its
// waits. WaitFor probes before it sleeps, so a healthy delivery still costs one
// poll and the slower cadence costs nothing; what it buys is that an unhealthy
// one reports the timeout it actually is.
//
// Apply it to waits that poll getComments, and ONLY those: every other endpoint
// sits under the global limiter alone, where 250ms is both affordable and worth
// paying for the tighter failure detection.
func withReadCadence() testkit.WaitOption {
	return testkit.WithPollInterval(contractCappedReadInterval)
}

// contractCappedReadInterval is the poll rate withReadCadence installs. See
// there for why it is this number and not the default.
const contractCappedReadInterval = 2500 * time.Millisecond

// pipeline is the fixture every contract starts from: the stack's PDS, for
// writes the AppView cannot see, and the AppView, for the observations that can
// only be explained by the firehose having delivered them.
type pipeline struct {
	PDS     *testkit.PDS
	AppView *testkit.AppView
	// clientIP is the rate-limit bucket this contract spends from. Kept so a
	// 429 can name it — "which bucket did I exhaust" is otherwise unanswerable
	// from a failure message.
	clientIP string
}

// newPipeline returns the two endpoints a contract talks to.
//
// Both come from testkit.Endpoints(), so a contract never spells an address —
// which is what keeps this tier pointed at the hermetic stack (§3.7) rather
// than at whatever a developer happens to be running.
//
// The AppView client is bound to a synthetic client IP unique to this run and
// this test, giving the contract a rate-limit bucket of its own (see the
// package doc). Call it once per contract: two pipelines in one test would
// split its quota across two buckets, which is harmless, but two tests sharing
// one pipeline would share a bucket, which is the thing being avoided.
func newPipeline(t *testing.T) *pipeline {
	t.Helper()
	clientIP := testkit.SyntheticClientIP(t.Name())
	return &pipeline{
		PDS:      testkit.NewPDS(t),
		AppView:  testkit.NewAppView(t, testkit.WithAppViewClientIP(clientIP)),
		clientIP: clientIP,
	}
}

// FreshReadQuota re-points this contract's AppView client at a NEW rate-limit
// bucket, for arcs that legitimately need more requests than one bucket allows.
//
// # WHEN THIS IS THE RIGHT ANSWER, WHICH IS RARELY
//
// The package doc explains the per-contract bucket: it exists so that contracts
// do not steal each other's quota. This is the escape hatch for the other case
// — a SINGLE contract whose honest arc costs more requests than the endpoint it
// observes will serve — and the comment domain is why it exists.
//
// social.coves.community.comment.getComments is not bounded by the global
// 100/minute limiter that everything else in this tier meets. It has a
// dedicated, far tighter one: commentQueryRateLimit = 20 per minute
// (cmd/server/routes.go), because a nested tree query fans out across the whole
// comment tree and is the most expensive read the AppView serves. It is the only
// route in the product with its own cap.
//
// Twenty is generous for a human reading threads and nowhere near enough for an
// ingestion contract, which must watch that one endpoint through create, reply,
// update and delete, plus two Holds windows — the arc costs upward of twenty
// reads no matter how it is written, and it is the thread endpoint or nothing,
// because no other endpoint shows reply placement or serves a deleted comment's
// placeholder at all (actor.getComments omits deleted comments entirely).
//
// # WHY THIS IS LEGITIMATE AND NOT QUOTA LAUNDERING
//
// The rate limiter is not under test in an ingestion contract, and taking a
// fresh bucket at a phase boundary is exactly what a second reader arriving at
// the thread would do — the tier already synthesises a client IP per contract,
// and this synthesises one more. What it must NOT be used for is making an
// assertion about rate limiting pass, or papering over a contract that polls
// wastefully; the poll intervals exist to keep the cost honest in the first
// place, and a contract reaching for this more than a couple of times is
// describing a tier problem rather than solving one.
//
// reason is recorded in the client IP's label, so an exhausted bucket in a
// failure message can be traced to the phase that spent it.
//
// It REBUILDS the AppView client rather than mutating one, so anything else
// carried on that client has to be carried across deliberately. Today that is
// the bearer token, preserved below; a future option added to NewAppView must
// be added here too, or a contract will silently lose it half-way through and
// fail somewhere that looks unrelated to the rotation.
func (p *pipeline) FreshReadQuota(t *testing.T, reason string) {
	t.Helper()
	// Read before the rebuild. Nothing in the tier authenticates a T2 client
	// today (§3.4b — no sealed session can be minted), so this is empty in
	// every current caller; it is preserved anyway because the day that
	// changes, a dropped credential turns into a 401 in a contract that never
	// mentions auth.
	bearer := p.AppView.Bearer

	p.clientIP = testkit.SyntheticClientIP(t.Name() + "/" + reason)
	p.AppView = testkit.NewAppView(t,
		testkit.WithAppViewClientIP(p.clientIP),
		testkit.WithAppViewBearer(bearer))
}

// Await waits for probe to become true within contractBudget, attaching the
// AppView's consumer health to the failure if it does not.
//
// That attachment is the difference between a T2 timeout an agent can act on
// and one it cannot: "the record never appeared" and "the consumer that indexes
// it has been disconnected for four minutes with 12 dead letters" are the same
// failure, and only the second one names a next step.
// Trailing options are applied AFTER the defaults, so a caller can override
// one — withReadCadence being the reason the parameter exists.
func (p *pipeline) Await(t *testing.T, description string, probe testkit.Probe, opts ...testkit.WaitOption) {
	t.Helper()
	testkit.WaitFor(t, contractBudget, p.explainRateLimit(probe),
		append([]testkit.WaitOption{
			testkit.WithPollInterval(contractPollInterval),
			testkit.WithDescription("%s", description),
			testkit.WithConsumerHealth(p.AppView),
		}, opts...)...)
}

// explainRateLimit rewrites a 429 into a sentence about this tier's own
// polling, because that is what a 429 here means.
//
// Nothing else in the hermetic stack sends traffic to the AppView, so a rate
// limit reached during a wait was reached by the wait. Left unexplained it
// reads as "the serving endpoint rejected us", sending whoever is debugging
// after an auth or handler problem that does not exist. It is NOT converted
// into "not yet": the wait still fails, immediately, which is correct — the
// contract was already past the point where it could pass.
func (p *pipeline) explainRateLimit(probe testkit.Probe) testkit.Probe {
	return func() (bool, error) {
		done, err := probe()
		if err != nil && testkit.IsStatus(err, http.StatusTooManyRequests) {
			return false, fmt.Errorf(
				"the AppView rate limited this contract's polling (bucket %s, %s per poll while waiting "+
					"and %s while holding). Which limiter it was decides what to do: the GLOBAL one is "+
					"100/minute and means the wait had already run long enough that the pipeline was not "+
					"going to deliver — treat it as the timeout it is and read the consumer health below. "+
					"But social.coves.community.comment.getComments carries its OWN cap of 20/minute "+
					"(commentQueryRateLimit, cmd/server/routes.go), which a healthy contract can reach "+
					"simply by observing a long arc — if that is the endpoint being polled here, see "+
					"pipeline.FreshReadQuota: %w",
				p.clientIP, contractPollInterval, contractHoldPollInterval, err)
		}
		return done, err
	}
}

// Holds asserts probe stays true for contractHoldWindow.
//
// The destructive half of every contract: an eventually-check cannot catch
// resurrection-by-replay, because the record is correctly absent at the moment
// it looks. Deletes must STAY deleted (§3.4a).
func (p *pipeline) Holds(t *testing.T, description string, probe testkit.Probe) {
	t.Helper()
	testkit.Holds(t, contractHoldWindow, p.explainRateLimit(probe),
		testkit.WithPollInterval(contractHoldPollInterval),
		testkit.WithDescription("%s", description),
		testkit.WithConsumerHealth(p.AppView))
}

// IndexedAccount creates an account the AppView knows about, and returns a live
// PDS session on it.
//
// It goes through social.coves.actor.signup rather than straight to
// com.atproto.server.createAccount, and that is not laziness — it is forced by
// the consumer's policy. internal/atproto/jetstream's user consumer indexes
// profile and identity events only for DIDs it has already seen ("this prevents
// us from indexing millions of Bluesky users we don't care about"), so a repo
// created directly on the PDS is invisible to the AppView no matter what it
// writes. Signup is how an identity enters the index; it is also, per §3.4, a
// SYNCHRONOUS path and therefore proves nothing about the pipeline.
//
// The value here is the session: with it a contract writes records DIRECTLY to
// the repo, which the AppView never sees, and any subsequent appearance on a
// serving endpoint can only be firehose delivery.
func (p *pipeline) IndexedAccount(t *testing.T, prefix string) *testkit.Account {
	t.Helper()

	label := testkit.UniqueIDWithPrefix(t, prefix)
	handle := p.PDS.Endpoint.Handle(label)
	password := testkit.DefaultPassword

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	var signup struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	}
	err := p.AppView.Procedure(ctx, "social.coves.actor.signup", map[string]string{
		"handle":   handle,
		"email":    label + "@test.coves.dev",
		"password": password,
	}, &signup)
	if err != nil {
		t.Fatalf("signing up %s through the AppView: %v", handle, err)
	}
	if signup.DID == "" {
		t.Fatalf("signup of %s answered 200 without a DID", handle)
	}

	// Signup indexes synchronously, so this is a fast assertion rather than a
	// wait on the pipeline — but it is worth making, because every contract
	// built on this account will otherwise blame the firehose for an identity
	// that was never indexed at all.
	p.Await(t, fmt.Sprintf("signup to index %s", handle), func() (bool, error) {
		_, err := p.Profile(context.Background(), signup.DID)
		return testkit.PendingIfNotFound(err)
	})

	account := p.PDS.Login(t, handle, password)
	if account.DID != signup.DID {
		t.Fatalf("signup reported DID %s but a session on %s is %s", signup.DID, handle, account.DID)
	}
	return account
}

// ProfileView is the slice of social.coves.actor.getProfile's response that
// contracts observe. The endpoint returns a full profileViewDetailed; modelling
// only what is asserted keeps an added lexicon field from breaking every
// contract that reads a profile.
type ProfileView struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// Profile reads an actor's profile from the AppView. A missing actor comes back
// as a not-found StatusError, which testkit.PendingIfNotFound turns into "not
// indexed yet" inside a probe.
func (p *pipeline) Profile(ctx context.Context, actor string) (ProfileView, error) {
	var view ProfileView
	err := p.AppView.Query(ctx, "social.coves.actor.getProfile", url.Values{"actor": {actor}}, &view)
	return view, err
}
