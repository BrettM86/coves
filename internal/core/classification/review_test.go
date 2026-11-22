package classification_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedPrecision is a hand-computed QuestionPrecision; nil ratios mean
// undefined.
type expectedPrecision struct {
	question                         string
	atOrAbove, present, truePositive int
	precision, recall                *float64
}

func ratio(value float64) *float64 { return &value }

func assertPrecisions(t *testing.T, expected []expectedPrecision, actual []classification.QuestionPrecision) {
	t.Helper()
	require.Len(t, actual, len(expected), "one precision row per question")
	for index, want := range expected {
		got := actual[index]
		assert.Equal(t, want.question, got.Question, "row %d question", index)
		assert.Equal(t, want.atOrAbove, got.AtOrAbove, "%s at or above", want.question)
		assert.Equal(t, want.present, got.Present, "%s present", want.question)
		assert.Equal(t, want.truePositive, got.TruePositive, "%s true positives", want.question)
		for _, pair := range []struct {
			name      string
			want, got *float64
		}{{"precision", want.precision, got.Precision}, {"recall", want.recall, got.Recall}} {
			if pair.want == nil {
				assert.Nil(t, pair.got, "%s %s must be undefined", want.question, pair.name)
				continue
			}
			if assert.NotNil(t, pair.got, "%s %s must be defined", want.question, pair.name) {
				assert.InDelta(t, *pair.want, *pair.got, 1e-6, "%s %s", want.question, pair.name)
			}
		}
	}
}

// TestComputePrecision checks hand-computed precision and recall at 0.5: a
// probability equal to the threshold is at or above it, no sample at or above
// leaves precision undefined, no present verdict leaves recall undefined, and
// a sample without a question's probability is ignored for that question (B1).
func TestComputePrecision(t *testing.T) {
	samples := []classification.VerdictSample{
		{
			Probabilities: map[string]float64{"spam_scam": 0.9, "hate_harassment": 0.2, "graphic_violence": 0.2, "sexual_minors": 0.95},
			Present:       map[string]bool{"spam_scam": true, "hate_harassment": false, "graphic_violence": true, "sexual_minors": false},
		},
		{
			Probabilities: map[string]float64{"spam_scam": 0.5, "hate_harassment": 0.7, "graphic_violence": 0.1},
			Present:       map[string]bool{"spam_scam": false, "hate_harassment": false, "graphic_violence": false},
		},
		{
			Probabilities: map[string]float64{"spam_scam": 0.8, "hate_harassment": 0.1},
			Present:       map[string]bool{"spam_scam": true, "hate_harassment": true},
		},
		{
			Probabilities: map[string]float64{"spam_scam": 0.3, "hate_harassment": 0.4},
			Present:       map[string]bool{"spam_scam": true, "hate_harassment": false},
		},
		{
			// No spam_scam probability: its present verdict is ignored.
			Probabilities: map[string]float64{"hate_harassment": 0.6},
			Present:       map[string]bool{"spam_scam": true, "hate_harassment": false},
		},
		{
			// No hate_harassment probability: its present verdict is ignored.
			Probabilities: map[string]float64{"spam_scam": 0.1},
			Present:       map[string]bool{"spam_scam": true, "hate_harassment": true},
		},
	}
	questions := []string{"spam_scam", "hate_harassment", "graphic_violence", "sexual_minors", "sexual_explicit"}

	actual := classification.ComputePrecision(questions, samples, 0.5)

	assertPrecisions(t, []expectedPrecision{
		// at or above: 0.9, 0.5, 0.8; present: 1, 3, 4, 6; both: 1, 3.
		{question: "spam_scam", atOrAbove: 3, present: 4, truePositive: 2, precision: ratio(0.6666667), recall: ratio(0.5)},
		// at or above: 0.7, 0.6, neither present; present: sample 3 only.
		{question: "hate_harassment", atOrAbove: 2, present: 1, truePositive: 0, precision: ratio(0), recall: ratio(0)},
		{question: "graphic_violence", atOrAbove: 0, present: 1, truePositive: 0, precision: nil, recall: ratio(0)},
		{question: "sexual_minors", atOrAbove: 1, present: 0, truePositive: 0, precision: ratio(0), recall: nil},
		{question: "sexual_explicit", atOrAbove: 0, present: 0, truePositive: 0, precision: nil, recall: nil},
	}, actual)
}

// TestComputePrecisionNoSamples reports every question with zero counts and
// undefined ratios when nothing is verdicted (B1).
func TestComputePrecisionNoSamples(t *testing.T) {
	actual := classification.ComputePrecision([]string{"hate_harassment", "spam_scam"}, nil, 0.5)

	assertPrecisions(t, []expectedPrecision{
		{question: "hate_harassment"},
		{question: "spam_scam"},
	}, actual)
}

