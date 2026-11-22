package classification_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reenqueueRepositoryFake records which repository method a request reached
// and with what arguments, and answers with the configured report or error.
type reenqueueRepositoryFake struct {
	report classification.ReenqueueReport
	err    error

	communitiesEvaluation *classification.Evaluation
	rangeEvaluation       *classification.Evaluation
	rangeSince            time.Time
	rangeUntil            time.Time
}

func (repo *reenqueueRepositoryFake) EnqueueCommunities(_ context.Context, evaluation classification.Evaluation) (classification.ReenqueueReport, error) {
	repo.communitiesEvaluation = &evaluation
	return repo.report, repo.err
}

func (repo *reenqueueRepositoryFake) EnqueuePostsAndComments(_ context.Context, evaluation classification.Evaluation, since, until time.Time) (classification.ReenqueueReport, error) {
	repo.rangeEvaluation = &evaluation
	repo.rangeSince, repo.rangeUntil = since, until
	return repo.report, repo.err
}

func (repo *reenqueueRepositoryFake) reached() bool {
	return repo.communitiesEvaluation != nil || repo.rangeEvaluation != nil
}

var (
	reenqueueEvaluation = classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-x", ImageModelID: "image-y"}
	reenqueueSince      = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	reenqueueUntil      = time.Date(2026, 11, 3, 6, 30, 0, 0, time.UTC)
)

func newReenqueueService(repo *reenqueueRepositoryFake) *classification.ReenqueueService {
	evaluation := reenqueueEvaluation
	return classification.NewReenqueueService(repo, &evaluation)
}

// TestReenqueueRejectsInvalidRequests refuses an unknown or empty target, a
// postsAndComments request without a well-ordered [since, until) range, and an
// allCommunities request carrying a range, without reaching the repository (B1).
func TestReenqueueRejectsInvalidRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		request classification.ReenqueueRequest
	}{
		{name: "empty target", request: classification.ReenqueueRequest{}},
		{name: "unknown target", request: classification.ReenqueueRequest{Target: "everything"}},
		{name: "posts and comments without since", request: classification.ReenqueueRequest{
			Target: classification.ReenqueuePostsAndComments, Until: reenqueueUntil,
		}},
		{name: "posts and comments without until", request: classification.ReenqueueRequest{
			Target: classification.ReenqueuePostsAndComments, Since: reenqueueSince,
		}},
		{name: "posts and comments with since equal to until", request: classification.ReenqueueRequest{
			Target: classification.ReenqueuePostsAndComments, Since: reenqueueUntil, Until: reenqueueUntil,
		}},
		{name: "posts and comments with since after until", request: classification.ReenqueueRequest{
			Target: classification.ReenqueuePostsAndComments, Since: reenqueueUntil, Until: reenqueueSince,
		}},
		{name: "all communities with since", request: classification.ReenqueueRequest{
			Target: classification.ReenqueueAllCommunities, Since: reenqueueSince,
		}},
		{name: "all communities with until", request: classification.ReenqueueRequest{
			Target: classification.ReenqueueAllCommunities, Until: reenqueueUntil,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &reenqueueRepositoryFake{}
			_, err := newReenqueueService(repo).Reenqueue(context.Background(), test.request)
			assert.ErrorIs(t, err, classification.ErrInvalidRequest)
			assert.False(t, repo.reached(), "an invalid request must not reach the repository")
		})
	}
}

// TestReenqueueWithoutClassifier refuses a valid request with
// ErrClassifierNotConfigured when the service has no evaluation, without
// reaching the repository (B1).
func TestReenqueueWithoutClassifier(t *testing.T) {
	for _, request := range []classification.ReenqueueRequest{
		{Target: classification.ReenqueueAllCommunities},
		{Target: classification.ReenqueuePostsAndComments, Since: reenqueueSince, Until: reenqueueUntil},
	} {
		t.Run(string(request.Target), func(t *testing.T) {
			repo := &reenqueueRepositoryFake{report: classification.ReenqueueReport{Created: 1}}
			_, err := classification.NewReenqueueService(repo, nil).Reenqueue(context.Background(), request)
			assert.ErrorIs(t, err, classification.ErrClassifierNotConfigured)
			assert.False(t, repo.reached(), "without a classifier no job may be written")
		})
	}
}

// TestReenqueueAllCommunities reaches EnqueueCommunities with the service's
// evaluation and returns the repository's report (B1).
func TestReenqueueAllCommunities(t *testing.T) {
	repo := &reenqueueRepositoryFake{report: classification.ReenqueueReport{Created: 2, AlreadyPresent: 1}}
	report, err := newReenqueueService(repo).Reenqueue(context.Background(), classification.ReenqueueRequest{
		Target: classification.ReenqueueAllCommunities,
	})
	require.NoError(t, err)
	assert.Equal(t, classification.ReenqueueReport{Created: 2, AlreadyPresent: 1}, report)
	require.NotNil(t, repo.communitiesEvaluation, "allCommunities must reach EnqueueCommunities")
	assert.Equal(t, classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-x", ImageModelID: "image-y"}, *repo.communitiesEvaluation)
	assert.Nil(t, repo.rangeEvaluation, "allCommunities must not enqueue posts or comments")
}

// TestReenqueuePostsAndComments reaches EnqueuePostsAndComments with the
// service's evaluation and the requested range and returns the repository's
// report (B1).
func TestReenqueuePostsAndComments(t *testing.T) {
	repo := &reenqueueRepositoryFake{report: classification.ReenqueueReport{Created: 5, AlreadyPresent: 2}}
	report, err := newReenqueueService(repo).Reenqueue(context.Background(), classification.ReenqueueRequest{
		Target: classification.ReenqueuePostsAndComments, Since: reenqueueSince, Until: reenqueueUntil,
	})
	require.NoError(t, err)
	assert.Equal(t, classification.ReenqueueReport{Created: 5, AlreadyPresent: 2}, report)
	require.NotNil(t, repo.rangeEvaluation, "postsAndComments must reach EnqueuePostsAndComments")
	assert.Equal(t, classification.Evaluation{PolicyVersion: "v1", TextModelID: "text-x", ImageModelID: "image-y"}, *repo.rangeEvaluation)
	assert.True(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Equal(repo.rangeSince), "since %s", repo.rangeSince)
	assert.True(t, time.Date(2026, 11, 3, 6, 30, 0, 0, time.UTC).Equal(repo.rangeUntil), "until %s", repo.rangeUntil)
	assert.Nil(t, repo.communitiesEvaluation, "postsAndComments must not enqueue communities")
}

// TestReenqueuePropagatesRepositoryErrors returns the repository's error for
// either target (B1).
func TestReenqueuePropagatesRepositoryErrors(t *testing.T) {
	storeFailure := errors.New("store unavailable")
	for _, request := range []classification.ReenqueueRequest{
		{Target: classification.ReenqueueAllCommunities},
		{Target: classification.ReenqueuePostsAndComments, Since: reenqueueSince, Until: reenqueueUntil},
	} {
		t.Run(string(request.Target), func(t *testing.T) {
			repo := &reenqueueRepositoryFake{err: storeFailure}
			_, err := newReenqueueService(repo).Reenqueue(context.Background(), request)
			assert.ErrorIs(t, err, storeFailure)
		})
	}
}
