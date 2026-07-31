package communities_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
)

// The validation rules that are pure functions of the request, and therefore
// have no business needing a PDS to exercise.
//
// This file is the T0 complement to two T1 files, and the split is deliberate
// rather than tidy:
//
//   - service_create_validation_test.go (integration) owns the NAME rules,
//     because a name that passes has to go on and provision a real account for
//     the accepting direction to mean anything.
//   - service_identifier_resolution_test.go (integration) owns identifier
//     resolution, because three of the four accepted syntaxes end in a database
//     lookup and a stubbed repository would answer whatever key it was handed.
//
// What is left is what neither can reach cheaply: the request fields that are
// rejected without any lookup at all, and the handle regex, which is a pure
// function that happens to hang off the service.
//
// # HOW "THE REQUEST GOT PAST VALIDATION" IS OBSERVED
//
// There is nothing between validateCreateRequest and the provisioner, so
// acceptance has no return value of its own — a request that passes goes
// straight on to ProvisionCommunityAccount. Rather than leave the provisioner
// nil and read the resulting panic (which would also swallow a nil-dereference
// regression inside validation itself, and quietly turn it into a pass), these
// services get a REAL provisioner aimed at a PDS URL that cannot be parsed. It
// fails inside net/url before any socket is opened — microseconds, no DNS, no
// dial, so this stays a T0 test — and it fails with a message no validation rule
// produces. That message is the sentinel: seeing it means validation accepted
// the request and the next step refused it; not seeing it means validation is
// still the thing that answered.

// unparseablePDSURL is a PDS host that net/url rejects, so the provisioner
// returns before it can open a connection. It is a scheme-less "://", which
// url.Parse refuses with "missing protocol scheme".
const unparseablePDSURL = "://"

// reachedProvisioning is the substring ProvisionCommunityAccount wraps every
// account-creation failure in. No validation rule in the package produces it.
const reachedProvisioning = "PDS account creation failed"

// newValidationService wires the fake repository to the fail-fast provisioner
// described above.
func newValidationService(t *testing.T) (communities.Service, *fakeCommunityRepo) {
	t.Helper()
	repo := newFakeCommunityRepo()
	provisioner := communities.NewPDSAccountProvisioner(fakeInstanceDomain, unparseablePDSURL)
	service := communities.NewCommunityServiceWithPDSFactory(
		repo, unparseablePDSURL, fakeInstanceDID, fakeInstanceDomain, provisioner, nil, nil)
	return service, repo
}

// aValidCreateRequest is a request with nothing wrong with it. Each test breaks
// exactly one field, so a failure names the rule under test rather than
// whichever rule happens to run first.
func aValidCreateRequest() communities.CreateCommunityRequest {
	return communities.CreateCommunityRequest{
		Name:                   "gardening",
		DisplayName:            "Gardening",
		Description:            "things that grow",
		Visibility:             "public",
		CreatedByDID:           "did:plc:creator00000000000000",
		AllowExternalDiscovery: true,
	}
}

// requireRejectedBeforeProvisioning runs a create request and asserts it was
// refused by validation — not merely that it failed, but that it stopped before
// the provisioner was asked to create anything.
func requireRejectedBeforeProvisioning(
	t *testing.T, req communities.CreateCommunityRequest,
) error {
	t.Helper()
	service, repo := newValidationService(t)

	community, err := service.CreateCommunity(context.Background(), req)
	require.Error(t, err, "the request was accepted and reached provisioning")
	assert.Nil(t, community)
	assert.NotContains(t, err.Error(), reachedProvisioning,
		"the request reached the PDS. Validation must refuse it first: past this point a name or a "+
			"visibility we do not accept has already cost an account creation on a live PDS")
	assert.Empty(t, repo.calls,
		"a rejected create must not touch the repository: a half-created community is worse than a 400")
	return err
}

func TestCreateCommunity_RejectsADescriptionNoRecordShouldCarry(t *testing.T) {
	t.Parallel()

	t.Run("over three thousand characters", func(t *testing.T) {
		t.Parallel()
		req := aValidCreateRequest()
		req.Description = strings.Repeat("a", 3001)

		err := requireRejectedBeforeProvisioning(t, req)
		var validation *communities.ValidationError
		require.ErrorAs(t, err, &validation,
			"the description is written into an atProto record and echoed on every community page; "+
				"the cap has to be refused here rather than discovered by the PDS")
		assert.Equal(t, "description", validation.Field)
	})

	t.Run("exactly three thousand characters is allowed through", func(t *testing.T) {
		t.Parallel()
		req := aValidCreateRequest()
		req.Description = strings.Repeat("a", 3000)

		// An off-by-one in the cap would reject a description a client is
		// entitled to send, and the client has no way to tell that from a real
		// rule.
		requireAcceptedBy(t, req, "description")
	})

	t.Run("an empty description is allowed", func(t *testing.T) {
		t.Parallel()
		req := aValidCreateRequest()
		req.Description = ""

		requireAcceptedBy(t, req, "description")
	})
}

