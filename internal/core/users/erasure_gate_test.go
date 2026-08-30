package users

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// erasureAwareRepo is a UserRepository that CAN answer the erasure question,
// with the answer written down by the test.
//
// The embedded interface is nil: every method but the one under test panics
// rather than returning a zero value, so a service that started calling
// something else here would say so instead of quietly passing.
type erasureAwareRepo struct {
	UserRepository

	erased bool
	err    error

	// calls records what the service asked this repository to do, in order, as
	// "verb:did". Order is recorded rather than a set of booleans because the
	// sequence is itself part of the contract — see
	// TestIndexAuthenticatedUser_ReinstatesBeforeItIndexes.
	calls []string

	// reinstated is what ReinstateAccount reports back: whether a marker was
	// actually there. reinstateErr and createErr are how a test asks for each
	// of the two failures separately, which is the whole of what distinguishes
	// them below.
	reinstated   bool
	reinstateErr error
	createErr    error
}

func (r *erasureAwareRepo) IsAccountDeleted(_ context.Context, _ string) (bool, error) {
	return r.erased, r.err
}

// ReinstateAccount is the store's other half. It records the call rather than
// doing anything, because whether the service reaches it — for which DID, and
// in what order — is what the reinstatement tests are about.
func (r *erasureAwareRepo) ReinstateAccount(_ context.Context, did string) (bool, error) {
	r.calls = append(r.calls, "reinstate:"+did)
	return r.reinstated, r.reinstateErr
}

// Create is implemented so the reinstatement test can let a whole
// IndexAuthenticatedUser call run rather than stopping it at the nil embedded
// interface: what that test is about is the order of the two writes, and an
// aborted call has no second write to be in order with.
func (r *erasureAwareRepo) Create(_ context.Context, user *User) (*User, error) {
	r.calls = append(r.calls, "create:"+user.DID)
	if r.createErr != nil {
		return nil, r.createErr
	}
	return user, nil
}

// The service's erasure question is answered by the repository through the
// OPTIONAL ErasureMarkerStore interface, and the interesting case is the one where
// the repository does not implement it at all.
//
// # THIS FAILS CLOSED, AND IndexUser DELIBERATELY DOES NOT
//
// IndexUser asks the same question and treats a repository without the lookup
// as "gate disabled" — it indexes. That is defensible there and only there:
// its callers are firehose consumers, there is no attacker choosing when they
// run, and a disabled gate degrades to the behaviour the AppView had before the
// marker existed.
//
// This method's caller is the unauthenticated registration endpoint, where
// somebody IS on the other side choosing the moment. "The repository cannot
// tell me whether this account was erased" and "this account was not erased"
// must not be the same answer there, because the second one registers. So the
// two call sites resolve the same ambiguity in opposite directions on purpose,
// and this test is where that choice is written down.
func TestIsAccountDeleted_FailsClosedWhenTheRepositoryCannotAnswer(t *testing.T) {
	// MockUserRepository implements UserRepository and nothing more, which is
	// exactly the shape under test: the erasure lookup is not part of
	// UserRepository, so a repository may legitimately lack it.
	repo := new(MockUserRepository)
	service := NewUserService(repo, nil, "https://pds.example.invalid", nil, "")

	erased, err := service.IsAccountDeleted(context.Background(), "did:plc:whoever")

	require.Error(t, err,
		"the repository cannot answer whether this DID was erased, and the caller is a public "+
			"unauthenticated endpoint. Returning no error here makes 'I could not check' "+
			"indistinguishable from 'not erased', and the second one registers the account")
	require.False(t, erased,
		"a failed lookup must not also claim the account is fine; the error is the answer")
}

// When the repository CAN answer, the service is a pass-through: it adds no
// interpretation of its own in either direction.
//
// The false case is not filler. A gate that answered "erased" for everyone
// would satisfy the true case and the error case and would refuse every
// legitimate registration on the endpoint above, so the only assertion that
// separates a working gate from a stuck one is that a live account comes back
// live.
func TestIsAccountDeleted_ReturnsTheRepositorysAnswer(t *testing.T) {
	lookupFailed := errors.New("the deleted_accounts table could not be read")

	tests := []struct {
		name       string
		repo       *erasureAwareRepo
		wantErased bool
		wantErr    error
	}{
		{
			name:       "an erased account",
			repo:       &erasureAwareRepo{erased: true},
			wantErased: true,
		},
		{
			name:       "a live account",
			repo:       &erasureAwareRepo{erased: false},
			wantErased: false,
		},
		{
			// The repository's failure must reach the caller as a failure. A
			// service that swallowed it into (false, nil) would reintroduce the
			// fail-open the test above exists to prevent, one layer lower down.
			name:    "a lookup that failed",
			repo:    &erasureAwareRepo{err: lookupFailed},
			wantErr: lookupFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := NewUserService(tt.repo, nil, "https://pds.example.invalid", nil, "")

			erased, err := service.IsAccountDeleted(context.Background(), "did:plc:whoever")

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr,
					"the repository's failure must reach the caller; wrapping is fine, swallowing is not")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantErased, erased,
				"the service must report what the repository said, unchanged")
		})
	}
}

