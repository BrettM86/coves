//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The community domain's pipeline contracts: the ingestion proof for
// social.coves.community.profile, and the client-facing surface a third-party
// client actually reaches.
//
// # WHY THE INGESTION CONTRACT OWNS THE COMMUNITY'S REPO OUTRIGHT
//
// docs/TEST_ARCHITECTURE.md §3.4 names community creation as one of the two
// SYNCHRONOUS client paths — communities.CreateCommunity provisions the
// community's PDS account and writes the row itself ("the Jetstream consumer
// will eventually index the community profile from the firehose, but it won't
// have the PDS credentials. We must store them now"). Anything created that way
// is in Postgres before the firehose is consulted, so it proves nothing here.
//
// This contract therefore never asks the AppView for a community. It registers
// the community's PDS account itself, exactly as an account is registered for a
// user, and writes social.coves.community.profile/self into that repo with a
// session the AppView has never seen. Nothing in the AppView is aware the
// community exists until Jetstream says so, which makes the serving endpoint's
// answer un-fakeable in the strongest form the tier has: not "the sync write
// was disarmed", but "there was never a sync write".
//
// What that buys, beyond the ingestion proof, is the consumer's UNANNOUNCED-REPO
// path: a profile record arriving from a repo the AppView did not provision, so
// createCommunity really inserts a row instead of colliding with one the
// synchronous path already wrote. That is the path a federated community's
// records would take.
//
// It is NOT federation, and the difference is worth keeping straight because
// tasks 12-15 copy this file. The repo lives on the same single PDS the stack
// fronts, resolves through the same PLC, and is reachable by the same AppView
// credentials as everything else here; nothing about a second instance, a
// second PDS or cross-host identity is exercised. §3.4a is explicit that
// single-PDS direct writes are "honest direct-PDS-path testing, not
// federation", and real federation-path contracts are the second-PDS-plus-relay
// topology of Phase 5.
//
// # RECONCILIATION PATHS: NONE FOR THIS COLLECTION (checked, see contracts_test.go)
//
// The package doc's second hazard is code that reads the PDS on its own and can
// satisfy a wait with every consumer dead. For communities the search comes up
// empty, and the search is worth recording because the next reader should not
// have to repeat it:
//
//   - users.maybeBackfillProfile, the known instance, is actor.profile-only. It
//     is reached from users.IndexUser and touches the users table.
//   - cmd/backfill-profiles is an operator CLI over the same user path. It is
//     not wired into cmd/server and does not run in the stack.
//   - the community read path (communities.GetCommunity → repo) never fetches a
//     record; the only PDS reads in internal/core/communities are write-forwards
//     (createRecordOnPDSAs / putRecordOnPDSAs) on the client path.
//
// So a single create → visible observation is already honest here, and the
// contract does not need the smoke test's arming write.
//
// # WHY THE RECORD CARRIES A handle FIELD (a finding, not a convenience)
//
// The consumer resolves a community's handle from the DID document when the
// record omits one (community_consumer.go, "NO FALLBACK - if PLC is down, we
// fail"), and resolution goes through indigo's BaseDirectory.LookupDID, which
// VERIFIES the declared handle bidirectionally: DNS TXT at _atproto.<handle>, or
// HTTPS at https://<handle>/.well-known/atproto-did. The hermetic stack is
// egress-blocked by design (§3.7), so neither can answer, and indigo's contract
// for an unresolvable handle is not an error — it is syntax.HandleInvalid.
//
// Measured, in this stack: a community indexed from the firehose without a
// handle field lands with handle "handle.invalid". The communities table has a
// UNIQUE constraint on handle, so the SECOND such community collides, the
// consumer reads the collision as idempotent replay ("Community already
// indexed") and returns nil, and the community is silently dropped. A contract
// written that way would pass once per fresh stack and then quietly stop
// proving anything.
//
// Writing the handle into the record takes that branch out of the picture: the
// consumer uses the record's handle verbatim (the path
// community_v2_validation_test.go pins) and each community keeps its own. The
// cost is stated plainly — this contract does NOT exercise PLC handle
// resolution, and cannot until the stack can answer handle lookups, which is
// the second-PDS-and-relay topology of Phase 5. The silent-drop behaviour
// itself is a production defect (one unverifiable federated community squats
// the handle.invalid row for all the others) and is reported, not worked around
// here.
//
// # WHAT THIS CONTRACT DOES NOT PROVE: hostedBy VERIFICATION
//
// The stack sets SKIP_DID_WEB_VERIFICATION=true (.env.ci, matching .env.dev),
// which makes the consumer's verifyHostedByClaim return nil on its first line.
// So the hostedBy below is indexed because verification is OFF, not because it
// passed — and an assertion phrased as "the AppView accepted this hostedBy"
// would keep passing if the check were deleted outright.
//
// Nothing in this tier can fix that: verification fetches
// https://<domain>/.well-known/did.json, which an egress-blocked network cannot
// answer, and enabling it would fail every community event here for reasons
// that have nothing to do with the pipeline. The security behaviour —
// mismatched domains rejected, non-did:web hostedBy rejected, bidirectional
// alsoKnownAs required — is covered where it can be covered honestly: at T1,
// against a local TLS server that serves the DID document
// (tests/integration/community_hostedby_security_test.go). The hostedBy
// assertion below is about FIELD TRANSPORT, that the value in the record is the
// value the endpoint serves, and nothing more.
const (
	// communityProfileCollection is spelled out rather than imported, for the
	// reason TestPipelineSmoke gives: it is a wire identifier the PDS, Jetstream
	// and the consumer must independently agree on.
	communityProfileCollection = "social.coves.community.profile"

	// communityInstanceDID is the AppView's instance identity, which in the CI
	// stack is internal/config's compiled-in default: INSTANCE_DID is unset in
	// .env.ci, so Instance.DID is "did:web:coves.social" and Instance.Domain is
	// derived from it.
	//
	// A contract cannot read it from an endpoint — the AppView publishes its
	// instance DID nowhere — so it is written down here, and the ingestion
	// contract asserts that the hostedBy it wrote is the hostedBy served back.
	// The domain half is derived from this one value rather than spelled twice,
	// and it must stay inside the PDS' PDS_SERVICE_HANDLE_DOMAINS, which is what
	// allows c-*.coves.social accounts to be registered at all.
	communityInstanceDID = "did:web:coves.social"
)

