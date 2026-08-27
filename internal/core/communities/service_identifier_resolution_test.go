//go:build integration

package communities_test

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// How a community may be addressed, and what happens when it is addressed
// badly.
//
// ResolveCommunityIdentifier and GetCommunity are the front door of every
// community-scoped API: social.coves.community.get, subscribe, block, and every
// post or comment that names its community take a client-supplied string and
// come through here. Four syntaxes are accepted — a DID, an atProto handle, an
// @-prefixed handle, and the Coves-specific scoped form !name@instance — and
// each of them normalises differently before it reaches a database lookup.
//
// # WHY THESE TESTS NEED INFRASTRUCTURE
//
// Resolution is not a string transformation with a lookup bolted on: three of
// the four forms END in a repository query, and the interesting failures are
// about which query gets made. A scoped identifier whose domain is not
// lowercased looks up a handle that cannot exist; an @-prefix left on the
// string does the same. Both are invisible to a test that stubs the repository,
// because a stub answers whatever key it is given. So the assertions are made
// against a real community row created through the real provisioning path, and
// the identifier under test is derived from what that row actually holds.
//
// The negative cases share the same service for the same reason: "rejected" has
// to mean "rejected before any lookup" or "rejected because the lookup found
// nothing", and only a live repository can tell those apart.

// alternatingCase upper-cases every other character, producing the kind of
// domain a client types by hand rather than one any code would generate.
func alternatingCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i%2 == 0 {
			b.WriteString(strings.ToUpper(string(r)))
		} else {
			b.WriteString(strings.ToLower(string(r)))
		}
	}
	return b.String()
}

// newResolvableCommunity provisions one real community and returns the service
// that owns it, so the identifiers under test can be built from the row rather
// than from a guess at the naming convention.
//
// HostedByDID is deliberately not set on the request: CreateCommunity overwrites
// it from the instance configuration precisely so a client cannot claim someone
// else's instance hosts a community, and passing one here would suggest the
// field is an input.
func newResolvableCommunity(t *testing.T, prefix string) (communities.Service, *communities.Community) {
	t.Helper()

	service, _, _ := newCommunityService(t)

	name := testkit.UniqueIDWithPrefix(t, prefix)
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)

	community, err := service.CreateCommunity(context.Background(), communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            "Identifier Resolution",
		Description:            "a community addressed every way the resolver accepts",
		Visibility:             "public",
		CreatedByDID:           "did:plc:identifierresolution",
		AllowExternalDiscovery: true,
	})
	require.NoError(t, err)
	return service, community
}

func TestService_ResolveIdentifierAcceptsEveryAddressableForm(t *testing.T) {
	t.Parallel()

	service, community := newResolvableCommunity(t, "res")
	ctx := context.Background()
	name := community.Name

	// Every one of these must land on the same DID. Case is the recurring theme:
	// DNS is case-insensitive and users type community names the way they read
	// them, so a resolver that lower-cases only some of the string produces a
	// lookup key no row can match — a 404 on a community that plainly exists.
	for _, testCase := range []struct {
		name       string
		identifier string
	}{
		{"DID", community.DID},
		{"canonical handle", community.Handle},
		{"canonical handle with an upper-cased domain", "c-" + name + "." + strings.ToUpper(instanceDomain)},
		{"at-identifier", "@" + community.Handle},
		{"at-identifier with an upper-cased domain", "@c-" + name + "." + strings.ToUpper(instanceDomain)},
		{"scoped identifier", "!" + name + "@" + instanceDomain},
		{"scoped identifier with an upper-cased domain", "!" + name + "@" + strings.ToUpper(instanceDomain)},
		{"scoped identifier with an alternating-case domain", "!" + name + "@" + alternatingCase(instanceDomain)},
		{"scoped identifier with an upper-cased name", "!" + strings.ToUpper(name) + "@" + instanceDomain},
		{"scoped identifier without the ! prefix", name + "@" + instanceDomain},
		{"scoped identifier without the ! prefix, upper-cased domain", name + "@" + strings.ToUpper(instanceDomain)},
		{"bare name", name},
		{"bare name upper-cased", strings.ToUpper(name)},
		{"handle padded with whitespace", "  " + community.Handle + "  "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			did, err := service.ResolveCommunityIdentifier(ctx, testCase.identifier)
			require.NoErrorf(t, err, "%q addresses a community that exists", testCase.identifier)
			assert.Equal(t, community.DID, did)
		})
	}
}

