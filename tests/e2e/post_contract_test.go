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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The post domain's pipeline contracts: the ingestion proof for
// social.coves.community.post, and the client-facing surface a third-party
// client actually reaches.
//
// # POSTS LIVE IN THE COMMUNITY'S REPO, WHICH IS MOST OF THE SETUP
//
// A post record is not written to its author's repository. It is written to the
// COMMUNITY's, with the community's own PDS credentials, carrying an `author`
// field naming the human who wrote it. That single fact drives everything below:
//
//   - The consumer's first security check is repoDID == record.community
//     (post_consumer.go validatePostEvent). A post in any other repo is rejected
//     as a spoof, permanently.
//   - The community must be INDEXED before the post arrives, or the post is
//     rejected — transiently, so a redrive can succeed once the community lands.
//   - The author must be indexed too, and identities enter the index only
//     through social.coves.actor.signup (contracts_test.go's IndexedAccount says
//     why).
//
// So an ingestion contract for posts needs three things standing before it can
// write a single record: a signed-up author, a community whose repo the AppView
// learned about from the firehose, and a session on that community. The first
// two are pipeline proofs of their own domains, which is why this file waits for
// each of them explicitly rather than assuming — a post that never appears
// because its community never indexed would otherwise be reported as a post
// pipeline failure.
//
// # NOTHING BUT THE FIREHOSE CAN PUT A POST IN THE INDEX (checked, see contracts_test.go)
//
// The package doc's reconciliation hazard — code that reads the PDS by itself
// and can satisfy a wait with every consumer dead — comes up empty for posts,
// and more cleanly than it did for communities. The search, recorded so the next
// reader need not repeat it:
//
//   - posts.CreatePost writes NOTHING to Postgres. Unlike communities.CreateCommunity
//     (the synchronous client path §3.4 warns about), it validates, forwards the
//     record to the community's PDS, and returns the URI — "AppView will index
//     via Jetstream consumer", service.go. So even the client path is honest here.
//   - posts.DeletePost also writes nothing. It does fetch the record from the PDS,
//     but only to read `author` out of it for the authorization check; the result
//     is discarded.
//   - The read paths (posts.GetPosts, timeline, discover, communityFeeds,
//     comments) are SELECT-only. None constructs a PDS client.
//   - The only INSERT INTO posts reachable in a running server is
//     post_consumer.go's, inside the rev-gated transaction. postgres.postRepo's
//     own Create and SoftDelete have no non-test callers at all.
//
// A single create → visible observation is therefore already honest, with no
// arming write of the kind the actor.profile contract needs.
//
// # WHAT THE DELETE ASSERTION IS MADE AGAINST, AND WHY IT IS NOT getComments
//
// deletePost SOFT-deletes: `UPDATE posts SET deleted_at = NOW()`, keeping the row
// so comment threads keep their parent and the rev gate has somewhere to hang a
// tombstone. The row's survival is asserted at T1
// (internal/atproto/jetstream/post_delete_test.go); what belongs here is that the
// deletion is OBSERVABLE — social.coves.community.post.get stops serving the post
// and keeps not serving it.
//
// It is worth saying which endpoint that is measured on, because a second one
// disagrees. social.coves.community.comment.getComments?post=<uri> hydrates its
// post context through postRepo.GetByURI, which has no `deleted_at IS NULL`
// filter — so a deleted post is still served in full (title, content, author) by
// the thread endpoint while post.get correctly reports it gone. That is a
// content-visibility defect, reported rather than asserted here: pinning it would
// cement it, and asserting the intended behaviour would fail this suite over a
// bug this task is not fixing.
//
// # THE ONE SEAM NO SINGLE TEST SPANS, AND WHY THAT IS ACCEPTABLE
//
// The file this contract replaces had a test — TestPostE2E_DeleteWithJetstream —
// that ran the whole arc in one process: call the SERVICE's delete, watch the
// firehose, assert the row was soft-deleted. That arc is unsayable at T2 now. The
// service's delete is reached through social.coves.community.post.delete, which
// is behind RequireAuth, and §3.4b's known limitation is that nothing outside the
// browser OAuth callback mints a credential RequireAuth accepts. So the tier can
// prove the endpoint REFUSES an unauthenticated caller and no more.
//
// What covers the seam instead is composition, and the join is tighter than it
// first looks because the two halves meet at a protocol guarantee rather than at
// an assumption:
//
//   - internal/core/posts/service_writeforward_test.go proves the service's
//     delete removes the record from the community's repo, against a real PDS.
//     A repo mutation on a PDS IS a commit on that PDS' firehose — that is what
//     a PDS is — so the service's delete necessarily emits a delete commit.
//   - TestPostIngestion below proves that a delete commit on exactly that
//     collection, in exactly that repo, reaching the AppView's own consumers,
//     soft-deletes the post and keeps it gone.
//
// The honest caveat: no single test observes both halves of one delete, so a
// defect that lived precisely in the handoff — the service deleting a DIFFERENT
// rkey than the one it reports, say — would need both tests to be wrong in the
// same direction to escape. The gap closes when the Phase-5 test-only session
// mint lands and the API contract can drive an authenticated delete end to end.
const postCollection = "social.coves.community.post"

