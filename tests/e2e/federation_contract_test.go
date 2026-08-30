//go:build e2e

// Federation-path contracts: the same collections the direct-PDS ingestion
// contracts prove, proven again against a PDS THE APPVIEW DOES NOT FRONT.
//
// docs/TEST_ARCHITECTURE.md §3.4a is explicit that a direct-PDS write is
// "honest direct-PDS-path testing, not federation (the record still lives on
// the one PDS the stack fronts)". This file is what §4's Phase 5 asks for
// instead. Every record below is written into a repo hosted on pds2
// (docker-compose.ci.yml), a host the AppView has no session on, no credentials
// for, and no configuration naming. Everything it learns about those repos it
// learns from the relay's firehose and from resolving their DIDs against the
// shared PLC.
//
// These contracts live ALONGSIDE the direct-PDS ones rather than replacing
// them, and they carry no ingestion markers of their own: the collections are
// the same, cmd/contract-manifest is already satisfied by post_contract_test.go,
// comment_contract_test.go and vote_contract_test.go, and a second marker for a
// collection would claim a second inventory entry that does not exist.
//
// # WHAT IS ACTUALLY FEDERATED IN EACH CASE
//
// Which repo a record lives in decides what "hosted remotely" can mean, and it
// is different for each of the three:
//
//	post     lives in the COMMUNITY's repo → a federated post needs a federated
//	         COMMUNITY. So the community's own repo is on pds2 and the post is
//	         written into it. The author is a local, indexed user (see the
//	         limit below).
//	comment  lives in the AUTHOR's repo → a federated comment needs a federated
//	         AUTHOR. The commenter's repo is on pds2; the post being replied to
//	         is local.
//	vote     lives in the VOTER's repo → same shape as comments.
//
// # THE STRUCTURAL LIMIT: A FEDERATED IDENTITY CANNOT BECOME A USER
//
// Measured on this topology, not inferred. A repo on pds2 can carry comments
// and votes that index perfectly well, because neither table has a foreign key
// on its author (migrations 014 and 016 both removed/omitted one deliberately,
// so out-of-order firehose delivery cannot fail a write). What that repo's
// owner CANNOT do is become a row in `users`, and two independent things
// compose to make it so:
//
//  1. The user consumer indexes profile events only for DIDs it has already
//     seen, with exactly one exception — an identity whose resolved PDS host is
//     in TRUSTED_BRIDGE_PDS_HOSTS (user_consumer.go's handleProfileCommit). That
//     list is EMPTY in .env.ci, so a pds2 profile is default-denied and silently
//     ignored. TestFederatedIdentityIsNotIndexed asserts exactly that.
//  2. (Historical.) Until 2026-08-30 the identity could not have been indexed
//     even if trusted: a `.pds2.test` handle could not verify on an
//     egress-blocked network, indigo returned `handle.invalid`, and
//     users.CreateUser rejected it — the defect filed as
//     2026-07-22-bridged-author-handle-invalid-dead-letter. That half is gone:
//     .env.ci's HANDLE_WELL_KNOWN_HOSTS completes the well-known leg in-stack
//     and pds2 handles verify. Only (1) keeps the identity out of `users` now.
//
// So a federated comment is served with its author's handle rendered as the
// DID, which is the honest, observable consequence and is asserted below rather
// than worked around.
//
// The environment half of (2) is now GONE: .env.ci's HANDLE_WELL_KNOWN_HOSTS
// maps .pds2.test to pds2, so the AppView's resolver completes the well-known
// leg in-stack and pds2 handles DO verify. (1) alone keeps the identity out of
// `users`, and TestFederatedIdentityIsNotIndexed is what asserts it.
//
// # THE FEDERATED COMMUNITY PROFILES' handle FIELD IS INERT
//
// The consumer always resolves a community's handle from its DID and stores
// only the verified result (identity.VerifiedHandle); the record's
// handle is at most a warning when it disagrees. Verification of a .pds2.test
// handle completes in this stack through HANDLE_WELL_KNOWN_HOSTS, so these
// communities are indexed under their REAL, verified handle with their real
// pds_url — the two symptoms of 2026-07-29-community-handle-invalid-silently-
// dropped are both fixed on this topology, and the contracts below observe the
// fix rather than a workaround. The field stays in the fixture because
// production's service.go still writes it (removal is a filed follow-up).
//
// (Separated from the package clause by a blank line on purpose: the package
// doc lives in contracts_test.go, and a second one here would compete with it.)

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"Coves/tests/testkit"
)

