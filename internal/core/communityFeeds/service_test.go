// The community feed service: the layer between "GET
// /xrpc/social.coves.communityFeed.getCommunity?community=!tech" and a SQL
// query over indexed posts.
//
// # WHAT THIS LAYER ACTUALLY DOES, AND WHY IT NEEDS ITS OWN TESTS
//
// It does three things, and each one is invisible from either side of it:
//
//  1. It fills in defaults. A request with no sort is a request for "hot"; a
//     request with no limit is a request for 15. The repository never sees the
//     caller's blanks, so a default that drifts is a silently different feed.
//  2. It bounds the request. A limit above 50 is refused rather than clamped,
//     which matters because it is the difference between a client getting fewer
//     rows than it asked for and a client being told so. Nothing downstream
//     re-checks it: the repository interpolates the number straight into LIMIT.
//  3. It resolves the community. The caller names a community by handle or by
//     scoped identifier; the repository only knows DIDs. The DID substitution
//     happens here and nowhere else, so if it stops happening, the repository
//     filters on a string that matches no rows and the feed is simply empty —
//     a 200 with no posts, which no error monitor will ever notice.
//
// # WHY THE FAKES RECORD
//
// Every one of those three is a property of what REACHED the repository, not of
// what came back. A fake that answered from a fixture and was asserted against
// that same fixture would pass with every default dropped, every bound removed
// and the identifier unresolved. So the repository fake below appends each
// request to a call log and the tests assert on the log. The community service
// fake does the same for the identifier it was asked to resolve.
//
// This is an external test package because the fakes it needs are of
// interfaces that live here, and because nothing in the assertions needs to
// reach inside the package. It is untagged: no database, no sockets, nothing
// out of process.
package communityFeeds_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
)

// recordingFeedRepo logs every request it is handed and answers with whatever
// the test seeded.
type recordingFeedRepo struct {
	mu sync.Mutex

	feed   []*communityFeeds.FeedViewPost
	cursor *string
	err    error

	requests []communityFeeds.GetCommunityFeedRequest
}

func (r *recordingFeedRepo) GetCommunityFeed(_ context.Context, req communityFeeds.GetCommunityFeedRequest) ([]*communityFeeds.FeedViewPost, *string, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	if r.err != nil {
		return nil, nil, r.err
	}
	return r.feed, r.cursor, nil
}

// only returns the single request the repository received, or fails the test
// naming what it actually saw. A test that indexed blindly would panic on the
// most interesting failure — the one where the repository was never called.
func (r *recordingFeedRepo) only(t *testing.T) communityFeeds.GetCommunityFeedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 {
		t.Fatalf("expected the repository to be asked exactly once, got %d call(s): %+v",
			len(r.requests), r.requests)
	}
	return r.requests[0]
}

func (r *recordingFeedRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// recordingCommunityService is a communities.Service that answers only
// ResolveCommunityIdentifier. The embedded interface is nil, so any other call
// panics — the feed service is supposed to use exactly one method of it, and
// this is what keeps that true.
type recordingCommunityService struct {
	communities.Service

	mu sync.Mutex

	resolved string
	err      error

	asked []string
}

func (c *recordingCommunityService) ResolveCommunityIdentifier(_ context.Context, identifier string) (string, error) {
	c.mu.Lock()
	c.asked = append(c.asked, identifier)
	c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	return c.resolved, nil
}

func (c *recordingCommunityService) askedFor() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// theCommunityDID is what the identifier resolves to in the tests that get that
// far. It is deliberately unlike any identifier passed in, so "the repository
// received the resolved DID" cannot accidentally hold.
const theCommunityDID = "did:plc:resolvedcommunity000000"

// errRepositoryUnavailable stands in for anything the datastore can go wrong
// with. It is not one of the domain's typed errors, so "propagated" can be told
// apart from "mapped".
var errRepositoryUnavailable = errors.New("connection pool exhausted")

// newFeedService wires the service the way cmd/server does, over fakes.
func newFeedService(repo *recordingFeedRepo, communityService *recordingCommunityService) communityFeeds.Service {
	return communityFeeds.NewCommunityFeedService(repo, communityService)
}

// aRequest is a request that passes validation, so that each test can change
// exactly the one field it is about.
func aRequest() communityFeeds.GetCommunityFeedRequest {
	return communityFeeds.GetCommunityFeedRequest{Community: "tech.coves.social"}
}

func TestGetCommunityFeed_DefaultsReachTheRepository(t *testing.T) {
	t.Parallel()

	t.Run("an unspecified sort becomes hot", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})

		if _, err := service.GetCommunityFeed(context.Background(), aRequest()); err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		if got := repo.only(t).Sort; got != "hot" {
			t.Errorf("the repository was asked to sort by %q, want %q. The default decides what "+
				"every visitor to a community sees first", got, "hot")
		}
	})

	t.Run("an unspecified limit becomes 15", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})

		if _, err := service.GetCommunityFeed(context.Background(), aRequest()); err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		if got := repo.only(t).Limit; got != 15 {
			t.Errorf("the repository was asked for %d posts, want the default 15. A zero would reach "+
				"SQL as LIMIT 0 and serve an empty feed", got)
		}
	})

	t.Run("a negative limit becomes 15 rather than reaching SQL", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
		req := aRequest()
		req.Limit = -1

		if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		if got := repo.only(t).Limit; got != 15 {
			t.Errorf("the repository was asked for %d posts; a negative LIMIT is a SQL error, so this "+
				"is the layer that has to absorb it", got)
		}
	})

	t.Run("sort=top with no timeframe becomes a day", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
		req := aRequest()
		req.Sort = "top"

		if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		if got := repo.only(t).Timeframe; got != "day" {
			t.Errorf("top with no timeframe reached the repository as %q, want %q. An empty timeframe "+
				"means no time filter at all, so \"top\" would quietly become \"top of all time\" — a "+
				"feed that never changes", got, "day")
		}
	})

	t.Run("the other sorts get no timeframe", func(t *testing.T) {
		t.Parallel()
		for _, sort := range []string{"hot", "new"} {
			t.Run(sort, func(t *testing.T) {
				t.Parallel()
				repo := &recordingFeedRepo{}
				service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
				req := aRequest()
				req.Sort = sort

				if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
					t.Fatalf("GetCommunityFeed: %v", err)
				}

				if got := repo.only(t).Timeframe; got != "" {
					t.Errorf("sort=%s reached the repository with timeframe %q, want none: a time "+
						"window on hot or new would hide older posts that are still ranking", sort, got)
				}
			})
		}
	})
}

