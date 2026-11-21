package adminreports

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"Coves/internal/core/communities"
)

const (
	// DefaultCommunityQueueLimit is the page size when a request names none.
	DefaultCommunityQueueLimit = 50
	// MaxCommunityQueueLimit is the largest page a request may ask for.
	MaxCommunityQueueLimit = 100
	// MaxDismissIdempotencyKeyBytes bounds a dismissal's idempotency key.
	MaxDismissIdempotencyKeyBytes = 128
)

// Community report queue errors.
var (
	// ErrCommunityQueueForbidden indicates the caller may not read or act on
	// the community's report queue.
	ErrCommunityQueueForbidden = errors.New("community report queue forbidden")

	// ErrInvalidQueueRequest indicates a malformed community queue request.
	ErrInvalidQueueRequest = errors.New("invalid community report queue request")

	// ErrIdempotencyConflict indicates an idempotency key reused for a
	// different subject.
	ErrIdempotencyConflict = errors.New("idempotency key already used for a different subject")
)

// CommunityQueueEntry is one reported subject in a community's queue.
type CommunityQueueEntry struct {
	SubjectURI      string
	SubjectType     TargetType
	Reasons         []Reason
	ReportCount     int
	FirstReportedAt time.Time
	LastReportedAt  time.Time
}

// CommunityQueuePage is one page of a community's report queue.
type CommunityQueuePage struct {
	Entries []CommunityQueueEntry
	Cursor  string
}

// DismissCommunitySubjectRequest is a repository dismissal of one subject.
type DismissCommunitySubjectRequest struct {
	CommunityDID   string
	SubjectURI     string
	ModeratorDID   string
	IdempotencyKey string
}

// DismissResult is the outcome of a dismissal.
type DismissResult struct {
	DismissedCount int
}

// CommunityQueueRepository reads and dismisses community queue entries.
type CommunityQueueRepository interface {
	ListCommunityQueue(ctx context.Context, communityDID string, limit int, cursor string) (CommunityQueuePage, error)
	DismissCommunitySubject(ctx context.Context, req DismissCommunitySubjectRequest) (DismissResult, error)
}

// CommunityAuthorizer decides community role authority.
type CommunityAuthorizer interface {
	Authorize(ctx context.Context, communityDID, callerDID, required string) (communities.RoleAuthorization, error)
}

// ListCommunityQueueRequest asks for one page of a community's queue.
type ListCommunityQueueRequest struct {
	CommunityDID string
	CallerDID    string
	Limit        int
	Cursor       string
}

// DismissCommunityReportsRequest asks to dismiss a subject's reports.
type DismissCommunityReportsRequest struct {
	CommunityDID   string
	CallerDID      string
	SubjectURI     string
	IdempotencyKey string
}

// CommunityQueueService serves a community's report queue.
type CommunityQueueService struct {
	repo       CommunityQueueRepository
	authorizer CommunityAuthorizer
}

// NewCommunityQueueService builds the community queue service.
func NewCommunityQueueService(repo CommunityQueueRepository, authorizer CommunityAuthorizer) *CommunityQueueService {
	return &CommunityQueueService{repo: repo, authorizer: authorizer}
}

// ListQueue returns one page of the community's queue. The caller must be
// the community's owner or a moderator, or an instance admin; a refused
// caller reads nothing.
func (s *CommunityQueueService) ListQueue(ctx context.Context, req ListCommunityQueueRequest) (CommunityQueuePage, error) {
	if err := validateCommunityDID(req.CommunityDID); err != nil {
		return CommunityQueuePage{}, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = DefaultCommunityQueueLimit
	}
	if limit < 1 || limit > MaxCommunityQueueLimit {
		return CommunityQueuePage{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidQueueRequest, MaxCommunityQueueLimit)
	}
	if err := s.authorize(ctx, req.CommunityDID, req.CallerDID); err != nil {
		return CommunityQueuePage{}, err
	}
	page, err := s.repo.ListCommunityQueue(ctx, req.CommunityDID, limit, req.Cursor)
	if err != nil {
		return CommunityQueuePage{}, fmt.Errorf("listing the report queue of %s: %w", req.CommunityDID, err)
	}
	return page, nil
}

// Dismiss dismisses the community-visible open reports on one subject as the
// caller, who must be the community's owner or a moderator, or an instance
// admin.
func (s *CommunityQueueService) Dismiss(ctx context.Context, req DismissCommunityReportsRequest) (DismissResult, error) {
	if err := validateCommunityDID(req.CommunityDID); err != nil {
		return DismissResult{}, err
	}
	if _, err := syntax.ParseATURI(req.SubjectURI); err != nil {
		return DismissResult{}, fmt.Errorf("%w: subject must be an AT-URI", ErrInvalidQueueRequest)
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > MaxDismissIdempotencyKeyBytes {
		return DismissResult{}, fmt.Errorf("%w: idempotencyKey must be 1 to %d bytes", ErrInvalidQueueRequest, MaxDismissIdempotencyKeyBytes)
	}
	if err := s.authorize(ctx, req.CommunityDID, req.CallerDID); err != nil {
		return DismissResult{}, err
	}
	result, err := s.repo.DismissCommunitySubject(ctx, DismissCommunitySubjectRequest{
		CommunityDID:   req.CommunityDID,
		SubjectURI:     req.SubjectURI,
		ModeratorDID:   req.CallerDID,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return DismissResult{}, fmt.Errorf("dismissing the reports on %s in %s: %w", req.SubjectURI, req.CommunityDID, err)
	}
	return result, nil
}

func validateCommunityDID(communityDID string) error {
	if _, err := syntax.ParseDID(communityDID); err != nil {
		return fmt.Errorf("%w: community must be a DID", ErrInvalidQueueRequest)
	}
	return nil
}

// authorize requires moderator authority in the community; a missing role or
// a removed community is ErrCommunityQueueForbidden.
func (s *CommunityQueueService) authorize(ctx context.Context, communityDID, callerDID string) error {
	if callerDID == "" {
		return fmt.Errorf("%w: no caller", ErrCommunityQueueForbidden)
	}
	_, err := s.authorizer.Authorize(ctx, communityDID, callerDID, communities.RoleModerator)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, communities.ErrUnauthorized), errors.Is(err, communities.ErrCommunityRemoved):
		return fmt.Errorf("%w: %w", ErrCommunityQueueForbidden, err)
	default:
		return fmt.Errorf("authorizing the report queue of %s: %w", communityDID, err)
	}
}
