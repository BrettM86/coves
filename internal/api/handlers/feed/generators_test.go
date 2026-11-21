package feed

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"Coves/internal/core/feedgen"
	"Coves/internal/validation"
)

// describeFeedGenerator and getFeedGenerators over a registry built in the
// test from the real builtin constructors. No infrastructure. T0.

const (
	discoverHotFeedURI = "at://did:web:instance.test/social.coves.feed.generator/discover-hot"
	extraFeedURI       = "at://did:web:instance.test/social.coves.feed.generator/extra"
	otherInstanceHot   = "at://did:web:other.test/social.coves.feed.generator/hot"
)

// fourBuiltinRegistry registers the instance's four production builtins.
func fourBuiltinRegistry() *feedgen.Registry {
	return feedgen.NewRegistry(testInstanceDID,
		feedgen.HotBuiltin(nil),
		feedgen.NewBuiltin(nil),
		feedgen.TopBuiltin(nil),
		feedgen.DiscoverHotBuiltin(nil),
	)
}

// topAndExtraRegistry registers Top and a hand-built builtin with no
// description.
func topAndExtraRegistry() *feedgen.Registry {
	return feedgen.NewRegistry(testInstanceDID,
		feedgen.TopBuiltin(nil),
		feedgen.Builtin{RKey: "extra", DisplayName: "Extra", Kind: feedgen.KindPosts, Scopes: []string{"community"}},
	)
}

func describeFeedGenerator(t *testing.T, registry *feedgen.Registry) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.describeFeedGenerator", nil)
	NewFeedGeneratorsHandler(registry).HandleDescribeFeedGenerator(rec, req)
	return rec
}

// getFeedGenerators sends rawQuery verbatim so empty and repeated values reach
// the handler exactly as written.
func getFeedGenerators(t *testing.T, registry *feedgen.Registry, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	target := "/xrpc/social.coves.feed.getFeedGenerators"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	NewFeedGeneratorsHandler(registry).HandleGetFeedGenerators(rec, req)
	return rec
}

// feedsQuery is the query string with one repeated feeds value per uri.
func feedsQuery(uris ...string) string {
	v := url.Values{}
	for _, uri := range uris {
		v.Add("feeds", uri)
	}
	return v.Encode()
}

// requireValidOutput requires a 200 whose body validates against ref.
func requireValidOutput(t *testing.T, rec *httptest.ResponseRecorder, ref string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %q)", rec.Code, rec.Body.String())
	}
	validator, err := validation.NewEmbeddedLexiconValidator(true)
	if err != nil {
		t.Fatalf("building lexicon validator: %v", err)
	}
	if err := validator.ValidateObject(rec.Body.Bytes(), ref); err != nil {
		t.Fatalf("body does not validate as %s: %v (body: %q)", ref, err, rec.Body.String())
	}
}

