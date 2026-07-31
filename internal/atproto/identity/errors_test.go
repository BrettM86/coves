package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// What an identity error says when it reaches a log or a client.
//
// These four types are the vocabulary the rest of the AppView routes on, and
// their messages are the only place the identifier that failed is recorded —
// the errors carry no wrapped cause, so a message that drops the identifier
// leaves an operator with "resolution failed" and nowhere to go. Each case
// below asserts the identifier survives; the exact prose is asserted with it
// because these strings are matched by substring in base_resolver.go's own
// taxonomy and in the API error mappers.

func TestIdentityErrorMessages(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "not found with a reason",
			err:  &ErrNotFound{Identifier: "alice.invalid", Reason: "NoRecordsFound"},
			want: "identity not found: alice.invalid (NoRecordsFound)",
		},
		{
			// The reason is optional because a caller may know the identity is
			// absent without knowing why (a cache negative, a bare 404).
			name: "not found with no reason",
			err:  &ErrNotFound{Identifier: "alice.invalid"},
			want: "identity not found: alice.invalid",
		},
		{
			name: "invalid identifier",
			err:  &ErrInvalidIdentifier{Identifier: "not a handle", Reason: "handle syntax didn't validate"},
			want: "invalid identifier not a handle: handle syntax didn't validate",
		},
		{
			name: "cache miss",
			err:  &ErrCacheMiss{Identifier: fixtureDID},
			want: "cache miss: " + fixtureDID,
		},
		{
			name: "resolution failed",
			err:  &ErrResolutionFailed{Identifier: fixtureHandle, Reason: "dial tcp: connection refused"},
			want: "resolution failed for alice.invalid: dial tcp: connection refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}
