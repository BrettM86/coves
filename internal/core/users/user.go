package users

import (
	"time"
)

// User represents an atProto user tracked in the Coves AppView
// This is NOT the user's repository - that lives in the PDS
// This table only tracks metadata for efficient AppView queries
type User struct {
	DID       string    `json:"did" db:"did"`           // atProto DID (e.g., did:plc:xyz123)
	Handle    string    `json:"handle" db:"handle"`     // Human-readable handle (e.g., alice.coves.dev)
	PDSURL    string    `json:"pdsUrl" db:"pds_url"`    // User's PDS host URL (supports federation)
	CreatedAt time.Time `json:"createdAt" db:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" db:"updated_at"`
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
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	AccessJwt   string `json:"accessJwt"`
	RefreshJwt  string `json:"refreshJwt"`
	PDSURL      string `json:"pdsUrl"`
}