// TestService_ResolveIdentifierRejectsMalformedScopedIdentifiers is the ONLY
// coverage in the repository for a malformed !name@instance string, and the
// reason it is worth saying so out loud is that the scoped form is the one
// syntax atProto does not define for us.
//
// A DID and a handle both arrive pre-validated by convention — clients copy
// them from records — but !gardening@coves.social is a Coves invention that
// users TYPE, so every one of these four shapes is something a real client will
// send. Each has a distinct consequence if it is not caught here:
//
//   - no @ at all: SplitN would hand the whole string to the name, and the
//     lookup would be for a handle built from a domain the user never named;
//   - an empty name: the constructed handle is "c-.<domain>", which is a
//     syntactically valid DNS name and therefore a lookup that could one day
//     match something;
//   - a foreign instance: the identifier is well-formed and refers to a
//     community this AppView does not host, and silently rewriting it to a
//     LOCAL handle would resolve one instance's community to another's;
//   - a well-formed local name with nothing behind it: the only one of the four
//     that is allowed to reach the database, and it must come back not-found
//     rather than as a validation failure, because the two mean different
//     things to the handler that maps them to HTTP status codes.
func TestService_ResolveIdentifierRejectsMalformedScopedIdentifiers(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	t.Run("rejects scoped identifier without @ symbol", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "!testcommunity")
		require.Error(t, err)
		assert.ErrorContains(t, err, "must include @ symbol")
		assert.True(t, communities.IsValidationError(err),
			"a syntax failure must classify as validation, not as a missing community")
	})

	t.Run("rejects scoped identifier with empty name", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "!@"+instanceDomain)
		require.Error(t, err)
		assert.ErrorContains(t, err, "community name cannot be empty")
		assert.True(t, communities.IsValidationError(err),
			"an empty name must be refused before it becomes the handle c-.%s", instanceDomain)
	})

	t.Run("unknown remote origin is a miss, not a validation failure", func(t *testing.T) {
		// A remote origin is no longer refused up front: it is looked up against
		// the (name, origin) pairs this AppView has indexed, and a pair it has
		// never seen is a community it does not know about.
		_, err := service.ResolveCommunityIdentifier(ctx, "!testcommunity@wrong.social")
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found")
		assert.ErrorContains(t, err, "testcommunity@wrong.social")
		assert.True(t, communities.IsNotFound(err))
		assert.False(t, communities.IsValidationError(err))
	})

	t.Run("rejects non-existent community in scoped format", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "!nonexistent@"+instanceDomain)
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found")
		assert.True(t, communities.IsNotFound(err),
			"a well-formed identifier for a community that does not exist is a 404, not a 400")
	})
}

// TestService_ResolveScopedIdentifierRejectsNamesThatAreNotDNSLabels covers the
// other half of scoped-identifier parsing: the name and the domain both become
// part of a handle, so anything that is not a legal DNS label has to be refused
// before it is concatenated into one.
//
// The characters in the table are not hypothetical. The name is interpolated
// into "c-%s.%s" and the result is used as a database lookup key and echoed
// back in error messages, so the validation here is what keeps a caller from
// steering either with punctuation.
func TestService_ResolveScopedIdentifierRejectsNamesThatAreNotDNSLabels(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	for _, testCase := range []struct {
		name       string
		identifier string
		message    string
	}{
		{"rejects special characters in name", "!<script>@" + instanceDomain, "valid DNS label"},
		{"rejects name with spaces", "!test community@" + instanceDomain, "valid DNS label"},
		{"rejects name starting with hyphen", "!-test@" + instanceDomain, "valid DNS label"},
		{"rejects name ending with hyphen", "!test-@" + instanceDomain, "valid DNS label"},
		{"rejects name exceeding 63 characters", "!" + strings.Repeat("a", 64) + "@" + instanceDomain, "valid DNS label"},
		// The original of this case built its over-long name out of NUL bytes,
		// which tested control characters by accident and the length limit not at
		// all. Both are worth having, so both are here.
		{"rejects name of control characters", "!" + string(make([]byte, 64)) + "@" + instanceDomain, "valid DNS label"},
		{"rejects invalid domain format", "!test@not a domain", "invalid"},
		{"rejects domain with special characters", "!test@coves$.social", "invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, testCase.identifier)
			require.Error(t, err)
			assert.ErrorContains(t, err, testCase.message)
			assert.True(t, communities.IsValidationError(err),
				"%q must be refused as malformed input, not looked up", testCase.identifier)
		})
	}

	// The accepting direction of the same rule: a hyphen and a digit are legal
	// in a DNS label, so these names must survive validation and fail only
	// because no such community exists. If one of them ever comes back as a
	// validation error, the label rule has been tightened past what a user is
	// allowed to name a community.
	for _, testCase := range []struct {
		name       string
		identifier string
	}{
		{"accepts valid name with hyphens", "!test-community@" + instanceDomain},
		{"accepts valid name with numbers", "!test123@" + instanceDomain},
		{"accepts a single-character name", "!a@" + instanceDomain},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := service.ResolveCommunityIdentifier(ctx, testCase.identifier)
			require.Error(t, err, "no community by this name was created")
			assert.True(t, communities.IsNotFound(err),
				"%q is a legal identifier and must reach the lookup; got %v", testCase.identifier, err)
		})
	}
}

