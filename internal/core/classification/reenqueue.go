package classification

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Operations errors.
var (
	ErrClassifierNotConfigured = errors.New("content classifier is not configured")
	ErrCommunityNotFound       = errors.New("community not found")
)

// ReenqueueTarget names what a re-enqueue request queues.
type ReenqueueTarget string

// Re-enqueue targets.
const (
	ReenqueueAllCommunities   ReenqueueTarget = "allCommunities"
	ReenqueuePostsAndComments ReenqueueTarget = "postsAndComments"
)

// ReenqueueRequest asks to queue jobs for existing content.
type ReenqueueRequest struct {
	Target       ReenqueueTarget
	Since, Until time.Time
}

// ReenqueueReport counts the subjects a re-enqueue covered: jobs created,
// failed jobs under the same evaluation re-queued (Revived), and subjects
// whose identical job already existed in another status.
type ReenqueueReport struct {
	Created, Revived, AlreadyPresent int
}

// ReenqueueRepository inserts jobs for existing content.
type ReenqueueRepository interface {
	EnqueueCommunities(ctx context.Context, evaluation Evaluation) (ReenqueueReport, error)
	EnqueuePostsAndComments(ctx context.Context, evaluation Evaluation, since, until time.Time) (ReenqueueReport, error)
}

// ReenqueueService queues classification jobs for existing content.
type ReenqueueService struct {
	repository ReenqueueRepository
	evaluation *Evaluation
}

// NewReenqueueService builds the re-enqueue service.
func NewReenqueueService(repo ReenqueueRepository, evaluation *Evaluation) *ReenqueueService {
	return &ReenqueueService{repository: repo, evaluation: evaluation}
}

// Reenqueue queues jobs for the requested target under the current
// evaluation. postsAndComments covers content indexed in [Since, Until);
// allCommunities takes no range. It returns ErrClassifierNotConfigured when
// the service has no evaluation.
func (s *ReenqueueService) Reenqueue(ctx context.Context, req ReenqueueRequest) (ReenqueueReport, error) {
	if err := validateReenqueueRequest(req); err != nil {
		return ReenqueueReport{}, err
	}
	if s.evaluation == nil {
		return ReenqueueReport{}, ErrClassifierNotConfigured
	}
	if req.Target == ReenqueueAllCommunities {
		report, err := s.repository.EnqueueCommunities(ctx, *s.evaluation)
		if err != nil {
			return ReenqueueReport{}, fmt.Errorf("enqueue communities: %w", err)
		}
		return report, nil
	}
	report, err := s.repository.EnqueuePostsAndComments(ctx, *s.evaluation, req.Since, req.Until)
	if err != nil {
		return ReenqueueReport{}, fmt.Errorf("enqueue posts and comments: %w", err)
	}
	return report, nil
}

func validateReenqueueRequest(req ReenqueueRequest) error {
	switch req.Target {
	case ReenqueueAllCommunities:
		if !req.Since.IsZero() || !req.Until.IsZero() {
			return fmt.Errorf("%w: allCommunities takes no since or until", ErrInvalidRequest)
		}
	case ReenqueuePostsAndComments:
		if req.Since.IsZero() || req.Until.IsZero() {
			return fmt.Errorf("%w: postsAndComments requires since and until", ErrInvalidRequest)
		}
		if !req.Since.Before(req.Until) {
			return fmt.Errorf("%w: since must be before until", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unknown target %q", ErrInvalidRequest, req.Target)
	}
	return nil
}
