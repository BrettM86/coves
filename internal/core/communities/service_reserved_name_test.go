package communities_test

import (
	"testing"

	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The "c-" prefix namespaces community actors apart from user actors on the
// PDS, and clients derive the handle they display and link to by stripping
// exactly one leading "c-". That derivation is only unambiguous while no
// community name starts with "c-" itself:
//
//	name "sharp"    -> stored c-sharp.coves.example   -> displayed sharp.coves.example
//	name "c-sharp"  -> stored c-c-sharp.coves.example -> displayed c-sharp.coves.example
//
// The second display string is the first community's *stored* handle, so
// "c-sharp" would link to "sharp". Resolution cannot break the tie either: the
// prefixed retry is skipped for identifiers already starting with "c-", so the
// URL either resolves to the wrong community or 404s. Reserving the prefix at
// creation is what keeps the mapping one-to-one.
func TestCreateCommunity_ReservesTheCommunityHandlePrefix(t *testing.T) {
	t.Parallel()

	reserved := []struct {
		name   string
		reason string
	}{
		{"c-sharp", "collides with the display form of the community named \"sharp\""},
		{"c-", "degenerate case: strips to an empty first label"},
		{"c-c-sharp", "nesting the prefix compounds the ambiguity"},
		{"C-Sharp", "the prefix check must not be case-sensitive, since handles are lowercased"},
	}

	for _, tc := range reserved {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := aValidCreateRequest()
			req.Name = tc.name

			err := requireRejectedBeforeProvisioning(t, req)
			var validation *communities.ValidationError
			require.ErrorAs(t, err, &validation, tc.reason)
			assert.Equal(t, "name", validation.Field)
		})
	}
}

// The reservation must be narrow: it keys off the "c-" prefix, not the letter
// "c", and not a hyphen anywhere in the name. Over-rejecting would refuse names
// clients are entitled to use.
func TestCreateCommunity_AllowsNamesThatMerelyResembleThePrefix(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"c",         // the bare letter is not the prefix
		"csharp",    // no hyphen, no prefix
		"cats",      // starts with "c" but not "c-"
		"self-host", // a hyphen elsewhere is fine
		"c2-fast",   // "c" followed by something other than "-"
	}

	for _, name := range allowed {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := aValidCreateRequest()
			req.Name = name

			requireAcceptedBy(t, req, "name")
		})
	}
}
