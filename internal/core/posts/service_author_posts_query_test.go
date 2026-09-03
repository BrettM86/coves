//go:build integration

package posts_test

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What social.coves.actor.getPosts serves for an author, against a real index.
//
// # WHY THE ROUTE AND NOT THE SERVICE
//
// GetAuthorPosts is one of the few read paths whose contract is spread across
// three layers: the handler parses actor, filter, community, limit and cursor;
// the user service turns a handle into a DID; the post service turns the rest
// into one keyset-paginated query. service_author_posts_test.go, next door,
// already covers the validation half in isolation. What it cannot cover is
// whether the pieces agree — that an opaque cursor minted by page one is
// accepted by page two, that "posts_with_media" reaches SQL as a predicate on
// the embed column rather than being dropped, that an unknown community is a
// 404 and not an empty feed. Those are only true of the whole stack, so the
// tests here mount the real routes on an httptest server over a real database.
//
// # WHY THE ROWS ARE SEEDED RATHER THAN POSTED
//
// Most of these cases are about SELECT, and a post written through the service
// would have to travel a community's PDS repo and the firehose to arrive at the
// same row — proving the write path over again, slowly, in a test about reads.
// The write path has its own coverage in service_writeforward_test.go, and the
// consumer that produces these rows for real is exercised at the bottom of this
// file, where one test indexes a post through the Jetstream consumer and then
// reads it back through the route.
//
// # WHY IT NEEDS THE PDS
//
// Only for identities. The handle-resolution case needs a handle that belongs
// to an account that actually exists, so those two tests register one; the
// others use fixture DIDs, which never have to resolve.

// authorPostsFixture is the actor read surface wired the way cmd/server wires
// it — post, user and vote services behind the real chi routes — over a
// throwaway database.
//
// The repositories and the user service are kept on the struct because the
// consumer test needs to feed the same database from the other side.
type authorPostsFixture struct {
	db            *sql.DB
	postRepo      posts.Repository
	communityRepo communities.Repository
	userService   users.UserService
	auth          *fixtures.OAuthMiddleware
	baseURL       string
}

func newAuthorPostsFixture(t *testing.T) *authorPostsFixture {
	t.Helper()

	db := testkit.DB(t)
	pdsURL := testkit.Endpoints().PDS.BaseURL

	postRepo := postgres.NewPostRepository(db)
	userRepo := postgres.NewUserRepository(db)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	voteRepo := postgres.NewVoteRepository(db)

	resolver := identity.NewResolver(db, identity.DefaultConfig())
	userService := users.NewUserService(userRepo, resolver, pdsURL, nil, "")
	// No provisioner and no blob service: nothing here creates a community, and
	// the post service consults this one only to resolve an identifier to a DID
	// and to check the community exists.
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo, pdsURL, fixtures.InstanceDID(), "", nil, nil, nil,
		communities.PrivateHostOptions(true)...)
	// The optional post collaborators (aggregators, blobs, unfurl, bluesky) are
	// all write-path concerns and stay nil.
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()))
	voteService := votes.NewServiceWithPDSFactory(voteRepo, nil, nil, fixtures.PasswordAuthPDSClientFactory())

	auth := fixtures.NewOAuthMiddleware()
	router := chi.NewRouter()
	routes.RegisterActorRoutes(router, postService, userService, voteService, nil, nil, auth.OAuthAuthMiddleware)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &authorPostsFixture{
		db:            db,
		postRepo:      postRepo,
		communityRepo: communityRepo,
		userService:   userService,
		auth:          auth,
		baseURL:       server.URL,
	}
}

// call issues a GET against social.coves.actor.getPosts. An empty token means
// an anonymous request, which the route also has to serve — the viewer's own
// vote state is the only thing authentication adds.
func (f *authorPostsFixture) call(t *testing.T, token string, query url.Values) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet,
		f.baseURL+"/xrpc/social.coves.actor.getPosts?"+query.Encode(), nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoErrorf(t, err, "GET getPosts?%s", query.Encode())
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// feed calls the route and decodes a successful response, reporting the body of
// anything that is not a 200 — an error page decoded into a zero-valued feed
// would otherwise surface as "expected 5 posts, got 0".
func (f *authorPostsFixture) feed(t *testing.T, token string, query url.Values) posts.GetAuthorPostsResponse {
	t.Helper()

	resp := f.call(t, token, query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getPosts?%s: expected 200, got %d: %s", query.Encode(), resp.StatusCode, readBody(t, resp))
	}

	var decoded posts.GetAuthorPostsResponse
	require.NoErrorf(t, json.NewDecoder(resp.Body).Decode(&decoded), "decoding getPosts?%s", query.Encode())
	return decoded
}

