package votes

import (
	"time"
)

// Vote represents a vote in the AppView database
// Votes are indexed from the firehose after being written to user repositories
type Vote struct {
	ID         int64      `json:"id" db:"id"`
	URI        string     `json:"uri" db:"uri"`
	CID        string     `json:"cid" db:"cid"`
	RKey       string     `json:"rkey" db:"rkey"`
	VoterDID   string     `json:"voterDid" db:"voter_did"`
	SubjectURI string     `json:"subjectUri" db:"subject_uri"`
	SubjectCID string     `json:"subjectCid" db:"subject_cid"`
	Direction  string     `json:"direction" db:"direction"` // "up" or "down"
	CreatedAt  time.Time  `json:"createdAt" db:"created_at"`
	IndexedAt  time.Time  `json:"indexedAt" db:"indexed_at"`
	DeletedAt  *time.Time `json:"deletedAt,omitempty" db:"deleted_at"`
}

// CreateVoteRequest represents input for creating a new vote
// Matches social.coves.interaction.createVote lexicon input schema
type CreateVoteRequest struct {
	Subject   string `json:"subject"`   // AT-URI of post/comment
	Direction string `json:"direction"` // "up" or "down"
}

// CreateVoteResponse represents the response from creating a vote
// Matches social.coves.interaction.createVote lexicon output schema
type CreateVoteResponse struct {
	URI      string  `json:"uri"`                // AT-URI of created vote record
	CID      string  `json:"cid"`                // CID of created vote record
	Existing *string `json:"existing,omitempty"` // AT-URI of existing vote if updating
}

// DeleteVoteRequest represents input for deleting a vote
// Matches social.coves.interaction.deleteVote lexicon input schema
type DeleteVoteRequest struct {
	Subject string `json:"subject"` // AT-URI of post/comment
}

// VoteRecord represents the actual atProto record structure written to PDS
// This is the data structure that gets stored in the user's repository
type VoteRecord struct {
	Type      string            `json:"$type"`
	Subject   StrongRef         `json:"subject"`
	Direction string            `json:"direction"` // "up" or "down"
	CreatedAt string            `json:"createdAt"`
}

// StrongRef represents a strong reference to a record (URI + CID)
// Matches the strongRef definition in the vote lexicon
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}
