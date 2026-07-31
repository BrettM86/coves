// The timeline service: the layer between "GET
// /xrpc/social.coves.feed.getTimeline" and the subscription-scoped join that
// builds a signed-in user's home feed.
//
// # WHAT IS PROVEN HERE AND WHAT IS PROVEN NEXT DOOR
//
// timeline_feed_test.go drives this service, its Postgres repository and the
// HTTP handler together against a real database, and it is the only coverage
// anywhere of the subscription fan-out — the join that decides a post from an
// unsubscribed community must not appear. That test cannot see the decisions
// made here, because every one of them is a decision about what the repository
// is ASKED, and it asks a real one.
//
// This file covers those:
//
//   - the defaults (hot, 15, and "day" when the sort is top), which are what a
//     request with no query parameters actually means;
//   - the bounds, which nothing downstream re-applies — the limit is
//     interpolated into LIMIT and the sort into ORDER BY;
//   - the identity check, which is the reason this endpoint is the one feed
//     behind RequireAuth;
//   - and the order those happen in, which is observable and worth pinning.
//
// The repository fake records what it was handed rather than answering from a
// fixture: a fake asserted against its own seed would pass with every default
// dropped and every bound removed. External test package, untagged, no
// database and no sockets.
package timeline_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
)

// recordingTimelineRepo logs every request and answers with what the test
// seeded.
type recordingTimelineRepo struct {
	mu sync.Mutex

	feed   []*timeline.FeedViewPost
	cursor *string
	err    error

	requests []timeline.GetTimelineRequest
}

func (r *recordingTimelineRepo) GetTimeline(_ context.Context, req timeline.GetTimelineRequest) ([]*timeline.FeedViewPost, *string, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	if r.err != nil {
		return nil, nil, r.err
	}
	return r.feed, r.cursor, nil
}

// only returns the single request the repository received, failing with the
// full log otherwise — including the case that matters most, where it was
// never called at all.
func (r *recordingTimelineRepo) only(t *testing.T) timeline.GetTimelineRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 {
		t.Fatalf("expected the repository to be asked exactly once, got %d call(s): %+v",
			len(r.requests), r.requests)
	}
	return r.requests[0]
}

func (r *recordingTimelineRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// theSubscriber is the authenticated caller. The timeline is scoped to it, so
// it is the one field of a timeline request that has no default.
const theSubscriber = "did:plc:subscriber0000000000000"

// errRepositoryUnavailable is deliberately not one of the domain's typed
// errors, so "propagated" can be told apart from "mapped".
var errRepositoryUnavailable = errors.New("connection pool exhausted")

// aTimelineRequest is a request that passes validation, so each test changes
// only the field it is about.
func aTimelineRequest() timeline.GetTimelineRequest {
	return timeline.GetTimelineRequest{UserDID: theSubscriber}
}

func TestGetTimeline_DefaultsReachTheRepository(t *testing.T) {
	t.Parallel()

	t.Run("an unspecified sort becomes hot", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)

		if _, err := service.GetTimeline(context.Background(), aTimelineRequest()); err != nil {
			t.Fatalf("GetTimeline: %v", err)
		}

		if got := repo.only(t).Sort; got != "hot" {
			t.Errorf("the repository was asked to sort by %q, want %q — this is what a signed-in "+
				"user sees on opening the app", got, "hot")
		}
	})

	t.Run("an unspecified limit becomes 15", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)

		if _, err := service.GetTimeline(context.Background(), aTimelineRequest()); err != nil {
			t.Fatalf("GetTimeline: %v", err)
		}

		if got := repo.only(t).Limit; got != 15 {
			t.Errorf("the repository was asked for %d posts, want the default 15. A zero reaches SQL "+
				"as LIMIT 0 and serves an empty timeline to a user who subscribes to things", got)
		}
	})

	t.Run("a negative limit becomes 15 rather than reaching SQL", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)
		req := aTimelineRequest()
		req.Limit = -20

		if _, err := service.GetTimeline(context.Background(), req); err != nil {
			t.Fatalf("GetTimeline: %v", err)
		}

		if got := repo.only(t).Limit; got != 15 {
			t.Errorf("the repository was asked for %d posts; a negative LIMIT is a SQL error", got)
		}
	})

	t.Run("sort=top with no timeframe becomes a day", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)
		req := aTimelineRequest()
		req.Sort = "top"

		if _, err := service.GetTimeline(context.Background(), req); err != nil {
			t.Fatalf("GetTimeline: %v", err)
		}

		if got := repo.only(t).Timeframe; got != "day" {
			t.Errorf("top with no timeframe reached the repository as %q, want %q: with no window, "+
				"\"top\" becomes top of all time and the timeline stops changing", got, "day")
		}
	})

	t.Run("the other sorts get no timeframe", func(t *testing.T) {
		t.Parallel()
		for _, sort := range []string{"hot", "new"} {
			t.Run(sort, func(t *testing.T) {
				t.Parallel()
				repo := &recordingTimelineRepo{}
				service := timeline.NewTimelineService(repo)
				req := aTimelineRequest()
				req.Sort = sort

				if _, err := service.GetTimeline(context.Background(), req); err != nil {
					t.Fatalf("GetTimeline: %v", err)
				}

				if got := repo.only(t).Timeframe; got != "" {
					t.Errorf("sort=%s reached the repository with timeframe %q, want none", sort, got)
				}
			})
		}
	})
}

