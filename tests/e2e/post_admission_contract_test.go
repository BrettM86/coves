//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The admission-wiring proof: cmd/server's PRODUCTION-constructed post service
// routes an authenticated social.coves.community.post.create through the §8
// admission decision (PRD_AUTHOR_OWNED_POSTS, internal/core/posts/admit.go),
// observed end-to-end through the real XRPC surface.
//
// # THE CREDENTIAL, AND WHY IT IS AN AGGREGATOR'S
//
// This contract holds the first real write credential the tier has ever held.
// §3.4b's standing limitation still stands for USERS — nothing but the browser
// OAuth callback mints a sealed session token RequireAuth accepts — but
// post.create is the one Coves route behind DualAuth, and DualAuth's second
// path takes a PDS-signed service JWT from a REGISTERED AGGREGATOR. Every link
// of that chain is mintable inside the hermetic stack:
//
//   - the aggregator is a PDS account (provisionAggregatorRepo), whose DID the
//     hermetic PLC can resolve to a signing key;
//   - it becomes REGISTERED by declaring social.coves.aggregator.service in
//     its own repo, which only the firehose can index (aggregator contract's
//     opening note) — so holding a working credential at all already proves
//     pipeline delivery;
//   - the JWT itself comes from the PDS' own com.atproto.server.getServiceAuth,
//     signed with the account's repo key, audience'd to the AppView's instance
//     DID — exactly what a production bot does.
//
// internal/api/routes/post_aggregator_test.go names this seam as the one it
// cannot reach ("needs the running stack and a token") and injects the
// principal instead. This file is that missing half: the shipped binary's
// DualAuthMiddleware validating a real signature against the hermetic PLC and
// gating on the firehose-fed aggregators table.
//
// # WHAT THIS PROVES ABOUT THE ADMISSION POLICY — AND WHAT IT CANNOT
//
// admitPost classifies this principal ActorRegisteredAggregator, so the checks
// it walks through the wire are community resolution (step 1), aggregator
// authorization (step 4) and the dedupe ledger (step 5). Three properties of
// the NEW decision are pinned here:
//
//   - the decision is LIVE: the 403 flips to admitted when the community's
//     authorization record arrives over the firehose, with no redeploy;
//   - the check ORDER holds: an identical resubmission of a REFUSED submission
//     answers the same refusal again, never 409 DuplicateSubmission — with the
//     ledger wired, a decision that consulted dedupe ahead of authorization
//     would answer 409 the second time (§8: a refusal consumes no quota);
//   - reserve-then-release holds: a submission that is ADMITTED but which then
//     fails before the record exists must hand its ledger slot back, so retrying
//     it answers the same failure again, never 409 — a leaked reservation would
//     turn one transient failure into a lockout until the dedupe window rolls.
//
// The two USER-classified refusals — 403 Banned (step 3) and the per-author
// 429 RateLimitExceeded (step 6) — are structurally out of this tier's reach:
// they require an ActorUser principal, which requires the sealed-session mint
// that does not exist (§3.4b), and no aggregator credential is ever classified
// ActorUser. They are proven where they can be honestly: the decision matrix
// at T0 (internal/core/posts/admit_matrix_test.go, service_admission_test.go),
// the ledger against real Postgres at T1 (internal/db/postgres), and the
// refusal-to-status mapping at T0 (internal/api/handlers/post/errors_test.go).
// What none of those can see — the production construction in
// cmd/server/wiring.go actually enforcing the decision on the wire — is what
// this file adds. It carries NO ingestion marker: markers are for pipeline
// proofs (§3.4a), and this asserts the client path.
//
// # THE ADMITTED PATH'S KNOWN CEILING, STATED PLAINLY
//
// An admitted submission gets past every gate and then stops, and the write-path
// flip MOVED where. It used to stop at the community-credential refresh: a
// community indexed from the firehose carries no PDS credentials in the
// AppView's store, so EnsureFreshToken on an empty token failed and the mapper
// reported an unclassified 500.
//
// A post is written to its AUTHOR's repository now (PRD §4.2 step 3), so the
// credential that has to exist is the AUTHOR's, and this tier cannot mint one
// for the same reason §3.4b gives for everything else. The two ways to hold one
// are a browser OAuth session (which needs the sealed-session mint that does not
// exist here) or an aggregator's STORED tokens from migration 025 (which are
// written by that same OAuth grant, performed once by a human operator). A
// service JWT authenticates the caller to the AppView; it is not a repo
// credential and was never meant to be one.
//
// So the ceiling is now ErrNoAuthorCredentials, which the mapper reports as a
// NAMED 503 — and that is a strictly better probe than the 500 it replaces. A
// 500 was the absence of a classification: it would have been answered just as
// readily by a nil-pointer panic three layers down. A 503 NoAuthorCredentials is
// a specific outcome that can only be produced at one place in the write path,
// so reaching it proves the request travelled past admission, past the actor
// classification, past the ledger reservation, and all the way to the author-repo
// open. Any 4xx means the admission gate refused something it had authorized;
// any 500 now means something unclassified broke.
//
// The moment a credentialed author becomes reachable at T2, the same test
// upgrades itself to the full dedupe proof (the branch is written out below).
func TestPostAdmissionAPIContract(t *testing.T) {
	p := newPipeline(t)

	moderator := p.IndexedAccount(t, "nm")
	community := indexedCommunity(t, p, "n", moderator.DID)
	aggregator, _ := indexedAggregator(t, p, "na")

	botToken := mintServiceJWT(t, aggregator)
	asAggregator := p.AppView.As(botToken)

	// One submission, byte-identical on every attempt: the dedupe fingerprint
	// hashes the record as the client sent it, so proving what repeats DON'T
	// trigger requires the repeats to be genuine.
	title := "admission " + testkit.UniqueID(t)

	t.Run("a service JWT from a DID that is no aggregator stops at the middleware", func(t *testing.T) {
		// The security property internal/api/routes/post_aggregator_test.go
		// documents as unprovable there: a VALID signature from a real,
		// PLC-resolvable identity is still refused when the DID is not in the
		// aggregators table. The moderator is exactly that — an indexed USER
		// whose PDS mints service JWTs as willingly as anyone's.
		//
		// The message is asserted as well as the code, and it is load-bearing:
		// the middleware answers 401 AuthenticationRequired for a broken
		// signature too, and only the message tells "refused by the aggregator
		// gate" from "the validator could not resolve the issuer" — the second
		// would mean the stack's PLC plumbing is broken, not that the gate held.
		err := submitPost(p.AppView.As(mintServiceJWT(t, moderator)), community.DID, title)
		refusal := requireXRPCRefusal(t, err, http.StatusUnauthorized, "AuthenticationRequired",
			"a non-aggregator's service JWT")
		require.Equal(t, "Not a registered aggregator", refusal.XRPCMessage,
			"the 401 must come from the aggregator gate, not from signature validation: %v", err)
	})

	t.Run("an unknown community is the decision's first refusal", func(t *testing.T) {
		// Answering a POST-mapper refusal at all — not 401 — is the positive
		// half of the credential proof: DualAuth validated the aggregator's JWT
		// against the hermetic PLC, found the DID in the firehose-fed
		// aggregators table, and let the request through to the service, where
		// admitPost step 1 refused it.
		//
		// The DID literal is spelled at 24 base32 characters (a-z, 2-7) for the
		// reason TestPostAPIContract gives: UniqueID does not promise that
		// alphabet, and a malformed identifier would take the 400 validation
		// path instead of the resolution path under test. Nothing indexes this
		// DID, on a fresh stack or a kept one.
		err := submitPost(asAggregator, "did:plc:aaaaaaaaaanevercommunity", title)
		requireXRPCRefusal(t, err, http.StatusNotFound, "CommunityNotFound",
			"a submission to a community nobody has indexed")
	})

	t.Run("an unauthorized aggregator is refused, and the refusal consumes nothing", func(t *testing.T) {
		// The community exists and is indexed, but has written no authorization
		// record for this aggregator — admitPost step 4, carried through the
		// mapper as the aggregators-package 403.
		err := submitPost(asAggregator, community.DID, title)
		requireXRPCRefusal(t, err, http.StatusForbidden, "NotAuthorized",
			"a submission from an aggregator the community never authorized")

		// The SAME submission again. §8's check order is observable right here:
		// authorization runs AHEAD of dedupe, and a refusal reserves nothing —
		// so the identical resubmission meets the identical 403. A decision
		// that consulted the ledger first, or leaked a reservation on refusal,
		// would answer 409 DuplicateSubmission instead, and this is the only
		// tier that can catch the production wiring doing that.
		err = submitPost(asAggregator, community.DID, title)
		requireXRPCRefusal(t, err, http.StatusForbidden, "NotAuthorized",
			"the identical resubmission of a refused submission — a 409 here means a refusal "+
				"consumed a dedupe slot, which §8 forbids")
	})

	// The community lets the aggregator in, the way production does: an
	// authorization record in the COMMUNITY's own repo, delivered over the
	// firehose. The wait observes the same table ValidateAggregatorPost reads.
	community.PutRecord(t, aggregatorAuthorizationCollection, testkit.TID(),
		aggregatorAuthorizationRecord(aggregator.DID, community.DID, moderator.DID, true))
	p.Await(t, "the authorization to reach the index the admission decision reads", func() (bool, error) {
		enabled, err := p.Authorizations(context.Background(), aggregator.DID, true)
		if err != nil {
			return false, err
		}
		return len(enabled) == 1, nil
	})

	t.Run("the authorization's arrival flips the decision without a redeploy", func(t *testing.T) {
		// Byte-identical to the submission refused twice above — so everything
		// that changed between that 403 and this answer is the firehose-fed
		// authorization row, which is the liveness of the decision in one
		// assertion. Its two prior refusals reserved nothing, so this attempt's
		// own reservation cannot collide with them.
		err := submitPost(asAggregator, community.DID, title)

		if err == nil {
			// The stack can complete an author-credentialed write — the admitted
			// path ran to the PDS and back. The reservation is now CONFIRMED on
			// the ledger, so the identical resubmission is the full dedupe proof.
			err = submitPost(asAggregator, community.DID, title)
			requireXRPCRefusal(t, err, http.StatusConflict, "DuplicateSubmission",
				"an identical resubmission of an admitted post inside the dedupe window")
			return
		}

		// Today's ceiling (see the file comment): admission PASSED and the write
		// then stopped at the AUTHOR-repo open, because no principal this tier
		// can mint holds a repo credential. The status and the NAME are both
		// pinned, and the name is what makes this branch worth having — it is
		// produced at exactly one place in the write path, so meeting it proves
		// the submission travelled past every gate this contract is about.
		//
		// The two ways this assertion fails are the two regressions it exists to
		// catch. A 4xx means the admission decision refused a submission the
		// community has authorized. A 500 means the write path broke somewhere
		// that has no classification at all — which is what this arc used to
		// assert, back when the ceiling was the community-credential refresh, and
		// is precisely the vagueness the named sentinel removed.
		requireXRPCRefusal(t, err, http.StatusServiceUnavailable, "NoAuthorCredentials",
			"an ADMITTED submission stopping at the author-repo open — any 4xx here means the "+
				"admission decision refused a submission the community has authorized, and any 500 "+
				"means it broke somewhere unclassified instead")

		// And the failed write handed its ledger slot back: the identical retry
		// meets the same failure, never 409. This property is UNCHANGED by the
		// flip and had to be re-verified rather than assumed — the release now
		// happens at a different step (the author-repo open, which sits between
		// admission and the community token), and a reservation released on the
		// old step but not the new one would look identical from every other
		// tier. A leaked one refuses the retry as a duplicate of a post that
		// does not exist: the §8 failure mode where a transient outage becomes a
		// lockout until the dedupe window rolls.
		err = submitPost(asAggregator, community.DID, title)
		requireXRPCRefusal(t, err, http.StatusServiceUnavailable, "NoAuthorCredentials",
			"the retry of a failed write — a 409 means the failed write's reservation was never "+
				"released, turning one failure into a lockout until the dedupe window rolls")
	})
}