func TestDescribeFeedGenerator_ListsRegisteredFeeds(t *testing.T) {
	tests := []struct {
		name     string
		registry *feedgen.Registry
		wantURIs []string
	}{
		{
			name:     "four production builtins",
			registry: fourBuiltinRegistry(),
			wantURIs: []string{
				"at://did:web:instance.test/social.coves.feed.generator/hot",
				"at://did:web:instance.test/social.coves.feed.generator/new",
				"at://did:web:instance.test/social.coves.feed.generator/top",
				"at://did:web:instance.test/social.coves.feed.generator/discover-hot",
			},
		},
		{
			name:     "registry-driven: top and extra only",
			registry: topAndExtraRegistry(),
			wantURIs: []string{
				"at://did:web:instance.test/social.coves.feed.generator/top",
				"at://did:web:instance.test/social.coves.feed.generator/extra",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := describeFeedGenerator(t, tc.registry)
			requireValidOutput(t, rec, "social.coves.feed.describeFeedGenerator#output")

			var body struct {
				DID   string `json:"did"`
				Feeds []struct {
					URI string `json:"uri"`
				} `json:"feeds"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding body: %v (body: %q)", err, rec.Body.String())
			}
			if body.DID != "did:web:instance.test" {
				t.Errorf("did = %q, want did:web:instance.test", body.DID)
			}
			gotURIs := make([]string, 0, len(body.Feeds))
			for _, f := range body.Feeds {
				gotURIs = append(gotURIs, f.URI)
			}
			if !reflect.DeepEqual(gotURIs, tc.wantURIs) {
				t.Errorf("feeds = %v, want %v", gotURIs, tc.wantURIs)
			}
		})
	}
}

// decodeGeneratorViews requires a getFeedGenerators body whose feeds key is a
// JSON array (present, not null) and returns each view keyed by uri.
func decodeGeneratorViews(t *testing.T, rec *httptest.ResponseRecorder) map[string]map[string]any {
	t.Helper()
	var raw struct {
		Feeds json.RawMessage `json:"feeds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding body: %v (body: %q)", err, rec.Body.String())
	}
	if len(raw.Feeds) == 0 || raw.Feeds[0] != '[' {
		t.Fatalf("feeds = %s, want a JSON array (body: %q)", raw.Feeds, rec.Body.String())
	}
	var views []map[string]any
	if err := json.Unmarshal(raw.Feeds, &views); err != nil {
		t.Fatalf("decoding feeds: %v", err)
	}
	byURI := make(map[string]map[string]any, len(views))
	for _, view := range views {
		uri, _ := view["uri"].(string)
		if _, dup := byURI[uri]; dup {
			t.Fatalf("feeds lists %q twice (body: %s)", uri, rec.Body.String())
		}
		byURI[uri] = view
	}
	return byURI
}

// Expected generatorView JSON objects. reflect.DeepEqual against these
// requires acceptsInteractions to be present as false, and description to be
// absent where it is not listed.
var (
	hotView = map[string]any{
		"uri":                 "at://did:web:instance.test/social.coves.feed.generator/hot",
		"did":                 "did:web:instance.test",
		"displayName":         "Hot",
		"description":         "Popular posts ranked by score, with newer posts favored over older ones",
		"kind":                "posts",
		"scopes":              []any{"home", "community"},
		"acceptsInteractions": false,
	}
	newView = map[string]any{
		"uri":                 "at://did:web:instance.test/social.coves.feed.generator/new",
		"did":                 "did:web:instance.test",
		"displayName":         "New",
		"description":         "The newest posts first",
		"kind":                "posts",
		"scopes":              []any{"home", "community", "all"},
		"acceptsInteractions": false,
	}
	topView = map[string]any{
		"uri":                 "at://did:web:instance.test/social.coves.feed.generator/top",
		"did":                 "did:web:instance.test",
		"displayName":         "Top",
		"description":         "The highest-scoring posts within the chosen timeframe",
		"kind":                "posts",
		"scopes":              []any{"home", "community", "all"},
		"acceptsInteractions": false,
	}
	discoverHotView = map[string]any{
		"uri":                 "at://did:web:instance.test/social.coves.feed.generator/discover-hot",
		"did":                 "did:web:instance.test",
		"displayName":         "Discover",
		"description":         "Popular posts from every community, ranked by score with newer posts favored and communities mixed",
		"kind":                "posts",
		"scopes":              []any{"all"},
		"acceptsInteractions": false,
	}
	extraView = map[string]any{
		"uri":                 "at://did:web:instance.test/social.coves.feed.generator/extra",
		"did":                 "did:web:instance.test",
		"displayName":         "Extra",
		"kind":                "posts",
		"scopes":              []any{"community"},
		"acceptsInteractions": false,
	}
)

func TestGetFeedGenerators_Views(t *testing.T) {
	tests := []struct {
		name      string
		registry  *feedgen.Registry
		requested []string
		want      []map[string]any
	}{
		{
			name:      "all four builtins",
			registry:  fourBuiltinRegistry(),
			requested: []string{hotFeedURI, newFeedURI, topFeedURI, discoverHotFeedURI},
			want:      []map[string]any{hotView, newView, topView, discoverHotView},
		},
		{
			name:      "unknown uri is omitted",
			registry:  fourBuiltinRegistry(),
			requested: []string{hotFeedURI, unknownFeedURI},
			want:      []map[string]any{hotView},
		},
		{
			name:      "only another instance's uri gives an empty array",
			registry:  fourBuiltinRegistry(),
			requested: []string{otherInstanceHot},
			want:      []map[string]any{},
		},
		{
			name:      "builtin without a description omits the key",
			registry:  topAndExtraRegistry(),
			requested: []string{extraFeedURI},
			want:      []map[string]any{extraView},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := getFeedGenerators(t, tc.registry, feedsQuery(tc.requested...))
			requireValidOutput(t, rec, "social.coves.feed.getFeedGenerators#output")
			got := decodeGeneratorViews(t, rec)

			if len(got) != len(tc.want) {
				t.Errorf("got %d views, want %d (body: %s)", len(got), len(tc.want), rec.Body.String())
			}
			for _, want := range tc.want {
				uri := want["uri"].(string)
				view, ok := got[uri]
				if !ok {
					t.Errorf("no view for %s (body: %s)", uri, rec.Body.String())
					continue
				}
				if _, present := view["acceptsInteractions"]; !present {
					t.Errorf("view %s has no acceptsInteractions key; it must be emitted even when false", uri)
				}
				if !reflect.DeepEqual(view, want) {
					t.Errorf("view %s =\n  %v\nwant\n  %v", uri, view, want)
				}
			}
		})
	}
}

// getFeedGenerators answers in request order, not registration order.
func TestGetFeedGenerators_RequestOrder(t *testing.T) {
	rec := getFeedGenerators(t, fourBuiltinRegistry(), feedsQuery(topFeedURI, hotFeedURI))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Feeds []struct {
			URI string `json:"uri"`
		} `json:"feeds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v (body: %q)", err, rec.Body.String())
	}
	got := make([]string, 0, len(body.Feeds))
	for _, view := range body.Feeds {
		got = append(got, view.URI)
	}
	if want := []string{topFeedURI, hotFeedURI}; !reflect.DeepEqual(got, want) {
		t.Errorf("feed uris = %v, want %v", got, want)
	}
}

func TestGetFeedGenerators_Validation(t *testing.T) {
	twentySixURIs := make([]string, 0, 26)
	for i := range 26 {
		twentySixURIs = append(twentySixURIs, fmt.Sprintf("at://did:web:instance.test/social.coves.feed.generator/u%02d", i))
	}

	tests := []struct {
		name     string
		rawQuery string
	}{
		{name: "no feeds param", rawQuery: ""},
		{name: "empty feeds value", rawQuery: "feeds="},
		{name: "26 values", rawQuery: feedsQuery(twentySixURIs...)},
		{name: "not a uri", rawQuery: feedsQuery("not-a-uri")},
		{name: "https url", rawQuery: feedsQuery("https://example.com/x")},
		{name: "bare at scheme", rawQuery: feedsQuery("at://")},
		{name: "second value invalid", rawQuery: feedsQuery(hotFeedURI, "not-a-uri")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := getFeedGenerators(t, fourBuiltinRegistry(), tc.rawQuery)
			requireXRPCError(t, rec, http.StatusBadRequest, "InvalidRequest")
		})
	}

	t.Run("25 values is accepted", func(t *testing.T) {
		uris := []string{hotFeedURI}
		for i := range 24 {
			uris = append(uris, fmt.Sprintf("at://did:web:instance.test/social.coves.feed.generator/u%02d", i))
		}
		rec := getFeedGenerators(t, fourBuiltinRegistry(), feedsQuery(uris...))
		requireValidOutput(t, rec, "social.coves.feed.getFeedGenerators#output")
		got := decodeGeneratorViews(t, rec)
		if len(got) != 1 || !reflect.DeepEqual(got[hotFeedURI], hotView) {
			t.Errorf("views = %v, want only %v", got, hotView)
		}
	})
}