// communityInstanceDomain is the domain half of communityInstanceDID: the
// suffix community handles are issued under.
func communityInstanceDomain() string {
	return strings.TrimPrefix(communityInstanceDID, "did:web:")
}

// communityHandleFor renders the canonical handle of a community named name:
// the c- prefix and the instance domain that internal/core/communities'
// provisioner uses (pds_provisioning.go, "c-%s.%s").
func communityHandleFor(name string) string {
	return "c-" + name + "." + communityInstanceDomain()
}

// communityView is the slice of social.coves.community.get's response that the
// contracts observe. As with ProfileView, modelling only the asserted fields
// keeps a new lexicon field from breaking every contract that reads a community.
type communityView struct {
	DID             string `json:"did"`
	Handle          string `json:"handle"`
	Name            string `json:"name"`
	DisplayName     string `json:"displayName"`
	Description     string `json:"description"`
	CreatedBy       string `json:"createdBy"`
	HostedBy        string `json:"hostedBy"`
	Visibility      string `json:"visibility"`
	SubscriberCount int    `json:"subscriberCount"`

	// Avatar and Banner are HYDRATED image URLs, not the CIDs the record
	// carries — the same shape actor.getProfile serves and for the same reason
	// (blobs.HydrateImageURL). user_contract_test.go's opening note traces the
	// whole blob path; the only thing these fields add is that communities take
	// it too, through a different consumer.
	Avatar string `json:"avatar"`
	Banner string `json:"banner"`
}

// Community reads a community from the AppView by any identifier the endpoint
// accepts: a DID, a canonical handle, or the scoped !name@instance form.
func (p *pipeline) Community(ctx context.Context, identifier string) (communityView, error) {
	var view communityView
	err := p.AppView.Query(ctx, "social.coves.community.get",
		url.Values{"community": {identifier}}, &view)
	return view, err
}

