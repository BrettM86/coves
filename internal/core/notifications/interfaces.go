package notifications

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Lookups are the reads a fan-out needs, bound to the index transaction.
type Lookups interface {
	// LegacyPostAuthor returns posts.author_did for a legacy
	// social.coves.community.post URI; found is false when no row exists.
	LegacyPostAuthor(ctx context.Context, postURI string) (authorDID string, found bool, err error)
	// ActivatedAt returns notification_activation.activated_at. A missing row
	// is an error, never "no cutoff".
	ActivatedAt(ctx context.Context) (time.Time, error)
	// IndexTime returns the index transaction's timestamp, now(). Writes set
	// notifications.sort_at to clock_timestamp(), which is at or after it.
	IndexTime(ctx context.Context) (time.Time, error)
	// IsAggregator reports whether did is in aggregators. It is kept for 06's
	// voter eligibility, which must not require a users row.
	IsAggregator(ctx context.Context, did string) (bool, error)
	// RecipientFacts returns the per-recipient facts for each of recipientDIDs
	// that has a users row; a DID without one is absent from the map.
	RecipientFacts(ctx context.Context, actorDID string, recipientDIDs []string) (map[string]RecipientFacts, error)
	// EarlierUpvoteExists reports whether voterDID has another votes row on
	// subjectURI, live or soft-deleted and under any rkey, with direction 'up'
	// and a URI other than voteURI.
	EarlierUpvoteExists(ctx context.Context, voterDID, subjectURI, voteURI string) (bool, error)
	// ExistingMentionRecipients returns the recipient DIDs of every surviving
	// notification with reason='mention' and this record_uri. Its length is
	// the used portion of MaxMentionsPerRecord's budget.
	ExistingMentionRecipients(ctx context.Context, recordURI string) ([]string, error)
	// ReferenceStates classifies uris in one transaction-bound lookup. A URI is
	// in the map when it is an indexed post or comment row that is withdrawn,
	// when it has an active admin removal decision, even if it is not indexed,
	// or, as ReferenceUnindexed, when it names a post or comment with neither
	// an indexed row nor an active removal. A URI absent from the map is an
	// indexed, live row or a record in a collection that is never indexed.
	ReferenceStates(ctx context.Context, uris []string) (map[string]ReferenceState, error)
}

// RecipientFacts is what the per-recipient write-time rules read about one DID.
// The zero value describes an eligible recipient, and it is meaningful only for
// a DID present in the map Lookups.RecipientFacts returns: an absent DID has no
// users row and is never notified.
type RecipientFacts struct {
	// PDSURL is the recipient's users.pds_url. Fan-out suppresses the recipient
	// when the BridgeHostChecker trusts this host.
	PDSURL string
	// Erased reports a deleted_accounts row for the recipient.
	Erased bool
	// Aggregator reports an aggregators row for the recipient.
	Aggregator bool
	// Community reports a communities row for the recipient. Fan-out uses it
	// only to suppress mentions; a community can still receive a reply.
	Community bool
	// BlockedWithActor reports a user_blocks row in either direction between the
	// recipient and the actor.
	BlockedWithActor bool
}

// BridgeHostChecker reports whether a PDS URL is a trusted bridge PDS host.
// *jetstream.BridgeTrust satisfies it. A nil BridgeHostChecker, including a nil
// pointer stored in the interface such as a nil *jetstream.BridgeTrust, trusts no
// host. Fan-out calls TrustsPDS on a nil pointer, so an implementation must return
// false for a nil receiver, as *jetstream.BridgeTrust does.
type BridgeHostChecker interface {
	TrustsPDS(pdsURL string) bool
}

