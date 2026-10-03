package moderation

import (
	"context"
	"time"

	"Coves/internal/core/imageproxy"
)

// Action kinds, scope kinds and origins recorded on moderation actions.
const (
	ActionRemove       = "remove"
	ActionRestore      = "restore"
	ActionLabel        = "label"
	ActionRetractLabel = "retract-label"

	// LabelNSFW is the only content label value the local classification
	// procedures accept.
	LabelNSFW = "nsfw"

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
	// ReversedActionReason is the reason of the action named by ReversesActionID,
	// read with the row rather than stored as a column. Store.ListActions must
	// populate it: it decides whether the action is hidden, and an empty value
	// reads as not hidden.
	ReversedActionReason string
	Origin               string
	CreatedAt            time.Time
	// SubjectAccess is whether the public may know the subject exists, decided
	// when the log is read. Store.ListActions must set it on every row; the
	// service refuses a row that leaves it unset. It is read-time state, so it
	// stays out of stored idempotency results.
	SubjectAccess SubjectAccess `json:"-"`
}

// SubjectAccess says whether an action's subject is content the anonymous
// public can see. It follows post.get's anonymous admission rule: a post is
// public when its own community admitted it (or, outside postv2, when it has
// no admission row), and a comment follows its root post. A subject with no
// indexed content row to decide from is restricted.
type SubjectAccess string

const (
	SubjectAccessPublic     SubjectAccess = "public"
	SubjectAccessRestricted SubjectAccess = "restricted"
)

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
	ActiveLabels  []Action
}

// Store persists moderation state. Mutations run inside InTransaction.
type Store interface {
	InTransaction(ctx context.Context, fn func(ctx context.Context, tx Transaction) error) error
	SubjectModeration(ctx context.Context, authorityDID, subjectURI string) (*SubjectModeration, error)
	// ListActions returns up to query.Limit actions, newest first, each with
	// ReversedActionReason and SubjectAccess populated. Under ExcludeHidden it
	// must omit every hidden action, under ExcludeLabelActions every action
	// whose kind is in PublicExcludedActions, and under ExcludeRestricted every
	// action whose subject is not SubjectAccessPublic; the service fails closed
	// if any of them comes back.
	ListActions(ctx context.Context, query ActionListQuery) ([]Action, error)
}

// ActionListQuery selects a page of the action log, newest first.
type ActionListQuery struct {
	// Limit is the page size plus one look-ahead row, which tells the service
	// whether a next page exists.
	Limit             int
	Before            *ActionKey
	SubjectURI        string
	SubjectCollection string
	Action            string
	Origin            string
	AuthorityDID      string
	ActorDID          string
	CommunityDID      string
	ActionID          string
	Since             *time.Time
	Until             *time.Time
	ExcludeHidden     bool
	// ExcludeRestricted omits every action whose subject is restricted.
	ExcludeRestricted bool
	// ExcludeLabelActions omits every action kind in PublicExcludedActions.
	ExcludeLabelActions bool
}

// ActionKey is an action log position: rows strictly older than it follow.
type ActionKey struct {
	CreatedAt time.Time
	ID        string
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
	// ActiveLabels returns active instance-scope label actions, ordered by value.
	ActiveLabels(ctx context.Context, authorityDID, subjectURI string) ([]Action, error)
	InsertAction(ctx context.Context, action Action) (*Action, error)
	SetRemovalDecision(ctx context.Context, authorityDID, subjectURI, actionID string, active bool) error
	// SetLabelDecision changes the instance-scope label decision for
	// (authorityDID, subjectURI, value). With active true it activates the
	// decision keyed to actionID, inserting it or reactivating an inactive row,
	// and fails if the decision is already active. With active false it
	// deactivates exactly one active decision whose action is actionID, and
	// fails if there is none.
	SetLabelDecision(ctx context.Context, authorityDID, subjectURI, value, actionID string, active bool) error
	SetSubjectVersion(ctx context.Context, subjectURI string, version int64) error
	InsertMediaBlocks(ctx context.Context, blocks []MediaBlock) error
	DeactivateMediaBlocks(ctx context.Context, actionID string) error
	RecordCDNPurgeTargets(ctx context.Context, blobs []imageproxy.BlockedBlob) error
}

// MediaPurger removes cached bytes of newly blocked blobs.
type MediaPurger interface {
	PurgeOwnerBlob(ctx context.Context, ownerDID, blobCID string) error
	PurgeBlob(ctx context.Context, blobCID string) error
}