func TestCreateCommunity_RejectsAVisibilityTheLexiconDoesNotDefine(t *testing.T) {
	t.Parallel()

	for _, visibility := range []string{"secret", "PUBLIC", "hidden", "friends-only", " public"} {
		t.Run(visibility, func(t *testing.T) {
			t.Parallel()
			req := aValidCreateRequest()
			req.Visibility = visibility

			err := requireRejectedBeforeProvisioning(t, req)
			require.ErrorIsf(t, err, communities.ErrInvalidVisibility,
				"%q is not one of public, unlisted or private. Visibility decides who is served the "+
					"community's posts, so an unrecognised value must not be stored and then compared "+
					"against by string equality forever", visibility)
		})
	}

	// The three legal values, plus the empty string that CreateCommunity fills
	// in for a client that omitted the field entirely. None may be rejected for
	// their visibility.
	for _, visibility := range []string{"", "public", "unlisted", "private"} {
		t.Run("accepts "+visibilityName(visibility), func(t *testing.T) {
			t.Parallel()
			req := aValidCreateRequest()
			req.Visibility = visibility

			_, err := validationOutcome(t, req)
			assert.NotErrorIsf(t, err, communities.ErrInvalidVisibility,
				"%q is a value the lexicon defines", visibilityName(visibility))
		})
	}
}

func visibilityName(visibility string) string {
	if visibility == "" {
		return "an omitted visibility, which defaults to public"
	}
	return visibility
}

func TestCreateCommunity_RequiresACreator(t *testing.T) {
	t.Parallel()
	req := aValidCreateRequest()
	req.CreatedByDID = ""

	err := requireRejectedBeforeProvisioning(t, req)
	var validation *communities.ValidationError
	require.ErrorAs(t, err, &validation)
	assert.Equal(t, "createdByDid", validation.Field,
		"createdBy is written into the community's profile record and is the only thing that ties a "+
			"community back to whoever asked for it; a community with no creator cannot be moderated "+
			"or reclaimed")
}

// TestCreateCommunity_DoesNotValidateHostedBy pins the asymmetry between the
// two DID fields on the request, which looks like an oversight and is not.
//
// createdByDid is client-supplied and required. hostedByDid is NOT validated,
// because CreateCommunity overwrites whatever the client sent with the
// instance's own DID before validation runs — a client must not be able to
// claim that some other instance hosts a community this one is creating. The
// observable consequence at T0 is narrow but exact: a request that supplies a
// foreign hostedByDid and omits createdByDid is rejected for the creator, never
// for the host.
func TestCreateCommunity_DoesNotValidateHostedBy(t *testing.T) {
	t.Parallel()

	t.Run("a foreign hostedByDid is never the reason a request is refused", func(t *testing.T) {
		t.Parallel()
		req := aValidCreateRequest()
		req.CreatedByDID = ""
		req.HostedByDID = "did:web:someone-else.invalid"

		err := requireRejectedBeforeProvisioning(t, req)
		var validation *communities.ValidationError
		require.ErrorAs(t, err, &validation)
		assert.Equal(t, "createdByDid", validation.Field,
			"if this ever says hostedByDid, the field has become a validated input — which means a "+
				"client can influence it, and the overwrite that makes it safe has been removed")
	})

	t.Run("an omitted hostedByDid is not a validation failure either", func(t *testing.T) {
		t.Parallel()
		req := aValidCreateRequest()
		req.HostedByDID = ""

		requireAcceptedBy(t, req, "hostedByDid")
	})
}

