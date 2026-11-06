// Package moderation is the instance-admin moderation domain: who may act
// (Authority) and what the AppView knows about a subject (SubjectState).
package moderation

import (
	"context"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
)

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
	ExpiresAt  *time.Time `json:",omitempty"`
}

// LocalLabel is a content label established by a local action.
type LocalLabel struct {
	Value  string
	Action ActionRef
}

// ModerationView is the effective public moderation of a subject: its removal
// state and its active content labels.
type ModerationView struct {
	State         string
	ContentLabels []ContentLabel
}

// ContentLabel is one active classification value with the sources applying it.
type ContentLabel struct {
	Value   string
	Sources []DecisionSource
}

// DecisionSource attributes a decision to its authority and scope.
type DecisionSource struct {
	AuthorityDID string
	ScopeKind    string
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

// MutationResult is the outcome of a removeContent, restoreContent,
// labelContent or retractContentLabel call. Action is nil for an unchanged
// outcome.
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

// LabelContentRequest is the caller-supplied part of a labelContent call.
// Reason is optional; the doxing and illegal-content reasons are rejected
// because they are only for removal.
type LabelContentRequest struct {
	Subject         StrongRef
	LabelValue      string
	ExpectedVersion string
	IdempotencyKey  string
	Reason          string
	PrivateNote     string
}

// RetractContentLabelRequest is the caller-supplied part of a retractContentLabel call.
// Reason is optional; the doxing and illegal-content reasons are rejected
// because they are only for removal.
type RetractContentLabelRequest struct {
	ActionID        string
	ReviewedSubject *StrongRef
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

// BanAccountRequest is the caller-supplied part of a banAccount call.
type BanAccountRequest struct {
	Subject         string
	ExpectedVersion string
	IdempotencyKey  string
	Reason          string
	PrivateNote     string
	ExpiresAt       *time.Time
}

// UnbanAccountRequest is the caller-supplied part of an unbanAccount call.
type UnbanAccountRequest struct {
	Subject         string
	ExpectedVersion string
	IdempotencyKey  string
	Reason          string
	PrivateNote     string
}

// RemoveCommunityRequest is the caller-supplied part of a removeCommunity call.
type RemoveCommunityRequest struct {
	Subject         string
	ExpectedVersion string
	IdempotencyKey  string
	Reason          string
	PrivateNote     string
}

// RestoreCommunityRequest is the caller-supplied part of a restoreCommunity call.
type RestoreCommunityRequest struct {
	Subject         string
	ExpectedVersion string
	IdempotencyKey  string
	Reason          string
	PrivateNote     string
}

// CommunityChecker reports whether a DID is an indexed community.
type CommunityChecker interface {
	ExistsByDID(ctx context.Context, did string) (bool, error)
}

// BridgeCommunityTrust reports whether a community belongs to a trusted
// bridge, whose communities author their bridged content as themselves.
// jetstream.BridgeTrust implements it with the same rule the community
// consumer applies when it indexes a community despite account activity.
type BridgeCommunityTrust interface {
	// TrustsCommunity reports whether a community with this handle and PDS is
	// a trusted bridge's community.
	TrustsCommunity(handle, pdsURL string) bool
}

// AccountSubjectFacts are the facts about an at://<did> subject that decide
// whether an indexed community DID can be banned as an account.
type AccountSubjectFacts struct {
	HasAccountActivity bool   // an account-sourced users row, or an authored post, comment or vote
	CommunityPDSURL    string // the indexed community row's pds_url; "" when there is no community row
	CommunityHandle    string // the indexed community row's stored handle; "" when there is no community row
	IsCommunity        bool   // an indexed communities row exists for the DID
}

// Config is the moderation service's configuration.
type Config struct {
	InstanceDID            string
	IdempotencyRetention   time.Duration
	MaxLiveIdempotencyKeys int
	Purger                 MediaPurger
	CDNPurgeTargets        CDNPurgeTargets
	Now                    func() time.Time
	CursorSecret           string
	CommunityResolver      CommunityResolver
	HandleResolver         HandleResolver
	Communities            CommunityChecker
	// BridgeTrust reports trusted bridge communities; nil trusts nothing.
	BridgeTrust BridgeCommunityTrust
	// LabelSigningKey signs the labels decisions publish; nil disables publication.
	LabelSigningKey atcrypto.PrivateKey
}