// provisionedCommunity is a community's repo: the PDS account that owns it and
// the name every identifier form is derived from.
type provisionedCommunity struct {
	*testkit.Account
	Name string
}

// provisionCommunityRepo registers the PDS account a community's records live
// in, and returns a session on it.
//
// It is the community analogue of pipeline.IndexedAccount, with one difference
// that matters: there is no signup step and nothing tells the AppView this
// account exists. Communities enter the index through the profile record alone
// — the community consumer, unlike the user consumer, indexes repos it has
// never seen — so the account is invisible until a record is written to it.
//
// Tasks 12-15 need communities to hang posts, comments and votes on; this is
// the cheap way to get one that the AppView learned about honestly.
func provisionCommunityRepo(t *testing.T, p *pipeline, prefix string) provisionedCommunity {
	t.Helper()

	name := testkit.UniqueIDWithPrefix(t, prefix)
	handle := communityHandleFor(name)
	// The PDS caps a handle's local label at 18 characters, and the label here
	// is "c-" plus the name. Checked rather than assumed: over the cap, account
	// creation fails with "Handle too long", which reads like a PDS problem
	// rather than a naming-budget problem.
	if label := strings.SplitN(handle, ".", 2)[0]; len(label) > testkit.MaxIDLength {
		t.Fatalf("community handle label %q is %d characters, over the PDS' %d-character cap: "+
			"shorten the %q prefix", label, len(label), testkit.MaxIDLength, prefix)
	}

	account := p.PDS.CreateAccount(t,
		testkit.WithHandle(handle),
		testkit.WithEmail(name+"@community.test.coves.dev"))
	return provisionedCommunity{Account: account, Name: name}
}

// communityProfile builds a social.coves.community.profile record in the shape
// internal/core/communities writes it (plus the handle field the package doc
// explains), so the consumer parses exactly what production hands it.
func communityProfile(c provisionedCommunity, creatorDID, displayName, description, visibility string) map[string]any {
	return map[string]any{
		"$type":       communityProfileCollection,
		"name":        c.Name,
		"handle":      c.Handle,
		"displayName": displayName,
		"description": description,
		"visibility":  visibility,
		"createdBy":   creatorDID,
		"hostedBy":    communityInstanceDID,
		"createdAt":   time.Now().UTC().Format(time.RFC3339),
		"federation":  map[string]any{"allowExternalDiscovery": true},
	}
}

// withCommunityImage attaches a blob reference to a community profile record.
//
// Separate from communityProfile rather than a parameter on it because most
// callers want a community to hang other records on and do not care about
// images; only the ingestion contract exercises the blob path, and it should
// read as the extra step it is.
func withCommunityImage(record map[string]any, field string, ref testkit.BlobRef) map[string]any {
	record[field] = blobRefValue(ref)
	return record
}

