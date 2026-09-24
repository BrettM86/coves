package moderation

import (
	"context"
	"time"
)

// Action kinds, scope kinds and origins recorded on moderation actions.
const (
	ActionRemove  = "remove"
	ActionRestore = "restore"

	ScopeInstance = "instance"

	OriginLocal = "local"

	// ModerationStateRemoved is the effective state of a subject with an
	// active removal decision.
	ModerationStateRemoved = "removed"

	OutcomeApplied   = "applied"
	OutcomeUnchanged = "unchanged"
)

// Action is one immutable row of the moderation action log.
type Action struct {
	ID                  string
	ActorDID            string
	AuthorityDID        string
	ScopeKind           string
	ScopeCommunityDID   string
	SubjectURI          string
	SubjectCollection   string
	SubjectCommunityDID string
	ObservedCID         string
	Action              string
	LabelValue          string
	Reason              string
	PrivateNote         string
	ReversesActionID    string
	Origin              string
	CreatedAt           time.Time
}

// IndexedComment is the indexed comment row a mutation inspects, read under a
// share lock so consumer writes serialize against the CID check.
type IndexedComment struct {
	URI           string
	CID           string
	AuthorDeleted bool
	// OwnerDID is the repository holding the comment and its blobs.
	OwnerDID string
	// CommunityDID is the root post's community, empty when the root post is
	// not indexed.
	CommunityDID string
	// ImageCIDs are the blob CIDs of the comment's indexed image embed.
	ImageCIDs []string
}

// IndexedPost is the indexed post row a mutation inspects, read under a share
// lock so consumer writes serialize against the CID check.
type IndexedPost struct {
	URI           string
	CID           string
	AuthorDeleted bool
	// OwnerDID is the repository holding the post's blobs: the author for
	// postv2, the community for legacy posts.
	OwnerDID     string
	CommunityDID string
	// BlobCIDs are the canonical CIDs of the post's proxy-served blobs.
	BlobCIDs []string
}

// MediaBlock suppresses Coves-served bytes of a blob. An empty OwnerDID
// blocks the CID for every owner.
type MediaBlock struct {
	OwnerDID string
	BlobCID  string
	ActionID string
}

// IdempotencyRecord is a stored mutation result bound to a request
// fingerprint, scoped to (actor DID, authority DID, key).
type IdempotencyRecord struct {
	ActorDID     string
	AuthorityDID string
	Key          string
	Fingerprint  string
	Result       MutationResult
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// SubjectModeration is the stored moderation state of a subject.
type SubjectModeration struct {
	Version       int64
	ActiveRemoval *Action
}

// Store persists moderation state. Mutations run inside InTransaction.
type Store interface {
	InTransaction(ctx context.Context, fn func(ctx context.Context, tx Transaction) error) error
	SubjectModeration(ctx context.Context, authorityDID, subjectURI string) (*SubjectModeration, error)
}

// Transaction is the set of operations a mutation performs atomically.
type Transaction interface {
	// LockActor serializes idempotency admission for one actor.
	LockActor(ctx context.Context, actorDID string) error
	// LiveIdempotencyRecord returns the unexpired record for the key, or nil.
	LiveIdempotencyRecord(ctx context.Context, actorDID, authorityDID, key string, now time.Time) (*IdempotencyRecord, error)
	CountLiveIdempotencyKeys(ctx context.Context, actorDID string, now time.Time) (int, error)
	// SaveIdempotencyRecord stores the record, replacing an expired one.
	SaveIdempotencyRecord(ctx context.Context, record IdempotencyRecord) error
	// LockSubject locks the subject's version row, creating it at 0.
	LockSubject(ctx context.Context, subjectURI string) (int64, error)
	// ReadIndexedComment returns ErrSubjectNotIndexed for a never-indexed URI.
	ReadIndexedComment(ctx context.Context, subjectURI string) (*IndexedComment, error)
	// ReadIndexedPost returns ErrSubjectNotIndexed for a never-indexed URI.
	ReadIndexedPost(ctx context.Context, subjectURI string) (*IndexedPost, error)
	// GetAction returns ErrDecisionNotFound for an unknown id.
	GetAction(ctx context.Context, actionID string) (*Action, error)
	// ActiveRemoval returns the active removal action, or nil.
	ActiveRemoval(ctx context.Context, authorityDID, subjectURI string) (*Action, error)
	InsertAction(ctx context.Context, action Action) (*Action, error)
	SetRemovalDecision(ctx context.Context, authorityDID, subjectURI, actionID string, active bool) error
	SetSubjectVersion(ctx context.Context, subjectURI string, version int64) error
	InsertMediaBlocks(ctx context.Context, blocks []MediaBlock) error
	DeactivateMediaBlocks(ctx context.Context, actionID string) error
}

// MediaPurger removes cached bytes of newly blocked blobs.
type MediaPurger interface {
	PurgeOwnerBlob(ownerDID, blobCID string) error
	PurgeBlob(blobCID string) error
}
