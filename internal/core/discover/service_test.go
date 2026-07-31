// The discover service: the layer between "GET
// /xrpc/social.coves.feed.getDiscover" and the query that ranks posts from
// every community at once.
//
// # WHY IT IS TESTED SEPARATELY FROM THE TIMELINE, WHICH IT RESEMBLES
//
// discover_feed_test.go drives this service, its Postgres repository and the
// HTTP handler together against a real database, and it owns the things that
// need real SQL: the log-damped hot ranking, pagination across negative scores,
// future-dated posts, viewer vote state. None of that can see the decisions
// made HERE, which are all decisions about what the repository is asked.
//
// Discover and the timeline share a validation shape almost line for line, and
// that resemblance is the reason both get their own file rather than one shared
// one. They are separate packages with separately-declared valid sorts and
// limits, and the interesting failure is one of them drifting from the other —
// a bound raised in discover and not in the timeline, a sort accepted here and
// refused there. A test that abstracted over both would express the drift as
// shared and never catch it.
//
// The two real differences from the timeline are asserted below: discover has
// no identity requirement at all (it is the feed for someone who has just
// arrived), and its viewer DID is optional rather than mandatory — supplied it
// filters blocks, absent it serves the public feed.
//
// The repository fake records what it was handed rather than answering from a
// fixture. External test package, untagged, no database and no sockets.
package discover_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"Coves/internal/core/discover"
	"Coves/internal/core/posts"
)

// recordingDiscoverRepo logs every request and answers with what the test
// seeded.
type recordingDiscoverRepo struct {
	mu sync.Mutex

	feed   []*discover.FeedViewPost
	cursor *string
	err    error

	requests []discover.GetDiscoverRequest
}

func (r *recordingDiscoverRepo) GetDiscover(_ context.Context, req discover.GetDiscoverRequest) ([]*discover.FeedViewPost, *string, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	if r.err != nil {
		return nil, nil, r.err
	}
	return r.feed, r.cursor, nil
}

// only returns the single request the repository received, failing with the
// full log otherwise — including the case that matters most, where it was never
// called at all.
func (r *recordingDiscoverRepo) only(t *testing.T) discover.GetDiscoverRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 {
		t.Fatalf("expected the repository to be asked exactly once, got %d call(s): %+v",
			len(r.requests), r.requests)
	}
	return r.requests[0]
}

func (r *recordingDiscoverRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// errRepositoryUnavailable is deliberately not one of the domain's typed
// errors, so "propagated" can be told apart from "mapped".
var errRepositoryUnavailable = errors.New("connection pool exhausted")

func TestGetDiscover_DefaultsReachTheRepository(t *testing.T) {
	t.Parallel()

	t.Run("an unspecified sort becomes hot", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{}); err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}

		if got := repo.only(t).Sort; got != "hot" {
			t.Errorf("the repository was asked to sort by %q, want %q. Discover is the first page a "+
				"logged-out visitor sees, so this default is the product's shop window", got, "hot")
		}
	})

	t.Run("an unspecified limit becomes 15", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{}); err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}

		if got := repo.only(t).Limit; got != 15 {
			t.Errorf("the repository was asked for %d posts, want the default 15. A zero reaches SQL "+
				"as LIMIT 0, so a bare GET with no parameters would return nothing at all", got)
		}
	})

	t.Run("a negative limit becomes 15 rather than reaching SQL", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{Limit: -1}); err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}

		if got := repo.only(t).Limit; got != 15 {
			t.Errorf("the repository was asked for %d posts; a negative LIMIT is a SQL error", got)
		}
	})

	t.Run("sort=top with no timeframe becomes a day", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{Sort: "top"}); err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}

		if got := repo.only(t).Timeframe; got != "day" {
			t.Errorf("top with no timeframe reached the repository as %q, want %q: with no window, "+
				"discover's top tab would show the same all-time posts forever", got, "day")
		}
	})

	t.Run("the other sorts get no timeframe", func(t *testing.T) {
		t.Parallel()
		for _, sort := range []string{"hot", "new"} {
			t.Run(sort, func(t *testing.T) {
				t.Parallel()
				repo := &recordingDiscoverRepo{}
				service := discover.NewDiscoverService(repo)

				if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{Sort: sort}); err != nil {
					t.Fatalf("GetDiscover: %v", err)
				}

				if got := repo.only(t).Timeframe; got != "" {
					t.Errorf("sort=%s reached the repository with timeframe %q, want none", sort, got)
				}
			})
		}
	})
}

func TestGetDiscover_RejectsWhatTheRepositoryCannotBound(t *testing.T) {
	t.Parallel()

	t.Run("a limit above 50 is refused, not clamped", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		_, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{Limit: 51})

		if !discover.IsValidationError(err) {
			t.Fatalf("err = %v, want a validation error", err)
		}
		if repo.callCount() != 0 {
			t.Errorf("the repository was asked %d time(s) for an out-of-range page. Discover is "+
				"unauthenticated, so an unbounded page size here is an unauthenticated way to make "+
				"the database do arbitrary work", repo.callCount())
		}
	})

	t.Run("a limit of exactly 50 is allowed", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{Limit: 50}); err != nil {
			t.Fatalf("a limit of 50 was refused (%v); the boundary is inclusive", err)
		}
		if got := repo.only(t).Limit; got != 50 {
			t.Errorf("the repository was asked for %d, want 50 unchanged", got)
		}
	})

	t.Run("an unknown sort is refused", func(t *testing.T) {
		t.Parallel()
		for _, sort := range []string{"controversial", "Hot", "rising"} {
			t.Run(sort, func(t *testing.T) {
				t.Parallel()
				repo := &recordingDiscoverRepo{}
				service := discover.NewDiscoverService(repo)

				_, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{Sort: sort})

				if !discover.IsValidationError(err) {
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
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		_, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{
			Sort: "new", Timeframe: "fortnight",
		})

		if !discover.IsValidationError(err) {
			t.Fatalf("err = %v, want a validation error", err)
		}
	})

	t.Run("every documented timeframe is accepted", func(t *testing.T) {
		t.Parallel()
		for _, timeframe := range []string{"hour", "day", "week", "month", "year", "all"} {
			t.Run(timeframe, func(t *testing.T) {
				t.Parallel()
				repo := &recordingDiscoverRepo{}
				service := discover.NewDiscoverService(repo)

				if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{
					Sort: "top", Timeframe: timeframe,
				}); err != nil {
					t.Fatalf("timeframe=%q was refused: %v", timeframe, err)
				}
				if got := repo.only(t).Timeframe; got != timeframe {
					t.Errorf("timeframe reached the repository as %q, want %q unchanged", got, timeframe)
				}
			})
		}
	})
}