// IndexAuthenticatedUser fails closed for the same reason IsAccountDeleted
// does, and it is worth stating separately because the failure mode is the
// opposite one.
//
// IsAccountDeleted without a store might wrongly say "not erased" and let
// somebody in. This method without a store would wrongly go on to index a user
// whose marker it could not remove — a row written for an account the ingestion
// gate still refuses, which is the half-reinstated state nothing downstream
// knows how to read. Neither is a state to reach by accident, so a repository
// that cannot answer stops the call rather than doing the part of it that
// happens to be possible.
func TestIndexAuthenticatedUser_FailsClosedWithoutAMarkerStore(t *testing.T) {
	// MockUserRepository implements UserRepository and nothing more, which is a
	// legitimate shape: the marker store is deliberately not part of
	// UserRepository.
	repo := new(MockUserRepository)
	service := NewUserService(repo, nil, "https://pds.example.invalid", nil, "")

	err := service.IndexAuthenticatedUser(context.Background(), "did:plc:whoever", "someone.test.coves.dev", "https://pds.example.invalid")

	require.Error(t, err,
		"this repository cannot clear an erasure marker, so an erased account cannot be reinstated "+
			"through it. Indexing anyway writes a users row the ingestion gate still refuses — the "+
			"account looks present and none of its content ever appears")

	// Nothing was written, which is the other half of failing closed: the
	// mock's expectations are empty, so any repository call at all is a
	// failure rather than a call returning a zero value.
	repo.AssertExpectations(t)
}

// The reinstatement really does reach the store, it names the DID being
// indexed, and it happens BEFORE the row is written.
//
// This is the T0 half of the T1 lifecycle test: that one proves the marker row
// disappears, and this one proves which call did it, for whom, and in what
// order. A service that reinstated some other DID — the handle, say, or a stale
// variable — would satisfy an integration test that only counts rows for the
// one account it set up.
//
// The order is the part worth pinning. Clearing the marker first means a
// failure in between leaves an un-erased account with no row, which the next
// login or firehose event repairs. Writing the row first means a failure in
// between leaves a row the ingestion gate still refuses — an account that looks
// present and whose content never appears, with nothing to make it try again.
// Of the two ways to fail halfway, only one is self-healing.
func TestIndexAuthenticatedUser_ReinstatesBeforeItIndexes(t *testing.T) {
	const did = "did:plc:returning"

	repo := &erasureAwareRepo{erased: true}
	service := NewUserService(repo, nil, "https://pds.example.invalid", nil, "")

	err := service.IndexAuthenticatedUser(context.Background(), did, "returning.test.coves.dev", "https://pds.example.invalid")
	require.NoError(t, err, "the account authenticated and the store answered; nothing here should fail")

	require.Equalf(t, []string{"reinstate:" + did, "create:" + did}, repo.calls,
		"the service must clear %s's erasure marker and then index it, in that order. No other call "+
			"clears a marker, so a missed or misaddressed reinstatement leaves the account erased with "+
			"a users row sitting on top of it", did)
}

// A marker that could not be cleared is the ONE indexing failure a caller must
// not shrug off, and this is the sentinel that lets it tell.
//
// # WHY THIS FAILURE IS NOT LIKE THE OTHERS
//
// The OAuth callback treats indexing as best-effort, and for almost everything
// that is right: a users row that failed to appear is written by the next login
// or the next firehose event, and failing the login instead would lock people
// out over something self-healing.
//
// A marker left standing is the exception, because nothing repairs it. The
// firehose path refuses erased DIDs by design, and this call is the marker's
// only exit — so the account gets a working session, writes posts, and every
// one of them is dropped. Nobody is told. The user sees an account that
// silently publishes nothing, which is worse than a failed login and much
// harder to report.
//
// So the caller has to be able to tell this failure from the rest, and a
// sentinel is how: errors.Is survives the wrapping that says which DID and what
// went wrong, where a string match on the message would not.
func TestIndexAuthenticatedUser_MarksAFailedReinstatement(t *testing.T) {
	const did = "did:plc:stillerased"

	repo := &erasureAwareRepo{erased: true, reinstateErr: errors.New("deleted_accounts is unreachable")}
	service := NewUserService(repo, nil, "https://pds.example.invalid", nil, "")

	err := service.IndexAuthenticatedUser(context.Background(), did, "stillerased.test.coves.dev", "https://pds.example.invalid")

	require.ErrorIs(t, err, ErrReinstateFailed,
		"the erasure marker could not be cleared, and the caller cannot tell that from any other "+
			"indexing failure. Every other one is repaired by the next login; this one leaves the "+
			"account able to log in and unable to publish, permanently and silently")

	require.Equalf(t, []string{"reinstate:" + did}, repo.calls,
		"the account is still erased, so nothing may be indexed for it. Writing the users row anyway "+
			"leaves %s looking present while the ingestion gate goes on dropping everything it writes", did)
}

// The sentinel means the marker specifically, not "indexing went wrong".
//
// Without this the sentinel is worthless in the direction that matters: a
// service that wrapped every failure in ErrReinstateFailed would satisfy the
// test above, and the callback would then fail a login over an ordinary
// transient write error that the next login repairs by itself. The two failures
// are told apart by which one is self-healing, and that distinction is the only
// reason either the sentinel or the branch reading it exists.
func TestIndexAuthenticatedUser_DoesNotMarkAnOrdinaryIndexingFailure(t *testing.T) {
	const did = "did:plc:writefailed"

	repo := &erasureAwareRepo{reinstated: true, createErr: errors.New("the users table is unreachable")}
	service := NewUserService(repo, nil, "https://pds.example.invalid", nil, "")

	err := service.IndexAuthenticatedUser(context.Background(), did, "writefailed.test.coves.dev", "https://pds.example.invalid")

	require.Error(t, err, "a failed write must still be reported")
	require.NotErrorIs(t, err, ErrReinstateFailed,
		"the marker WAS cleared and the users row is what failed to write — a failure the next login "+
			"or firehose event repairs. Reporting it as a reinstatement failure makes the caller refuse "+
			"a login it should have allowed")
}