// statusOf calls the route anonymously and returns the status and body, for the
// cases where the rejection is the assertion.
func (f *authorPostsFixture) statusOf(t *testing.T, query url.Values) (int, string) {
	t.Helper()
	resp := f.call(t, "", query)
	return resp.StatusCode, readBody(t, resp)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "reading the response body")
	return string(body)
}

// feedURIs is the set of post URIs in a page, for asserting that two pages do
// not overlap.
func feedURIs(t *testing.T, feed posts.GetAuthorPostsResponse) map[string]bool {
	t.Helper()
	uris := make(map[string]bool, len(feed.Feed))
	for i, entry := range feed.Feed {
		require.NotNilf(t, entry.Post, "feed entry %d has no post", i)
		uris[entry.Post.URI] = true
	}
	return uris
}

// postTitle reads the title out of a PostView's record, which the wire format
// carries as the raw lexicon record rather than as typed fields.
func postTitle(t *testing.T, view *posts.PostView) string {
	t.Helper()
	require.NotNilf(t, view.Record, "post %s was served without its record", view.URI)
	record, ok := view.Record.(map[string]interface{})
	require.Truef(t, ok, "post %s: record is %T, not an object", view.URI, view.Record)
	title, ok := record["title"].(string)
	require.Truef(t, ok, "post %s: record has no string title: %+v", view.URI, record)
	return title
}

// TestGetAuthorPosts_ServesAnAuthorsPostsByEitherIdentifier covers the happy
// path an actor profile is built from: five posts, addressed by DID and by
// handle, walked with a cursor.
func TestGetAuthorPosts_ServesAnAuthorsPostsByEitherIdentifier(t *testing.T) {
	t.Parallel()

	f := newAuthorPostsFixture(t)
	ctx := context.Background()

	// A real PDS account, because one of the cases below addresses the author by
	// handle and handle resolution is only meaningful for an identity that
	// exists. The row in the index is what the query reads; the account is what
	// makes the handle real.
	author := testkit.NewPDS(t).CreateAccount(t, testkit.WithHandlePrefix("apt"))
	fixtures.User(t, f.db, author.Handle, author.DID)

	communityDID, err := fixtures.Community(ctx, f.db, "author-posts-test", "owner.test")
	require.NoError(t, err)

	now := time.Now()
	for i := 0; i < 5; i++ {
		fixtures.Post(t, f.db, communityDID, author.DID,
			fmt.Sprintf("Test Post %d", i+1), i*10, now.Add(-time.Duration(i)*time.Hour))
	}

	token := f.auth.AddUser(author.DID)

	t.Run("By DID", func(t *testing.T) {
		feed := f.feed(t, token, url.Values{"actor": {author.DID}, "limit": {"10"}})
		require.Len(t, feed.Feed, 5)
		for i, entry := range feed.Feed {
			require.NotNilf(t, entry.Post, "feed entry %d has no post", i)
		}
	})

	t.Run("By handle", func(t *testing.T) {
		// Same five posts, addressed by the handle: the route has to resolve it
		// to the DID the posts are stored under before it can query at all.
		feed := f.feed(t, "", url.Values{"actor": {author.Handle}, "limit": {"5"}})
		require.Len(t, feed.Feed, 5)
	})

	t.Run("Cursor walks the pages without repeating a post", func(t *testing.T) {
		firstPage := f.feed(t, "", url.Values{"actor": {author.DID}, "limit": {"3"}})
		require.Len(t, firstPage.Feed, 3)
		require.NotNil(t, firstPage.Cursor, "a full page should carry a cursor to the next one")

		secondPage := f.feed(t, "", url.Values{
			"actor":  {author.DID},
			"limit":  {"3"},
			"cursor": {*firstPage.Cursor},
		})
		require.Len(t, secondPage.Feed, 2, "the remaining two posts")

		seen := feedURIs(t, firstPage)
		for uri := range feedURIs(t, secondPage) {
			assert.Falsef(t, seen[uri], "post %s appeared on both pages", uri)
		}
	})

	t.Run("An actor with no posts is an empty feed, not an error", func(t *testing.T) {
		// An actor's post list is a projection, so asking for one that happens
		// to be empty is a legitimate question with an empty answer — the same
		// shape Bluesky's getAuthorFeed returns. A 404 here would make "this
		// author has posted nothing" indistinguishable from "this endpoint is
		// broken" for every client rendering a new user's profile.
		//
		// The DID must be a REAL did:plc shape — 24 base32 characters — and that
		// is the whole reason this case is written out rather than reusing
		// fixtures.DID. An earlier version asked about "did:plc:nonexistent123",
		// got a 400 for invalid DID syntax, and logged the answer without
		// asserting it: it was a second, worse copy of the malformed-input case
		// in TestGetAuthorPosts_RejectsMalformedRequests, wearing the name of a
		// case nobody had written. Anything that fails syntax validation cannot
		// reach the query this subtest is about.
		const nobody = "did:plc:aaaaaaaaaaaanopostsatall"

		status, body := f.statusOf(t, url.Values{"actor": {nobody}})
		require.Equalf(t, http.StatusOK, status,
			"a syntactically valid DID with no posts must answer 200 and an empty feed, not %d: %s",
			status, body)

		feed := f.feed(t, "", url.Values{"actor": {nobody}})
		assert.Empty(t, feed.Feed, "nobody has posted under %s", nobody)
	})
}