func TestGetCommunityFeed_RejectsWhatTheRepositoryCannotBound(t *testing.T) {
	t.Parallel()

	t.Run("a limit above 50 is refused, not clamped", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
		req := aRequest()
		req.Limit = 5000

		_, err := service.GetCommunityFeed(context.Background(), req)

		if !communityFeeds.IsValidationError(err) {
			t.Fatalf("err = %v, want a validation error", err)
		}
		// Refusing rather than clamping is a deliberate choice and worth
		// pinning: a client that silently receives 50 rows for a request of
		// 5,000 has no way to know its paging is wrong.
		if repo.callCount() != 0 {
			t.Errorf("the repository was asked %d time(s) for an out-of-range page. Nothing below "+
				"this layer re-checks the number — it is interpolated into LIMIT", repo.callCount())
		}
	})

	t.Run("a limit of exactly 50 is allowed", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
		req := aRequest()
		req.Limit = 50

		if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
			t.Fatalf("a limit of 50 was refused (%v); the boundary is inclusive", err)
		}
		if got := repo.only(t).Limit; got != 50 {
			t.Errorf("the repository was asked for %d, want 50 unchanged", got)
		}
	})

	t.Run("an unknown sort is refused", func(t *testing.T) {
		t.Parallel()
		for _, sort := range []string{"controversial", "HOT", "rising", " hot"} {
			t.Run(sort, func(t *testing.T) {
				t.Parallel()
				repo := &recordingFeedRepo{}
				service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
				req := aRequest()
				req.Sort = sort

				_, err := service.GetCommunityFeed(context.Background(), req)

				if !communityFeeds.IsValidationError(err) {
					t.Fatalf("sort=%q gave err = %v, want a validation error", sort, err)
				}
				if repo.callCount() != 0 {
					t.Errorf("sort=%q reached the repository, which builds an ORDER BY from it", sort)
				}
			})
		}
	})

	t.Run("an unknown timeframe is refused even on a sort that ignores it", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
		req := aRequest()
		req.Sort = "new"
		req.Timeframe = "fortnight"

		_, err := service.GetCommunityFeed(context.Background(), req)

		if !communityFeeds.IsValidationError(err) {
			t.Fatalf("err = %v, want a validation error", err)
		}
	})

	t.Run("every documented timeframe is accepted", func(t *testing.T) {
		t.Parallel()
		for _, timeframe := range []string{"hour", "day", "week", "month", "year", "all"} {
			t.Run(timeframe, func(t *testing.T) {
				t.Parallel()
				repo := &recordingFeedRepo{}
				service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
				req := aRequest()
				req.Sort = "top"
				req.Timeframe = timeframe

				if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
					t.Fatalf("timeframe=%q was refused: %v", timeframe, err)
				}
				if got := repo.only(t).Timeframe; got != timeframe {
					t.Errorf("timeframe reached the repository as %q, want %q unchanged", got, timeframe)
				}
			})
		}
	})

	t.Run("a missing community is refused before anything is resolved", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		communityService := &recordingCommunityService{resolved: theCommunityDID}
		service := newFeedService(repo, communityService)

		_, err := service.GetCommunityFeed(context.Background(), communityFeeds.GetCommunityFeedRequest{})

		if !communityFeeds.IsValidationError(err) {
			t.Fatalf("err = %v, want a validation error", err)
		}
		if len(communityService.askedFor()) != 0 {
			t.Errorf("the community service was asked to resolve %v; an empty identifier is a bad "+
				"request, not a lookup", communityService.askedFor())
		}
	})
}