// mintServiceJWT asks the stack's PDS to sign a service JWT for account,
// audience'd to the AppView's instance identity — the credential a production
// aggregator bot presents to post.create.
//
// The audience is communityInstanceDID for the reason that constant documents:
// INSTANCE_DID is unset in .env.ci, so the AppView's DualAuth validator was
// built with internal/config's compiled-in default, and a JWT for any other
// audience is refused before the signature is even consulted. lxm is pinned to
// the one route the token is spent on; the AppView validates service JWTs
// endpoint-agnostically (auth.go: lexMethod nil), so this is defence on the
// MINTING side — a leaked test token authorizes nothing else.
func mintServiceJWT(t *testing.T, account *testkit.Account) string {
	t.Helper()

	var minted struct {
		Token string `json:"token"`
	}
	err := account.XRPC().Query(context.Background(), "com.atproto.server.getServiceAuth", url.Values{
		"aud": {communityInstanceDID},
		"lxm": {"social.coves.community.post.create"},
	}, &minted)
	if err != nil {
		t.Fatalf("minting a service JWT for %s via com.atproto.server.getServiceAuth: %v", account.DID, err)
	}
	if minted.Token == "" {
		t.Fatalf("com.atproto.server.getServiceAuth answered 200 with no token for %s", account.DID)
	}
	return minted.Token
}

// submitPost drives social.coves.community.post.create as the holder of
// client's credential, with the minimal well-formed body the lexicon requires.
// The transport error is returned rather than asserted: half of this contract
// is about which refusal comes back.
func submitPost(client *testkit.AppView, community, title string) error {
	return client.Procedure(context.Background(), "social.coves.community.post.create", map[string]any{
		"community": community,
		"title":     title,
		"content":   "submitted through the admission gate",
	}, nil)
}

// requireXRPCRefusal asserts err is an XRPC error envelope with exactly this
// status and error name, and returns it for callers that assert further. The
// name is asserted as well as the status because it is the machine-readable
// half clients switch on: a 403 NotAuthorized tells an aggregator to stop, a
// 403 with any other name tells it nothing.
func requireXRPCRefusal(t *testing.T, err error, status int, code, what string) *testkit.StatusError {
	t.Helper()

	var se *testkit.StatusError
	require.ErrorAsf(t, err, &se,
		"%s must be refused with an XRPC error envelope, got: %v", what, err)
	require.Equalf(t, status, se.StatusCode, "%s: answered %v", what, err)
	require.Equalf(t, code, se.XRPCError, "%s: answered %v", what, err)
	return se
}
