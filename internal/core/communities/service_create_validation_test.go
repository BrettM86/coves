//go:build integration

package communities_test

import (
	"Coves/internal/core/communities"
	"Coves/tests/testkit"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a community may be called, and why the rule is DNS rather than taste.
//
// A community's name is not a label on a row: the provisioner interpolates it
// into "c-%s.%s" and registers the result as a real atProto handle
// (pds_provisioning.go), so the name has to be a legal DNS label before
// anything else happens. That makes name validation a gate in front of an
// account creation on a live PDS, and the ordering matters twice over — a name
// that slips through produces a PDS account whose handle nothing can resolve,
// and a name rejected too late has already cost a network round trip and, on
// the update path, an uploaded blob.
//
// # WHY THE ACCEPTING CASES LOOK ODD
//
// The rejecting direction can be asserted exactly: validation fails before the
// provisioner is called, so the error is a validation error and nothing was
// created. The accepting direction cannot, because "the name was accepted" is
// only observable by watching the request continue INTO provisioning, which
// then succeeds or fails on its own terms. So those cases assert the negative
// that actually matters — that whatever went wrong, it was not the name — and
// the one case that can be fully asserted (a legal name that provisions) is
// asserted fully.
//
// Names are generated rather than literal wherever an account is really
// created. A test that provisions "c-gaming.coves.social" squats that handle on
// a PDS which keeps accounts between runs, and every later run silently
// exercises the handle-taken path instead of the one it meant to.

func TestService_CreateRejectsNamesAHandleCannotCarry(t *testing.T) {
	t.Parallel()

	service, _, _ := newCommunityService(t)
	ctx := context.Background()

	// createWith runs the request and returns only the error: none of these
	// reach the PDS, so there is never a community to look at.
	createWith := func(name string) error {
		_, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   name,
			DisplayName:            "Name Validation",
			Description:            "a name the handle scheme cannot carry",
			Visibility:             "public",
			CreatedByDID:           "did:plc:namevalidation",
			AllowExternalDiscovery: true,
		})
		return err
	}

	t.Run("rejects empty name", func(t *testing.T) {
		err := createWith("")
		require.Error(t, err)
		assert.ErrorContains(t, err, "name")
		assert.True(t, communities.IsValidationError(err))
	})

	t.Run("rejects a name over the 63-character DNS label limit", func(t *testing.T) {
		err := createWith(strings.Repeat("a", 64))
		require.Error(t, err)
		assert.ErrorContains(t, err, "63",
			"the limit has to appear in the message: a client cannot shorten a name to fit a number it was not told")
		assert.ErrorContains(t, err, "name")
		assert.True(t, communities.IsValidationError(err))
	})

	// Every character here is one that changes what the resulting handle MEANS
	// rather than merely looking wrong: a dot adds a label, an @ or a ! collides
	// with the scoped-identifier syntax the resolver parses, whitespace makes a
	// handle that cannot be typed, and a leading or trailing hyphen is illegal
	// in a DNS label even though it looks harmless.
	for _, testCase := range []struct {
		description string
		name        string
	}{
		{"exclamation mark", "test!community"},
		{"at symbol", "test@space"},
		{"space", "test community"},
		{"period", "test.community"},
		{"underscore", "test_community"},
		{"hash", "test#tag"},
		{"leading hyphen", "-testcommunity"},
		{"trailing hyphen", "testcommunity-"},
	} {
		t.Run("rejects a name containing "+testCase.description, func(t *testing.T) {
			err := createWith(testCase.name)
			require.Errorf(t, err, "%q is not a legal DNS label", testCase.name)
			assert.ErrorContains(t, err, "name")
			assert.Truef(t, communities.IsValidationError(err),
				"%q must be refused as invalid input, not attempted against the PDS", testCase.name)
		})
	}
}

func TestService_CreateAcceptsLegalDNSLabelNames(t *testing.T) {
	t.Parallel()

	service, _, pdsServer := newCommunityService(t)
	ctx := context.Background()

	t.Run("a name of hyphens, digits and mixed case provisions", func(t *testing.T) {
		// One generated name carrying all three legal-but-unusual features, so
		// the case that really creates an account creates exactly one.
		name := testkit.UniqueIDWithPrefix(t, "Ok-9")
		require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
			"the generated community name %q makes a handle label the PDS will refuse", name)

		community, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   name,
			DisplayName:            "Legal Name",
			Description:            "hyphens, digits and mixed case are all legal in a DNS label",
			Visibility:             "public",
			CreatedByDID:           "did:plc:namevalidation",
			AllowExternalDiscovery: true,
		})
		require.NoError(t, err)

		// The stored name keeps the case the creator chose; the handle does not.
		// DNS is case-insensitive and the provisioner lower-cases before building
		// the handle, so a mixed-case name must not produce a mixed-case handle —
		// the AppView looks communities up by a lower-cased handle string, and a
		// row holding "c-Ok-9x.coves.social" would never be found by anyone.
		assert.Equal(t, name, community.Name, "the name is stored as the creator typed it")
		assert.Equal(t, strings.ToLower("c-"+name+"."+instanceDomain), community.Handle)
		assert.Equal(t, community.Handle, strings.ToLower(community.Handle),
			"a handle with upper case in it is a row the resolver's lower-cased lookup cannot reach")

		// And it is a handle the PDS agrees exists, which is the only proof that
		// the characters survived account creation rather than being normalised
		// away.
		pdsServer.Login(t, community.Handle, community.PDSPassword)
	})

	t.Run("a 63-character name passes validation and fails later", func(t *testing.T) {
		// 63 is the DNS label limit and therefore the largest name the service
		// accepts — but "c-" plus 63 characters is 65, over the PDS' own handle
		// label budget, so this request is refused by the PDS rather than by
		// validation. That split is the assertion: the service must not be the
		// one saying no, or it has moved the limit.
		_, err := service.CreateCommunity(ctx, communities.CreateCommunityRequest{
			Name:                   strings.Repeat("a", 63),
			DisplayName:            "Maximum Length Name",
			Description:            "exactly at the DNS label limit",
			Visibility:             "public",
			CreatedByDID:           "did:plc:namevalidation",
			AllowExternalDiscovery: true,
		})
		require.Error(t, err, "the PDS refuses a 65-character handle label")
		assert.False(t, communities.IsValidationError(err),
			"a 63-character name is legal; the service must pass it to the provisioner, got %v", err)
		assert.ErrorContains(t, err, "provision",
			"the failure must come from account provisioning, not from name validation")
	})
}