func TestGetTimeline_RejectsWhatTheRepositoryCannotBound(t *testing.T) {
	t.Parallel()

	t.Run("a limit above 50 is refused, not clamped", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)
		req := aTimelineRequest()
		req.Limit = 51

		_, err := service.GetTimeline(context.Background(), req)

		if !timeline.IsValidationError(err) {
			t.Fatalf("err = %v, want a validation error", err)
		}
		if repo.callCount() != 0 {
			t.Errorf("the repository was asked %d time(s) for an out-of-range page", repo.callCount())
		}
	})

	t.Run("a limit of exactly 50 is allowed", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)
		req := aTimelineRequest()
		req.Limit = 50

		if _, err := service.GetTimeline(context.Background(), req); err != nil {
			t.Fatalf("a limit of 50 was refused (%v); the boundary is inclusive", err)
		}
		if got := repo.only(t).Limit; got != 50 {
			t.Errorf("the repository was asked for %d, want 50 unchanged", got)
		}
	})

	t.Run("an unknown sort is refused", func(t *testing.T) {
		t.Parallel()
		for _, sort := range []string{"controversial", "Hot", "best"} {
			t.Run(sort, func(t *testing.T) {
				t.Parallel()
				repo := &recordingTimelineRepo{}
				service := timeline.NewTimelineService(repo)
				req := aTimelineRequest()
				req.Sort = sort

				_, err := service.GetTimeline(context.Background(), req)

				if !timeline.IsValidationError(err) {
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
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)
		req := aTimelineRequest()
		req.Sort = "hot"
		req.Timeframe = "fortnight"

		_, err := service.GetTimeline(context.Background(), req)

		if !timeline.IsValidationError(err) {
			t.Fatalf("err = %v, want a validation error", err)
		}
	})

	t.Run("every documented timeframe is accepted", func(t *testing.T) {
		t.Parallel()
		for _, timeframe := range []string{"hour", "day", "week", "month", "year", "all"} {
			t.Run(timeframe, func(t *testing.T) {
				t.Parallel()
				repo := &recordingTimelineRepo{}
				service := timeline.NewTimelineService(repo)
				req := aTimelineRequest()
				req.Sort = "top"
				req.Timeframe = timeframe

				if _, err := service.GetTimeline(context.Background(), req); err != nil {
					t.Fatalf("timeframe=%q was refused: %v", timeframe, err)
				}
				if got := repo.only(t).Timeframe; got != timeframe {
					t.Errorf("timeframe reached the repository as %q, want %q unchanged", got, timeframe)
				}
			})
		}
	})
}

// TestGetTimeline_RequiresAnIdentity is the check that makes this endpoint
// different from every other feed.
//
// A timeline with no user is not an empty timeline — it is a query with no
// subscription filter, and depending on how the repository composed its join
// that could be either nothing or everything. Refusing before the query is the
// only safe answer, and it is defence in depth behind RequireAuth on the route
// (see internal/api/routes/registration_test.go, which asserts that guard).
func TestGetTimeline_RequiresAnIdentity(t *testing.T) {
	t.Parallel()

	repo := &recordingTimelineRepo{}
	service := timeline.NewTimelineService(repo)

	response, err := service.GetTimeline(context.Background(), timeline.GetTimelineRequest{})

	if !errors.Is(err, timeline.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if response != nil {
		t.Errorf("an unauthenticated request still produced a response: %+v", response)
	}
	if repo.callCount() != 0 {
		t.Errorf("the repository was queried %d time(s) with no user DID. The subscription filter is "+
			"the only thing scoping this query, so a query with no user is a query with no scope",
			repo.callCount())
	}
}

// TestGetTimeline_ValidationHappensBeforeTheIdentityCheck pins the order of the
// two guards, because it is observable from outside and neither ordering is
// obviously right.
//
// As written, an anonymous caller who also sends a bad sort is told the sort is
// bad rather than that they are not signed in. Nothing is disclosed by that —
// the validation rules are public and the response carries no data — but it is
// the kind of thing that gets reversed during a refactor and then quietly
// changes what clients see. If the order is ever deliberately swapped, this
// test is the place to record the new decision.
func TestGetTimeline_ValidationHappensBeforeTheIdentityCheck(t *testing.T) {
	t.Parallel()

	repo := &recordingTimelineRepo{}
	service := timeline.NewTimelineService(repo)

	_, err := service.GetTimeline(context.Background(), timeline.GetTimelineRequest{Sort: "nonsense"})

	if !timeline.IsValidationError(err) {
		t.Errorf("err = %v, want the validation error; validation runs first (service.go:22 before "+
			"service.go:27)", err)
	}
	if errors.Is(err, timeline.ErrUnauthorized) {
		t.Errorf("err = %v: the identity check now runs first. That is a defensible change — but it "+
			"is a change, and clients sending both problems at once will see a different answer", err)
	}
	if repo.callCount() != 0 {
		t.Errorf("the repository was queried despite both guards failing")
	}
}

func TestGetTimeline_PassesTheCallerThrough(t *testing.T) {
	t.Parallel()

	t.Run("the user DID reaches the repository", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)

		if _, err := service.GetTimeline(context.Background(), aTimelineRequest()); err != nil {
			t.Fatalf("GetTimeline: %v", err)
		}

		if got := repo.only(t).UserDID; got != theSubscriber {
			t.Errorf("the repository received user %q, want %q. This DID is what the subscription "+
				"join filters on: the wrong one serves somebody else's home feed", got, theSubscriber)
		}
	})

	t.Run("the cursor reaches the repository unchanged", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)
		cursor := "1717171717::at://did:plc:x/social.coves.community.post/3k"
		req := aTimelineRequest()
		req.Cursor = &cursor

		if _, err := service.GetTimeline(context.Background(), req); err != nil {
			t.Fatalf("GetTimeline: %v", err)
		}

		got := repo.only(t).Cursor
		if got == nil || *got != cursor {
			t.Errorf("the repository received cursor %v, want %q. A dropped cursor restarts every "+
				"page at the top of the timeline, so scrolling loops", got, cursor)
		}
	})
}

