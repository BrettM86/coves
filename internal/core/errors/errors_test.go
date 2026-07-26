// Package errors_test is an external test package on purpose: it imports the
// domain packages that alias these types, which an in-package test could not
// do without creating an import cycle.
package errors_test

import (
	"Coves/internal/core/aggregators"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
	"errors"
	"fmt"
	"testing"

	coreerrors "Coves/internal/core/errors"
)

// domainValidationErrors returns one validation error per domain package that
// aliases coreerrors.ValidationError.
func domainValidationErrors() map[string]error {
	return map[string]error{
		"posts":          posts.NewValidationError("uris", "must not be empty"),
		"communities":    communities.NewValidationError("name", "must not be empty"),
		"aggregators":    aggregators.NewValidationError("aggregatorDid", "is required"),
		"communityFeeds": communityFeeds.NewValidationError("limit", "out of range"),
		"discover":       discover.NewValidationError("cursor", "is malformed"),
		"timeline":       timeline.NewValidationError("cursor", "is malformed"),
	}
}

// This is the property the whole consolidation exists for. Without it, a
// handler has to enumerate every domain it might hear from, and the one it
// forgets returns 500 instead of 400.
//
// Converting any alias back to a locally-defined struct still compiles and
// still passes that domain's own tests — only this test notices.
func TestSingleErrorsAsMatchesEveryAliasingDomain(t *testing.T) {
	for domain, err := range domainValidationErrors() {
		t.Run(domain, func(t *testing.T) {
			var validationErr *coreerrors.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("%s validation error is not a *coreerrors.ValidationError; "+
					"the type alias has been broken", domain)
			}
			if validationErr.Field == "" {
				t.Error("Field did not survive the conversion")
			}
		})
	}
}

// Service layers add context with %w on the way up. A predicate that stops
// matching once wrapped silently downgrades a 400 to a 500.
func TestValidationErrorSurvivesWrapping(t *testing.T) {
	for domain, err := range domainValidationErrors() {
		t.Run(domain, func(t *testing.T) {
			wrapped := fmt.Errorf("creating record: %w", fmt.Errorf("validating input: %w", err))
			if !coreerrors.IsValidationError(wrapped) {
				t.Errorf("%s validation error stopped matching after two wraps", domain)
			}
		})
	}
}

// Each domain's own predicate must agree with the shared one, in both
// directions — that agreement is what lets a boundary mapper use either.
func TestDomainPredicatesAgreeWithShared(t *testing.T) {
	predicates := map[string]func(error) bool{
		"posts":          posts.IsValidationError,
		"communities":    communities.IsValidationError,
		"aggregators":    aggregators.IsValidationError,
		"communityFeeds": communityFeeds.IsValidationError,
		"discover":       discover.IsValidationError,
		"timeline":       timeline.IsValidationError,
	}

	for domain, err := range domainValidationErrors() {
		for predicateDomain, matches := range predicates {
			t.Run(domain+"/"+predicateDomain, func(t *testing.T) {
				if !matches(err) {
					t.Errorf("%s.IsValidationError rejected a %s validation error; "+
						"cross-domain matching is the point of the shared type",
						predicateDomain, domain)
				}
			})
		}
	}

	// And the negative direction: an unrelated error must not be swept up.
	for domain, matches := range predicates {
		t.Run(domain+"/negative", func(t *testing.T) {
			if matches(errors.New("some unrelated failure")) {
				t.Errorf("%s.IsValidationError matched an unrelated error", domain)
			}
			if matches(nil) {
				t.Errorf("%s.IsValidationError matched nil", domain)
			}
		})
	}
}

// NewValidationErrorFrom exists so a sentinel from a package like
// internal/validation stays matchable after being given field context.
func TestNewValidationErrorFromKeepsCauseMatchable(t *testing.T) {
	cause := errors.New("handle contains an invalid character")
	err := coreerrors.NewValidationErrorFrom("handle", cause)

	if !errors.Is(err, cause) {
		t.Error("the underlying cause is not reachable via errors.Is")
	}
	if !coreerrors.IsValidationError(err) {
		t.Error("the result is not recognised as a validation error")
	}
	if got, want := err.Error(), "handle: "+cause.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// Still true once a service layer wraps it.
	wrapped := fmt.Errorf("updating profile: %w", err)
	if !errors.Is(wrapped, cause) {
		t.Error("the cause stopped being reachable after wrapping")
	}
}