func TestService_ResolveIdentifierRejectsUnaddressableInput(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	t.Run("rejects empty identifier", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "")
		require.Error(t, err)
		assert.True(t, errors.Is(err, communities.ErrInvalidInput))
	})

	t.Run("rejects whitespace-only identifier", func(t *testing.T) {
		// Trimming happens before the emptiness check, which is the only reason
		// this is not a lookup for the handle "   ".
		_, err := service.ResolveCommunityIdentifier(ctx, "   ")
		require.Error(t, err)
		assert.True(t, errors.Is(err, communities.ErrInvalidInput))
	})

	t.Run("dotless identifier is a bare local name", func(t *testing.T) {
		// A dotless string is a community on THIS instance (/c/nodots), so it
		// is looked up as c-nodots.<instance> and misses as a 404, not a 400.
		_, err := service.ResolveCommunityIdentifier(ctx, "nodots")
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found for scoped identifier !nodots@"+instanceDomain)
		assert.True(t, communities.IsNotFound(err))
		assert.False(t, communities.IsValidationError(err))
	})

	t.Run("bare name that is not a DNS label is still refused", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "no dots")
		require.Error(t, err)
		assert.True(t, communities.IsValidationError(err))
	})

	t.Run("rejects non-existent DID", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "did:plc:nonexistent123")
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found")
		assert.True(t, communities.IsNotFound(err))
	})

	t.Run("rejects malformed DID", func(t *testing.T) {
		// Worth being precise about WHY this fails: the resolver does not parse
		// DID syntax at all, it just looks up anything starting with "did:". So a
		// malformed DID is refused only because nothing in the index matches it,
		// and the day a row is written with a malformed DID this would resolve.
		_, err := service.ResolveCommunityIdentifier(ctx, "did:invalid")
		require.Error(t, err)
		assert.True(t, communities.IsNotFound(err))
	})

	t.Run("rejects non-existent canonical handle", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "c-nonexistent."+instanceDomain)
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found")
		assert.True(t, communities.IsNotFound(err))
	})
}

// TestService_ResolveIdentifierErrorsNameWhatFailed pins the error TEXT, not
// just the error's classification.
//
// These strings reach a client: the XRPC layer maps a not-found to 404 and puts
// the message in the response body, and "community not found" without the
// identifier that was not found is a support ticket. The scoped case carries
// the stronger requirement — it must name the instance that WOULD have worked,
// because a user who typed the wrong instance has no other way to discover
// which one is right.
func TestService_ResolveIdentifierErrorsNameWhatFailed(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	t.Run("DID error includes the DID", func(t *testing.T) {
		const missingDID = "did:plc:nonexistent999"
		_, err := service.ResolveCommunityIdentifier(ctx, missingDID)
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found")
		assert.ErrorContains(t, err, missingDID)
	})

	t.Run("handle error includes the handle", func(t *testing.T) {
		missingHandle := "c-nonexistent." + instanceDomain
		_, err := service.ResolveCommunityIdentifier(ctx, missingHandle)
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found")
		assert.ErrorContains(t, err, missingHandle)
	})

	t.Run("scoped error includes the name and origin looked up", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, "!test@wrong.instance")
		require.Error(t, err)
		assert.ErrorContains(t, err, "community not found")
		assert.ErrorContains(t, err, "!test@wrong.instance")
	})
}