// reviewRepositoryFake is an in-memory ReviewRepository that records what the
// service asked of it.
type reviewRepositoryFake struct {
	rows             []classification.ResultView
	lastQuery        *classification.ResultQuery
	questions        []string
	questionsErr     error
	storeErr         error
	stored           []classification.Verdict
	samples          classification.PrecisionSamples
	samplesRequested []string
}

func (repo *reviewRepositoryFake) ListResults(_ context.Context, query classification.ResultQuery) ([]classification.ResultView, error) {
	repo.lastQuery = &query
	rows := repo.rows
	if query.Limit < len(rows) {
		rows = rows[:query.Limit]
	}
	return rows, nil
}

func (repo *reviewRepositoryFake) ResultQuestions(_ context.Context, _ int64) ([]string, error) {
	return repo.questions, repo.questionsErr
}

func (repo *reviewRepositoryFake) StoreVerdict(_ context.Context, verdict classification.Verdict) (classification.Verdict, error) {
	if repo.storeErr != nil {
		return classification.Verdict{}, repo.storeErr
	}
	repo.stored = append(repo.stored, verdict)
	return verdict, nil
}

func (repo *reviewRepositoryFake) PrecisionSamples(_ context.Context, modelID, policyVersion string) (classification.PrecisionSamples, error) {
	repo.samplesRequested = []string{modelID, policyVersion}
	return repo.samples, nil
}

var reviewNow = time.Date(2026, 11, 12, 10, 30, 0, 123456000, time.UTC)

func reviewPolicy() classification.Policy {
	return classification.Policy{Version: "v1", Questions: []classification.Question{
		{Key: "spam_scam", Type: "boolean", Instructions: "spam"},
		{Key: "hate_harassment", Type: "boolean", Instructions: "hate"},
	}}
}

func newReviewService(repo *reviewRepositoryFake) *classification.ReviewService {
	return classification.NewReviewService(repo, reviewPolicy(), func() time.Time { return reviewNow })
}

func probability(value float64) *float64 { return &value }

// TestReviewServiceListRejectsInvalidFilters maps each out-of-contract filter
// to ErrInvalidRequest (B2).
func TestReviewServiceListRejectsInvalidFilters(t *testing.T) {
	for _, test := range []struct {
		name   string
		filter classification.ResultFilter
	}{
		{name: "minProbability without question", filter: classification.ResultFilter{MinProbability: probability(0.5)}},
		{name: "minProbability below zero", filter: classification.ResultFilter{Question: "spam_scam", MinProbability: probability(-0.01)}},
		{name: "minProbability above one", filter: classification.ResultFilter{Question: "spam_scam", MinProbability: probability(1.01)}},
		{name: "unknown subject kind", filter: classification.ResultFilter{SubjectKind: "profile"}},
		{name: "limit above 100", filter: classification.ResultFilter{Limit: 101}},
		{name: "negative limit", filter: classification.ResultFilter{Limit: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newReviewService(&reviewRepositoryFake{}).ListResults(context.Background(), test.filter)
			assert.ErrorIs(t, err, classification.ErrInvalidRequest)
		})
	}
}

// TestReviewServiceListRejectsMalformedCursor maps a cursor that does not
// decode to a keyset position to ErrInvalidCursor (B2).
func TestReviewServiceListRejectsMalformedCursor(t *testing.T) {
	for _, cursor := range []string{"!!!not-a-cursor", "Z2FyYmFnZQ"} {
		t.Run(cursor, func(t *testing.T) {
			_, err := newReviewService(&reviewRepositoryFake{}).ListResults(context.Background(), classification.ResultFilter{Cursor: cursor})
			assert.ErrorIs(t, err, classification.ErrInvalidCursor)
		})
	}
}