// federatedInstanceDID is the did:web of the instance that hosts the federated
// communities in this file: the OTHER instance, not ours.
//
// It has to agree with pds2's handle domain, because the community consumer
// verifies that a community's hostedBy domain matches its handle's domain
// (verifyHostedByClaim). Derived from the handle domain rather than written
// twice, so a change to PDS2_SERVICE_HANDLE_DOMAINS cannot leave a stale
// literal behind — the failure that would produce is a security rejection deep
// in the consumer, which reads nothing like a configuration drift.
//
// The bidirectional half of that verification (fetching .well-known/did.json)
// is off in CI via SKIP_DID_WEB_VERIFICATION, as community_contract_test.go
// records: these contracts prove field transport, not domain ownership.
func federatedInstanceDID(remote *testkit.PDS) string {
	return "did:web:" + remote.Endpoint.HandleDomain
}

// federatedCommunity provisions a community whose REPO LIVES ON PDS2 and waits
// for the AppView to index it from the firehose.
//
// The local analogue is indexedCommunity (post_contract_test.go), and the
// difference is the whole point of this file: that one creates the repo on the
// PDS the AppView fronts, holds instance credentials on, and could read
// directly. This one creates it on a host the AppView cannot even authenticate
// to, so the community appearing on social.coves.community.get has exactly one
// possible explanation.
//
// Nothing tells the AppView the account exists — as in the local case, the
// community consumer indexes repos it has never seen — so the profile record is
// both the registration and the pipeline proof.
func federatedCommunity(t *testing.T, p *pipeline, remote *testkit.PDS, prefix, creatorDID string) provisionedCommunity {
	t.Helper()

	name := testkit.UniqueIDWithPrefix(t, prefix)
	handle := "c-" + name + "." + remote.Endpoint.HandleDomain
	if label := strings.SplitN(handle, ".", 2)[0]; len(label) > testkit.MaxIDLength {
		t.Fatalf("federated community handle label %q is %d characters, over the PDS' %d-character "+
			"cap: shorten the %q prefix", label, len(label), testkit.MaxIDLength, prefix)
	}

	account := remote.CreateAccount(t,
		testkit.WithHandle(handle),
		testkit.WithEmail(name+"@community.remote.test"))
	community := provisionedCommunity{Account: account, Name: name}

	community.PutRecord(t, communityProfileCollection, "self",
		federatedCommunityProfile(remote, community, creatorDID,
			"remote "+name, "a community whose repo lives on the PDS the AppView does not front"))

	p.Await(t, "the federated community to be indexed from the firehose", func() (bool, error) {
		_, err := p.Community(context.Background(), community.DID)
		return testkit.PendingIfNotFound(err)
	})
	return community
}

// federatedCommunityProfile is communityProfile's remote twin: the same record
// shape, hosted by the OTHER instance.
func federatedCommunityProfile(remote *testkit.PDS, c provisionedCommunity, creatorDID, displayName, description string) map[string]any {
	return map[string]any{
		"$type":       communityProfileCollection,
		"name":        c.Name,
		"handle":      "c-" + c.Name + "." + remote.Endpoint.HandleDomain,
		"displayName": displayName,
		"description": description,
		"visibility":  "public",
		"createdBy":   creatorDID,
		"hostedBy":    federatedInstanceDID(remote),
		"createdAt":   time.Now().UTC().Format(time.RFC3339),
		"federation":  map[string]any{"allowExternalDiscovery": true},
	}
}

// remoteActor registers an account on pds2 and returns a session on it.
//
// Deliberately NOT pipeline.IndexedAccount's remote equivalent, because there
// is no such thing: signup mints accounts on the PDS the AppView fronts, and
// nothing else puts a DID in the index. This actor is, and stays, unknown to
// the AppView — which is the condition the comment and vote contracts below
// index records under.
func remoteActor(t *testing.T, remote *testkit.PDS, prefix string) *testkit.Account {
	t.Helper()
	label := testkit.UniqueIDWithPrefix(t, prefix)
	return remote.CreateAccount(t,
		testkit.WithHandle(remote.Endpoint.Handle(label)),
		testkit.WithEmail(label+"@remote.test"))
}