// TestService_GetCommunityAcceptsEveryAddressableForm covers the same four
// syntaxes through the other entry point.
//
// GetCommunity is not a wrapper around ResolveCommunityIdentifier — it has its
// own copy of the branch (service.go: DID, scoped, @-prefix, handle), and only
// the scoped branch delegates. That duplication is the reason both are tested:
// a fix applied to one and not the other is the likeliest way for the two to
// start disagreeing about what a valid identifier is.
func TestService_GetCommunityAcceptsEveryAddressableForm(t *testing.T) {
	t.Parallel()

	service, community := newResolvableCommunity(t, "get")
	ctx := context.Background()

	for _, testCase := range []struct {
		name       string
		identifier string
	}{
		{"DID", community.DID},
		{"canonical handle", community.Handle},
		{"upper-cased handle", strings.ToUpper(community.Handle)},
		{"at-identifier", "@" + community.Handle},
		{"scoped identifier", "!" + community.Name + "@" + instanceDomain},
		{"handle padded with whitespace", "  " + community.Handle + "  "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := service.GetCommunity(ctx, testCase.identifier)
			require.NoErrorf(t, err, "%q addresses a community that exists", testCase.identifier)
			assert.Equal(t, community.DID, result.DID)
			assert.Equal(t, community.Handle, result.Handle,
				"whichever way it was addressed, the row that comes back is the same one")
		})
	}
}

func TestService_GetCommunityRejectsUnaddressableInput(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	t.Run("rejects empty identifier", func(t *testing.T) {
		_, err := service.GetCommunity(ctx, "")
		require.Error(t, err)
		assert.True(t, errors.Is(err, communities.ErrInvalidInput))
	})

	t.Run("bare name that names no local community is a miss", func(t *testing.T) {
		// A bare name is a community on this instance (/c/nodots), so the
		// failure is a 404 that carries what the client sent.
		_, err := service.GetCommunity(ctx, "nodots")
		require.Error(t, err)
		assert.True(t, communities.IsNotFound(err))
		assert.ErrorContains(t, err, `"nodots"`)
	})

	// The error text carries the identifier AS THE CLIENT SENT IT — GetCommunity
	// keeps the original around for exactly this — so a user who pasted a
	// trailing space or the wrong instance can see what the server received.
	t.Run("DID error includes the identifier", func(t *testing.T) {
		const missingDID = "did:plc:nonexistent789"
		_, err := service.GetCommunity(ctx, missingDID)
		require.Error(t, err)
		assert.ErrorContains(t, err, missingDID)
	})

	t.Run("handle error includes the identifier", func(t *testing.T) {
		missingHandle := "c-nonexistent." + instanceDomain
		_, err := service.GetCommunity(ctx, missingHandle)
		require.Error(t, err)
		assert.ErrorContains(t, err, missingHandle)
	})

	t.Run("scoped error includes the identifier", func(t *testing.T) {
		missingScoped := "!nonexistent@" + instanceDomain
		_, err := service.GetCommunity(ctx, missingScoped)
		require.Error(t, err)
		assert.ErrorContains(t, err, missingScoped)
	})
}

// TestCommunity_GetDisplayHandle covers the inverse of scoped resolution: the
// stored atProto handle turned back into the !name@instance string a client
// shows a user.
//
// It is a pure function on the domain type and touches nothing out of process;
// it lives in this file because it is the other half of the round trip the
// tests above make, and splitting the two would leave neither able to say
// whether they agree. The multi-part-TLD case is the one with teeth: naive
// splitting on the first dot after the name would render
// "c-gaming.coves.co.uk" as "!gaming@coves.co", which resolves to nothing.
func TestCommunity_GetDisplayHandle(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		handle  string
		display string
	}{
		{"standard two-part domain", "c-gardening.coves.social", "!gardening@coves.social"},
		{"multi-part TLD", "c-gaming.coves.co.uk", "!gaming@coves.co.uk"},
		{"subdomain instance", "c-test.dev.coves.social", "!test@dev.coves.social"},
		{"single part name", "c-a.coves.social", "!a@coves.social"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			community := &communities.Community{Handle: testCase.handle}
			assert.Equal(t, testCase.display, community.GetDisplayHandle())
		})
	}

	// Anything it cannot decompose comes back unchanged rather than as a
	// half-built string. A display handle is rendered directly into a client, so
	// the failure mode to avoid is not an error — it is "!@coves.social" shown
	// to a user as if it were a name.
	t.Run("returns malformed input unchanged", func(t *testing.T) {
		for _, handle := range []string{
			"nodots",
			"single.dot",
			"",
			"c-",
			"c-.",
			"c-.coves.social",
			"c-nodot",
		} {
			community := &communities.Community{Handle: handle}
			assert.Equalf(t, handle, community.GetDisplayHandle(),
				"a handle it cannot decompose must be returned as-is: %q", handle)
		}
	})
}