// TestCommunityProfileIngestion is the pipeline proof for community profiles.
//
// coves:ingestion-contract social.coves.community.profile
//
// Create, update and delete, each written straight into the community's own
// repo and each observed only through social.coves.community.get:
//
//	create → the community appears, carrying the record's own field values
//	update → the same DID serves the new values
//	delete → the community is gone, and STAYS gone (Holds, §3.4a)
//
// The delete assertion is the one worth being precise about, because the
// consumer's choice is not obvious from the outside: deleteCommunity calls
// repo.Delete, which is `DELETE FROM communities WHERE did = $1` — a hard
// delete, not a tombstone. So the correct post-delete observation is the
// endpoint's 404, not an emptied row, and Holds is what distinguishes a real
// delete from one a replayed create resurrects a second later.
func TestCommunityProfileIngestion(t *testing.T) {
	p := newPipeline(t)

	// A real indexed user for createdBy: communities are created by somebody,
	// and a DID the AppView knows keeps the row honest for anything that later
	// joins communities to users.
	creator := p.IndexedAccount(t, "cmt")
	community := provisionCommunityRepo(t, p, "c")

	created := "created " + testkit.UniqueID(t)
	updated := "updated " + testkit.UniqueID(t)

	observe := func(description string, accept func(communityView) bool) communityView {
		t.Helper()
		var observed communityView
		p.Await(t, description, func() (bool, error) {
			view, err := p.Community(context.Background(), community.DID)
			if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
				return done, err
			}
			observed = view
			return accept(view), nil
		})
		return observed
	}

	// ---- create -----------------------------------------------------------
	// With an avatar, so the blob path is proven on the same arc rather than in
	// a file of its own. It is the community half of what
	// user_contract_test.go's opening note traces for actors: bytes uploaded to
	// the repo's PDS, a reference embedded in the record, a CID extracted by the
	// consumer, and a hydrated URL served back. The consumer is a different one
	// (community_consumer.go, not user_consumer.go) and the serving endpoint is
	// a different one, so neither contract covers the other.
	// The avatar only — NO banner, on purpose. The update step below adds one,
	// which is what exercises the nil→value transition the consumer's `if ok`
	// guards (community_consumer.go's create and update paths) actually govern:
	// a profile that gains a picture it did not have.
	avatar := community.UploadBlob(t, testkit.TestPNG(64, 64), "image/png")
	community.PutRecord(t, communityProfileCollection, "self",
		withCommunityImage(
			communityProfile(community, creator.DID, created, "a community the AppView never provisioned", "public"),
			"avatar", avatar))

	view := observe("the directly-written community profile to reach social.coves.community.get via the consumers",
		func(v communityView) bool { return v.DisplayName == created })

	require.Containsf(t, view.Avatar, avatar.CID(),
		"the community's avatar URL %q does not name the CID of the blob that was uploaded (%s): "+
			"the community consumer either failed to extract the blob ref or extracted the wrong one",
		view.Avatar, avatar.CID())
	require.Emptyf(t, view.Banner,
		"the community was created with no banner and the endpoint served one anyway: %q", view.Banner)
	requireServesImage(t, p, "community avatar", view.Avatar)

	require.Equal(t, community.DID, view.DID,
		"the AppView served a different community than the one that owns the repo")
	require.Equal(t, community.Handle, view.Handle,
		"the handle in the record is what the consumer must index")
	require.Equal(t, community.Name, view.Name)
	require.Equal(t, "a community the AppView never provisioned", view.Description)
	require.Equal(t, creator.DID, view.CreatedBy)
	// Field transport only — verification is off in this stack, see the package
	// doc. This says the record's hostedBy reached the endpoint intact, not that
	// the AppView checked it.
	require.Equal(t, communityInstanceDID, view.HostedBy)
	require.Equal(t, "public", view.Visibility)

	// ---- update -----------------------------------------------------------
	// Same rkey, so this is an update commit rather than a second create — the
	// consumer's updateCommunity path, which reads the existing row and writes
	// the changed fields back.
	// The avatar is replaced in the same commit: different bytes, so necessarily
	// a different CID, so necessarily a different URL. This is what catches an
	// avatar_cid the update path failed to refresh — the old URL keeps being
	// served and looks entirely healthy.
	// Two image changes in one commit, each exercising a different transition:
	// the avatar is REPLACED (value→different value) and the banner is ADDED
	// (nil→value, on a community that had none).
	replacement := community.UploadBlob(t, testkit.TestPNG(48, 48), "image/png")
	banner := community.UploadBlob(t, testkit.TestJPEG(96, 32), "image/jpeg")
	require.NotEqual(t, avatar.CID(), replacement.CID(),
		"the replacement image must differ from the original, or this step proves nothing")
	require.NotEqual(t, replacement.CID(), banner.CID(),
		"the banner must differ from the avatar, or neither assertion below can tell them apart")

	community.PutRecord(t, communityProfileCollection, "self",
		withCommunityImage(
			withCommunityImage(
				communityProfile(community, creator.DID, updated, "edited through the firehose", "unlisted"),
				"avatar", replacement),
			"banner", banner))

	view = observe("the updated profile to reach social.coves.community.get",
		func(v communityView) bool { return v.DisplayName == updated })

	require.Equal(t, "edited through the firehose", view.Description)
	require.Equal(t, "unlisted", view.Visibility,
		"the update path must carry every changed field, not only the display name")
	require.Equal(t, community.DID, view.DID)
	require.Containsf(t, view.Avatar, replacement.CID(),
		"the update did not refresh the community's avatar: still serving %q", view.Avatar)
	require.NotContains(t, view.Avatar, avatar.CID(),
		"the community still names the previous avatar's CID after the update")
	require.Containsf(t, view.Banner, banner.CID(),
		"the banner added by the update never reached the endpoint (%q): this is the nil→value "+
			"transition the consumer's `if ok` blob guards govern", view.Banner)
	requireServesImage(t, p, "community avatar after replacement", view.Avatar)
	requireServesImage(t, p, "community banner", view.Banner)

	// ---- delete -----------------------------------------------------------
	// DeleteExistingRecord rather than DeleteRecord: deleting a key that is not
	// there answers 200 and emits no commit, so a wrong rkey would turn into a
	// timeout blaming the firehose (testkit/pds.go).
	community.DeleteExistingRecord(t, communityProfileCollection, "self")

	gone := func() (bool, error) {
		_, err := p.Community(context.Background(), community.DID)
		switch {
		case err == nil:
			return false, nil
		case testkit.IsNotFound(err):
			return true, nil
		default:
			return false, err
		}
	}
	p.Await(t, "the deleted community to disappear from social.coves.community.get", gone)
	p.Holds(t, "the deleted community to stay deleted", gone)
}

