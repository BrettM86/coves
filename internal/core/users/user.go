package users

import (
	"time"
)

// User represents an atProto user tracked in the Coves AppView
// This is NOT the user's repository - that lives in the PDS
// This table only tracks metadata for efficient AppView queries
type User struct {
	CreatedAt time.Time `json:"createdAt" db:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" db:"updated_at"`
	DID       string    `json:"did" db:"did"`
	Handle    string    `json:"handle" db:"handle"`
	PDSURL    string    `json:"pdsUrl" db:"pds_url"`
}

// CreateUserRequest represents the input for creating a new user
type CreateUserRequest struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
	PDSURL string `json:"pdsUrl"` // User's PDS host URL
}

// RegisterAccountRequest represents the input for registering a new account on the PDS
type RegisterAccountRequest struct {
	Handle     string `json:"handle"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	InviteCode string `json:"inviteCode,omitempty"`
}

// RegisterAccountResponse represents the response from PDS account creation
type RegisterAccountResponse struct {
	DID        string `json:"did"`
	Handle     string `json:"handle"`
	AccessJwt  string `json:"accessJwt"`
	RefreshJwt string `json:"refreshJwt"`
	PDSURL     string `json:"pdsUrl"`
}

// ProfileStats contains aggregated user statistics
// Matches the social.coves.actor.defs#profileStats lexicon
type ProfileStats struct {
	PostCount       int `json:"postCount"`
	CommentCount    int `json:"commentCount"`
	CommunityCount  int `json:"communityCount"`  // Number of communities subscribed to
	Reputation      int `json:"reputation"`      // Global reputation score (sum across communities)
	MembershipCount int `json:"membershipCount"` // Number of communities with active membership
}

// ProfileViewDetailed is the full profile response
// Matches the social.coves.actor.defs#profileViewDetailed lexicon
type ProfileViewDetailed struct {
	DID       string        `json:"did"`
	Handle    string        `json:"handle,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
	Stats     *ProfileStats `json:"stats,omitempty"`
	// Future fields (require additional infrastructure):
	// DisplayName, Bio, Avatar, Banner (from PDS profile record)
	// Viewer (requires user-to-user blocking infrastructure)
}