func TestValidationErrorMessageFormat(t *testing.T) {
	err := coreerrors.NewValidationError("communityDid", "is required")
	if got, want := err.Error(), "communityDid: is required"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// A nil Err must not make Unwrap misbehave — ValidationError is usually
// constructed without a cause.
func TestValidationErrorUnwrapWithoutCause(t *testing.T) {
	err := coreerrors.NewValidationError("field", "message")
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Errorf("Unwrap() = %v, want nil when there is no cause", unwrapped)
	}
	if errors.Is(err, errors.New("anything")) {
		t.Error("a causeless ValidationError should not match arbitrary errors")
	}
}

// The Is method bridges the typed error to the shared sentinel, so callers can
// ask "is this missing?" without knowing which resource was missing.
func TestNotFoundErrorMatchesSentinel(t *testing.T) {
	err := coreerrors.NewNotFoundError("post", "at://did:plc:abc/social.coves.community.post/xyz")

	if !errors.Is(err, coreerrors.ErrNotFound) {
		t.Error("NotFoundError does not match ErrNotFound")
	}
	if !coreerrors.IsNotFound(err) {
		t.Error("IsNotFound rejected a NotFoundError")
	}
	if !errors.Is(fmt.Errorf("loading post: %w", err), coreerrors.ErrNotFound) {
		t.Error("NotFoundError stopped matching ErrNotFound after wrapping")
	}
	if errors.Is(err, coreerrors.ErrAlreadyExists) {
		t.Error("NotFoundError must not match unrelated sentinels")
	}
	if got, want := err.Error(),
		"post not found: at://did:plc:abc/social.coves.community.post/xyz"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestConflictErrorMatchesSentinel(t *testing.T) {
	err := coreerrors.NewConflictError("community", "handle", "!golang@coves.social")

	if !errors.Is(err, coreerrors.ErrAlreadyExists) {
		t.Error("ConflictError does not match ErrAlreadyExists")
	}
	if !coreerrors.IsConflict(err) {
		t.Error("IsConflict rejected a ConflictError")
	}
	if errors.Is(err, coreerrors.ErrNotFound) {
		t.Error("ConflictError must not match unrelated sentinels")
	}
}

// posts keeps its own ErrNotFound sentinel alongside the shared typed error,
// so its predicate has to bridge both. This pins that it does — an
// unbridged predicate would map a typed not-found to a 500.
func TestPostsIsNotFoundBridgesBothRepresentations(t *testing.T) {
	cases := map[string]error{
		"shared typed error": posts.NewNotFoundError("post", "at://example"),
		"package sentinel":   posts.ErrNotFound,
		"community sentinel": posts.ErrCommunityNotFound,
		"wrapped sentinel":   fmt.Errorf("fetching: %w", posts.ErrNotFound),
		"wrapped typed":      fmt.Errorf("fetching: %w", posts.NewNotFoundError("post", "at://example")),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if !posts.IsNotFound(err) {
				t.Errorf("posts.IsNotFound rejected %s", name)
			}
		})
	}

	if posts.IsNotFound(errors.New("unrelated")) {
		t.Error("posts.IsNotFound matched an unrelated error")
	}
}

// The predicates are called on error paths where nil is possible; none of them
// may panic.
func TestPredicatesHandleNil(t *testing.T) {
	if coreerrors.IsValidationError(nil) {
		t.Error("IsValidationError(nil) should be false")
	}
	if coreerrors.IsNotFound(nil) {
		t.Error("IsNotFound(nil) should be false")
	}
	if coreerrors.IsConflict(nil) {
		t.Error("IsConflict(nil) should be false")
	}
	if posts.IsNotFound(nil) {
		t.Error("posts.IsNotFound(nil) should be false")
	}
	if posts.IsConflict(nil) {
		t.Error("posts.IsConflict(nil) should be false")
	}
}