// TestGetCommunityFeed_TheRepositoryQueriesTheResolvedDID is the substitution
// this layer exists for.
//
// A caller names a community however they like — a handle, a scoped
// identifier, or a DID they already have. The repository filters
// posts.community_did, so anything but a DID matches nothing. That failure is
// silent: an empty feed and a 200.
func TestGetCommunityFeed_TheRepositoryQueriesTheResolvedDID(t *testing.T) {
	t.Parallel()

	for _, identifier := range []string{
		"tech.coves.social",
		"!tech@coves.social",
		"did:plc:somethingelse00000000000",
	} {
		t.Run(identifier, func(t *testing.T) {
			t.Parallel()
			repo := &recordingFeedRepo{}
			communityService := &recordingCommunityService{resolved: theCommunityDID}
			service := newFeedService(repo, communityService)
			req := aRequest()
			req.Community = identifier

			if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
				t.Fatalf("GetCommunityFeed: %v", err)
			}

			if asked := communityService.askedFor(); len(asked) != 1 || asked[0] != identifier {
				t.Errorf("the community service was asked to resolve %v, want exactly [%q]", asked, identifier)
			}
			if got := repo.only(t).Community; got != theCommunityDID {
				t.Errorf("the repository was asked for community %q, want the resolved DID %q. "+
					"Filtering on an unresolved identifier matches no rows, so this failure looks "+
					"like an empty community rather than a bug", got, theCommunityDID)
			}
		})
	}
}

// TestGetCommunityFeed_ResolutionFailuresKeepTheirMeaning covers the three-way
// split the service makes on a resolution error.
//
// The handler turns each of these into a different status, and getting one
// wrong is a real product failure: a community that does not exist must be a
// 404 so a client can say so, a malformed identifier must be a 400 so the
// caller fixes their input, and a database outage must be a 500 so it is
// visible on a dashboard instead of being reported to users as "no such
// community".
func TestGetCommunityFeed_ResolutionFailuresKeepTheirMeaning(t *testing.T) {
	t.Parallel()

	t.Run("a community that does not exist", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{
			err: fmt.Errorf("resolving: %w", communities.ErrCommunityNotFound),
		})

		_, err := service.GetCommunityFeed(context.Background(), aRequest())

		if !errors.Is(err, communityFeeds.ErrCommunityNotFound) {
			t.Errorf("err = %v, want ErrCommunityNotFound", err)
		}
		if repo.callCount() != 0 {
			t.Errorf("the repository was queried for a community that does not exist")
		}
	})

	t.Run("an identifier that is not well formed", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{
			err: communities.NewValidationError("community", "not a handle, DID or scoped identifier"),
		})

		_, err := service.GetCommunityFeed(context.Background(), aRequest())

		if !communityFeeds.IsValidationError(err) {
			t.Errorf("err = %v, want a validation error", err)
		}
		if errors.Is(err, communityFeeds.ErrCommunityNotFound) {
			t.Errorf("a malformed identifier was reported as a missing community")
		}
	})

	t.Run("a resolver that could not answer", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{err: errRepositoryUnavailable})

		_, err := service.GetCommunityFeed(context.Background(), aRequest())

		if !errors.Is(err, errRepositoryUnavailable) {
			t.Fatalf("err = %v, want the underlying failure to be preserved for errors.Is", err)
		}
		// The distinction that matters: an outage must not be indistinguishable
		// from an absent community, or every dashboard reads clean while the
		// product is down.
		if errors.Is(err, communityFeeds.ErrCommunityNotFound) || communityFeeds.IsValidationError(err) {
			t.Errorf("a resolver outage was mapped to %v, which the handler answers as a 4xx", err)
		}
	})
}