func TestGetTimeline_ShapesTheResponse(t *testing.T) {
	t.Parallel()

	t.Run("the repository's rows and cursor are what the caller gets", func(t *testing.T) {
		t.Parallel()
		cursor := "the-next-page"
		rows := []*timeline.FeedViewPost{
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/1"}},
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/2"}},
		}
		repo := &recordingTimelineRepo{feed: rows, cursor: &cursor}
		service := timeline.NewTimelineService(repo)

		response, err := service.GetTimeline(context.Background(), aTimelineRequest())
		if err != nil {
			t.Fatalf("GetTimeline: %v", err)
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

	t.Run("a user who subscribes to nothing gets an empty feed, not an error", func(t *testing.T) {
		t.Parallel()
		repo := &recordingTimelineRepo{}
		service := timeline.NewTimelineService(repo)

		response, err := service.GetTimeline(context.Background(), aTimelineRequest())
		if err != nil {
			t.Fatalf("GetTimeline: %v", err)
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
		repo := &recordingTimelineRepo{err: errRepositoryUnavailable}
		service := timeline.NewTimelineService(repo)

		response, err := service.GetTimeline(context.Background(), aTimelineRequest())

		if !errors.Is(err, errRepositoryUnavailable) {
			t.Fatalf("err = %v, want the repository's failure to survive wrapping", err)
		}
		if response != nil {
			t.Errorf("a failed query still returned a response (%+v), which a caller would render as "+
				"an empty timeline", response)
		}
		// An outage must not read as "you subscribe to nothing".
		if timeline.IsValidationError(err) || errors.Is(err, timeline.ErrUnauthorized) {
			t.Errorf("a database failure was mapped to %v, which the handler answers as a 4xx", err)
		}
	})
}

// TestGetTimeline_FeedViewPostExposesItsPost covers the accessor the handler
// uses to hang viewer state on each row.
//
// GetPost is how the vote-state enricher reaches into a feed item without the
// enricher knowing which of the three feed packages produced it. A nil return
// would silently skip enrichment for the whole timeline — every post rendered
// as unvoted for a user who has voted.
func TestGetTimeline_FeedViewPostExposesItsPost(t *testing.T) {
	t.Parallel()

	post := &posts.PostView{URI: "at://did:plc:x/social.coves.community.post/1"}
	item := &timeline.FeedViewPost{Post: post}

	if item.GetPost() != post {
		t.Errorf("GetPost returned %v, want the wrapped post", item.GetPost())
	}
}
