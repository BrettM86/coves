package classification

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Review errors.
var (
	ErrInvalidRequest = errors.New("invalid classification review request")
	ErrInvalidCursor  = errors.New("invalid classification result cursor")
	ErrResultNotFound = errors.New("classification result not found")
	ErrUnreviewable   = errors.New("classification result is not reviewable")
)

// CommentFields are the classified fields of a comment.
type CommentFields struct{ Body string }

// SubjectView is a classified subject as the worker would load it now.
type SubjectView struct {
	Visibility SubjectVisibility
	Deleted    bool
	CurrentCID string
	Post       *PostFields
	Comment    *CommentFields
	Community  *CommunityFields
	Images     []ImageRef
}

// ResultFilter narrows a classification result listing.
type ResultFilter struct {
	Question       string
	MinProbability *float64
	SubjectKind    SubjectKind
	CommunityDID   string
	ModelID        string
	PolicyVersion  string
	Since, Until   time.Time
	Limit          int
	Cursor         string
}

// ResultView is one stored classification result with its subject's current view.
type ResultView struct {
	ID            int64
	SubjectKind   SubjectKind
	SubjectURI    string
	ScoredCID     string
	CommunityDID  string
	AuthorDID     string
	ModelID       string
	PolicyVersion string
	Probabilities map[string]float64
	ImagesSent    int
	ImagesSkipped int
	TextTruncated bool
	// Partial marks a result stored from the answered batches of a job that
	// failed at the attempt limit. It accepts a verdict like any result.
	Partial            bool
	CreatedAt          time.Time
	ScoredCIDIsCurrent bool
	Subject            SubjectView
}

// ResultPage is one page of results.
type ResultPage struct {
	Results []ResultView
	Cursor  string
}

// ResultCursor is the keyset position after which a page starts.
type ResultCursor struct {
	CreatedAt time.Time
	ID        int64
}

// ResultQuery is the repository form of a result listing.
type ResultQuery struct {
	Filter ResultFilter
	After  *ResultCursor
	Limit  int
}

// VerdictRequest is an admin's verdict on a result.
type VerdictRequest struct {
	ResultID int64
	AdminDID string
	Present  map[string]bool
}

// Verdict is a stored verdict.
type Verdict struct {
	ResultID   int64
	AdminDID   string
	Present    map[string]bool
	RecordedAt time.Time
}

// VerdictSample is one verdicted reviewable result.
type VerdictSample struct {
	Probabilities map[string]float64
	Present       map[string]bool
}

// PrecisionSamples are an evaluation's verdicted reviewable results.
type PrecisionSamples struct {
	Samples      []VerdictSample
	Questions    []string
	Unreviewable int
}

// PrecisionRequest names the evaluation and threshold to measure.
type PrecisionRequest struct {
	ModelID       string
	PolicyVersion string
	Threshold     float64
}

// QuestionPrecision is one question's precision and recall.
type QuestionPrecision struct {
	Question                         string
	AtOrAbove, Present, TruePositive int
	Precision, Recall                *float64
}

// PrecisionReport is an evaluation's precision at a threshold.
type PrecisionReport struct {
	ModelID                           string
	PolicyVersion                     string
	Threshold                         float64
	VerdictedCount, UnreviewableCount int
	Questions                         []QuestionPrecision
}

// ReviewRepository stores results and verdicts for review.
type ReviewRepository interface {
	ListResults(ctx context.Context, query ResultQuery) ([]ResultView, error)
	ResultQuestions(ctx context.Context, resultID int64) ([]string, error)
	StoreVerdict(ctx context.Context, verdict Verdict) (Verdict, error)
	PrecisionSamples(ctx context.Context, modelID, policyVersion string) (PrecisionSamples, error)
}

// Result listing page sizes.
const (
	defaultResultLimit = 50
	maximumResultLimit = 100
)

// ComputePrecision computes per-question precision and recall at threshold.
// A probability equal to the threshold counts as at or above it. A sample
// without a probability for a question is ignored for that question.
// Precision is nil when no sample is at or above the threshold, and recall is
// nil when no sample was verdicted present.
func ComputePrecision(questions []string, samples []VerdictSample, threshold float64) []QuestionPrecision {
	precisions := make([]QuestionPrecision, 0, len(questions))
	for _, question := range questions {
		row := QuestionPrecision{Question: question}
		for _, sample := range samples {
			probability, scored := sample.Probabilities[question]
			if !scored {
				continue
			}
			atOrAbove := probability >= threshold
			present := sample.Present[question]
			if atOrAbove {
				row.AtOrAbove++
			}
			if present {
				row.Present++
			}
			if atOrAbove && present {
				row.TruePositive++
			}
		}
		if row.AtOrAbove > 0 {
			value := float64(row.TruePositive) / float64(row.AtOrAbove)
			row.Precision = &value
		}
		if row.Present > 0 {
			value := float64(row.TruePositive) / float64(row.Present)
			row.Recall = &value
		}
		precisions = append(precisions, row)
	}
	return precisions
}

// ReviewService lists results, records verdicts and reports precision.
type ReviewService struct {
	repository ReviewRepository
	policy     Policy
	now        func() time.Time
}

// NewReviewService builds the review service.
func NewReviewService(repo ReviewRepository, policy Policy, now func() time.Time) *ReviewService {
	return &ReviewService{repository: repo, policy: policy, now: now}
}

