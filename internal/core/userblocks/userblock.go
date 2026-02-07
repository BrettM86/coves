package userblocks

import "time"

// UserBlock represents a one-directional block relationship between two users.
// The block record lives in the blocker's PDS repository at:
// at://blocker_did/social.coves.actor.block/{tid}
//
// One-directional: Only the blocker's feeds/comments are filtered.
// The blocked user is unaffected and unaware.
type UserBlock struct {
	ID         int       `json:"id" db:"id"`
	BlockerDID string    `json:"blockerDid" db:"blocker_did"`
	BlockedDID string    `json:"blockedDid" db:"blocked_did"`
	BlockedAt  time.Time `json:"blockedAt" db:"blocked_at"`
	RecordURI  string    `json:"recordUri" db:"record_uri"`
	RecordCID  string    `json:"recordCid" db:"record_cid"`
}

// BlockResult is returned by BlockUser after a successful PDS write-forward.
// Contains only the record URI and CID from PDS. The full UserBlock will be
// indexed asynchronously via the firehose consumer.
type BlockResult struct {
	RecordURI string `json:"recordUri"`
	RecordCID string `json:"recordCid"`
}
