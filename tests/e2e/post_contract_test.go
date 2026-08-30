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

// The post domain's client-facing contract. Author-owned postv2 ingestion and
// community admission are proven in author_post_contract_test.go; this file
// exercises the public post APIs against accepted postv2 fixtures.
//
// The authenticated write paths remain T1 concerns because this tier cannot
// mint the browser OAuth session RequireAuth accepts. Here the running router
// proves those endpoints reject anonymous callers and the public reads preserve
// their positional, validation, and author-feed contracts.

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
// service puts in the author's repo, and who may delete it) plus
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
	firstURI, secondURI := authorPostURI(author.DID, first), authorPostURI(author.DID, second)

	firstRecord := author.PutRecord(t, postV2Collection, first,
		postV2Record(community.DID, title, "read back through the client surface"))
	awaitStatus(t, p, firstURI, community.DID, "pending",
		"the first API fixture to reach the community's admission queue")
	community.PutRecord(t, acceptanceCollection, subjectRkey(firstURI),
		acceptanceRecord(firstURI, firstRecord.CID))
	awaitStatus(t, p, firstURI, community.DID, "accepted",
		"the first API fixture to be accepted")

	secondRecord := author.PutRecord(t, postV2Collection, second,
		postV2Record(community.DID, title+" (second)", "the batch's other member"))
	awaitStatus(t, p, secondURI, community.DID, "pending",
		"the second API fixture to reach the community's admission queue")
	community.PutRecord(t, acceptanceCollection, subjectRkey(secondURI),
		acceptanceRecord(secondURI, secondRecord.CID))
	awaitStatus(t, p, secondURI, community.DID, "accepted",
		"the second API fixture to be accepted")

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
		missing := authorPostURI(author.DID, testkit.TID())

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
			url.Values{"uris": {"at://" + author.Handle + "/" + postV2Collection + "/" + first}}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusBadRequest),
			"a handle-authority URI must be a 400, answered: %v", err)
	})

	t.Run("the batch is bounded", func(t *testing.T) {
		// An unbounded batch endpoint is an amplification lever: one request, N
		// joins. The lexicon's limit is 25 and the handler enforces it before the
		// service is called.
		uris := make([]string, 0, 26)
		for i := 0; i < 26; i++ {
			uris = append(uris, authorPostURI(author.DID, testkit.TID()))
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
