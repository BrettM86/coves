package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"Coves/internal/core/feedgen"
	postgresRepo "Coves/internal/db/postgres"
)

// fakeDiscoverHot stands in for the asserted Discover repository.
type fakeDiscoverHot struct{}

func (*fakeDiscoverHot) HydratedPage(context.Context, feedgen.SkeletonRequest) (feedgen.HydratedPostPage, error) {
	return feedgen.HydratedPostPage{}, nil
}

// TestProductionBuiltins_RegistersPostAndCommentBuiltins pins the production
// generator set: dropping one would stop its record being published and make
// getFeed or getComments answer UnknownFeed for it.
func TestProductionBuiltins_RegistersPostAndCommentBuiltins(t *testing.T) {
	discoverHot := &fakeDiscoverHot{}
	builtins := productionBuiltins(nil, postgresRepo.WithAccountRestrictionAuthority("did:web:instance.test"), discoverHot)

	want := []string{"hot", "new", "top", "discover-hot", "comments-hot", "comments-top", "comments-new"}
	if got := builtinRKeys(builtins); !slices.Equal(got, want) {
		t.Fatalf("productionBuiltins rkeys = %v, want %v", got, want)
	}
	for _, builtin := range builtins {
		rkey := builtin.RKey
		if strings.HasPrefix(rkey, "comments-") {
			if builtin.Kind != feedgen.KindComments {
				t.Errorf("builtin %q kind = %q, want %q", rkey, builtin.Kind, feedgen.KindComments)
			}
			if builtin.CommentGenerator == nil {
				t.Errorf("builtin %q has no comment generator", rkey)
			}
			continue
		}
		if builtin.Kind != feedgen.KindPosts {
			t.Errorf("builtin %q kind = %q, want %q", rkey, builtin.Kind, feedgen.KindPosts)
		}
		if rkey == "discover-hot" {
			if builtin.HydratedGenerator != feedgen.HydratedPostGenerator(discoverHot) {
				t.Errorf("discover-hot HydratedGenerator = %v, want the passed Discover Hot generator", builtin.HydratedGenerator)
			}
			continue
		}
		if builtin.Generator == nil {
			t.Errorf("builtin %q has no generator", rkey)
		}
		if (rkey == "new" || rkey == "top") && !slices.Contains(builtin.Scopes, "all") {
			t.Errorf("builtin %q scopes = %v, want them to include all", rkey, builtin.Scopes)
		}
	}

	registry := feedgen.NewRegistry("did:web:instance.test", builtins...)
	for _, rkey := range want {
		if _, ok := registry.Lookup(registry.URI(rkey)); !ok {
			t.Errorf("registry has no builtin at %s", registry.URI(rkey))
		}
	}
}

func builtinRKeys(builtins []feedgen.Builtin) []string {
	rkeys := make([]string, 0, len(builtins))
	for _, b := range builtins {
		rkeys = append(rkeys, b.RKey)
	}
	return rkeys
}
