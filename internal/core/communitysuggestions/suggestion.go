package communitysuggestions

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Status represents the processing status of a community suggestion
type Status string

// Valid status values for community suggestions
const (
	StatusOpen        Status = "open"
	StatusUnderReview Status = "under_review"
	StatusApproved    Status = "approved"
	StatusDeclined    Status = "declined"
)

// MaxTitleLength is the maximum number of characters allowed in a suggestion title
const MaxTitleLength = 200

// MaxDescriptionLength is the maximum number of characters allowed in a suggestion description
const MaxDescriptionLength = 5000

// MaxSuggestionsPerDay is the maximum number of suggestions a single user can create per day
const MaxSuggestionsPerDay = 3

// ValidStatuses returns all valid status values
func ValidStatuses() []Status {
	return []Status{StatusOpen, StatusUnderReview, StatusApproved, StatusDeclined}
}

// IsValidStatus checks if a status value is valid
func IsValidStatus(status string) bool {
	for _, s := range ValidStatuses() {
		if string(s) == status {
			return true
		}
	}
	return false
}

// IsValidVoteValue checks if a vote value is valid (must be 1 or -1)
func IsValidVoteValue(v int) bool {
	return v == 1 || v == -1
}

// CommunitySuggestion represents a community suggestion in the AppView database
type CommunitySuggestion struct {
	ID           int64        `json:"id" db:"id"`
	Title        string       `json:"title" db:"title"`
	Description  string       `json:"description" db:"description"`
	SubmitterDID string       `json:"submitterDid" db:"submitter_did"`
	Status       Status       `json:"status" db:"status"`
	VoteCount    int          `json:"voteCount" db:"vote_count"`
	CreatedAt    time.Time    `json:"createdAt" db:"created_at"`
	UpdatedAt    time.Time    `json:"updatedAt" db:"updated_at"`
	Viewer       *ViewerState `json:"viewer,omitempty"`
}

// ViewerState contains information about the authenticated viewer's relationship
// to a community suggestion (e.g., their vote)
type ViewerState struct {
	Vote *int `json:"vote"`
}

// SuggestionVote represents a single vote on a community suggestion
type SuggestionVote struct {
	ID           int64     `json:"id" db:"id"`
	SuggestionID int64     `json:"suggestionId" db:"suggestion_id"`
	VoterDID     string    `json:"voterDid" db:"voter_did"`
	Value        int       `json:"value" db:"value"`
	CreatedAt    time.Time `json:"createdAt" db:"created_at"`
}

// CreateSuggestionRequest contains the data needed to create a new community suggestion
type CreateSuggestionRequest struct {
	// Title is the title of the community suggestion
	Title string

	// Description is a detailed description of the suggested community
	Description string

	// SubmitterDID is the DID of the user submitting the suggestion
	SubmitterDID string
}

// Validate validates the CreateSuggestionRequest and returns an error if invalid
func (r *CreateSuggestionRequest) Validate() error {
	if r.SubmitterDID == "" {
		return ErrSubmitterRequired
	}

	r.Title = strings.TrimSpace(r.Title)
	if r.Title == "" {
		return ErrTitleRequired
	}
	if utf8.RuneCountInString(r.Title) > MaxTitleLength {
		return ErrTitleTooLong
	}

	r.Description = strings.TrimSpace(r.Description)
	if r.Description == "" {
		return ErrDescriptionRequired
	}
	if utf8.RuneCountInString(r.Description) > MaxDescriptionLength {
		return ErrDescriptionTooLong
	}

	return nil
}

// ListSuggestionsRequest contains the parameters for listing community suggestions
type ListSuggestionsRequest struct {
	// Sort determines the ordering: "popular" (vote_count DESC) or "new" (created_at DESC)
	Sort string

	// Status optionally filters by suggestion status
	Status string

	// Limit is the maximum number of results to return
	Limit int

	// Offset is the number of results to skip for pagination
	Offset int

	// ViewerDID is the DID of the authenticated viewer (for populating viewer state)
	ViewerDID string
}

// Validate validates the ListSuggestionsRequest, applying defaults for missing values
func (r *ListSuggestionsRequest) Validate() error {
	// Default/validate sort
	if r.Sort == "" {
		r.Sort = "popular"
	}
	if r.Sort != "popular" && r.Sort != "new" {
		return fmt.Errorf("invalid sort value: must be popular or new")
	}

	// Validate status if provided
	if r.Status != "" && !IsValidStatus(r.Status) {
		return ErrInvalidStatus
	}

	// Default/validate limit
	if r.Limit <= 0 {
		r.Limit = 50
	}
	if r.Limit > 100 {
		r.Limit = 100
	}

	// Validate offset
	if r.Offset < 0 {
		r.Offset = 0
	}

	return nil
}

// VoteRequest contains the data needed to cast a vote on a community suggestion
type VoteRequest struct {
	// SuggestionID is the ID of the suggestion to vote on
	SuggestionID int64

	// VoterDID is the DID of the user casting the vote
	VoterDID string

	// Value is the vote value: 1 (upvote) or -1 (downvote)
	Value int
}

// Validate validates the VoteRequest and returns an error if invalid
func (r *VoteRequest) Validate() error {
	if r.SuggestionID <= 0 {
		return ErrInvalidSuggestionID
	}
	if r.VoterDID == "" {
		return ErrVoterRequired
	}
	if !IsValidVoteValue(r.Value) {
		return ErrInvalidVoteValue
	}
	return nil
}

// UpdateStatusRequest contains the data needed to update a suggestion's status
type UpdateStatusRequest struct {
	// SuggestionID is the ID of the suggestion to update
	SuggestionID int64

	// Status is the new status value
	Status Status

	// AdminDID is the DID of the admin performing the update
	AdminDID string
}

// Validate validates the UpdateStatusRequest and returns an error if invalid
func (r *UpdateStatusRequest) Validate() error {
	if r.SuggestionID <= 0 {
		return ErrInvalidSuggestionID
	}
	if r.AdminDID == "" {
		return ErrNotAuthorized
	}
	if !IsValidStatus(string(r.Status)) {
		return ErrInvalidStatus
	}
	return nil
}