func TestValidateHandle(t *testing.T) {
	t.Parallel()
	service, _ := newFakeBackedService(t)

	t.Run("accepts the handles the provisioner generates", func(t *testing.T) {
		t.Parallel()
		for _, handle := range []string{
			"c-gardening.coves.social",
			"c-gardening.coves.co.uk",
			"c-book-club.dev.coves.social",
			"c-a.b.c",
			"c-123.coves.social",
			"C-Gardening.Coves.Social",
			strings.Repeat("a", 63) + ".coves.social",
		} {
			assert.NoErrorf(t, service.ValidateHandle(handle),
				"%q is a legal DNS hostname and the PDS will accept it as a handle; rejecting it here "+
					"fails a community after its account has already been provisioned", handle)
		}
	})

	t.Run("requires a handle at all", func(t *testing.T) {
		t.Parallel()
		err := service.ValidateHandle("")
		var validation *communities.ValidationError
		require.ErrorAs(t, err, &validation,
			"an empty handle is a missing field, distinct from a malformed one: the caller maps the "+
				"two to different messages")
		assert.Equal(t, "handle", validation.Field)
		assert.NotErrorIs(t, err, communities.ErrInvalidHandle)
	})

	t.Run("rejects strings a DNS name cannot be", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			handle string
		}{
			{"no dot at all", "cgardening"},
			{"a leading dot", ".coves.social"},
			{"a trailing dot", "c-gardening.coves.social."},
			{"consecutive dots", "c-gardening..social"},
			{"a leading hyphen in a label", "-gardening.coves.social"},
			{"a trailing hyphen in a label", "gardening-.coves.social"},
			{"a space", "c gardening.coves.social"},
			{"an underscore", "c_gardening.coves.social"},
			{"a slash", "c-gardening.coves.social/admin"},
			{"an at sign", "!gardening@coves.social"},
			{"a numeric TLD", "c-gardening.coves.123"},
			{"a label over 63 characters", strings.Repeat("a", 64) + ".coves.social"},
			{"a NUL byte", "c-garden\x00ing.coves.social"},
			{"a newline in the middle", "c-gardening.coves.social\nc-evil.coves.social"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				require.ErrorIsf(t, service.ValidateHandle(tc.handle), communities.ErrInvalidHandle,
					"%q must be refused: this handle is registered on a PDS and used as a lookup key, "+
						"so anything the regex lets through becomes a row that no client can address",
					tc.handle)
			})
		}
	})

	// The newline case above is the one worth calling out. Go's regexp is not
	// line-anchored by default only for ^ and $ in multiline mode, which this
	// pattern does not enable — but the distinction is subtle enough that a
	// future rewrite using (?m) would silently accept a two-line handle, and a
	// handle containing a newline is a log-injection primitive as well as an
	// unusable name.
}

// TestResolveScopedIdentifier_RejectsDomainsThatAreNotDomains covers the two
// isValidDomain branches the integration suite's table does not reach: an empty
// domain and one past the 253-character limit a DNS name has.
//
// Both are cheap here and only here, because neither can reach a lookup: the
// name is refused before "c-%s.%s" is ever assembled.
func TestResolveScopedIdentifier_RejectsDomainsThatAreNotDomains(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		identifier string
	}{
		{"an empty domain", "!gardening@"},
		{"a domain of only whitespace", "!gardening@   "},
		{"a domain past the 253-character DNS limit", "!gardening@" + strings.Repeat("a.", 130) + "com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service, repo, _ := seededService(t)

			_, err := service.ResolveCommunityIdentifier(ctx, tc.identifier)
			require.Error(t, err)
			assert.True(t, communities.IsValidationError(err),
				"%q must be refused as malformed rather than looked up", tc.identifier)
			assert.Empty(t, repo.calls,
				"a domain that cannot be a domain must not become half of a database lookup key")
		})
	}
}

// validationOutcome reports whether a request survived validateCreateRequest,
// and — when it did not — the error validation answered with.
//
// Acceptance is read off the provisioner sentinel described in the file
// comment: an error naming the account-creation step means validation let the
// request through and the fail-fast provisioner refused it, which is as far as
// T0 can follow. The full accepting path, where a legal request goes on to
// provision a real account, is proven in service_create_validation_test.go
// against a real PDS.
func validationOutcome(t *testing.T, req communities.CreateCommunityRequest) (passed bool, err error) {
	t.Helper()
	service, _ := newValidationService(t)

	_, err = service.CreateCommunity(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), reachedProvisioning) {
		return true, nil
	}
	return err == nil, err
}

// requireAcceptedBy asserts that whatever happened to the request, the named
// field was not the reason it stopped.
func requireAcceptedBy(t *testing.T, req communities.CreateCommunityRequest, field string) {
	t.Helper()
	passed, err := validationOutcome(t, req)
	if passed {
		return
	}
	var validation *communities.ValidationError
	require.ErrorAs(t, err, &validation, "the request failed for something other than a validation rule")
	require.NotEqualf(t, field, validation.Field,
		"%s was rejected, and this value is one a client is entitled to send", field)
}
