package communityFeeds_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/posts"
)

func aSearchRequest() communityFeeds.SearchPostsRequest {
	return communityFeeds.SearchPostsRequest{Query: "rust"}
}

func TestSearchPosts_DefaultsReachTheRepository(t *testing.T) {
	t.Parallel()

	repo := &recordingFeedRepo{}
	communityService := &recordingCommunityService{resolved: theCommunityDID}
	service := newFeedService(repo, communityService)

	if _, err := service.SearchPosts(context.Background(), communityFeeds.SearchPostsRequest{Query: " rust "}); err != nil {
		t.Fatalf("SearchPosts: %v", err)
	}

	seen := repo.onlySearch(t)
	if seen.Query != "rust" {
		t.Errorf("the repository received query %q, want the caller's query trimmed to %q. Leading whitespace must not change the search or its cursor binding", seen.Query, "rust")
	}
	if seen.Sort != "relevance" {
		t.Errorf("the repository was asked to sort search results by %q, want the default %q", seen.Sort, "relevance")
	}
	if seen.Timeframe != "all" {
		t.Errorf("search reached the repository with timeframe %q, want %q. Search has no reason to silently bound top results to a day the way a community feed does", seen.Timeframe, "all")
	}
	if seen.Limit != 15 {
		t.Errorf("the repository was asked for %d search results, want the default 15. A zero would reach SQL as LIMIT 0", seen.Limit)
	}
	if seen.Community != "" {
		t.Errorf("an unscoped search reached the repository with community %q, want no community filter", seen.Community)
	}
	if seen.Cursor != nil {
		t.Errorf("a first-page search reached the repository with cursor %v, want nil", seen.Cursor)
	}
	if asked := communityService.askedFor(); len(asked) != 0 {
		t.Errorf("the community service was asked to resolve %v for an unscoped search; an empty community means search every community, not perform a lookup", asked)
	}
}

func TestSearchPosts_RejectsWhatTheRepositoryCannotBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  communityFeeds.SearchPostsRequest
	}{
		{name: "an empty query", req: communityFeeds.SearchPostsRequest{}},
		{name: "a whitespace-only query", req: communityFeeds.SearchPostsRequest{Query: " \t\n "}},
		{name: "a query above 500 bytes", req: communityFeeds.SearchPostsRequest{Query: strings.Repeat("a", 501)}},
		{name: "hot sort", req: communityFeeds.SearchPostsRequest{Query: "rust", Sort: "hot"}},
		{name: "an unknown sort", req: communityFeeds.SearchPostsRequest{Query: "rust", Sort: "banana"}},
		{name: "an unknown timeframe", req: communityFeeds.SearchPostsRequest{Query: "rust", Timeframe: "fortnight"}},
		{name: "a limit above 50", req: communityFeeds.SearchPostsRequest{Query: "rust", Limit: 51}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := &recordingFeedRepo{}
			service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})

			_, err := service.SearchPosts(context.Background(), test.req)

			if !communityFeeds.IsValidationError(err) {
				t.Fatalf("err = %v, want a validation error", err)
			}
			if repo.searchCallCount() != 0 {
				t.Errorf("the repository was asked to search %d time(s) for a request this layer must reject: %+v", repo.searchCallCount(), test.req)
			}
		})
	}

	for _, limit := range []int{0, -1} {
		limit := limit
		t.Run(fmt.Sprintf("limit %d defaults to 15", limit), func(t *testing.T) {
			t.Parallel()
			repo := &recordingFeedRepo{}
			service := newFeedService(repo, &recordingCommunityService{resolved: theCommunityDID})
			req := aSearchRequest()
			req.Limit = limit

			if _, err := service.SearchPosts(context.Background(), req); err != nil {
				t.Fatalf("SearchPosts with limit %d: %v", limit, err)
			}
			if got := repo.onlySearch(t).Limit; got != 15 {
				t.Errorf("limit %d reached the repository as %d, want the safe default 15", limit, got)
			}
		})
	}
}

func TestSearchPosts_ResolvesTheCommunityOnlyWhenGiven(t *testing.T) {
	t.Parallel()

	t.Run("a community identifier is resolved to its DID", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		communityService := &recordingCommunityService{resolved: theCommunityDID}
		service := newFeedService(repo, communityService)
		req := aSearchRequest()
		req.Community = "gaming"

		if _, err := service.SearchPosts(context.Background(), req); err != nil {
			t.Fatalf("SearchPosts: %v", err)
		}

		if asked := communityService.askedFor(); len(asked) != 1 || asked[0] != "gaming" {
			t.Errorf("the community service was asked to resolve %v, want exactly [%q]", asked, "gaming")
		}
		if got := repo.onlySearch(t).Community; got != theCommunityDID {
			t.Errorf("the repository received community %q, want the resolved DID %q. An unresolved identifier silently matches no posts", got, theCommunityDID)
		}
	})

	t.Run("an empty community keeps the search cross-community", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		communityService := &recordingCommunityService{resolved: theCommunityDID}
		service := newFeedService(repo, communityService)

		if _, err := service.SearchPosts(context.Background(), aSearchRequest()); err != nil {
			t.Fatalf("SearchPosts: %v", err)
		}

		if asked := communityService.askedFor(); len(asked) != 0 {
			t.Errorf("the community service was asked to resolve %v, want no lookup for a cross-community search", asked)
		}
		if got := repo.onlySearch(t).Community; got != "" {
			t.Errorf("the repository received community %q, want an empty cross-community filter", got)
		}
	})
}

