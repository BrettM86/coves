package routes

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The production Caddyfile no longer sends every coves.social request to the
// AppView: the SvelteKit frontend is the catch-all, and the AppView is served
// from an explicit `@appview` path allowlist. That allowlist is hand-written,
// so a newly registered Go route outside /xrpc is silently answered by the
// frontend's 404 page until someone remembers to add it. This test is the
// reminder: it walks the real router and checks every route the AppView is
// meant to serve on the apex against the matcher, in both directions.

// caddyfileAllowlistExceptions are registered routes that deliberately do NOT
// route to the AppView on coves.social, each answered by an earlier handle in
// the same site block. Adding a path here is a routing decision, not a way to
// make the test pass — the Caddyfile comment on `@appview` lists the same set.
var caddyfileAllowlistExceptions = map[string]string{
	"/":     "apex is split by Accept: text/html -> frontend, otherwise Tidepool's instance actor",
	"/img/": "301 to the media hostname img.coves.social; never served from the apex",
	"/.well-known/": "served by the static file_server from ./static (DID document, app links); " +
		"the AppView's own /.well-known handlers are shadowed there by design",
}

// allowlistEntriesRegisteredElsewhere are `@appview` entries whose routes live
// outside this package (cmd/server/routes.go), so the walk below cannot see
// them. They are asserted present in the Caddyfile rather than skipped, so
// removing one from Caddy still fails here.
var allowlistEntriesRegisteredElsewhere = []string{"/health", "/health/*"}

func TestCaddyfile_AppViewAllowlistCoversEveryNonXRPCRoute(t *testing.T) {
	patterns := caddyAppViewPathPatterns(t)

	for _, entry := range allowlistEntriesRegisteredElsewhere {
		if !patterns[entry] {
			t.Errorf("Caddyfile @appview is missing %q (registered in cmd/server/routes.go)", entry)
		}
	}

	matched := map[string]bool{}
	err := chi.Walk(theRouter().mux, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/xrpc/") {
			return nil // /xrpc/* is the first allowlist entry; nothing to check per route
		}
		for exception := range caddyfileAllowlistExceptions {
			// "/" is the apex and matches exactly; every other entry is a
			// directory prefix. (A bare HasPrefix on "/" would swallow
			// every route and turn this test into a no-op.)
			if route == exception || (exception != "/" && strings.HasPrefix(route, exception)) {
				return nil
			}
		}
		pattern, ok := matchingCaddyPattern(patterns, route)
		if !ok {
			t.Errorf("route %s is registered on the AppView but no Caddyfile @appview path matches it — "+
				"the frontend catch-all would answer it with a 404; add it to the @appview matcher", route)
			return nil
		}
		matched[pattern] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}

	// The other direction: an allowlist entry nothing registers is a typo or a
	// route that was deleted from Go without touching Caddy.
	elsewhere := map[string]bool{}
	for _, entry := range allowlistEntriesRegisteredElsewhere {
		elsewhere[entry] = true
	}
	for pattern := range patterns {
		if pattern == "/xrpc/*" || elsewhere[pattern] || matched[pattern] {
			continue
		}
		t.Errorf("Caddyfile @appview path %q matches no registered AppView route", pattern)
	}
}

// caddyAppViewPathPatterns parses the `@appview { path ... }` named matcher out
// of the production Caddyfile at the repo root. It is a deliberately literal
// reader of that one block — a real Caddyfile parser would be overkill, and
// the block's shape is fixed by the comment above it.
func caddyAppViewPathPatterns(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "Caddyfile"))
	if err != nil {
		t.Fatalf("opening the production Caddyfile: %v", err)
	}
	defer f.Close()

	patterns := map[string]bool{}
	inBlock := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "@appview {":
			inBlock = true
		case inBlock && line == "}":
			return patterns
		case inBlock && strings.HasPrefix(line, "path "):
			for _, p := range strings.Fields(line)[1:] {
				patterns[p] = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the Caddyfile: %v", err)
	}
	t.Fatal("Caddyfile has no `@appview {` named matcher in the coves.social block")
	return nil
}

// matchingCaddyPattern applies Caddy's path-matcher rule: an entry is an exact
// match unless it ends in `*`, in which case it is a prefix match.
func matchingCaddyPattern(patterns map[string]bool, route string) (string, bool) {
	for pattern := range patterns {
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(route, strings.TrimSuffix(pattern, "*")) {
				return pattern, true
			}
		} else if pattern == route {
			return pattern, true
		}
	}
	return "", false
}
