package communitysuggestions

import (
	"context"
	"time"
)

// service implements the Service interface for community suggestions
type service struct {
	repo Repository
}

// NewService creates a new community suggestions service
func NewService(repo Repository) Service {
	return &service{
		repo: repo,
	}
}

// CreateSuggestion validates the request, checks the rate limit, and creates a new suggestion
func (s *service) CreateSuggestion(ctx context.Context, req CreateSuggestionRequest) (*CommunitySuggestion, error) {
	// Validate the request
	if err := req.Validate(); err != nil {
		return nil, err
	}

	// Check rate limit: max 3 suggestions per day per user
	since := time.Now().UTC().Add(-24 * time.Hour)
	count, err := s.repo.CountBySubmitterSince(ctx, req.SubmitterDID, since)
	if err != nil {
		return nil, err
	}
	if count >= MaxSuggestionsPerDay {
		return nil, ErrRateLimitExceeded
	}

	// Create the suggestion
	suggestion := &CommunitySuggestion{
		Title:        req.Title,
		Description:  req.Description,
		SubmitterDID: req.SubmitterDID,
		Status:       StatusOpen,
	}

	if err := s.repo.Create(ctx, suggestion); err != nil {
		return nil, err
	}

	return suggestion, nil
}

// GetSuggestion retrieves a suggestion by ID and populates viewer state if viewerDID is provided
func (s *service) GetSuggestion(ctx context.Context, id int64, viewerDID string) (*CommunitySuggestion, error) {
	suggestion, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Populate viewer state if authenticated
	if viewerDID != "" {
		vote, err := s.repo.GetVote(ctx, id, viewerDID)
		if err != nil && !IsNotFound(err) {
			return nil, err
		}
		if vote != nil {
			suggestion.Viewer = &ViewerState{Vote: &vote.Value}
		}
	}

	return suggestion, nil
}

// ListSuggestions retrieves suggestions with filtering, sorting, and pagination
// Populates viewer state for all returned suggestions if viewerDID is provided
func (s *service) ListSuggestions(ctx context.Context, req ListSuggestionsRequest) ([]*CommunitySuggestion, error) {
	suggestions, err := s.repo.List(ctx, req)
	if err != nil {
		return nil, err
	}

	// Batch populate viewer state if authenticated
	if req.ViewerDID != "" && len(suggestions) > 0 {
		ids := make([]int64, len(suggestions))
		for i, sg := range suggestions {
			ids[i] = sg.ID
		}

		votes, err := s.repo.GetVotesForViewer(ctx, req.ViewerDID, ids)
		if err != nil {
			return nil, err
		}

		for _, sg := range suggestions {
			if v, ok := votes[sg.ID]; ok {
				sg.Viewer = &ViewerState{Vote: &v}
			}
		}
	}

	return suggestions, nil
}

// Vote handles casting, toggling, and flipping votes on a suggestion
// - If no existing vote: create a new vote
// - If existing vote in the same direction: remove the vote (toggle off)
// - If existing vote in the opposite direction: flip the vote
func (s *service) Vote(ctx context.Context, req VoteRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return s.repo.AtomicVote(ctx, req.SuggestionID, req.VoterDID, req.Value)
}

// RemoveVote removes a user's vote from a suggestion
func (s *service) RemoveVote(ctx context.Context, suggestionID int64, voterDID string) error {
	if suggestionID <= 0 {
		return ErrInvalidSuggestionID
	}
	if voterDID == "" {
		return ErrVoterRequired
	}

	_, err := s.repo.DeleteVote(ctx, suggestionID, voterDID)
	return err
}

// UpdateStatus validates the request and updates the suggestion's status
func (s *service) UpdateStatus(ctx context.Context, req UpdateStatusRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return s.repo.UpdateStatus(ctx, req.SuggestionID, req.Status)
}
