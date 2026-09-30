// Package moderation is the instance-admin moderation domain: who may act
// (Authority) and what the AppView knows about a subject (SubjectState).
package moderation

import "time"

// RecordState is the repository availability of a subject record as the
// AppView has indexed it.
type RecordState string

const (
	RecordStatePresent     RecordState = "present"
	RecordStateDeleted     RecordState = "deleted"
	RecordStateUnavailable RecordState = "unavailable"
)

// ModerationStateClear is the effective removal state of a subject with no
// applicable removal decision.
const ModerationStateClear = "clear"

// Subject collections getSubjectState and the mutations accept.
const (
	CommentCollection    = "social.coves.community.comment"
	PostV2Collection     = "social.coves.community.postv2"
	LegacyPostCollection = "social.coves.community.post"
)

// SubjectCollections is the set of NSIDs a moderation subject may live in.
var SubjectCollections = map[string]struct{}{
	CommentCollection:    {},
	PostV2Collection:     {},
	LegacyPostCollection: {},
}

// IsSubjectCollection reports whether nsid is a supported subject collection.
func IsSubjectCollection(nsid string) bool {
	_, ok := SubjectCollections[nsid]
	return ok
}

// StrongRef is a com.atproto.repo.strongRef.
type StrongRef struct {
	URI string
	CID string
}

// ActionRef identifies an action in a moderation service's log.
type ActionRef struct {
	ServiceDID string
	ActionID   string
}

// LocalLabel is a content label established by a local action.
type LocalLabel struct {
	Value  string
	Action ActionRef
}

// ModerationView is the effective public removal state of a subject.
type ModerationView struct {
	State string
}

// SubjectState is the versioned moderation and repository state of a subject.
type SubjectState struct {
	Subject        string
	Version        string
	Moderation     ModerationView
	RecordState    RecordState
	CurrentSubject *StrongRef
	LocalRemoval   *ActionRef
	LocalLabels    []LocalLabel
}

// IndexedRecord is what the AppView has indexed for a subject URI.
type IndexedRecord struct {
	URI     string
	CID     string
	Deleted bool
}

// MutationResult is the outcome of a removeContent/restoreContent call. Action
// is nil for an unchanged outcome.
type MutationResult struct {
	Outcome string
	State   SubjectState
	Action  *Action
}

// RemoveContentRequest is the caller-supplied part of a removeContent call.
type RemoveContentRequest struct {
	Subject         StrongRef
	ExpectedVersion string
	IdempotencyKey  string
	Reason          string
	PrivateNote     string
}

// RestoreContentRequest is the caller-supplied part of a restoreContent call.
type RestoreContentRequest struct {
	ActionID        string
	ReviewedSubject *StrongRef
	ExpectedVersion string
	IdempotencyKey  string
	Reason          string
	PrivateNote     string
}

// Config is the moderation service's configuration.
type Config struct {
	InstanceDID            string
	IdempotencyRetention   time.Duration
	MaxLiveIdempotencyKeys int
	Purger                 MediaPurger
	Now                    func() time.Time
	CursorSecret           string
	CommunityResolver      CommunityResolver
	HandleResolver         HandleResolver
}