// requireRemoteHost fails unless a DID's repo really is on pds2.
//
// The failure it guards against is the one that would make this whole file
// worthless: a "federation" contract that has quietly been writing to the local
// PDS all along would pass every assertion below while proving nothing. The
// check is against the PDS' own describeRepo, so it reflects where the repo
// IS rather than where the test believes it put it.
func requireRemoteHost(t *testing.T, remote *testkit.PDS, did string) {
	t.Helper()
	require.NotEqual(t, testkit.Endpoints().PDS.BaseURL, remote.URL(),
		"the federated PDS and the local PDS are the same host: this contract would prove nothing")

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	var described struct {
		DID    string `json:"did"`
		Handle string `json:"handle"`
	}
	err := remote.Anon.Query(ctx, "com.atproto.repo.describeRepo",
		url.Values{"repo": {did}}, &described)
	require.NoErrorf(t, err, "the federated PDS at %s does not host repo %s", remote.URL(), did)
	require.Equal(t, did, described.DID)
}

// TestPostFederationIngestion is the post ingestion contract on the federation
// path: the record lives in a community repo on pds2.
//
// The direct-PDS contract (TestPostIngestion) proves the consumer chain works
// for a repo on the PDS the AppView fronts. This proves the same chain when the
// AppView has no relationship with the host at all — the record reaches it
// through the relay, and the community it belongs to was itself discovered the
// same way moments earlier.
//
//	create → the post appears, carrying the record's own values, hydrated with
//	         a community whose handle is under the REMOTE instance's domain
//	update → the same URI serves the new values
//	delete → the post is gone, and STAYS gone (Holds, §3.4a)
//
// # ORDERING, RE-AUDITED FOR THE RELAY
//
// TestPostIngestion's spoof step is bounded by a later write INTO THE SAME
// REPO, because per-repo commit order is the only ordering the path guarantees.
// A relay does not weaken that — it merges two PDS streams into one sequence
// while preserving each repo's own order — so that contract's bound survives
// this topology unchanged. It is not repeated here: this contract asserts only
// positives and a delete, none of which need an ordering bound, and re-proving
// the ownership check against a remote repo would test the same branch twice.
//
// What the relay DOES remove is any hope of cross-repo ordering, which was
// already topology luck within one PDS and is now luck across two. Nothing in
// this file depends on it.
func TestPostFederationIngestion(t *testing.T) {
	p := newPipeline(t)
	remote := testkit.NewFederatedPDS(t)

	// The author is LOCAL and indexed. It has to be: the post consumer requires
	// the author to exist in `users` before it will index a post, and the
	// package doc explains why a pds2 identity cannot get there. So this
	// contract federates the community and the record, not the authorship —
	// which is the shape a bridged community with local participants takes.
	author := p.IndexedAccount(t, "fpa")
	community := federatedCommunity(t, p, remote, "fp", author.DID)
	requireRemoteHost(t, remote, community.DID)

	rkey := testkit.TID()
	title := "federated " + testkit.UniqueID(t)
	community.PutRecord(t, postCollection, rkey,
		postRecord(community.DID, author.DID, title, "written into a repo on the other PDS"))
	uri := postURI(community.DID, rkey)

	observe := func(description string, accept func(postView) bool) postView {
		t.Helper()
		var observed postView
		p.Await(t, description, func() (bool, error) {
			view, err := p.Post(context.Background(), uri)
			if err != nil {
				return false, err
			}
			if view.NotFound {
				return false, nil
			}
			observed = view
			return accept(view), nil
		})
		return observed
	}

	created := observe("the federated post to be served", func(view postView) bool {
		return view.Record["title"] == title
	})
	require.Equal(t, community.DID, created.Community.DID)
	require.Equal(t, author.DID, created.Author.DID)
	require.Truef(t, strings.HasSuffix(created.Community.Handle, "."+remote.Endpoint.HandleDomain),
		"the post was hydrated with community handle %q, which is not under the federated PDS' "+
			"domain %q — the record indexed, but against the wrong community",
		created.Community.Handle, remote.Endpoint.HandleDomain)

	edited := "edited " + testkit.UniqueID(t)
	community.PutRecord(t, postCollection, rkey,
		postRecord(community.DID, author.DID, edited, "edited on the other PDS"))
	updated := observe("the federated post's edit to be served", func(view postView) bool {
		return view.Record["title"] == edited
	})
	require.Equal(t, uri, updated.URI)

	community.DeleteExistingRecord(t, postCollection, rkey)
	p.Await(t, "the federated post to disappear after deletion", func() (bool, error) {
		view, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		return view.NotFound, nil
	})
	p.Holds(t, "the deleted federated post to STAY deleted", func() (bool, error) {
		view, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		return view.NotFound, nil
	})
}