func TestSearchPosts_ResolutionFailuresKeepTheirMeaning(t *testing.T) {
	t.Parallel()

	request := aSearchRequest()
	request.Community = "gaming"

	t.Run("a community that does not exist", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{
			err: fmt.Errorf("resolving: %w", communities.ErrCommunityNotFound),
		})

		_, err := service.SearchPosts(context.Background(), request)

		if !errors.Is(err, communityFeeds.ErrCommunityNotFound) {
			t.Errorf("err = %v, want ErrCommunityNotFound", err)
		}
		if repo.searchCallCount() != 0 {
			t.Errorf("the repository was queried for a community that does not exist")
		}
	})

	t.Run("an identifier that is not well formed", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{
			err: communities.NewValidationError("community", "not a handle, DID or scoped identifier"),
		})

		_, err := service.SearchPosts(context.Background(), request)

		if !communityFeeds.IsValidationError(err) {
			t.Errorf("err = %v, want a validation error", err)
		}
		if errors.Is(err, communityFeeds.ErrCommunityNotFound) {
			t.Errorf("a malformed identifier was reported as a missing community")
		}
		if repo.searchCallCount() != 0 {
			t.Errorf("the repository was queried after community validation failed")
		}
	})

	t.Run("a resolver that could not answer", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{err: errRepositoryUnavailable})

		_, err := service.SearchPosts(context.Background(), request)

		if !errors.Is(err, errRepositoryUnavailable) {
			t.Fatalf("err = %v, want the underlying failure to be preserved for errors.Is", err)
		}
		if errors.Is(err, communityFeeds.ErrCommunityNotFound) || communityFeeds.IsValidationError(err) {
			t.Errorf("a resolver outage was mapped to %v, which the handler answers as a 4xx", err)
		}
		if repo.searchCallCount() != 0 {
			t.Errorf("the repository was queried after the resolver failed")
		}
	})
}

func TestSearchPosts_PassesTheCallerThrough(t *testing.T) {
	t.Parallel()

	repo := &recordingFeedRepo{}
	communityService := &recordingCommunityService{resolved: theCommunityDID}
	service := newFeedService(repo, communityService)
	cursor := "signed-search-position"
	req := aSearchRequest()
	req.ViewerDID = "did:plc:searchviewer00000000000"
	req.Cursor = &cursor

	if _, err := service.SearchPosts(context.Background(), req); err != nil {
		t.Fatalf("SearchPosts: %v", err)
	}

	seen := repo.onlySearch(t)
	if seen.ViewerDID != req.ViewerDID {
		t.Errorf("the repository received viewer %q, want %q. Losing it disables author-block filtering and the author-only pending branch", seen.ViewerDID, req.ViewerDID)
	}
	if seen.Cursor != req.Cursor {
		t.Errorf("the repository received cursor %v, want the caller's pointer %v unchanged. Dropping it restarts search at page one", seen.Cursor, req.Cursor)
	}
	if asked := communityService.askedFor(); len(asked) != 0 {
		t.Errorf("passing viewer and cursor through unexpectedly resolved a community: %v", asked)
	}
}

func TestSearchPosts_ShapesTheResponse(t *testing.T) {
	t.Parallel()

	t.Run("the repository's rows and cursor are what the caller gets", func(t *testing.T) {
		t.Parallel()
		cursor := "the-next-search-page"
		rows := []*communityFeeds.FeedViewPost{
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.postv2/1"}},
			{Post: &posts.PostView{URI: "at://did:plc:x/social.coves.community.postv2/2"}},
		}
		repo := &recordingFeedRepo{searchFeed: rows, searchCursor: &cursor}
		service := newFeedService(repo, &recordingCommunityService{})

		response, err := service.SearchPosts(context.Background(), aSearchRequest())
		if err != nil {
			t.Fatalf("SearchPosts: %v", err)
		}
		if response == nil {
			t.Fatalf("SearchPosts returned a nil response for repository rows")
		}
		if len(response.Feed) != len(rows) {
			t.Fatalf("the response carried %d posts, want %d", len(response.Feed), len(rows))
		}
		for index, row := range rows {
			if response.Feed[index] != row {
				t.Errorf("post %d is not the row the repository returned", index)
			}
		}
		if response.Cursor != repo.searchCursor {
			t.Errorf("the response cursor is %v, want the repository's pointer %v", response.Cursor, repo.searchCursor)
		}
	})

	t.Run("nil repository rows become an empty non-nil feed", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{}
		service := newFeedService(repo, &recordingCommunityService{})

		response, err := service.SearchPosts(context.Background(), aSearchRequest())
		if err != nil {
			t.Fatalf("SearchPosts: %v", err)
		}
		if response == nil {
			t.Fatalf("SearchPosts returned a nil response for an empty result")
		}
		if response.Feed == nil {
			t.Errorf("an empty search response carried a nil feed, which JSON renders as null; clients require []")
		}
		if len(response.Feed) != 0 {
			t.Errorf("the response carried %d posts, want none", len(response.Feed))
		}
		if response.Cursor != nil {
			t.Errorf("the response carried cursor %v with no rows", response.Cursor)
		}
	})

	t.Run("a repository failure is wrapped, not flattened", func(t *testing.T) {
		t.Parallel()
		repo := &recordingFeedRepo{searchErr: errRepositoryUnavailable}
		service := newFeedService(repo, &recordingCommunityService{})

		response, err := service.SearchPosts(context.Background(), aSearchRequest())

		if !errors.Is(err, errRepositoryUnavailable) {
			t.Fatalf("err = %v, want the repository's failure preserved for errors.Is", err)
		}
		if response != nil {
			t.Errorf("a failed search still returned a response (%+v), which a caller could serve as an empty feed", response)
		}
	})
}
