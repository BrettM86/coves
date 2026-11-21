package feed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"Coves/internal/core/discover"
)

// TestGetFeed_DiscoverUnavailable: a discover-hot generator that reports
// Discover unavailable, alone or joined with the caller's deadline, is a 503
// DiscoverUnavailable with Retry-After 30 and no feed.
func TestGetFeed_DiscoverUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"discover unavailable", discover.ErrDiscoverUnavailable},
		{"caller deadline joined with discover unavailable", errors.Join(context.DeadlineExceeded, discover.ErrDiscoverUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFeedFixture(t, twoPostGenerator(), nil)
			f.discoverHot.err = tc.err

			rec := f.get(t, values("scope", "all", "limit", "5"), viewerOneDID)

			requireXRPCError(t, rec, http.StatusServiceUnavailable, "DiscoverUnavailable")
			if got := rec.Header().Get("Retry-After"); got != "30" {
				t.Errorf("Retry-After = %q, want %q", got, "30")
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not a JSON object: %v (body: %q)", err, rec.Body.String())
			}
			if _, ok := body["feed"]; ok {
				t.Errorf("503 body has a feed key: %s", rec.Body.String())
			}
		})
	}

	t.Run("plain caller cancellation is not DiscoverUnavailable", func(t *testing.T) {
		f := newFeedFixture(t, twoPostGenerator(), nil)
		f.discoverHot.err = context.Canceled

		rec := f.get(t, values("scope", "all", "limit", "5"), viewerOneDID)

		if got := len(f.discoverHot.received()); got != 1 {
			t.Fatalf("discover-hot generator received %d requests, want 1 (status %d, body %s)", got, rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusServiceUnavailable {
			t.Errorf("status = 503 for plain cancellation, want not 503 (body: %s)", rec.Body.String())
		}
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error == "DiscoverUnavailable" {
			t.Errorf("error = DiscoverUnavailable for plain cancellation (body: %s)", rec.Body.String())
		}
		if rec.Header().Get("Retry-After") != "" {
			t.Errorf("Retry-After = %q for plain cancellation, want none", rec.Header().Get("Retry-After"))
		}
	})
}

// TestGetFeed_ScopeAllAnonymous: scope all needs no sign-in and is served by
// discover-hot when feed is omitted.
func TestGetFeed_ScopeAllAnonymous(t *testing.T) {
	f := newFeedFixture(t, twoPostGenerator(), nil)

	body := decodeFeed(t, f.get(t, values("scope", "all", "limit", "5"), ""))

	uris := make([]string, 0, len(body.Feed))
	for _, item := range body.Feed {
		uris = append(uris, item.Post.URI)
	}
	if want := []string{"at://did:plc:author/social.coves.community.post/discover1"}; !reflect.DeepEqual(uris, want) {
		t.Errorf("feed URIs = %v, want %v", uris, want)
	}
	requests := f.discoverHot.received()
	if len(requests) != 1 || requests[0].ViewerDID != "" {
		t.Errorf("discover-hot generator requests = %+v, want one anonymous request", requests)
	}
}