// Repository writes notifications inside a consumer's own index transaction.
// Consumers depend on this interface and never import the service.
type Repository interface {
	// ErasureGateTx takes the shared erasure lock for actorDID and then reports
	// whether the actor is erased. Call it before touching any content row.
	ErasureGateTx(ctx context.Context, tx *sql.Tx, actorDID string) (erased bool, err error)
	// LookupsTx returns Lookups that read through tx.
	LookupsTx(tx *sql.Tx) Lookups
	// ApplyTx writes intents through tx.
	ApplyTx(ctx context.Context, tx *sql.Tx, intents []Intent) error
	// ApplyUpvoteGroupTx inserts or bumps an upvote group through tx, deletes it
	// when no qualifying native upvote remains and, with bridged totals enabled,
	// the bridged total is 0, or makes no database call for a
	// no-change intent. RootPostURI is written only on insert; a bump keeps the
	// stored root, so a comment re-created under a different root needs its own
	// update of the group. An upsert whose recipient was erased after the
	// fan-out read (23503 on notifications_recipient_did_fkey) is skipped with
	// a nil error and the caller's earlier writes in tx kept; any other error
	// is returned. A caller applying delete-if-empty must hold the subject's
	// posts or comments row lock, take it first, or re-check in a separate
	// statement: at READ COMMITTED the DELETE's NOT EXISTS reads its original
	// snapshot, so an upvote committed by a concurrent bump it waited on goes
	// unseen and the group is deleted.
	ApplyUpvoteGroupTx(ctx context.Context, tx *sql.Tx, intent UpvoteGroupIntent) error
	// ReplaceUpvoteGroupRootTx sets root_post_uri on recipientDID's upvote group
	// whose subject is subjectURI, through tx. A rootPostURI that does not name a
	// post collection leaves the group's root unchanged.
	ReplaceUpvoteGroupRootTx(ctx context.Context, tx *sql.Tx, recipientDID, subjectURI, rootPostURI string) error
	// RecordPostAuthorDeleteWithdrawalTx records the publicly visible postv2 at
	// author deletion in tx, before acceptance is withdrawn.
	RecordPostAuthorDeleteWithdrawalTx(ctx context.Context, tx *sql.Tx, postURI string) error
	// RepairResurrectedCommentNotificationsTx reconciles kept rows with the
	// resurrected comment's new threading in tx before its create fan-out. It
	// deletes the record's reply rows whose subject is not replySubjectURI ("" when
	// no reply resolves deletes them all), and moves the remaining rows to
	// rootPostURI when that names a post.
	RepairResurrectedCommentNotificationsTx(ctx context.Context, tx *sql.Tx, recordURI, replySubjectURI, rootPostURI string) error
	// DeleteReplyRecipientMentionsTx deletes, through tx, the record's mention rows
	// held by a recipient who also holds a reply row for the record. It runs after
	// a resurrection's create fan-out, so a mention goes only when a reply replaced it.
	DeleteReplyRecipientMentionsTx(ctx context.Context, tx *sql.Tx, recordURI string) error
}

// RetentionSweeper removes one bounded batch per call from each retention category.
type RetentionSweeper interface {
	SweepReadNotifications(ctx context.Context) (int64, error)
	SweepUnreadOverflow(ctx context.Context) (int64, error)
	SweepEmptyUpvoteGroups(ctx context.Context) (int64, error)
	SweepHiddenReferenceNotifications(ctx context.Context) (int64, error)
}

// ReadRepository reads a recipient's notifications through the one read-time
// visibility predicate.
type ReadRepository interface {
	CountUnread(ctx context.Context, recipientDID string) (int, error)
	UpdateSeen(ctx context.Context, did string, seenAt time.Time) error
	List(ctx context.Context, recipientDID, cursor string, limit int) (ListPage, error)
}

// ListedNotification is one listed notification row ("" or zero for NULL columns).
// RootPost, Subject, and Record carry each reply reference's read-time state and
// current indexed CID, independent of the notification's stored record CID.
type ListedNotification struct {
	ID                int64
	Reason            Reason
	RecordURI         string
	ActorDID          string
	SubjectURI        string
	RootPostURI       string
	RecordCreatedAt   time.Time
	SortAt            time.Time
	IsRead            bool
	RootPost          ListedReference
	Subject           ListedReference
	Record            ListedReference
	UpvoteCount       int
	RecentUpvoterDIDs []string
}

// ListedReference is one referenced item's read-time state and current CID.
// A deleted or moderator-removed reference is rendered without lookup content.
type ListedReference struct {
	State ReferenceState
	CID   string
}

// ListPage is one page of listed notifications. Cursor is "" on the last page;
// SeenAt is nil when the recipient has no stored seen_at.
type ListPage struct {
	Notifications []ListedNotification
	Cursor        string
	SeenAt        *time.Time
}

// Preferences are a recipient's per-reason notification settings (true = enabled).
type Preferences struct {
	PostReply    bool
	CommentReply bool
	Mention      bool
	Upvote       bool
}

// PreferencesUpdate is a partial preferences change; nil leaves a reason unchanged.
type PreferencesUpdate struct {
	PostReply    *bool
	CommentReply *bool
	Mention      *bool
	Upvote       *bool
}

// ErrAccountNotIndexed means the caller has no users row, so notification
// state cannot be stored for it.
var ErrAccountNotIndexed = errors.New("account is not indexed")

// PreferencesRepository stores preferences in notification_state.disabled_reasons.
type PreferencesRepository interface {
	GetPreferences(ctx context.Context, did string) (Preferences, error)
	PutPreferences(ctx context.Context, did string, update PreferencesUpdate) (Preferences, error)
}