// TestCommentFederationIngestion is the comment ingestion contract on the
// federation path: the comment lives in a REMOTE author's own repo.
//
// This is the case a bridge produces constantly — a fediverse commenter whose
// repo the bridge hosts, replying to a post on this instance — and it works
// end to end today, with one visible degradation that this contract states
// rather than hides.
//
//	create → the remote comment is served as a node on the local post
//	author → its handle is served as the DID, because the commenter is not and
//	         cannot be a user (see the package doc)
//	update → a content edit reaches the same URI in place
//	delete → the comment becomes a placeholder, and STAYS one (Holds)
//
// The thread endpoint carries a per-route rate limit of 20/minute, so every
// wait here uses withReadCadence, exactly as the direct-PDS comment contract
// does.
func TestCommentFederationIngestion(t *testing.T) {
	p := newPipeline(t)
	remote := testkit.NewFederatedPDS(t)

	local := p.IndexedAccount(t, "fca")
	community := indexedCommunity(t, p, "fc", local.DID)
	post := indexedPost(t, p, community, local.DID, "a local post for a remote commenter")

	commenter := remoteActor(t, remote, "fcr")
	requireRemoteHost(t, remote, commenter.DID)

	rkey := testkit.TID()
	content := "from the other PDS " + testkit.UniqueID(t)
	commenter.PutRecord(t, commentCollection, rkey, commentRecord(post, post, content))
	uri := commentURI(commenter.DID, rkey)

	observe := func(description string, accept func(threadNode) bool) threadNode {
		t.Helper()
		var observed threadNode
		p.Await(t, description, func() (bool, error) {
			thread, err := p.Thread(context.Background(), post.URI, nil)
			if err != nil {
				return false, err
			}
			node, found := thread.find(uri)
			if !found {
				return false, nil
			}
			observed = node
			return accept(node), nil
		}, withReadCadence())
		return observed
	}

	created := observe("the remote author's comment to be served on the local post",
		func(node threadNode) bool { return node.Comment.Record["content"] == content })
	require.Equal(t, commenter.DID, created.Comment.Author.DID)

	// THE DEGRADATION, asserted rather than tolerated silently. The comment
	// indexes because `comments` has no foreign key on its author, and the view
	// then has no user row to hydrate a handle from, so it falls back to the
	// DID. A reader seeing a DID where a handle belongs is looking at exactly
	// this: an author the AppView cannot index. If this ever starts serving a
	// real handle, a federated identity has become indexable and the package
	// doc's structural limit — the empty TRUSTED_BRIDGE_PDS_HOSTS — has changed.
	require.Equalf(t, commenter.DID, created.Comment.Author.Handle,
		"the remote commenter's handle was served as %q rather than falling back to the DID. "+
			"That means a pds2-hosted identity reached the users table, which the package doc "+
			"says is impossible on this topology while TRUSTED_BRIDGE_PDS_HOSTS is empty in "+
			".env.ci — re-read it before relaxing this",
		created.Comment.Author.Handle)

	edited := "edited from the other PDS " + testkit.UniqueID(t)
	commenter.PutRecord(t, commentCollection, rkey, commentRecord(post, post, edited))
	observe("the remote author's edit to reach the thread",
		func(node threadNode) bool { return node.Comment.Record["content"] == edited })

	commenter.DeleteExistingRecord(t, commentCollection, rkey)
	p.FreshReadQuota(t, "federated-comment-delete")
	deletedIsServed := func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		node, found := thread.find(uri)
		return found && node.Comment.IsDeleted, nil
	}
	p.Await(t, "the deleted remote comment to become a placeholder", deletedIsServed,
		withReadCadence())
	p.Holds(t, "the deleted remote comment to STAY a placeholder", deletedIsServed)
}