// TestReviewServiceListFetchesOneExtraRow asks the repository for limit+1 rows
// (limit 0 meaning 50) and passes the filter through; boundary
// minProbability values are accepted (B2).
func TestReviewServiceListFetchesOneExtraRow(t *testing.T) {
	for _, test := range []struct {
		name          string
		filter        classification.ResultFilter
		expectedLimit int
	}{
		{name: "default limit", filter: classification.ResultFilter{}, expectedLimit: 51},
		{name: "limit 1", filter: classification.ResultFilter{Limit: 1}, expectedLimit: 2},
		{name: "limit 100", filter: classification.ResultFilter{Limit: 100}, expectedLimit: 101},
		{name: "minProbability 0", filter: classification.ResultFilter{Question: "spam_scam", MinProbability: probability(0), SubjectKind: classification.SubjectKindComment, CommunityDID: "did:plc:community"}, expectedLimit: 51},
		{name: "minProbability 1", filter: classification.ResultFilter{Question: "spam_scam", MinProbability: probability(1), SubjectKind: classification.SubjectKindPost}, expectedLimit: 51},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &reviewRepositoryFake{}
			page, err := newReviewService(repo).ListResults(context.Background(), test.filter)
			require.NoError(t, err)
			require.NotNil(t, repo.lastQuery, "a valid filter must reach the repository")
			assert.Equal(t, test.expectedLimit, repo.lastQuery.Limit)
			assert.Nil(t, repo.lastQuery.After, "a first page has no keyset position")
			assert.Equal(t, test.filter.Question, repo.lastQuery.Filter.Question)
			assert.Equal(t, test.filter.MinProbability, repo.lastQuery.Filter.MinProbability)
			assert.Equal(t, test.filter.SubjectKind, repo.lastQuery.Filter.SubjectKind)
			assert.Equal(t, test.filter.CommunityDID, repo.lastQuery.Filter.CommunityDID)
			assert.Empty(t, page.Cursor, "no rows beyond the page means no cursor")
		})
	}
}

// TestReviewServiceListCursorRoundTrip returns a cursor only when the
// repository has a row past the page, and that cursor becomes the next
// query's keyset position at the page's last row (B2).
func TestReviewServiceListCursorRoundTrip(t *testing.T) {
	newest := time.Date(2026, 11, 10, 9, 0, 0, 654321000, time.UTC)
	tie := time.Date(2026, 11, 10, 8, 0, 0, 123456000, time.UTC)
	repo := &reviewRepositoryFake{rows: []classification.ResultView{
		{ID: 30, CreatedAt: newest},
		{ID: 29, CreatedAt: tie},
		{ID: 28, CreatedAt: tie},
	}}
	service := newReviewService(repo)

	first, err := service.ListResults(context.Background(), classification.ResultFilter{Limit: 2})
	require.NoError(t, err)
	ids := make([]int64, 0, len(first.Results))
	for _, row := range first.Results {
		ids = append(ids, row.ID)
	}
	assert.Equal(t, []int64{30, 29}, ids, "the extra row is not returned")
	require.NotEmpty(t, first.Cursor, "a row past the page must yield a cursor")

	_, err = service.ListResults(context.Background(), classification.ResultFilter{Limit: 2, Cursor: first.Cursor})
	require.NoError(t, err)
	require.NotNil(t, repo.lastQuery)
	require.NotNil(t, repo.lastQuery.After, "the cursor must decode to a keyset position")
	assert.Equal(t, int64(29), repo.lastQuery.After.ID)
	assert.True(t, tie.Equal(repo.lastQuery.After.CreatedAt), "cursor created_at %s, want %s", repo.lastQuery.After.CreatedAt, tie)

	repo.rows = repo.rows[:2]
	exact, err := service.ListResults(context.Background(), classification.ResultFilter{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, exact.Results, 2)
	assert.Empty(t, exact.Cursor, "exactly limit rows means no further page")
}

// TestReviewServiceRecordVerdictRejectsQuestionMismatch refuses a verdict
// that misses or adds a question and stores nothing (B2).
func TestReviewServiceRecordVerdictRejectsQuestionMismatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		present map[string]bool
	}{
		{name: "missing question", present: map[string]bool{"spam_scam": true}},
		{name: "unknown question", present: map[string]bool{"spam_scam": true, "hate_harassment": false, "sexual_explicit": false}},
		{name: "no answers", present: map[string]bool{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &reviewRepositoryFake{questions: []string{"hate_harassment", "spam_scam"}}
			_, err := newReviewService(repo).RecordVerdict(context.Background(), classification.VerdictRequest{
				ResultID: 7, AdminDID: "did:plc:admin", Present: test.present,
			})
			assert.ErrorIs(t, err, classification.ErrInvalidRequest)
			assert.Empty(t, repo.stored, "a refused verdict must not be stored")
		})
	}
}

// TestReviewServiceRecordVerdictStores stores a verdict naming every question,
// stamped with the service clock, and returns the stored verdict (B2).
func TestReviewServiceRecordVerdictStores(t *testing.T) {
	repo := &reviewRepositoryFake{questions: []string{"hate_harassment", "spam_scam"}}
	verdict, err := newReviewService(repo).RecordVerdict(context.Background(), classification.VerdictRequest{
		ResultID: 7, AdminDID: "did:plc:admin", Present: map[string]bool{"spam_scam": true, "hate_harassment": false},
	})
	require.NoError(t, err)

	expected := classification.Verdict{
		ResultID: 7, AdminDID: "did:plc:admin",
		Present:    map[string]bool{"spam_scam": true, "hate_harassment": false},
		RecordedAt: reviewNow,
	}
	assert.Equal(t, []classification.Verdict{expected}, repo.stored)
	assert.Equal(t, expected, verdict)
}

