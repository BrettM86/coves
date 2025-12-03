package votes

import (
	"context"
	"time"
)

// SubjectValidator validates that vote subjects (posts/comments) exist
// This prevents creating votes on non-existent content
type SubjectValidator interface {
	// SubjectExists checks if a post or comment exists at the given URI
	// Returns true if found, false if not found
	SubjectExists(ctx context.Context, uri string) (bool, error)
}

// Vote represents a vote in the AppView database
// Votes are indexed from the firehose after being written to user repositories
type Vote struct {
	CreatedAt  time.Time  `json:"createdAt" db:"created_at"`
	IndexedAt  time.Time  `json:"indexedAt" db:"indexed_at"`
	DeletedAt  *time.Time `json:"deletedAt,omitempty" db:"deleted_at"`
	URI        string     `json:"uri" db:"uri"`
	CID        string     `json:"cid" db:"cid"`
	RKey       string     `json:"rkey" db:"rkey"`
	VoterDID   string     `json:"voterDid" db:"voter_did"`
	SubjectURI string     `json:"subjectUri" db:"subject_uri"`
	SubjectCID string     `json:"subjectCid" db:"subject_cid"`
	Direction  string     `json:"direction" db:"direction"`
	ID         int64      `json:"id" db:"id"`
}

// VoteRecord represents the atProto record structure indexed from Jetstream
// This is the data structure that gets stored in the user's repository
type VoteRecord struct {
	Type      string    `json:"$type"`
	Subject   StrongRef `json:"subject"`
	Direction string    `json:"direction"` // "up" or "down"
	CreatedAt string    `json:"createdAt"`
}

// StrongRef represents a strong reference to a record (URI + CID)
// Matches the strongRef definition in the vote lexicon
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}