// postView is the slice of social.coves.community.post.get's postView member
// that the contracts observe. As elsewhere in this package, modelling only the
// asserted fields keeps a new lexicon field from breaking every contract that
// reads a post.
type postView struct {
	URI       string         `json:"uri"`
	CID       string         `json:"cid"`
	RKey      string         `json:"rkey"`
	Author    identityRef    `json:"author"`
	Community identityRef    `json:"community"`
	Record    map[string]any `json:"record"`
	Stats     postStats      `json:"stats"`
	CreatedAt time.Time      `json:"createdAt"`
	IndexedAt time.Time      `json:"indexedAt"`
	EditedAt  *time.Time     `json:"editedAt,omitempty"`

	// NotFound is the discriminator of the notFoundPost union member, which
	// shares the array with postView. The endpoint answers 200 either way (a
	// missing post is not an error at this endpoint, unlike community.get), so
	// this field — not a status code — is how a contract tells them apart.
	NotFound bool `json:"notFound"`
}

// identityRef is the author/community reference a post view carries.
type identityRef struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

type postStats struct {
	Upvotes      int `json:"upvotes"`
	Downvotes    int `json:"downvotes"`
	Score        int `json:"score"`
	CommentCount int `json:"commentCount"`
}

// Posts reads post views from the AppView, in the order the URIs were asked for.
//
// The endpoint takes a REPEATED `uris` parameter rather than a single `uri`, and
// answers with one union member per requested URI, so the result is positional:
// index i is the answer about uris[i], found or not.
func (p *pipeline) Posts(ctx context.Context, uris ...string) ([]postView, error) {
	var out struct {
		Posts []postView `json:"posts"`
	}
	params := url.Values{}
	for _, uri := range uris {
		params.Add("uris", uri)
	}
	if err := p.AppView.Query(ctx, "social.coves.community.post.get", params, &out); err != nil {
		return nil, err
	}
	return out.Posts, nil
}

// Post reads one post view. A post that is not indexed comes back as a
// notFoundPost member, NOT as an error — see postView.NotFound.
func (p *pipeline) Post(ctx context.Context, uri string) (postView, error) {
	views, err := p.Posts(ctx, uri)
	if err != nil {
		return postView{}, err
	}
	if len(views) != 1 {
		// Returned rather than fataled, so that inside a probe it is a TERMINAL
		// error (§3.3): the endpoint's answer is positional, and a wait that
		// retried through a broken one would time out blaming the pipeline for a
		// response-shape bug.
		return postView{}, fmt.Errorf(
			"social.coves.community.post.get answered with %d results for 1 requested URI: "+
				"the response is positional and must carry one union member per requested URI",
			len(views))
	}
	return views[0], nil
}

// postURI renders the AT-URI a post record has once it is committed: the
// COMMUNITY's DID is the authority, which is the whole shape of this domain.
func postURI(communityDID, rkey string) string {
	return "at://" + communityDID + "/" + postCollection + "/" + rkey
}