// TestCommunityAPIContract covers the client-facing surface of the community
// endpoints as a third-party client meets it: what an unauthenticated caller
// gets from the write endpoints, and what any caller can read back about a
// community that exists.
//
// It carries NO ingestion marker — markers are for pipeline proofs (§3.4a), and
// this asserts the client path.
//
// # WHAT IS MISSING FROM IT, AND WHY (a finding for tasks 12-15)
//
// §3.4b asks this contract to drive the write endpoints "exactly as the mobile
// app calls them", and it cannot, because there is no non-interactive way for a
// test to hold an AppView session. OAuthAuthMiddleware.RequireAuth accepts one
// credential: a sealed session token (internal/atproto/oauth/seal.go) naming a
// row in the OAuth session store. Tokens are minted in exactly one place —
// /oauth/callback, at the end of the browser authorization-code flow against
// the PDS' own HTML login pages — and nothing else issues one:
// social.coves.actor.signup returns the PDS' accessJwt, which RequireAuth
// rejects (it is not sealed), and /oauth/refresh requires a sealed token to
// begin with.
//
// The two ways out are both bigger than one contract: drive the PDS' OAuth
// consent pages from Go (fragile against a PDS the project does not own), or
// give the AppView a test-only session-minting path (a production change, with
// the obvious care about how it is gated). The integration tier sidesteps it by
// constructing the session in-process — tests/integration/oauth_e2e_test.go
// calls store.SaveSession then client.SealSession — which T2 cannot do without
// writing to the AppView's own database, the one thing the package doc forbids.
//
// So the authenticated half of every write endpoint family (create, update,
// subscribe, block, and the post/comment/vote endpoints tasks 12-15 will meet)
// is proven at T1 today: handler behaviour against a mock service in
// internal/api/handlers/community, and write-forward record shape against a
// real PDS in internal/core/communities. What this contract adds on top is the
// part T1 cannot see — that the shipped binary really does route these NSIDs,
// really does guard them, and really does serve an indexed community back.
func TestCommunityAPIContract(t *testing.T) {
	p := newPipeline(t)
	creator := p.IndexedAccount(t, "api")
	community := provisionCommunityRepo(t, p, "a")

	displayName := "api contract " + testkit.UniqueID(t)
	community.PutRecord(t, communityProfileCollection, "self",
		communityProfile(community, creator.DID, displayName, "read back through the client surface", "public"))

	p.Await(t, "the community to be indexed before the client surface is exercised",
		func() (bool, error) {
			view, err := p.Community(context.Background(), community.DID)
			if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
				return done, err
			}
			return view.DisplayName == displayName, nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	t.Run("the write endpoints refuse an unauthenticated client", func(t *testing.T) {
		// One request each, no polling: this is the auth boundary of the shipped
		// router, and the answer does not become true later.
		//
		// Every community NSID that RegisterCommunityRoutes puts behind
		// RequireAuth is listed, including the four "un-" halves. That
		// completeness is the point of asserting it HERE rather than only in the
		// handler tests: a handler test proves the handler refuses an
		// unauthenticated call, and cannot see a route that was registered
		// without the middleware in front of it. Only a request to the running
		// router can.
		for _, endpoint := range []struct {
			nsid  string
			input map[string]any
		}{
			{"social.coves.community.create", map[string]any{"name": testkit.UniqueIDWithPrefix(t, "n"), "displayName": "nope"}},
			{"social.coves.community.update", map[string]any{"communityDid": community.DID, "displayName": "nope"}},
			{"social.coves.community.subscribe", map[string]any{"community": community.DID}},
			{"social.coves.community.unsubscribe", map[string]any{"community": community.DID}},
			{"social.coves.community.blockCommunity", map[string]any{"community": community.DID}},
			{"social.coves.community.unblockCommunity", map[string]any{"community": community.DID}},
		} {
			err := p.AppView.Procedure(ctx, endpoint.nsid, endpoint.input, nil)
			require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
				"%s must answer 401 to a client with no session, answered: %v", endpoint.nsid, err)
		}
	})

	t.Run("a client reads the community by every identifier form", func(t *testing.T) {
		// The three shapes internal/core/communities.ResolveCommunityIdentifier
		// accepts, checked here against the running router rather than the
		// service, because a handler that forgets to pass the parameter through
		// fails only at this level.
		scoped := "!" + community.Name + "@" + communityInstanceDomain()
		for _, identifier := range []string{community.DID, community.Handle, scoped} {
			view, err := p.Community(ctx, identifier)
			require.NoErrorf(t, err, "social.coves.community.get rejected identifier %q", identifier)
			require.Equalf(t, community.DID, view.DID,
				"identifier %q resolved to the wrong community", identifier)
			require.Equal(t, displayName, view.DisplayName)
		}
	})

	t.Run("an unknown community is a not-found, not a router miss", func(t *testing.T) {
		// XRPC-shaped, which is what testkit.IsNotFound insists on: a plain 404
		// would mean the route is gone, and every wait in the tier that treats
		// "not found" as "not yet" depends on being able to tell those apart.
		_, err := p.Community(ctx, "did:plc:"+testkit.UniqueID(t))
		require.Truef(t, testkit.IsNotFound(err), "expected an XRPC not-found, got: %v", err)
		require.True(t, testkit.IsStatus(err, http.StatusNotFound))
	})

	t.Run("the community is listed", func(t *testing.T) {
		// sort=new, so the assertion does not depend on how many communities a
		// kept stack has accumulated: this one was created last.
		var listed struct {
			Communities []communityView `json:"communities"`
			Cursor      string          `json:"cursor"`
		}
		err := p.AppView.Query(ctx, "social.coves.community.list",
			url.Values{"sort": {"new"}, "limit": {"25"}}, &listed)
		require.NoError(t, err)

		var found bool
		for _, c := range listed.Communities {
			if c.DID == community.DID {
				found = true
				require.Equal(t, community.Handle, c.Handle)
				require.Equal(t, displayName, c.DisplayName)
			}
		}
		require.Truef(t, found, "the community %s was not in the newest %d communities: %s",
			community.DID, len(listed.Communities), summarize(listed.Communities))
	})
}

// summarize renders a community list compactly for a failure message.
func summarize(views []communityView) string {
	if len(views) == 0 {
		return "(the list was empty)"
	}
	parts := make([]string, 0, len(views))
	for _, v := range views {
		parts = append(parts, fmt.Sprintf("%s(%s)", v.Handle, v.DID))
	}
	return strings.Join(parts, ", ")
}
