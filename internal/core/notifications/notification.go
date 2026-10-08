// Package notifications computes and stores in-app notifications: replies to a
// user's post or comment, mentions, and upvotes on their content.
package notifications

import "time"

// Reason is why a notification exists. The set is open on the wire.
type Reason string

const (
	ReasonPostReply    Reason = "postReply"
	ReasonCommentReply Reason = "commentReply"
	ReasonMention      Reason = "mention"
	ReasonUpvote       Reason = "upvote"
)

// Intent is one record-keyed notification a fan-out wants written: the record
// (a reply or a mention) that notifies RecipientDID. Writes are idempotent on
// (RecipientDID, Reason, RecordURI).
type Intent struct {
	Reason          Reason
	RecipientDID    string
	ActorDID        string
	RecordURI       string
	RecordCID       string
	SubjectURI      string
	RootPostURI     string
	RecordCreatedAt time.Time
}

// CommentRecord is the part of an indexed comment that a fan-out reads.
type CommentRecord struct {
	URI       string
	CID       string
	AuthorDID string
	ParentURI string
	RootURI   string
	CreatedAt time.Time
	// FacetsJSON is the facets JSON of the record version being indexed; empty
	// means none.
	FacetsJSON string
	// EditEventTime is the edit event's Jetstream time. Zero means no event
	// time; freshness falls back to index time without a lookup.
	EditEventTime time.Time
}

// MaxMentionsPerRecord limits surviving mention notifications for one record
// in total across its create, every edit, active re-create, and resurrection.
// The remaining budget is this limit minus the surviving mention rows, read
// inside the writing transaction while the record is serialized. A slot freed
// by retention or account erasure before a resurrection is available again;
// author deletion keeps rows, so their slots remain used on resurrection.
// richtext.MaxFacets remains the parse and lookup bound for facets.
const MaxMentionsPerRecord = 10

// Retention thresholds. The windows are exact durations, not calendar days:
// read rows go after 30 days, and when a recipient has more than 500 unread
// rows, unread rows more than 180 days older than their newest row go too.
// Each sweep statement deletes at most RetentionBatchSize rows.
const (
	RetentionReadWindow   = 720 * time.Hour
	RetentionUnreadCap    = 500
	RetentionUnreadWindow = 4320 * time.Hour
	RetentionBatchSize    = 10_000

	RetentionHiddenReferenceWindow = 168 * time.Hour
)

// PostRecord is the part of an indexed postv2 post that a fan-out reads.
type PostRecord struct {
	URI       string
	CID       string
	AuthorDID string
	CreatedAt time.Time
	// FacetsJSON is the facets JSON of the post version being indexed; empty
	// means none.
	FacetsJSON string
	// EditEventTime is the edit event's Jetstream time. Zero means no event
	// time; freshness falls back to index time without a lookup.
	EditEventTime time.Time
}

// UpvoteGroupAction is what one vote change does to its subject's upvote group.
type UpvoteGroupAction int

const (
	// UpvoteGroupNoChange leaves the group as it is. It is the zero value.
	UpvoteGroupNoChange UpvoteGroupAction = iota
	// UpvoteGroupBump creates the group or raises its sort_at to the write time.
	UpvoteGroupBump
	// UpvoteGroupDeleteIfEmpty deletes the group when no qualifying native
	// upvote remains and, with bridged totals enabled, the bridged total is 0.
	UpvoteGroupDeleteIfEmpty
)

// UpvoteGroupIntent is the change a vote fan-out wants applied to the upvote
// group keyed by (RecipientDID, SubjectURI).
type UpvoteGroupIntent struct {
	Action       UpvoteGroupAction
	RecipientDID string
	SubjectURI   string
	// RootPostURI is the post the subject belongs to. Only UpvoteGroupBump uses
	// it; it is empty for UpvoteGroupDeleteIfEmpty.
	RootPostURI string
}

// VoteRecord is the part of an indexed vote that the upvote-group fan-out reads.
// FanoutVoteRemoval reads only SubjectURI.
type VoteRecord struct {
	// URI is the vote's own AT-URI, excluded from FanoutVoteCreate's
	// earlier-upvote check.
	URI        string
	VoterDID   string
	SubjectURI string
	// SubjectRootURI is the stored root_uri of a comment subject, read under the
	// subject row lock; empty for a post subject.
	SubjectRootURI string
	// Direction is "up" or "down".
	Direction string
	CreatedAt time.Time
	// VoterErased is the voter's erasure state from the caller's in-transaction erasure gate.
	VoterErased bool
}

// ReferenceState describes whether content can receive new notifications. Only
// an indexed, live reference permits fan-out.
type ReferenceState int

const (
	// ReferenceLive permits new notifications for an indexed reference.
	ReferenceLive ReferenceState = iota
	// ReferenceDeleted marks content withdrawn by its author.
	ReferenceDeleted
	// ReferenceRemovedByModerator marks content removed by its own community:
	// a community withdrawal, or active moderation removals that are all
	// community-scope.
	ReferenceRemovedByModerator
	// ReferenceRemovedByServerAdmin marks content with at least one active
	// instance-scope moderation removal. It takes precedence over
	// ReferenceRemovedByModerator; an author's delete still reads as
	// ReferenceDeleted.
	ReferenceRemovedByServerAdmin
	// ReferenceUnindexed marks a post or comment reference with no indexed row
	// and no active removal. It cannot be checked for withdrawal, so it opens no
	// fan-out; a comment that arrives before its root post loses its
	// notifications. A URI in any other collection can never be indexed and is
	// never Unindexed. Write-side only: the read side reads such a reference as
	// hidden.
	ReferenceUnindexed
)