// TestVoteFederationIngestion is the vote ingestion contract on the federation
// path: the vote lives in a REMOTE voter's own repo.
//
//	create           → upvotes 1, score 1
//	new-rkey re-tap  → still exactly one vote's worth, and STAYS (Holds)
//	direction change → the up is withdrawn as the down lands (0/1/-1)
//	delete           → back to zero, and STAYS zero (Holds, §3.4a)
//
// # WHY THE RE-TAP AND THE DIRECTION CHANGE ARE HERE AND NOT LEFT TO THE LOCAL
// CONTRACT
//
// Both are the steps that depend on the consumer finding the voter's PREVIOUS
// vote, keyed on (voter_did, subject_uri), and cleaning it up. The voter's
// records now arrive from a different PDS than the subject's, through a relay
// that merges the two streams with no cross-repo ordering guarantee, so "the
// earlier vote is already indexed when the later one arrives" is a weaker
// assumption here than it is locally. Each step waits for the previous one to
// be observable before writing the next, which is what makes the arc
// deterministic rather than a race — and is legitimate per §3.4 rule 4.
//
// Measured on this topology, all four steps behave exactly as they do locally.
// The out-of-order case is NOT re-exercised here: it has its own contract in
// TestVoteBeforeSubjectIsCountedOnceSubjectIndexed, its cause is ordering
// rather than hosting, and the second PDS changes only how easily production
// reaches it.
func TestVoteFederationIngestion(t *testing.T) {
	p := newPipeline(t)
	remote := testkit.NewFederatedPDS(t)

	local := p.IndexedAccount(t, "fva")
	community := indexedCommunity(t, p, "fv", local.DID)
	post := indexedPost(t, p, community, local.DID, "a local post for a remote voter")

	voter := remoteActor(t, remote, "fvr")
	requireRemoteHost(t, remote, voter.DID)

	first := testkit.TID()
	voter.PutRecord(t, voteCollection, first, voteRecord(post, "up"))
	awaitStats(t, p, post.URI, "the remote voter's upvote to be counted",
		func(s postStats) bool { return s == postStats{Upvotes: 1, Score: 1} })

	// The re-tap a real client produces: votes.voteService always writes a
	// fresh TID rather than reusing the rkey, so the consumer has to recognise
	// the voter's earlier vote on the same subject and retire it. Getting this
	// wrong across hosts would show up as a doubled count.
	second := testkit.TID()
	voter.PutRecord(t, voteCollection, second, voteRecord(post, "up"))
	holdStats(t, p, post.URI, "a remote re-tap under a new rkey to STAY one vote",
		postStats{Upvotes: 1, Score: 1})

	third := testkit.TID()
	voter.PutRecord(t, voteCollection, third, voteRecord(post, "down"))
	awaitStats(t, p, post.URI, "the remote voter's direction change to land",
		func(s postStats) bool { return s == postStats{Downvotes: 1, Score: -1} })

	voter.DeleteExistingRecord(t, voteCollection, third)
	awaitStats(t, p, post.URI, "the remote voter's withdrawal to zero the counts",
		func(s postStats) bool { return s == postStats{} })
	holdStats(t, p, post.URI, "the withdrawn remote vote to STAY withdrawn", postStats{})
}