// ListResults lists results matching filter, newest first, one page at a
// time. The returned cursor is empty when no further page exists.
func (s *ReviewService) ListResults(ctx context.Context, filter ResultFilter) (ResultPage, error) {
	if err := validateResultFilter(filter); err != nil {
		return ResultPage{}, err
	}
	limit := filter.Limit
	if limit == 0 {
		limit = defaultResultLimit
	}
	query := ResultQuery{Filter: filter, Limit: limit + 1}
	query.Filter.Limit = limit
	if filter.Cursor != "" {
		after, err := decodeResultCursor(filter.Cursor)
		if err != nil {
			return ResultPage{}, err
		}
		query.After = &after
	}

	rows, err := s.repository.ListResults(ctx, query)
	if err != nil {
		return ResultPage{}, fmt.Errorf("list classification results: %w", err)
	}
	if len(rows) <= limit {
		return ResultPage{Results: rows}, nil
	}
	rows = rows[:limit]
	last := rows[len(rows)-1]
	return ResultPage{
		Results: rows,
		Cursor:  encodeResultCursor(ResultCursor{CreatedAt: last.CreatedAt, ID: last.ID}),
	}, nil
}

func validateResultFilter(filter ResultFilter) error {
	if filter.MinProbability != nil {
		if filter.Question == "" {
			return fmt.Errorf("%w: minProbability requires question", ErrInvalidRequest)
		}
		if !isProbability(*filter.MinProbability) {
			return fmt.Errorf("%w: minProbability must be between 0 and 1", ErrInvalidRequest)
		}
	}
	switch filter.SubjectKind {
	case "", SubjectKindPost, SubjectKindComment, SubjectKindCommunity:
	default:
		return fmt.Errorf("%w: unknown subject kind %q", ErrInvalidRequest, filter.SubjectKind)
	}
	if filter.Limit < 0 || filter.Limit > maximumResultLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidRequest, maximumResultLimit)
	}
	return nil
}

func isProbability(value float64) bool {
	return value >= 0 && value <= 1
}

// cursorSeparator splits a cursor's created_at from its id. RFC 3339
// timestamps never contain it.
const cursorSeparator = "|"

func encodeResultCursor(cursor ResultCursor) string {
	raw := cursor.CreatedAt.UTC().Format(time.RFC3339Nano) + cursorSeparator + strconv.FormatInt(cursor.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeResultCursor(encoded string) (ResultCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ResultCursor{}, fmt.Errorf("%w: not base64", ErrInvalidCursor)
	}
	createdAtText, idText, found := strings.Cut(string(raw), cursorSeparator)
	if !found {
		return ResultCursor{}, fmt.Errorf("%w: missing position", ErrInvalidCursor)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdAtText)
	if err != nil {
		return ResultCursor{}, fmt.Errorf("%w: bad created_at", ErrInvalidCursor)
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return ResultCursor{}, fmt.Errorf("%w: bad id", ErrInvalidCursor)
	}
	return ResultCursor{CreatedAt: createdAt.UTC(), ID: id}, nil
}

// RecordVerdict records an admin's verdict. The verdict must answer every
// question the result scored, and no other.
func (s *ReviewService) RecordVerdict(ctx context.Context, request VerdictRequest) (Verdict, error) {
	questions, err := s.repository.ResultQuestions(ctx, request.ResultID)
	if err != nil {
		return Verdict{}, fmt.Errorf("load questions of classification result %d: %w", request.ResultID, err)
	}
	if err := validateVerdictQuestions(questions, request.Present); err != nil {
		return Verdict{}, err
	}
	stored, err := s.repository.StoreVerdict(ctx, Verdict{
		ResultID:   request.ResultID,
		AdminDID:   request.AdminDID,
		Present:    request.Present,
		RecordedAt: s.now(),
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("store verdict on classification result %d: %w", request.ResultID, err)
	}
	return stored, nil
}

func validateVerdictQuestions(questions []string, present map[string]bool) error {
	expected := make(map[string]struct{}, len(questions))
	for _, question := range questions {
		expected[question] = struct{}{}
	}
	for question := range present {
		if _, known := expected[question]; !known {
			return fmt.Errorf("%w: result did not score question %q", ErrInvalidRequest, question)
		}
	}
	for question := range expected {
		if _, answered := present[question]; !answered {
			return fmt.Errorf("%w: verdict does not answer question %q", ErrInvalidRequest, question)
		}
	}
	return nil
}

// Precision reports an evaluation's precision at a threshold. The questions
// are the sorted union of the evaluation's stored questions and, when the
// evaluation uses the loaded policy version, that policy's questions.
func (s *ReviewService) Precision(ctx context.Context, request PrecisionRequest) (PrecisionReport, error) {
	if request.ModelID == "" || request.PolicyVersion == "" {
		return PrecisionReport{}, fmt.Errorf("%w: modelId and policyVersion are required", ErrInvalidRequest)
	}
	if !isProbability(request.Threshold) {
		return PrecisionReport{}, fmt.Errorf("%w: threshold must be between 0 and 1", ErrInvalidRequest)
	}
	samples, err := s.repository.PrecisionSamples(ctx, request.ModelID, request.PolicyVersion)
	if err != nil {
		return PrecisionReport{}, fmt.Errorf("load precision samples: %w", err)
	}

	questionSet := make(map[string]struct{}, len(samples.Questions)+len(s.policy.Questions))
	for _, question := range samples.Questions {
		questionSet[question] = struct{}{}
	}
	if request.PolicyVersion == s.policy.Version {
		for _, question := range s.policy.Questions {
			questionSet[question.Key] = struct{}{}
		}
	}
	questions := make([]string, 0, len(questionSet))
	for question := range questionSet {
		questions = append(questions, question)
	}
	sort.Strings(questions)

	return PrecisionReport{
		ModelID:           request.ModelID,
		PolicyVersion:     request.PolicyVersion,
		Threshold:         request.Threshold,
		VerdictedCount:    len(samples.Samples),
		UnreviewableCount: samples.Unreviewable,
		Questions:         ComputePrecision(questions, samples.Samples, request.Threshold),
	}, nil
}