func TestGetCommunityFeed_PassesTheCallerThrough(t *testing.T) {
	t.Parallel()

	t.Run("the cursor reaches the repository unchanged", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
		cursor := "1717171717::at://did:plc:x/social.coves.community.post/3k"
		req := aRequest()
		req.Cursor = &cursor

		if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		got := repo.only(t).Cursor
		if got == nil || *got != cursor {
			t.Errorf("the repository received cursor %v, want %q. A dropped cursor restarts every "+
				"page at the top of the feed", got, cursor)
		}
	})

	t.Run("the viewer DID reaches the repository", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
		req := aRequest()
		req.ViewerDID = "did:plc:theviewer0000000000000"

		if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		if got := repo.only(t).ViewerDID; got != req.ViewerDID {
			t.Errorf("the repository received viewer %q, want %q. The viewer DID is what the block "+
				"filter joins on, so losing it serves a blocked author's posts to the person who "+
				"blocked them", got, req.ViewerDID)
		}
	})

	t.Run("an anonymous caller reaches the repository with no viewer", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})

		if _, err := service.GetCommunityFeed(context.Background(), aRequest()); err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		if got := repo.only(t).ViewerDID; got != "" {
			t.Errorf("the repository received viewer %q for an unauthenticated request", got)
		}
	})
}

func TestGetCommunityFeed_ShapesTheResponse(t *testing.T) {
	t.Parallel()

	t.Run("the repository's rows and cursor are what the caller gets", func(t *testing.T) {
		t.Parallel()
		cursor := "the-next-page"
		rows := []*communityFeeds.FeedViewPost{
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/1"}},
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/2"}},
		}
		repo := &recordingFeedRepo{feed: rows, cursor: &cursor}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})

		response, err := service.GetCommunityFeed(context.Background(), aRequest())
		if err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}

		if len(response.Feed) != len(rows) {
			t.Fatalf("the response carried %d posts, want %d", len(response.Feed), len(rows))
		}
		for i, row := range rows {
			if response.Feed[i] != row {
				t.Errorf("post %d is not the row the repository returned", i)
			}
		}
		if response.Cursor == nil || *response.Cursor != cursor {
			t.Errorf("the response cursor is %v, want %q. Dropping it ends pagination at page one",
				response.Cursor, cursor)
		}
	})

	t.Run("an empty community is an empty feed, not an error", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})

		response, err := service.GetCommunityFeed(context.Background(), aRequest())
		if err != nil {
			t.Fatalf("GetCommunityFeed: %v", err)
		}
		if len(response.Feed) != 0 {
			t.Errorf("the response carried %d posts, want none", len(response.Feed))
		}
		if response.Cursor != nil {
			t.Errorf("the response carried cursor %q with no rows, which asks the client to page "+
				"into nothing", *response.Cursor)
		}
	})

	t.Run("a repository failure is propagated, not flattened", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{err: errRepositoryUnavailable}
		service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})

		response, err := service.GetCommunityFeed(context.Background(), aRequest())

		if !errors.Is(err, errRepositoryUnavailable) {
			t.Fatalf("err = %v, want the repository's failure", err)
		}
		if response != nil {
			t.Errorf("a failed query still returned a response (%+v), which a caller would serve as "+
				"an empty feed", response)
		}
	})
}

// TestGetCommunityFeed_FeedViewPostExposesItsPost covers the accessor the
// handler uses to hang viewer vote state on each row.
//
// GetPost is how the enricher reaches into a feed item without knowing which of
// the three feed packages produced it. A nil return would silently skip
// enrichment for the whole page — every post rendered as unvoted for a user who
// has voted.
func TestGetCommunityFeed_FeedViewPostExposesItsPost(t *testing.T) {
	t.Parallel()

	post := &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/1"}
	item := &communityFeeds.FeedViewPost{Post: post}

	if item.GetPost() != post {
		t.Errorf("GetPost returned %v, want the wrapped post", item.GetPost())
	}
}

// TestGetCommunityFeed_TheCallerIsNotMutated records a property clients depend
// on without knowing it.
//
// GetCommunityFeed takes its request BY VALUE and validateRequest fills the
// defaults into that copy, so the caller's struct is untouched. A handler that
// reuses one request struct across a retry — or logs it after the call — sees
// what the client actually sent. Switching the parameter to a pointer would
// change that silently.
func TestGetCommunityFeed_TheCallerIsNotMutated(t *testing.T) {
	t.Parallel()

	repo := &recordingFeedRepo{}
	service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
	req := aRequest()

	if _, err := service.GetCommunityFeed(context.Background(), req); err != nil {
		t.Fatalf("GetCommunityFeed: %v", err)
	}

	if req.Sort != "" || req.Limit != 0 || req.Community != "tech.coves.social" {
		t.Errorf("the caller's request was modified in place: %+v", req)
	}
	// ...while the repository really did see the filled-in version, so the
	// assertion above is about copying rather than about defaults being absent.
	if seen := repo.only(t); seen.Sort != "hot" || seen.Limit != 15 || seen.Community != theCommunityDID {
		t.Errorf("the repository saw %+v, want the defaulted and resolved request", seen)
	}
}
