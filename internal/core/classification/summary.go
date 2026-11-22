package classification

import (
	"context"
	"fmt"
	"time"
)

// summaryWindow is how far back a community summary looks.
const summaryWindow = 7 * 24 * time.Hour

// SummaryRequest asks for one community's summary for one question.
type SummaryRequest struct {
	CommunityDID, Question string
	MinProbability         float64
}

// SummaryQuery is the repository form of a summary request.
type SummaryQuery struct {
	CommunityDID, Question string
	MinProbability         float64
	Since, Until           time.Time
}

// KindSummary counts one kind of subject.
type KindSummary struct {
	Scored, Above int
}

// FlagEvent is one change of a community's NSFW flag.
type FlagEvent struct {
	OldNSFW, NewNSFW bool
	OccurredAt       time.Time
	Delivered        bool
}

// CommunitySummary is one community's classification summary.
type CommunitySummary struct {
	CommunityDID, Question string
	MinProbability         float64
	Since, Until           time.Time
	Posts, Comments        KindSummary
	FlagEvents             []FlagEvent
}

// Share is the fraction of scored subjects above the threshold, and false
// when nothing was scored.
func (s CommunitySummary) Share() (float64, bool) {
	scored := s.Posts.Scored + s.Comments.Scored
	if scored == 0 {
		return 0, false
	}
	return float64(s.Posts.Above+s.Comments.Above) / float64(scored), true
}

// SummaryRepository reads a community's summary. It returns
// ErrCommunityNotFound for an unknown community.
type SummaryRepository interface {
	CommunitySummary(ctx context.Context, query SummaryQuery) (CommunitySummary, error)
}

// SummaryService builds community classification summaries.
type SummaryService struct {
	repository SummaryRepository
	policy     Policy
	now        func() time.Time
}

// NewSummaryService builds the summary service.
func NewSummaryService(repo SummaryRepository, policy Policy, now func() time.Time) *SummaryService {
	return &SummaryService{repository: repo, policy: policy, now: now}
}

// Summary returns the community's summary over [now - 7 days, now). It
// returns ErrCommunityNotFound for an unknown community.
func (s *SummaryService) Summary(ctx context.Context, req SummaryRequest) (CommunitySummary, error) {
	if req.CommunityDID == "" {
		return CommunitySummary{}, fmt.Errorf("%w: community is required", ErrInvalidRequest)
	}
	if !s.policyHasQuestion(req.Question) {
		return CommunitySummary{}, fmt.Errorf("%w: question %q is not in policy %s", ErrInvalidRequest, req.Question, s.policy.Version)
	}
	if !isProbability(req.MinProbability) {
		return CommunitySummary{}, fmt.Errorf("%w: minProbability must be between 0 and 1", ErrInvalidRequest)
	}
	until := s.now().UTC()
	summary, err := s.repository.CommunitySummary(ctx, SummaryQuery{
		CommunityDID:   req.CommunityDID,
		Question:       req.Question,
		MinProbability: req.MinProbability,
		Since:          until.Add(-summaryWindow),
		Until:          until,
	})
	if err != nil {
		return CommunitySummary{}, fmt.Errorf("community classification summary: %w", err)
	}
	return summary, nil
}

func (s *SummaryService) policyHasQuestion(key string) bool {
	for _, question := range s.policy.Questions {
		if question.Key == key {
			return true
		}
	}
	return false
}
