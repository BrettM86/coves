package feedgen_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/feedgen"
	"Coves/internal/validation"
)

// B1: each builtin's record is a valid social.coves.feed.generator record
// naming the instance DID, its at-uri is built from the instance DID and
// resolves back to it, and the registry's builtin set (what the ensure step
// publishes) carries it. Hot serves home and community; New and Top also serve
// all; Discover Hot serves only all.
func TestRegistry_HotRecordAndURI(t *testing.T) {
	generator := &scriptedGenerator{}
	discoverHot := &scriptedHydratedGenerator{}
	registry := feedgen.NewRegistry(testInstanceDID,
		feedgen.HotBuiltin(generator), feedgen.NewBuiltin(generator), feedgen.TopBuiltin(generator),
		feedgen.DiscoverHotBuiltin(discoverHot))

	validator, err := validation.NewEmbeddedLexiconValidator(true)
	if err != nil {
		t.Fatalf("NewEmbeddedLexiconValidator: %v", err)
	}

	builtinsByRKey := map[string]feedgen.Builtin{}
	for _, b := range registry.Builtins() {
		builtinsByRKey[b.RKey] = b
	}

	for _, tc := range []struct {
		rkey         string
		wantURI      string
		wantDisplay  string
		wantScopes   []string
		wantHydrated bool
	}{
		{"hot", "at://did:web:instance.test/social.coves.feed.generator/hot", "Hot", []string{"home", "community"}, false},
		{"new", "at://did:web:instance.test/social.coves.feed.generator/new", "New", []string{"home", "community", "all"}, false},
		{"top", "at://did:web:instance.test/social.coves.feed.generator/top", "Top", []string{"home", "community", "all"}, false},
		{"discover-hot", "at://did:web:instance.test/social.coves.feed.generator/discover-hot", "Discover", []string{"all"}, true},
	} {
		t.Run(tc.rkey, func(t *testing.T) {
			builtin, ok := builtinsByRKey[tc.rkey]
			if !ok {
				t.Fatalf("Builtins() has no %q builtin; got rkeys %v", tc.rkey, builtinRKeys(registry.Builtins()))
			}
			if tc.wantHydrated {
				if builtin.HydratedGenerator != feedgen.HydratedPostGenerator(discoverHot) {
					t.Errorf("%s builtin does not carry the hydrated generator it was built with", tc.rkey)
				}
				if builtin.Generator != nil {
					t.Errorf("%s builtin also carries a skeleton generator; want only the hydrated one", tc.rkey)
				}
			} else if builtin.Generator != feedgen.PostGenerator(generator) {
				t.Errorf("%s builtin does not carry the generator it was built with", tc.rkey)
			}
			assertBuiltinRecordAndURI(t, registry, validator, builtin, tc.rkey, tc.wantURI, tc.wantDisplay, "posts", tc.wantScopes)
		})
	}
}

// stubCommentGenerator is a comment generator the registry only carries.
type stubCommentGenerator struct{ name string }

func (*stubCommentGenerator) CommentSkeleton(context.Context, feedgen.CommentSkeletonRequest) (feedgen.CommentSkeletonPage, error) {
	return feedgen.CommentSkeletonPage{}, nil
}