// postRecord builds a social.coves.community.post record in the shape
// internal/core/posts writes it (service.go step 9), so the consumer parses
// exactly what production hands it.
func postRecord(communityDID, authorDID, title, content string) map[string]any {
	return map[string]any{
		"$type":     postCollection,
		"community": communityDID,
		"author":    authorDID,
		"title":     title,
		"content":   content,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// indexedCommunity provisions a community's repo, writes its profile, and waits
// for the AppView to have learned about it from the firehose.
//
// Posts, comments and votes all need a community that is INDEXED, not merely
// provisioned — the post consumer refuses a post whose community it has never
// seen — and that wait is a step every one of tasks 12-15 would otherwise
// hand-roll. Left in this file rather than pushed into the community contract's:
// provisionCommunityRepo is the community domain's fixture, and this is what the
// domains hanging off it need on top.
func indexedCommunity(t *testing.T, p *pipeline, prefix, creatorDID string) provisionedCommunity {
	t.Helper()

	community := provisionCommunityRepo(t, p, prefix)
	community.PutRecord(t, communityProfileCollection, "self",
		communityProfile(community, creatorDID, "host "+community.Name, "a community to hang records on", "public"))

	p.Await(t, "the community hosting these records to be indexed", func() (bool, error) {
		_, err := p.Community(context.Background(), community.DID)
		return testkit.PendingIfNotFound(err)
	})
	return community
}

// TestPostIngestion is the pipeline proof for posts.
//
// coves:ingestion-contract social.coves.community.post
//
// Every record below is written straight into the community's own repo with the
// community's session, and every observation is made through
// social.coves.community.post.get:
//
//	spoof  → a post claiming ANOTHER community never appears, and stays absent (Holds)
//	create → the post appears, carrying the record's own field values
//	update → the same URI serves the new values, marked edited
//	delete → the post is gone, and STAYS gone (Holds, §3.4a)
//
// # HOW THE NEGATIVE IS BOUNDED, AND WHY THE SPOOF IS SHAPED THE WAY IT IS
//
// "The spoofed post never appears" is the one assertion here that cannot be
// proven by waiting — waiting only ever shows that it has not appeared YET. It
// is bounded instead by a later event: the spoof is written FIRST and the real
// post SECOND, INTO THE SAME REPO, so when the real post is visible the spoof's
// commit has necessarily already been through the same consumer. Only then is
// its absence meaningful, and Holds keeps watching in case a redrive changes its
// mind.
//
// Same repo is the load-bearing word, and it is why the spoof is a community
// forging a post for a DIFFERENT community rather than the more obvious shape (a
// USER writing a post claiming a community). Both trip the identical check —
// post_consumer.go's repoDID != record.community — but only the same-repo
// version has an ordering guarantee behind it:
//
//   - the PDS sequencer assigns one monotonic order to a repo's own commits;
//   - Jetstream serializes per repo and PARALLELIZES ACROSS repos;
//   - the connector hands one feed's events to the consumer sequentially.
//
// So two commits in one repo cannot overtake each other anywhere on the path,
// while two commits in DIFFERENT repos have no such guarantee — a cross-repo
// spoof would be bounded only by the 5-second Holds window and by the firehose
// happening to be idle, and Phase 5's relay topology would break even that,
// silently, leaving a test that still passes and no longer proves anything.
// Please do not "simplify" this back to writing the spoof into the author's repo.
//
// The victim community is real and indexed, not a fabricated DID, so the
// rejection can only be attributed to the repo mismatch: had it been a DID the
// AppView has never seen, the community-not-found gate would reject the record
// first and the ownership check would never be reached.
func TestPostIngestion(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "pi")
	community := indexedCommunity(t, p, "p", author.DID)
	victim := indexedCommunity(t, p, "v", author.DID)

	created := "created " + testkit.UniqueID(t)
	updated := "updated " + testkit.UniqueID(t)

	// ---- the spoof, written first so the create below bounds it -------------
	// This community's repo, forging a post that claims to belong to the victim
	// community. The consumer builds a post's URI from the REPO the commit
	// arrived in, so an indexed spoof would land under THIS community's DID —
	// which is the URI checked below.
	spoofRKey := testkit.TID()
	community.PutRecord(t, postCollection, spoofRKey,
		postRecord(victim.DID, author.DID, "spoofed", "a post forged for a community that does not own this repo"))
	spoofURI := postURI(community.DID, spoofRKey)

	// ---- create -------------------------------------------------------------
	rkey := testkit.TID()
	uri := postURI(community.DID, rkey)
	record := community.PutRecord(t, postCollection, rkey,
		postRecord(community.DID, author.DID, created, "written straight into the community's repo"))

	observe := func(description string, accept func(postView) bool) postView {
		t.Helper()
		var observed postView
		p.Await(t, description, func() (bool, error) {
			view, err := p.Post(context.Background(), uri)
			if err != nil {
				return false, err
			}
			observed = view
			return !view.NotFound && accept(view), nil
		})
		return observed
	}

	view := observe("the directly-written post to reach social.coves.community.post.get via the consumers",
		func(v postView) bool { return v.Record["title"] == created })

	require.Equal(t, uri, view.URI, "the AppView served a post under a different URI than the record's")
	require.Equal(t, record.CID, view.CID, "the indexed CID must be the commit's, not a re-derived one")
	require.Equal(t, rkey, view.RKey)
	require.Equal(t, author.DID, view.Author.DID, "the author is the record's author field, not the repo owner")
	require.Equal(t, author.Handle, view.Author.Handle,
		"the author reference is joined to the users table, so an unindexed author would have failed the join")
	require.Equal(t, community.DID, view.Community.DID)
	require.Equal(t, community.Handle, view.Community.Handle)
	require.Equal(t, community.Name, view.Community.Name)
	require.Equal(t, postCollection, view.Record["$type"])
	require.Equal(t, community.DID, view.Record["community"])
	require.Equal(t, author.DID, view.Record["author"])
	require.Equal(t, "written straight into the community's repo", view.Record["content"])
	require.Equal(t, postStats{}, view.Stats,
		"a post arrives with no votes and no comments; non-zero stats here would mean the consumer invented them")
	require.False(t, view.CreatedAt.IsZero())
	require.False(t, view.IndexedAt.IsZero())
	require.Nil(t, view.EditedAt, "a post that has never been edited must not claim an edit time")

	// ---- the spoof, now bounded ---------------------------------------------
	// The create above is visible, and it was committed to this same repo AFTER
	// the spoof — so the spoof's commit has already been through the consumer.
	spoofAbsent := func() (bool, error) {
		v, err := p.Post(context.Background(), spoofURI)
		if err != nil {
			return false, err
		}
		return v.NotFound, nil
	}
	absent, err := spoofAbsent()
	require.NoError(t, err)
	require.Truef(t, absent,
		"a post record in %s claiming to belong to community %s was INDEXED: the consumer's "+
			"repo-ownership check (repoDID == record.community) is the only thing stopping any repo "+
			"from publishing posts as any community, and the shipped binary is not applying it",
		community.DID, victim.DID)
	p.Holds(t, "the spoofed post to stay unindexed", spoofAbsent)

	// And the victim's own feed is untouched: the forged post must not have been
	// filed under the community it named, either.
	victimPost, err := p.Post(context.Background(), postURI(victim.DID, spoofRKey))
	require.NoError(t, err)
	require.Truef(t, victimPost.NotFound,
		"the forged post was indexed under the community it CLAIMED (%s) rather than the repo it "+
			"came from — an even worse outcome than indexing it, since the post would appear to be "+
			"the victim community's own", victim.DID)

	// ---- update -------------------------------------------------------------
	// Same rkey, so this is an update commit rather than a second create — the
	// consumer's updatePost path, which loads the stored row, refuses community
	// or author reassignment, and folds the changed content in.
	community.PutRecord(t, postCollection, rkey,
		postRecord(community.DID, author.DID, updated, "edited through the firehose"))

	view = observe("the updated post to reach social.coves.community.post.get",
		func(v postView) bool { return v.Record["title"] == updated })

	require.Equal(t, "edited through the firehose", view.Record["content"],
		"the update path must carry every changed field, not only the title")
	require.Equal(t, uri, view.URI, "an update must edit the post in place, not create a second one")
	require.NotEqual(t, record.CID, view.CID, "the update must index the new commit's CID")
	require.NotNil(t, view.EditedAt,
		"a content edit must set editedAt — it is what distinguishes the update path from a re-create")

	// ---- delete -------------------------------------------------------------
	// DeleteExistingRecord rather than DeleteRecord: deleting a key that is not
	// there answers 200 and emits no commit, so a wrong rkey would turn into a
	// timeout blaming the firehose (testkit/pds.go).
	community.DeleteExistingRecord(t, postCollection, rkey)

	gone := func() (bool, error) {
		v, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		return v.NotFound, nil
	}
	p.Await(t, "the deleted post to disappear from social.coves.community.post.get", gone)
	p.Holds(t, "the deleted post to stay deleted", gone)
}

// TestPostAPIContract covers the client-facing surface of the post endpoints as
// a third-party client meets it: what an unauthenticated caller gets from the
// write endpoints, and what any caller can read back about a post that exists.
//
// It carries NO ingestion marker — markers are for pipeline proofs (§3.4a), and
// this asserts the client path.
//
// The authenticated half of both write endpoints is proven at T1, for the reason
// §3.4b records and TestCommunityAPIContract spells out: nothing but the browser
// OAuth callback mints a session RequireAuth accepts. For posts specifically that
// half is internal/core/posts/service_writeforward_test.go (the record the
// service puts in the community's repo, and who may delete it) plus
// internal/api/handlers/post (handler validation). What this adds is
// the part neither can see — that the shipped binary really routes these NSIDs,
// really guards them, and really serves an indexed post back.
func TestPostAPIContract(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "pa")
	community := indexedCommunity(t, p, "a", author.DID)

	title := "api contract " + testkit.UniqueID(t)
	first := testkit.TID()
	second := testkit.TID()
	firstURI, secondURI := postURI(community.DID, first), postURI(community.DID, second)

	community.PutRecord(t, postCollection, first,
		postRecord(community.DID, author.DID, title, "read back through the client surface"))
	community.PutRecord(t, postCollection, second,
		postRecord(community.DID, author.DID, title+" (second)", "the batch's other member"))

	p.Await(t, "both posts to be indexed before the client surface is exercised", func() (bool, error) {
		views, err := p.Posts(context.Background(), firstURI, secondURI)
		if err != nil {
			return false, err
		}
		return !views[0].NotFound && !views[1].NotFound, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	t.Run("the write endpoints refuse an unauthenticated client", func(t *testing.T) {
		// One request each, no polling: this is the auth boundary of the shipped
		// router, and the answer does not become true later.
		//
		// Both post NSIDs that RegisterPostRoutes puts behind RequireAuth are
		// listed. Asserting it HERE rather than only in the handler tests is the
		// point: a handler test proves the handler refuses an unauthenticated
		// call, and structurally cannot see a route registered without the
		// middleware in front of it. Only a request to the running router can.
		//
		// post.create is additionally the one Coves route behind DualAuth (OAuth
		// users OR service-JWT aggregators), so it has two ways to be let in and
		// a correspondingly better chance of a wiring change opening it.
		for _, endpoint := range []struct {
			nsid  string
			input map[string]any
		}{
			{"social.coves.community.post.create", map[string]any{
				"community": community.DID, "title": "nope", "content": "nope"}},
			{"social.coves.community.post.delete", map[string]any{"uri": firstURI}},
		} {
			err := p.AppView.Procedure(ctx, endpoint.nsid, endpoint.input, nil)
			require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
				"%s must answer 401 to a client with no session, answered: %v", endpoint.nsid, err)
		}
	})

	t.Run("a batch read answers positionally, found and not-found alike", func(t *testing.T) {
		// The endpoint's contract is that result i is the answer about uris[i],
		// and that a valid URI nobody has indexed is a notFoundPost member inside
		// a 200 — NOT an error, and NOT a shorter array. A client hydrating a feed
		// skeleton zips these against its own list, so a compacted response would
		// silently misattribute every post after the gap.
		missing := postURI(community.DID, testkit.TID())

		views, err := p.Posts(ctx, secondURI, missing, firstURI)
		require.NoError(t, err, "an unresolvable URI in the batch must not fail the whole request")
		require.Len(t, views, 3, "the answer must have one member per requested URI")

		require.False(t, views[0].NotFound)
		require.Equal(t, secondURI, views[0].URI, "results must come back in request order")
		require.True(t, views[1].NotFound, "a valid but unindexed URI must be a notFoundPost member")
		require.Equal(t, missing, views[1].URI, "a notFoundPost must echo the URI it is about")
		require.False(t, views[2].NotFound)
		require.Equal(t, firstURI, views[2].URI)
	})

	t.Run("a handle-based URI is refused rather than resolved", func(t *testing.T) {
		// Handles are mutable, so a handle-authority URI would break on rename or,
		// worse, resolve to whoever holds the handle next. The service rejects the
		// whole request instead of degrading it to a silent notFound, which is the
		// difference between a client learning it has a bug and a client showing
		// an empty post.
		err := p.AppView.Query(ctx, "social.coves.community.post.get",
			url.Values{"uris": {"at://" + community.Handle + "/" + postCollection + "/" + first}}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusBadRequest),
			"a handle-authority URI must be a 400, answered: %v", err)
	})

	t.Run("the batch is bounded", func(t *testing.T) {
		// An unbounded batch endpoint is an amplification lever: one request, N
		// joins. The lexicon's limit is 25 and the handler enforces it before the
		// service is called.
		uris := make([]string, 0, 26)
		for i := 0; i < 26; i++ {
			uris = append(uris, postURI(community.DID, testkit.TID()))
		}
		_, err := p.Posts(ctx, uris...)
		require.Truef(t, testkit.IsStatus(err, http.StatusBadRequest),
			"asking for 26 URIs must be a 400, answered: %v", err)
	})

	t.Run("the post is served in its author's feed, by DID and by handle", func(t *testing.T) {
		// social.coves.actor.getPosts is the other read surface a post reaches a
		// client through, and it is a different query against different joins —
		// so an indexed post can be visible on post.get and missing here.
		//
		// Both identifier forms, because they take different paths through the
		// handler: a DID goes straight to the query, while a handle is resolved
		// first (get_posts.go resolveActor → users.ResolveHandleToDID). The
		// handle case doubles as proof that resolution is served from the
		// AppView's own index — the stack is egress-blocked, so a lookup that
		// escaped to DNS could not have answered at all.
		for _, actor := range []string{author.DID, author.Handle} {
			var feed struct {
				Feed []struct {
					Post postView `json:"post"`
				} `json:"feed"`
			}
			require.NoErrorf(t, p.AppView.Query(ctx, "social.coves.actor.getPosts",
				url.Values{"actor": {actor}, "limit": {"25"}}, &feed),
				"social.coves.actor.getPosts rejected identifier %q", actor)

			seen := make([]string, 0, len(feed.Feed))
			var found bool
			for _, item := range feed.Feed {
				seen = append(seen, item.Post.URI)
				if item.Post.URI == firstURI {
					found = true
					require.Equal(t, title, item.Post.Record["title"])
					require.Equal(t, community.DID, item.Post.Community.DID)
				}
			}
			require.Truef(t, found, "the post %s was not in the %d posts returned for author %q: %s",
				firstURI, len(feed.Feed), actor, strings.Join(seen, ", "))
		}
	})

	t.Run("an unknown author's feed is empty rather than an error", func(t *testing.T) {
		// A DID is passed straight through with no existence check
		// (get_posts.go resolveActor), so an unknown one answers 200 with an
		// empty feed — indistinguishable, by design, from a real account that
		// has not posted. Worth pinning because the alternative reading is
		// natural and wrong: a client cannot use this endpoint to ask whether an
		// actor exists.
		//
		// The DID is a literal rather than a generated one: the endpoint
		// validates did:plc identifiers as base32 (a-z and 2-7), which UniqueID
		// does not promise, and a malformed one would take the 400 path instead
		// of the lookup path under test. Nothing indexes it, on a fresh stack or
		// a kept one, so it needs no run-scoping.
		//
		// Spelled at the full 24 characters of a real did:plc even though the
		// validator only checks the character set: a shorter one passes today and
		// would start failing the moment that omission is corrected, quietly
		// converting this into a validation test.
		var empty struct {
			Feed []struct{} `json:"feed"`
		}
		require.NoError(t, p.AppView.Query(ctx, "social.coves.actor.getPosts",
			url.Values{"actor": {"did:plc:aaaaaaaaneverindexedactr"}}, &empty),
			"a well-formed DID nobody has indexed is an empty feed, not an error")
		assert.Empty(t, empty.Feed)

		// NOT asserted here: the handler's 404 ActorNotFound branch, which is the
		// answer to an unresolvable HANDLE. It cannot be reached honestly in this
		// stack. A handle missing from the AppView's index falls through to
		// external DNS/HTTPS resolution, and the hermetic network is egress-
		// blocked by design (§3.7), so the lookup fails with "server misbehaving"
		// and the handler renders that as a 400 resolution failure — the correct
		// answer to a broken resolver, and not the case anyone means to test.
		// Distinguishing a nonexistent handle from a broken resolver needs a
		// resolver that can answer, which is the second-PDS-and-relay topology of
		// Phase 5. The reachable half of the handle path — a handle the index DOES
		// know — is asserted in the feed subtest above.
	})
}
