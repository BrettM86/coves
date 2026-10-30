package votes_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/votes"

	"github.com/stretchr/testify/require"
)

// Repository method names, used as keys for the fake's error knobs, call
// counters and query hook.
const (
	repositoryCreate                    = "Create"
	repositoryDelete                    = "Delete"
	repositoryGetActiveVotesForSubjects = "GetActiveVotesForSubjects"
	repositoryGetIndexedStatesByURIs    = "GetIndexedStatesByURIs"
)

// fakeVoteRepository is the AppView's votes table in memory: rows keyed by
// URI, soft deletes, and at most one active row per (voter, subject), which
// is what Postgres's partial unique index unique_voter_subject_active
// enforces. Tests index and soft-delete rows themselves; no consumer runs.
//
// It is safe for concurrent use. The query hook runs after a read query has
// computed its result and before that result is returned, without the lock
// held, so a hook may write rows or call the vote service.
type fakeVoteRepository struct {
	mutex      sync.Mutex
	rows       map[string]*votes.Vote
	errors     map[string]error
	calls      map[string]int
	afterQuery func(method string)
}

func newFakeVoteRepository(t *testing.T) *fakeVoteRepository {
	t.Helper()
	return &fakeVoteRepository{
		rows:   map[string]*votes.Vote{},
		errors: map[string]error{},
		calls:  map[string]int{},
	}
}

// failMethod makes every later call to method return err; nil clears it.
func (r *fakeVoteRepository) failMethod(method string, err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.errors[method] = err
}

// callCount reports how many times method has been called.
func (r *fakeVoteRepository) callCount(method string) int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.calls[method]
}

// setAfterQuery installs a hook run after each GetActiveVotesForSubjects and
// GetIndexedStatesByURIs call has computed its result, before it returns.
func (r *fakeVoteRepository) setAfterQuery(hook func(method string)) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.afterQuery = hook
}

// indexActive indexes an active vote by testVoterDID, as the consumer does
// when Jetstream delivers the create.
func (r *fakeVoteRepository) indexActive(t *testing.T, uri, subjectURI, direction string) {
	t.Helper()
	require.NoError(t, r.Create(context.Background(), &votes.Vote{
		URI:        uri,
		RKey:       uri[strings.LastIndex(uri, "/")+1:],
		CID:        "bafyindexed",
		VoterDID:   testVoterDID,
		SubjectURI: subjectURI,
		SubjectCID: testSubjectCI,
		Direction:  direction,
		CreatedAt:  time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
	}))
}

// softDelete soft-deletes an indexed row, as the consumer does when Jetstream
// delivers the delete.
func (r *fakeVoteRepository) softDelete(t *testing.T, uri string) {
	t.Helper()
	r.mutex.Lock()
	_, exists := r.rows[uri]
	r.mutex.Unlock()
	require.Truef(t, exists, "test setup: no indexed row %s to soft-delete", uri)
	require.NoError(t, r.Delete(context.Background(), uri))
}

// begin counts a call and returns its configured error.
func (r *fakeVoteRepository) begin(method string) error {
	r.calls[method]++
	return r.errors[method]
}

func (r *fakeVoteRepository) Create(_ context.Context, vote *votes.Vote) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if err := r.begin(repositoryCreate); err != nil {
		return err
	}
	if _, exists := r.rows[vote.URI]; exists {
		return nil // ON CONFLICT (uri) DO NOTHING
	}
	for _, row := range r.rows {
		if row.DeletedAt == nil && row.VoterDID == vote.VoterDID && row.SubjectURI == vote.SubjectURI {
			return votes.ErrVoteAlreadyExists // unique_voter_subject_active
		}
	}
	stored := *vote
	stored.DeletedAt = nil
	r.rows[vote.URI] = &stored
	return nil
}

func (r *fakeVoteRepository) Delete(_ context.Context, uri string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if err := r.begin(repositoryDelete); err != nil {
		return err
	}
	if row, exists := r.rows[uri]; exists && row.DeletedAt == nil {
		deletedAt := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
		row.DeletedAt = &deletedAt
	}
	return nil
}

func (r *fakeVoteRepository) GetActiveVotesForSubjects(_ context.Context, voterDID string, subjectURIs []string) ([]*votes.Vote, error) {
	result, err := func() ([]*votes.Vote, error) {
		r.mutex.Lock()
		defer r.mutex.Unlock()
		if err := r.begin(repositoryGetActiveVotesForSubjects); err != nil {
			return nil, err
		}
		var active []*votes.Vote
		for _, row := range r.rows {
			if row.DeletedAt == nil && row.VoterDID == voterDID && slices.Contains(subjectURIs, row.SubjectURI) {
				copied := *row
				active = append(active, &copied)
			}
		}
		return active, nil
	}()
	r.runAfterQuery(repositoryGetActiveVotesForSubjects)
	return result, err
}

func (r *fakeVoteRepository) GetIndexedStatesByURIs(_ context.Context, uris []string) (map[string]votes.IndexedVoteState, error) {
	result, err := func() (map[string]votes.IndexedVoteState, error) {
		r.mutex.Lock()
		defer r.mutex.Unlock()
		if err := r.begin(repositoryGetIndexedStatesByURIs); err != nil {
			return nil, err
		}
		states := map[string]votes.IndexedVoteState{}
		for _, uri := range uris {
			row, exists := r.rows[uri]
			switch {
			case !exists:
			case row.DeletedAt == nil:
				states[uri] = votes.IndexedVoteActive
			default:
				states[uri] = votes.IndexedVoteDeleted
			}
		}
		return states, nil
	}()
	r.runAfterQuery(repositoryGetIndexedStatesByURIs)
	return result, err
}

func (r *fakeVoteRepository) runAfterQuery(method string) {
	r.mutex.Lock()
	hook := r.afterQuery
	r.mutex.Unlock()
	if hook != nil {
		hook(method)
	}
}

var errUnusedRepositoryMethod = errors.New("fake vote repository: method not used by the vote service")

func (r *fakeVoteRepository) GetByURI(context.Context, string) (*votes.Vote, error) {
	return nil, fmt.Errorf("GetByURI: %w", errUnusedRepositoryMethod)
}

func (r *fakeVoteRepository) GetByVoterAndSubject(context.Context, string, string) (*votes.Vote, error) {
	return nil, fmt.Errorf("GetByVoterAndSubject: %w", errUnusedRepositoryMethod)
}

func (r *fakeVoteRepository) ListBySubject(context.Context, string, int, int) ([]*votes.Vote, error) {
	return nil, fmt.Errorf("ListBySubject: %w", errUnusedRepositoryMethod)
}

func (r *fakeVoteRepository) ListByVoter(context.Context, string, int, int) ([]*votes.Vote, error) {
	return nil, fmt.Errorf("ListByVoter: %w", errUnusedRepositoryMethod)
}