// bridgedCommunity indexes a row the way the community consumer does for a
// Tidepool-bridged community: an unprefixed DNS handle on the bridge's domain
// and a validated origin naming the platform it was bridged FROM. It is written
// through the repository rather than CreateCommunity because provisioning only
// ever produces local rows.
func bridgedCommunity(t *testing.T, repo communities.Repository, name, origin string) *communities.Community {
	t.Helper()
	suffix := testkit.UniqueID(t)
	community := &communities.Community{
		DID:          "did:plc:bridged" + suffix,
		Handle:       name + "-" + suffix + ".lemmy-world.tdpl.io",
		Name:         name,
		DisplayName:  "Bridged " + name,
		OwnerDID:     "did:plc:bridged" + suffix,
		CreatedByDID: "did:plc:bridgedcreator",
		HostedByDID:  "did:web:tdpl.io",
		PDSURL:       "https://pds.tdpl.io",
		Visibility:   "public",
		Origin:       origin,
	}
	created, err := repo.Create(context.Background(), community)
	require.NoError(t, err)
	return created
}

// TestService_ResolveIdentifierByNameAndOrigin covers the remote half of the
// scoped form: comicstrips@lemmy.world is not a handle this AppView can rebuild,
// so it has to be found by the (name, origin) pair the consumer stored.
func TestService_ResolveIdentifierByNameAndOrigin(t *testing.T) {
	t.Parallel()

	service, repo, _ := newCommunityService(t)
	ctx := context.Background()

	name := testkit.UniqueIDWithPrefix(t, "cs")
	bridged := bridgedCommunity(t, repo, name, "lemmy.world")

	for _, testCase := range []struct {
		name       string
		identifier string
	}{
		{"name@origin", name + "@lemmy.world"},
		{"!name@origin", "!" + name + "@lemmy.world"},
		{"name@origin with mixed-case origin", name + "@Lemmy.World"},
		{"name@origin with an upper-cased name", strings.ToUpper(name) + "@lemmy.world"},
	} {
		t.Run("resolves "+testCase.name, func(t *testing.T) {
			did, err := service.ResolveCommunityIdentifier(ctx, testCase.identifier)
			require.NoError(t, err)
			assert.Equal(t, bridged.DID, did)

			got, err := service.GetCommunity(ctx, testCase.identifier)
			require.NoError(t, err)
			assert.Equal(t, bridged.DID, got.DID)
		})
	}

	t.Run("the same name at an origin nobody indexed is a miss", func(t *testing.T) {
		_, err := service.ResolveCommunityIdentifier(ctx, name+"@lemmy.ml")
		require.Error(t, err)
		assert.True(t, communities.IsNotFound(err))
	})

	t.Run("the bridged handle still resolves on its own", func(t *testing.T) {
		did, err := service.ResolveCommunityIdentifier(ctx, bridged.Handle)
		require.NoError(t, err)
		assert.Equal(t, bridged.DID, did)
	})

	t.Run("a second row with the same pair makes the identifier ambiguous", func(t *testing.T) {
		bridgedCommunity(t, repo, name, "lemmy.world")

		_, err := service.ResolveCommunityIdentifier(ctx, name+"@lemmy.world")
		require.Error(t, err)
		assert.True(t, communities.IsAmbiguous(err),
			"two rows carrying the same (name, origin) must be reported, not resolved to whichever came first")
		assert.False(t, communities.IsNotFound(err))

		_, err = service.GetCommunity(ctx, name+"@lemmy.world")
		require.Error(t, err)
		assert.True(t, communities.IsAmbiguous(err))

		// Both rows stay reachable by the forms that ARE unique.
		did, err := service.ResolveCommunityIdentifier(ctx, bridged.Handle)
		require.NoError(t, err)
		assert.Equal(t, bridged.DID, did)
	})
}
