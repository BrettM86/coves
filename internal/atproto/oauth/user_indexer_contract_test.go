package oauth

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	coreerrors "Coves/internal/core/errors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the OAuth callback is allowed to ask of the user service, and why the
// answer changed.
//
// # THE CALLBACK IS THE ONE PLACE THAT KNOWS
//
// An erased account is refused by every indexing path, and that refusal needs
// an exit or a mistaken deletion is permanent. The exit cannot be a firehose
// event: those say only that some repo emitted a record naming a DID, which a
// bridge or a replay produces without the account's involvement. Nor can it be
// a row write, which would put the decision within reach of anything able to
// cause an INSERT.
//
// This callback is the exit, because it is the only place in the tree that
// knows the account itself is present: the PDS attested the DID and the handle
// was verified bidirectionally before this code runs. So the interface it
// depends on must name that. IndexUser — the gated, firehose-facing method —
// would silently do nothing for exactly the accounts this path exists to
// restore, and nothing about the call site would say so.
//
// # WHY THE NEGATIVE IS ASSERTED, NOT JUST THE POSITIVE
//
// A widened interface — one asking for both methods — would satisfy any test
// that only checked the new method is present, and would leave the callback
// free to keep calling the gated one. Requiring that IndexUser is ABSENT is
// what makes the dependency say something: whoever implements UserIndexer
// cannot answer this callback with the method that refuses erased accounts.
type authenticatedOnlyIndexer struct{}

func (authenticatedOnlyIndexer) IndexAuthenticatedUser(ctx context.Context, did, handle, pdsURL string) error {
	return nil
}

// THE COMPILE-TIME HALF, and it is the assertion that cannot be written as a
// test body: a type offering only IndexAuthenticatedUser must be usable as a
// UserIndexer.
var _ UserIndexer = authenticatedOnlyIndexer{}

// indexUserOnlyIndexer offers only the gated, firehose-facing method, and is
// the negative case: it must NOT satisfy UserIndexer.
type indexUserOnlyIndexer struct{}

func (indexUserOnlyIndexer) IndexUser(ctx context.Context, did, handle, pdsURL string) error {
	return nil
}

func TestUserIndexer_RequiresTheAuthenticatedIndexingMethod(t *testing.T) {
	t.Parallel()

	// A runtime assertion rather than a compile-time one, because "does NOT
	// implement" is not something a var declaration can state.
	var gatedOnly any = indexUserOnlyIndexer{}
	_, satisfied := gatedOnly.(UserIndexer)
	assert.False(t, satisfied,
		"a user service offering only IndexUser still satisfies UserIndexer, so the OAuth callback can "+
			"go on calling the firehose-gated method. That method refuses erased accounts, which means "+
			"a returning account logs in successfully and is never indexed — and this is the only path "+
			"in the tree that knows the account itself is present")

	indexer := reflect.TypeOf((*UserIndexer)(nil)).Elem()

	var methods []string
	for i := range indexer.NumMethod() {
		methods = append(methods, indexer.Method(i).Name)
	}

	require.Containsf(t, methods, "IndexAuthenticatedUser",
		"UserIndexer must ask for the method that reinstates an erased account; it has %v", methods)
	assert.NotContainsf(t, methods, "IndexUser",
		"UserIndexer offers IndexUser (%v). That keeps the gated method reachable from the one call "+
			"site that must not use it, and a widened interface makes the callback's dependency say "+
			"nothing about which of the two it means", methods)
}

// stubIndexer is a UserIndexer that fails, or does not, on demand.
type stubIndexer struct {
	err   error
	calls int
}

func (s *stubIndexer) IndexAuthenticatedUser(context.Context, string, string, string) error {
	s.calls++
	return s.err
}

// Which indexing failures the callback may swallow, and the one it may not.
//
// # BEST-EFFORT IS RIGHT FOR ALMOST ALL OF THIS
//
// A users row that failed to write is written by the next login or the next
// firehose event, so failing the login over it would lock people out of a
// working account for something that repairs itself. That is why this call site
// logs and carries on, and it should go on doing so.
//
// A marker that failed to clear is the exception, and it is the reason this
// method exists at all rather than the branch being inlined. Nothing repairs
// it: the firehose path refuses erased DIDs by design, and this is the marker's
// only exit. So the account gets a session, writes posts, and every one of them
// is dropped — with nobody told, because the only signal was a log line on a
// server. A login that fails loudly is a far better outcome than an account
// that silently publishes nothing.
//
// The distinction is carried by a sentinel rather than by the indexer returning
// a special type, because errors.Is survives the wrapping that says which DID
// failed and why.
func TestIndexAuthenticatedIdentity_FailsOnlyWhenTheMarkerSurvives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		indexErr  error
		wantError bool
	}{
		{
			name:      "the erasure marker could not be cleared",
			indexErr:  fmt.Errorf("clearing the erasure marker for did:plc:whoever: %w", coreerrors.ErrReinstateFailed),
			wantError: true,
		},
		{
			// Wrapped, because that is how it will arrive: the service says
			// which DID and what went wrong on the way out. A check that only
			// worked on a bare sentinel would pass here and fail in production.
			name:      "an ordinary indexing failure",
			indexErr:  errors.New("the users table is unreachable"),
			wantError: false,
		},
		{
			name:      "a successful index",
			indexErr:  nil,
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			indexer := &stubIndexer{err: tt.indexErr}
			handler := &OAuthHandler{userIndexer: indexer}

			err := handler.indexAuthenticatedIdentity(context.Background(),
				"did:plc:whoever", "whoever.test.coves.dev", "https://pds.example.invalid")

			require.Equal(t, 1, indexer.calls, "the identity must actually be indexed")

			if tt.wantError {
				require.Errorf(t, err,
					"the marker survived, so this account can log in and cannot publish, and nothing "+
						"else in the system will ever repair it. Swallowing this leaves the only record "+
						"of it in a log line: got %v", err)
				return
			}
			require.NoErrorf(t, err,
				"this failure is repaired by the next login or firehose event, so the callback must "+
					"log it and let the session proceed. Failing here locks a user out of a working "+
					"account over a transient write: got %v", err)
		})
	}
}