// TestGetDiscover_NeedsNoIdentity is the difference from the timeline, and it
// is a product guarantee rather than an implementation detail: a visitor with
// no account can see what is happening on the instance.
//
// The timeline refuses a request with no user DID (ErrUnauthorized). Discover
// must not grow the same check by copy-paste, which is a live risk precisely
// because the two services are otherwise near-identical.
func TestGetDiscover_NeedsNoIdentity(t *testing.T) {
	t.Parallel()

	repo := &recordingDiscoverRepo{}
	service := discover.NewDiscoverService(repo)

	response, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{})

	if err != nil {
		t.Fatalf("an anonymous request was refused: %v. Discover is the public feed — the route "+
			"carries OptionalAuth, not RequireAuth", err)
	}
	if response == nil {
		t.Fatalf("an anonymous request produced no response")
	}
	if repo.callCount() != 1 {
		t.Errorf("the repository was queried %d time(s), want 1", repo.callCount())
	}
}

func TestGetDiscover_PassesTheViewerThrough(t *testing.T) {
	t.Parallel()

	t.Run("an authenticated viewer reaches the repository", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)
		const viewer = "did:plc:theviewer0000000000000"

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{ViewerDID: viewer}); err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}

		if got := repo.only(t).ViewerDID; got != viewer {
			t.Errorf("the repository received viewer %q, want %q. The viewer DID is what the block "+
				"filter joins on, so losing it shows a blocked author's posts to the person who "+
				"blocked them — on the most-visited page in the product", got, viewer)
		}
	})

	t.Run("an anonymous caller reaches the repository with no viewer", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{}); err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}

		if got := repo.only(t).ViewerDID; got != "" {
			t.Errorf("the repository received viewer %q for an unauthenticated request; a fabricated "+
				"viewer would apply somebody else's block list to a stranger's feed", got)
		}
	})

	t.Run("the cursor reaches the repository unchanged", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)
		cursor := "1717171717::at://did:plc:x/social.coves.community.post/3k"

		if _, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{Cursor: &cursor}); err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}

		got := repo.only(t).Cursor
		if got == nil || *got != cursor {
			t.Errorf("the repository received cursor %v, want %q. A dropped cursor restarts every "+
				"page at the top, so scrolling loops over the same posts", got, cursor)
		}
	})
}

func TestGetDiscover_ShapesTheResponse(t *testing.T) {
	t.Parallel()

	t.Run("the repository's rows and cursor are what the caller gets", func(t *testing.T) {
		t.Parallel()
		cursor := "the-next-page"
		rows := []*discover.FeedViewPost{
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/1"}},
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/2"}},
		}
		repo := &recordingDiscoverRepo{feed: rows, cursor: &cursor}
		service := discover.NewDiscoverService(repo)

		response, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{})
		if err != nil {
			t.Fatalf("GetDiscover: %v", err)
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
			t.Errorf("the response cursor is %v, want %q", response.Cursor, cursor)
		}
	})

	t.Run("an instance with no posts yet is an empty feed, not an error", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{}
		service := discover.NewDiscoverService(repo)

		response, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{})
		if err != nil {
			t.Fatalf("GetDiscover: %v", err)
		}
		if len(response.Feed) != 0 {
			t.Errorf("the response carried %d posts, want none", len(response.Feed))
		}
		if response.Cursor != nil {
			t.Errorf("the response carried cursor %q with no rows", *response.Cursor)
		}
	})

	t.Run("a repository failure is propagated, not flattened", func(t *testing.T) {
		t.Parallel()
		repo := &recordingDiscoverRepo{err: errRepositoryUnavailable}
		service := discover.NewDiscoverService(repo)

		response, err := service.GetDiscover(context.Background(), discover.GetDiscoverRequest{})

		if !errors.Is(err, errRepositoryUnavailable) {
			t.Fatalf("err = %v, want the repository's failure to survive wrapping", err)
		}
		if response != nil {
			t.Errorf("a failed query still returned a response (%+v), which a caller would render as "+
				"an empty instance", response)
		}
		if discover.IsValidationError(err) {
			t.Errorf("a database failure was mapped to a validation error, which the handler answers " +
				"as a 400")
		}
	})
}

// TestGetDiscover_FeedViewPostExposesItsPost covers the accessor the handler
// uses to hang viewer vote state on each row. A nil return would render every
// post as unvoted for a user who has voted.
func TestGetDiscover_FeedViewPostExposesItsPost(t *testing.T) {
	t.Parallel()

	post := &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/1"}
	item := &discover.FeedViewPost{Post: post}

	if item.GetPost() != post {
		t.Errorf("GetPost returned %v, want the wrapped post", item.GetPost())
	}
}
