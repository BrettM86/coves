package classification_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// summaryRepositoryFake records the query it received and answers with the
// configured summary or error.
type summaryRepositoryFake struct {
	summary   classification.CommunitySummary
	err       error
	lastQuery *classification.SummaryQuery
}

func (repo *summaryRepositoryFake) CommunitySummary(_ context.Context, query classification.SummaryQuery) (classification.CommunitySummary, error) {
	repo.lastQuery = &query
	return repo.summary, repo.err
}

const summaryCommunityDID = "did:plc:summarycommunity"

func summaryPolicy() classification.Policy {
	return classification.Policy{
		Version: "v1",
		Questions: []classification.Question{
			{Key: "sexual_explicit", Type: "probability", Instructions: "Is the content sexually explicit?"},
			{Key: "minors", Type: "probability", Instructions: "Does the content depict minors?"},
		},
	}
}

func newSummaryService(repo *summaryRepositoryFake) *classification.SummaryService {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return classification.NewSummaryService(repo, summaryPolicy(), func() time.Time { return now })
}

// TestSummaryQueriesLastSevenDays passes the community, question and
// threshold with the window [now - 168h, now) to the repository and returns
// its summary (B2).
func TestSummaryQueriesLastSevenDays(t *testing.T) {
	for _, test := range []struct {
		name           string
		question       string
		minProbability float64
	}{
		{name: "half", question: "sexual_explicit", minProbability: 0.5},
		{name: "zero threshold", question: "minors", minProbability: 0},
		{name: "full threshold", question: "minors", minProbability: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := classification.CommunitySummary{
				CommunityDID: summaryCommunityDID, Question: test.question, MinProbability: test.minProbability,
				Posts: classification.KindSummary{Scored: 3, Above: 1}, Comments: classification.KindSummary{Scored: 2, Above: 1},
				FlagEvents: []classification.FlagEvent{{OldNSFW: false, NewNSFW: true, OccurredAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC), Delivered: true}},
			}
			repo := &summaryRepositoryFake{summary: stored}
			summary, err := newSummaryService(repo).Summary(context.Background(), classification.SummaryRequest{
				CommunityDID: summaryCommunityDID, Question: test.question, MinProbability: test.minProbability,
			})
			require.NoError(t, err)
			require.NotNil(t, repo.lastQuery, "a valid request must reach the repository")
			assert.Equal(t, summaryCommunityDID, repo.lastQuery.CommunityDID)
			assert.Equal(t, test.question, repo.lastQuery.Question)
			assert.Equal(t, test.minProbability, repo.lastQuery.MinProbability)
			assert.True(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).Equal(repo.lastQuery.Since), "since %s", repo.lastQuery.Since)
			assert.True(t, time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC).Equal(repo.lastQuery.Until), "until %s", repo.lastQuery.Until)
			assert.Equal(t, classification.KindSummary{Scored: 3, Above: 1}, summary.Posts)
			assert.Equal(t, classification.KindSummary{Scored: 2, Above: 1}, summary.Comments)
			assert.Equal(t, []classification.FlagEvent{{OldNSFW: false, NewNSFW: true, OccurredAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC), Delivered: true}}, summary.FlagEvents)
		})
	}
}

// TestSummaryRejectsInvalidRequests refuses an empty community, a question
// outside the current policy and a threshold outside [0, 1] without reaching
// the repository (B2).
func TestSummaryRejectsInvalidRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		request classification.SummaryRequest
	}{
		{name: "empty community", request: classification.SummaryRequest{Question: "sexual_explicit", MinProbability: 0.5}},
		{name: "empty question", request: classification.SummaryRequest{CommunityDID: summaryCommunityDID, MinProbability: 0.5}},
		{name: "question not in policy", request: classification.SummaryRequest{CommunityDID: summaryCommunityDID, Question: "spam_scam", MinProbability: 0.5}},
		{name: "negative threshold", request: classification.SummaryRequest{CommunityDID: summaryCommunityDID, Question: "sexual_explicit", MinProbability: -0.1}},
		{name: "threshold above one", request: classification.SummaryRequest{CommunityDID: summaryCommunityDID, Question: "sexual_explicit", MinProbability: 1.1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &summaryRepositoryFake{}
			_, err := newSummaryService(repo).Summary(context.Background(), test.request)
			assert.ErrorIs(t, err, classification.ErrInvalidRequest)
			assert.Nil(t, repo.lastQuery, "an invalid request must not reach the repository")
		})
	}
}

// TestSummaryPropagatesRepositoryErrors returns ErrCommunityNotFound and other
// repository errors to the caller (B2).
func TestSummaryPropagatesRepositoryErrors(t *testing.T) {
	storeFailure := errors.New("store unavailable")
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "community not found", err: fmt.Errorf("lookup: %w", classification.ErrCommunityNotFound), want: classification.ErrCommunityNotFound},
		{name: "store failure", err: storeFailure, want: storeFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &summaryRepositoryFake{err: test.err}
			_, err := newSummaryService(repo).Summary(context.Background(), classification.SummaryRequest{
				CommunityDID: summaryCommunityDID, Question: "sexual_explicit", MinProbability: 0.5,
			})
			assert.ErrorIs(t, err, test.want)
		})
	}
}

// TestCommunitySummaryShare divides the above counts of both kinds by their
// summed scored counts (not the mean of per-kind shares), and is undefined
// when nothing was scored (B2).
func TestCommunitySummaryShare(t *testing.T) {
	for _, test := range []struct {
		name     string
		posts    classification.KindSummary
		comments classification.KindSummary
		share    float64
		defined  bool
	}{
		{name: "unequal denominators", posts: classification.KindSummary{Scored: 3, Above: 1}, comments: classification.KindSummary{Scored: 2, Above: 1}, share: 0.4, defined: true},
		{name: "posts only", posts: classification.KindSummary{Scored: 4, Above: 1}, share: 0.25, defined: true},
		{name: "comments only", comments: classification.KindSummary{Scored: 2, Above: 2}, share: 1, defined: true},
		{name: "scored but none above", posts: classification.KindSummary{Scored: 2}, comments: classification.KindSummary{Scored: 1}, share: 0, defined: true},
		{name: "nothing scored", defined: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			share, defined := classification.CommunitySummary{Posts: test.posts, Comments: test.comments}.Share()
			assert.Equal(t, test.defined, defined, "share defined")
			assert.InDelta(t, test.share, share, 1e-9)
		})
	}
}