// B9: the three comment builtins declare kind comments and exactly the
// thread scope, their records validate as social.coves.feed.generator records
// naming the registry DID, and each carries the comment generator it was built
// with (and no post generator).
func TestRegistry_CommentBuiltinsRecordAndURI(t *testing.T) {
	hot := &stubCommentGenerator{name: "hot"}
	top := &stubCommentGenerator{name: "top"}
	newest := &stubCommentGenerator{name: "new"}
	registry := feedgen.NewRegistry(testInstanceDID,
		feedgen.CommentsHotBuiltin(hot), feedgen.CommentsTopBuiltin(top), feedgen.CommentsNewBuiltin(newest))

	validator, err := validation.NewEmbeddedLexiconValidator(true)
	if err != nil {
		t.Fatalf("NewEmbeddedLexiconValidator: %v", err)
	}

	builtinsByRKey := map[string]feedgen.Builtin{}
	for _, b := range registry.Builtins() {
		builtinsByRKey[b.RKey] = b
	}

	for _, tc := range []struct {
		rkey        string
		wantURI     string
		wantDisplay string
		wantGen     feedgen.CommentGenerator
	}{
		{"comments-hot", "at://did:web:instance.test/social.coves.feed.generator/comments-hot", "Hot", hot},
		{"comments-top", "at://did:web:instance.test/social.coves.feed.generator/comments-top", "Top", top},
		{"comments-new", "at://did:web:instance.test/social.coves.feed.generator/comments-new", "New", newest},
	} {
		t.Run(tc.rkey, func(t *testing.T) {
			builtin, ok := builtinsByRKey[tc.rkey]
			if !ok {
				t.Fatalf("Builtins() has no %q builtin; got rkeys %v", tc.rkey, builtinRKeys(registry.Builtins()))
			}
			if builtin.CommentGenerator != tc.wantGen {
				t.Errorf("%s builtin does not carry the comment generator it was built with", tc.rkey)
			}
			if builtin.Generator != nil || builtin.HydratedGenerator != nil {
				t.Errorf("%s builtin carries a post generator; want only the comment generator", tc.rkey)
			}
			assertBuiltinRecordAndURI(t, registry, validator, builtin, tc.rkey, tc.wantURI, tc.wantDisplay, "comments", []string{"thread"})
		})
	}
}

func builtinRKeys(builtins []feedgen.Builtin) []string {
	rkeys := make([]string, 0, len(builtins))
	for _, b := range builtins {
		rkeys = append(rkeys, b.RKey)
	}
	return rkeys
}

func assertBuiltinRecordAndURI(t *testing.T, registry *feedgen.Registry, validator *validation.LexiconValidator, builtin feedgen.Builtin, rkey, wantURI, wantDisplay, wantKind string, wantScopes []string) {
	t.Helper()

	if got := registry.URI(rkey); got != wantURI {
		t.Errorf("URI(%q) = %q, want %q", rkey, got, wantURI)
	}
	looked, ok := registry.Lookup(wantURI)
	if !ok {
		t.Errorf("Lookup(%q) found nothing", wantURI)
	} else if looked.RKey != rkey {
		t.Errorf("Lookup(%q) = builtin %q, want %q", wantURI, looked.RKey, rkey)
	}
	if builtin.Description == "" || strings.Contains(builtin.Description, "\n") {
		t.Errorf("%s description = %q, want one non-empty line", rkey, builtin.Description)
	}

	createdAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	record := registry.Record(builtin, createdAt)
	if record == nil {
		t.Fatalf("Record(%s) returned nil", rkey)
	}
	if err := validator.ValidateRecord(record, "social.coves.feed.generator"); err != nil {
		t.Errorf("Record(%s) does not validate as social.coves.feed.generator: %v", rkey, err)
	}

	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var decoded struct {
		Type                string   `json:"$type"`
		DID                 string   `json:"did"`
		DisplayName         string   `json:"displayName"`
		Kind                string   `json:"kind"`
		Scopes              []string `json:"scopes"`
		AcceptsInteractions bool     `json:"acceptsInteractions"`
		CreatedAt           string   `json:"createdAt"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode record %s: %v", raw, err)
	}
	if decoded.Type != "social.coves.feed.generator" {
		t.Errorf("$type = %q, want %q", decoded.Type, "social.coves.feed.generator")
	}
	if decoded.DID != "did:web:instance.test" {
		t.Errorf("did = %q, want %q", decoded.DID, "did:web:instance.test")
	}
	if decoded.DisplayName != wantDisplay {
		t.Errorf("displayName = %q, want %q", decoded.DisplayName, wantDisplay)
	}
	if decoded.Kind != wantKind {
		t.Errorf("kind = %q, want %q", decoded.Kind, wantKind)
	}
	if !reflect.DeepEqual(decoded.Scopes, wantScopes) {
		t.Errorf("scopes = %v, want %v", decoded.Scopes, wantScopes)
	}
	if decoded.AcceptsInteractions {
		t.Errorf("acceptsInteractions = true, want false")
	}
	parsed, err := time.Parse(time.RFC3339, decoded.CreatedAt)
	if err != nil {
		t.Errorf("createdAt %q is not RFC3339: %v", decoded.CreatedAt, err)
	} else if !parsed.Equal(createdAt) {
		t.Errorf("createdAt = %s, want %s", parsed, createdAt)
	}
}