// TestGetAuthorPosts_FilterSelectsPostsWithMedia covers the filter parameter,
// which is the only thing that changes the SELECT's predicate.
func TestGetAuthorPosts_FilterSelectsPostsWithMedia(t *testing.T) {
	t.Parallel()

	f := newAuthorPostsFixture(t)
	ctx := context.Background()

	authorDID := fixtures.DID("filter")
	fixtures.User(t, f.db, "filtertest.test", authorDID)

	communityDID, err := fixtures.Community(ctx, f.db, "filter-test", "owner.test")
	require.NoError(t, err)

	now := time.Now()
	fixtures.Post(t, f.db, communityDID, authorDID, "Post without embed", 10, now)

	// Inserted directly rather than through fixtures.Post, which deliberately
	// has no embed parameter: the media filter is a predicate on this column and
	// nothing else, so the row has to carry one.
	embedJSON := `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com"}}`
	_, err = f.db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, embed, created_at, score)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 20)
	`,
		fmt.Sprintf("at://%s/social.coves.community.post/embed-post", communityDID),
		"bafyembed", "embed-post", authorDID, communityDID,
		"Post with embed", embedJSON, now.Add(-1*time.Hour))
	require.NoError(t, err, "seeding the post with an embed")

	t.Run("posts_with_media returns only the post carrying an embed", func(t *testing.T) {
		feed := f.feed(t, "", url.Values{"actor": {authorDID}, "filter": {"posts_with_media"}})
		require.Len(t, feed.Feed, 1)
		require.NotNil(t, feed.Feed[0].Post)
		assert.NotNil(t, feed.Feed[0].Post.Embed, "the one post returned should be the one with the embed")
	})

	t.Run("posts_with_replies returns everything", func(t *testing.T) {
		feed := f.feed(t, "", url.Values{"actor": {authorDID}, "filter": {"posts_with_replies"}})
		assert.Len(t, feed.Feed, 2)
	})

	t.Run("An unknown filter is rejected", func(t *testing.T) {
		// Rejected rather than ignored: silently serving the unfiltered feed for
		// a filter the client thought it had applied is the worst of the three
		// outcomes.
		status, body := f.statusOf(t, url.Values{"actor": {authorDID}, "filter": {"invalid_filter"}})
		assert.Equalf(t, http.StatusBadRequest, status, "body: %s", body)
	})
}

// TestGetAuthorPosts_RejectsMalformedRequests covers the parameters a caller can
// get wrong, and the status each one earns.
func TestGetAuthorPosts_RejectsMalformedRequests(t *testing.T) {
	t.Parallel()

	f := newAuthorPostsFixture(t)
	ctx := context.Background()

	authorDID := fixtures.DID("serviceerror")
	fixtures.User(t, f.db, "serviceerror.test", authorDID)

	communityDID, err := fixtures.Community(ctx, f.db, "serviceerror-test", "owner.test")
	require.NoError(t, err)
	fixtures.Post(t, f.db, communityDID, authorDID, "Test Post", 10, time.Now())

	t.Run("Missing actor", func(t *testing.T) {
		status, body := f.statusOf(t, url.Values{})
		assert.Equalf(t, http.StatusBadRequest, status, "body: %s", body)
	})

	t.Run("An actor that is neither a DID nor a resolvable handle", func(t *testing.T) {
		// Either answer is defensible — "you sent nonsense" or "no such actor" —
		// and both are refusals. What must not happen is a 200 over somebody
		// else's posts, or a 500.
		status, body := f.statusOf(t, url.Values{"actor": {"not-a-did"}})
		assert.Containsf(t, []int{http.StatusBadRequest, http.StatusNotFound}, status, "body: %s", body)
	})

	t.Run("Malformed cursor", func(t *testing.T) {
		// The cursor is opaque to the client, so a value the server did not mint
		// is a client error, not a reason to fall back to the first page.
		status, body := f.statusOf(t, url.Values{"actor": {authorDID}, "cursor": {"invalid-cursor-format"}})
		assert.Equalf(t, http.StatusBadRequest, status, "body: %s", body)
	})

	t.Run("Community filter naming a community that does not exist", func(t *testing.T) {
		// 404 rather than an empty feed: an empty feed would read as "this author
		// has posted nothing there", which is a different and misleading answer.
		status, body := f.statusOf(t, url.Values{
			"actor":     {authorDID},
			"community": {"did:plc:nonexistentcommunity"},
		})
		assert.Equalf(t, http.StatusNotFound, status, "body: %s", body)
	})
}

// TestGetAuthorPosts_ServesAPostIndexedByTheConsumer closes the loop between the
// two halves of the AppView: the firehose consumer writes the row, the read path
// serves it.
//
// The two are written against the same table by different code, and a column the
// consumer populates but the query does not select (or vice versa) is invisible
// to a test of either one alone.
func TestGetAuthorPosts_ServesAPostIndexedByTheConsumer(t *testing.T) {
	t.Parallel()

	f := newAuthorPostsFixture(t)
	ctx := context.Background()

	author := testkit.NewPDS(t).CreateAccount(t, testkit.WithHandlePrefix("jet"))
	fixtures.User(t, f.db, author.Handle, author.DID)

	communityDID, err := fixtures.Community(ctx, f.db, "jetstream-author-test", "owner.test")
	require.NoError(t, err)

	admissions := postgres.NewAdmissionRepository(f.db)
	postConsumer := jetstream.NewPostEventConsumer(
		f.postRepo, f.communityRepo, f.userService, f.db,
		jetstream.WithAdmissions(admissions),
	)

	// The postv2 commit comes from the author's repo; authorship is the event DID,
	// while the record names only the community it was submitted to.
	rkey := testkit.TID()
	const cid = "bafyjetstream"
	err = postConsumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
		Did:    author.DID,
		TimeUS: time.Now().UnixMicro(),
		Kind:   "commit",
		Commit: &jetstream.CommitEvent{
			Rev:        "test-post-rev",
			Operation:  "create",
			Collection: posts.PostV2Collection,
			RKey:       rkey,
			CID:        cid,
			Record: map[string]interface{}{
				"$type":     posts.PostV2Collection,
				"community": communityDID,
				"title":     "Jetstream Indexed Post",
				"content":   "This post was indexed via Jetstream",
				"createdAt": time.Now().Format(time.RFC3339),
			},
		},
	})
	require.NoError(t, err, "indexing the post from the firehose")

	postURI := fmt.Sprintf("at://%s/%s/%s", author.DID, posts.PostV2Collection, rkey)
	acceptanceRkey := testkit.TID()
	_, err = admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   communityDID,
		PostURI:        postURI,
		AcceptanceURI:  fmt.Sprintf("at://%s/social.coves.community.acceptance/%s", communityDID, acceptanceRkey),
		AcceptanceRkey: acceptanceRkey,
		PinnedCID:      cid,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err, "accepting the indexed post for the author feed")

	feed := f.feed(t, "", url.Values{"actor": {author.DID}})
	require.Len(t, feed.Feed, 1)
	require.NotNil(t, feed.Feed[0].Post)
	assert.Equal(t, "Jetstream Indexed Post", postTitle(t, feed.Feed[0].Post))
	assert.Equal(t, postURI,
		feed.Feed[0].Post.URI, "the served post should be the one the consumer indexed")
}

// TestGetAuthorPosts_CommunityFilterNarrowsToOneCommunity covers the community
// parameter, which is how a profile shows "this author, in this community".
func TestGetAuthorPosts_CommunityFilterNarrowsToOneCommunity(t *testing.T) {
	t.Parallel()

	f := newAuthorPostsFixture(t)
	ctx := context.Background()

	authorDID := fixtures.DID("communityfilter")
	fixtures.User(t, f.db, "communityfilter.test", authorDID)

	firstCommunity, err := fixtures.Community(ctx, f.db, "filter-community-1", "owner1.test")
	require.NoError(t, err)
	secondCommunity, err := fixtures.Community(ctx, f.db, "filter-community-2", "owner2.test")
	require.NoError(t, err)

	now := time.Now()
	fixtures.Post(t, f.db, firstCommunity, authorDID, "Post in Community 1 - A", 10, now)
	fixtures.Post(t, f.db, firstCommunity, authorDID, "Post in Community 1 - B", 20, now.Add(-1*time.Hour))
	fixtures.Post(t, f.db, secondCommunity, authorDID, "Post in Community 2", 30, now.Add(-2*time.Hour))

	t.Run("Filtered to the first community", func(t *testing.T) {
		feed := f.feed(t, "", url.Values{"actor": {authorDID}, "community": {firstCommunity}})
		require.Len(t, feed.Feed, 2)
		for i, entry := range feed.Feed {
			require.NotNilf(t, entry.Post, "feed entry %d has no post", i)
			assert.Equal(t, firstCommunity, entry.Post.Community.DID)
		}
	})

	t.Run("Unfiltered returns both communities", func(t *testing.T) {
		feed := f.feed(t, "", url.Values{"actor": {authorDID}})
		assert.Len(t, feed.Feed, 3)
	})
}