// TestFederationRemoteBlobFetch is the remote-identity-resolution and
// remote-blob-fetching proof docs/TEST_ARCHITECTURE.md §4 Phase 5 asks for
// explicitly.
//
// # WHY A BLOB IS THE RIGHT OBSERVABLE FOR IDENTITY RESOLUTION
//
// The AppView resolves remote DIDs constantly, and almost none of it is visible
// from outside: a resolution feeds a database column a contract may not read.
// The image path is the exception. social.coves.community.get serves a
// HYDRATED image URL pointing back at the AppView's own proxy, and fetching it
// makes the AppView, at request time:
//
//  1. resolve the community's DID against the shared PLC,
//  2. read the service endpoint out of the DID document — which is pds2, a host
//     nothing in its configuration mentions,
//  3. fetch com.atproto.sync.getBlob from there, and
//  4. re-encode and serve the bytes.
//
// A 200 with image bytes is therefore a statement about all four. The proxy
// answers 502 when resolution fails or the blob cannot be fetched, so there is
// no path where this passes without the remote round trip having happened.
//
// The blob is uploaded ONLY to pds2 and the contract proves it: the same CID is
// requested from the local PDS and must NOT be there. Without that step a
// passing fetch would be consistent with the proxy having found the blob on the
// PDS it already knows about.
func TestFederationRemoteBlobFetch(t *testing.T) {
	p := newPipeline(t)
	remote := testkit.NewFederatedPDS(t)
	creator := p.IndexedAccount(t, "frb")

	name := testkit.UniqueIDWithPrefix(t, "fb")
	handle := "c-" + name + "." + remote.Endpoint.HandleDomain
	account := remote.CreateAccount(t,
		testkit.WithHandle(handle),
		testkit.WithEmail(name+"@community.remote.test"))
	community := provisionedCommunity{Account: account, Name: name}
	requireRemoteHost(t, remote, community.DID)

	avatar := account.UploadBlob(t, testkit.TestPNG(64, 64), "image/png")
	requireBlobAbsentLocally(t, p, community.DID, avatar.CID())

	profile := federatedCommunityProfile(remote, community, creator.DID,
		"remote avatar "+name, "its avatar exists only on the other PDS")
	profile["avatar"] = blobRefValue(avatar)
	community.PutRecord(t, communityProfileCollection, "self", profile)

	var served communityView
	p.Await(t, "the federated community's avatar URL to be served", func() (bool, error) {
		view, err := p.Community(context.Background(), community.DID)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		served = view
		return view.Avatar != "", nil
	})
	require.Containsf(t, served.Avatar, avatar.CID(),
		"the served avatar URL %q does not name the CID uploaded to the remote PDS", served.Avatar)

	// The round trip itself. requireServesImage asserts the URL points at the
	// AppView, answers 200, carries an image content type and has a non-empty
	// body — the four things a 502 from a failed remote fetch would each break.
	requireServesImage(t, p, "federated community avatar", served.Avatar)
}

// requireBlobAbsentLocally proves a blob is not on the PDS the AppView fronts.
//
// This is what makes TestFederationRemoteBlobFetch's success attributable. The
// local PDS is asked for the same (did, cid) the proxy will later fetch, and
// must refuse it — it hosts neither the repo nor the blob. A 200 here would
// mean the blob had somehow been uploaded to both, and every conclusion the
// contract draws about remote fetching would be unfounded.
//
// IT MUST BE A REFUSAL, NOT MERELY A FAILURE. "Any error means absent" is the
// weak form of this check and it would accept the two answers that prove
// nothing: a 5xx (the PDS broke while looking, so nobody knows what it holds)
// and a transport error (nothing was asked at all — a wrong port, a stopped
// container). Only a definite negative answer from a PDS that understood the
// question — atProto spells "not here" as a 400 with RepoNotFound/BlobNotFound,
// and a 404 would do as well — supports the conclusion the contract draws.
func requireBlobAbsentLocally(t *testing.T, p *pipeline, did, cid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	err := p.PDS.Anon.Query(ctx, "com.atproto.sync.getBlob",
		url.Values{"did": {did}, "cid": {cid}}, nil)
	require.Errorf(t, err,
		"the LOCAL PDS at %s served blob %s for %s. It should host neither, and if it does, "+
			"the remote-fetch contract below proves nothing", p.PDS.URL(), cid, did)

	switch status := testkit.StatusOf(err); {
	case status == http.StatusBadRequest, status == http.StatusNotFound:
		// The PDS looked and said no. That is the whole point of the helper.
	case status == 0:
		t.Fatalf("the LOCAL PDS at %s could not be reached at all while proving it does not "+
			"host blob %s for %s, so nothing was proven: %v", p.PDS.URL(), cid, did, err)
	default:
		t.Fatalf("the LOCAL PDS at %s answered HTTP %d when asked for blob %s for %s. That is a "+
			"fault, not a refusal — the PDS did not tell us whether it holds the blob, and the "+
			"remote-fetch contract needs it to have said no: %v",
			p.PDS.URL(), status, cid, did, err)
	}
}