// TestReviewServiceRecordVerdictPropagatesRepositoryErrors keeps
// ErrResultNotFound and ErrUnreviewable identifiable (B2).
func TestReviewServiceRecordVerdictPropagatesRepositoryErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		repo *reviewRepositoryFake
		want error
	}{
		{name: "unknown result", repo: &reviewRepositoryFake{questionsErr: fmt.Errorf("result 7: %w", classification.ErrResultNotFound)}, want: classification.ErrResultNotFound},
		{name: "edited subject", repo: &reviewRepositoryFake{questions: []string{"spam_scam"}, storeErr: fmt.Errorf("result 7: %w", classification.ErrUnreviewable)}, want: classification.ErrUnreviewable},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newReviewService(test.repo).RecordVerdict(context.Background(), classification.VerdictRequest{
				ResultID: 7, AdminDID: "did:plc:admin", Present: map[string]bool{"spam_scam": true},
			})
			assert.ErrorIs(t, err, test.want)
		})
	}
}

// TestReviewServicePrecisionRejectsInvalidRequests requires a model, a policy
// version and a threshold within [0,1] (B2).
func TestReviewServicePrecisionRejectsInvalidRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		request classification.PrecisionRequest
	}{
		{name: "no model", request: classification.PrecisionRequest{PolicyVersion: "v1", Threshold: 0.5}},
		{name: "no policy version", request: classification.PrecisionRequest{ModelID: "model-a", Threshold: 0.5}},
		{name: "threshold below zero", request: classification.PrecisionRequest{ModelID: "model-a", PolicyVersion: "v1", Threshold: -0.01}},
		{name: "threshold above one", request: classification.PrecisionRequest{ModelID: "model-a", PolicyVersion: "v1", Threshold: 1.01}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newReviewService(&reviewRepositoryFake{}).Precision(context.Background(), test.request)
			assert.ErrorIs(t, err, classification.ErrInvalidRequest)
		})
	}
}

// TestReviewServicePrecisionCombinesQuestions lists the sorted union of the
// current policy's questions and the evaluation's stored keys when the policy
// version matches, only the stored keys otherwise, and fills the counts from
// the repository (B2).
func TestReviewServicePrecisionCombinesQuestions(t *testing.T) {
	samples := []classification.VerdictSample{
		{Probabilities: map[string]float64{"spam_scam": 0.9, "legacy_question": 0.2}, Present: map[string]bool{"spam_scam": true, "legacy_question": false}},
		{Probabilities: map[string]float64{"spam_scam": 0.6, "legacy_question": 0.7}, Present: map[string]bool{"spam_scam": false, "legacy_question": true}},
		{Probabilities: map[string]float64{"spam_scam": 0.1, "legacy_question": 0.8}, Present: map[string]bool{"spam_scam": true, "legacy_question": true}},
	}
	for _, test := range []struct {
		name          string
		policyVersion string
		expected      []expectedPrecision
	}{
		{
			name: "current policy", policyVersion: "v1",
			expected: []expectedPrecision{
				{question: "hate_harassment"},
				{question: "legacy_question", atOrAbove: 2, present: 2, truePositive: 2, precision: ratio(1), recall: ratio(1)},
				{question: "spam_scam", atOrAbove: 2, present: 2, truePositive: 1, precision: ratio(0.5), recall: ratio(0.5)},
			},
		},
		{
			name: "other policy version", policyVersion: "v0",
			expected: []expectedPrecision{
				{question: "legacy_question", atOrAbove: 2, present: 2, truePositive: 2, precision: ratio(1), recall: ratio(1)},
				{question: "spam_scam", atOrAbove: 2, present: 2, truePositive: 1, precision: ratio(0.5), recall: ratio(0.5)},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &reviewRepositoryFake{samples: classification.PrecisionSamples{
				Samples: samples, Questions: []string{"spam_scam", "legacy_question"}, Unreviewable: 4,
			}}
			report, err := newReviewService(repo).Precision(context.Background(), classification.PrecisionRequest{
				ModelID: "model-a", PolicyVersion: test.policyVersion, Threshold: 0.5,
			})
			require.NoError(t, err)
			assert.Equal(t, []string{"model-a", test.policyVersion}, repo.samplesRequested, "the evaluation is the model and policy version")
			assert.Equal(t, "model-a", report.ModelID)
			assert.Equal(t, test.policyVersion, report.PolicyVersion)
			assert.InDelta(t, 0.5, report.Threshold, 1e-9)
			assert.Equal(t, 3, report.VerdictedCount)
			assert.Equal(t, 4, report.UnreviewableCount)
			assertPrecisions(t, test.expected, report.Questions)
		})
	}
}