// TestFederatedIdentityIsNotIndexed asserts the structural limit the package
// doc describes, so that it is a measured fact rather than a comment.
//
// A pds2-hosted identity writes social.coves.actor.profile into its own repo.
// The user consumer PROCESSES the event — proven below by a bound, not by
// waiting and hoping — and deliberately does nothing with it, because the
// identity's PDS is not in TRUSTED_BRIDGE_PDS_HOSTS. No user row appears, and
// none appears later either (Holds).
//
// This is correct behaviour for the shipped configuration: the alternative is
// indexing every profile on the network. It is worth a contract anyway, for
// two reasons. It is the precondition every other test in this file depends on
// — the comment contract's DID-shaped handle assertion is only meaningful while
// this holds — and it is the boundary that a change to the trust configuration
// would move, which is exactly the change that turns a silent no-op into the
// dead-letter loop of
// 2026-07-22-bridged-author-handle-invalid-dead-letter (measured on this
// topology; see the issue).
//
// # HOW THE NEGATIVE IS BOUNDED
//
// "No user appeared" cannot be proven by waiting, so the profile is bounded by
// a LATER EVENT IN THE SAME REPO THAT THE SAME SUBSCRIPTION CARRIES: a
// deliberately malformed social.coves.actor.block, written into the remote
// actor's own repo straight after the profile, and observed through the users
// consumer's dead-letter counter moving by exactly one
// (block_contract_test.go's measurement window, and the same handler that
// window relies on — createUserBlock rejects a subject-less record as
// ErrPermanentEvent). When that delta appears, the profile that preceded it in
// the same repo's commit order has necessarily already been through the same
// handler, so the notAUser() check below is a statement about a verdict the
// consumer has reached rather than about one it has not got to yet.
//
// BOTH HALVES OF THE BOUND ARE LOAD-BEARING, and each rules out a way of
// getting this wrong:
//
//   - SAME REPO, because per-repo commit order is the only ordering the path
//     guarantees — and now less than ever, with a relay merging two PDS
//     streams (see this file's TestPostFederationIngestion note).
//   - SAME SUBSCRIPTION, because every consumer has its OWN websocket with its
//     own collection filter: a later COMMENT in the same repo is delivered to
//     comments@self and says nothing about where users@self has got to. Both
//     collections here are the users consumer's.
//
// What was tried first and is NOT sufficient, recorded so it does not come
// back: waiting for the users consumer's eventsProcessed counter to advance.
// That consumer also handles kind=identity and kind=account, Jetstream ships
// those regardless of wantedCollections, and the connector counts an event as
// processed even when the handler ignores it — so the actor's OWN
// account-creation events can advance the counter before the profile commit is
// anywhere near the handler, and the bound silently measures nothing.
//
// The Holds stays as belt and braces: the bound proves the verdict was reached,
// the Holds proves nothing arrives later to change it.
func TestFederatedIdentityIsNotIndexed(t *testing.T) {
	p := newPipeline(t)
	remote := testkit.NewFederatedPDS(t)

	actor := remoteActor(t, remote, "fid")
	requireRemoteHost(t, remote, actor.DID)

	before := p.counters(t, "users")

	actor.PutRecord(t, profileCollection, "self", map[string]any{
		"$type":       profileCollection,
		"displayName": "remote " + testkit.UniqueID(t),
		"description": "a profile written on the PDS the AppView does not front",
		"createdAt":   time.Now().UTC().Format(time.RFC3339),
	})

	// THE BOUND. Written second, into the same repo, and consumed by the same
	// subscription as the profile — see the note above for why both matter.
	actor.PutRecord(t, actorBlockCollection, testkit.TID(),
		malformedBlockRecord(actorBlockCollection))

	var window blockDelivery
	p.Await(t, "the users consumer to dead-letter the malformed block that bounds the profile",
		func() (bool, error) {
			after, err := p.countersOrErr("users")
			if err != nil {
				return false, err
			}
			window = blockDelivery{before: before, after: after}
			return after.DeadLettered > before.DeadLettered, nil
		})
	// Exactly one: the malformed block. A delta of two would mean the profile
	// event ALSO failed on its way through — which is not the no-op this
	// contract is about, and would make the bound's conclusion unsound rather
	// than merely surprising.
	window.requireRejected(t,
		"a subject-less block written into a repo on the federated PDS, bounding the profile")

	notAUser := func() (bool, error) {
		_, err := p.Profile(context.Background(), actor.DID)
		if err == nil {
			return false, nil
		}
		if testkit.IsStatus(err, http.StatusNotFound) {
			return true, nil
		}
		return false, err
	}
	ok, err := notAUser()
	require.NoError(t, err)
	require.Truef(t, ok,
		"the AppView indexed a user for %s, an identity hosted on the federated PDS. Either "+
			"TRUSTED_BRIDGE_PDS_HOSTS now names that host, or the user consumer's "+
			"must-know-first policy changed; both move the boundary this file's package doc "+
			"describes, and the comment contract's DID-handle assertion depends on it", actor.DID)
	p.Holds(t, "the federated identity to STAY unindexed", notAUser)
}
