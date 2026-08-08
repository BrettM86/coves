package posts

import (
	"context"
	"errors"
	"time"
)

// Community admission state: whether a community has accepted an author-owned
// post, and the ordering rule that keeps that answer stable.
//
// Under author-owned posts a post record lives in the AUTHOR's repo and each
// community publishes its own acceptance (and, for moderation, removal) record
// in the COMMUNITY's repo. The AppView therefore holds one admission row per
// (community, post) pair rather than a status column on the post — a post can
// carry independent decisions from several communities at once.
//
// See docs/PRD_AUTHOR_OWNED_POSTS.md §5.2, §5.5 and §6.1.

// AdmissionStatus is the state of one community's decision about one post.
type AdmissionStatus string

const (
	// AdmissionStatusPending means the post is indexed and awaiting a decision.
	AdmissionStatusPending AdmissionStatus = "pending"

	// AdmissionStatusAccepted means a live community acceptance pins the exact
	// CID the AppView has indexed. This is the only status that renders the post
	// to non-authors.
	AdmissionStatusAccepted AdmissionStatus = "accepted"

	// AdmissionStatusPendingReacceptance means an acceptance exists but pins a
	// CID other than the current content: the author edited the post, or an
	// acceptance arrived before the edit that produced the CID it pins. Edited
	// content is never rendered under the old acceptance.
	AdmissionStatusPendingReacceptance AdmissionStatus = "pending_reacceptance"

	// AdmissionStatusRejected is an AppView-LOCAL decision. It never corresponds
	// to a community-repo record, and so never advances the community watermark.
	AdmissionStatusRejected AdmissionStatus = "rejected"

	// AdmissionStatusRemoved means a community removal record covers the post.
	// It is terminal against author-repo events and against community events at
	// or below the removal's watermark; only a community acceptance at a
	// strictly greater watermark restores it.
	AdmissionStatusRemoved AdmissionStatus = "removed"
)

// AdmissionOutcome reports what a mutation DID, as a value rather than an error.
//
// Migration 033 set the precedent and §5.2 restates it: an event skipped by the
// ordering gate is the system working. Returning it as an error would route
// healthy skips — multi-feed duplicates, dead-letter redrives, an author edit of
// a removed post — into the dead-letter queue, which is where genuine failures
// are supposed to be visible.
type AdmissionOutcome string

const (
	// AdmissionApplied means the event advanced the row's state.
	AdmissionApplied AdmissionOutcome = "applied"

	// AdmissionSkippedStale means the event's watermark was not strictly greater
	// than the row's: either an out-of-order copy from another feed, or an exact
	// replay of the event already applied.
	AdmissionSkippedStale AdmissionOutcome = "skipped_stale"

	// AdmissionSkippedTerminal means the row's state refused the transition
	// regardless of ordering — the removal terminality of §5.5. Audit columns
	// may still have advanced; the decision did not.
	AdmissionSkippedTerminal AdmissionOutcome = "skipped_terminal"
)

// CommunityOpRank ranks the operations that can appear in ONE community commit,
// so that a commit's events order among themselves.
//
// A commit carries at most one delete and one put about a given subject — the
// removal commit is {delete acceptance, create removal}, the restore commit is
// {delete removal, create acceptance} — and in both the put is the operation
// that expresses the intent. Ranking put above delete is what makes the removal
// commit converge on `removed` and the restore commit converge on `accepted`
// no matter which half the consumer sees first.
type CommunityOpRank int16

const (
	// CommunityOpDelete ranks a record deletion below a write in the same commit.
	CommunityOpDelete CommunityOpRank = 0

	// CommunityOpPut ranks a record create/update above a deletion in the same
	// commit.
	CommunityOpPut CommunityOpRank = 1
)

// ErrInvalidWatermark reports a community event carrying a watermark that can
// never have come off the wire — today, an empty Rev. It is a genuine error
// rather than a skip outcome: skips are the ordering gate WORKING, while an
// empty rev means the event was decoded wrong upstream, and stamping it would
// write a fabricated clock value onto the row. Routing it to the dead-letter
// queue is exactly right.
var ErrInvalidWatermark = errors.New("invalid community watermark")

// CommunityWatermark is the subject-scoped composite ordering key of §5.2:
// the repo revision of the last APPLIED community event about this (community,
// post) pair, plus that event's rank within its commit.
//
// Rev is a base32-sortable atProto TID, so lexicographic comparison of Rev IS
// commit order within one repo — the same property migration 033's per-record
// gate relies on, which is why the column carries COLLATE "C". Rev MUST be
// non-empty: every Jetstream commit carries one, so an empty rev is an
// upstream decoding bug, and repositories refuse it with a genuine error
// wrapping ErrInvalidWatermark rather than stamping a clock value that never
// existed.
//
// On a COMMAND, only Rev is consumed: OpRank is derived by the repository from
// the operation itself (a put ranks 1, a delete ranks 0), because the rank IS
// the operation's kind and letting a caller assert otherwise would let one
// mislabeled event reorder a commit. On a RESULT (Admission.LastCommunityEvent)
// both halves are meaningful — they report the tuple actually stored.
//
// The per-record gate cannot do this job: acceptance and removal are DIFFERENT
// record URIs describing the SAME subject, so ordering them requires a key
// scoped to the subject rather than to the record.
type CommunityWatermark struct {
	Rev    string
	OpRank CommunityOpRank
}

// Admission is one community's decision about one post, with the audit metadata
// the decision was made from.
type Admission struct {
	CommunityDID string
	PostURI      string
	Status       AdmissionStatus

	// AcceptanceURI, AcceptanceRkey and AcceptedCID describe the LIVE community
	// acceptance record. They are populated only while an acceptance stands; a
	// removal or an acceptance deletion clears them.
	AcceptanceURI  *string
	AcceptanceRkey *string
	AcceptedCID    *string

	// DecisionCode and DecisionAt record a rejection or removal. Rejections are
	// AppView-local; removals mirror a community removal record.
	DecisionCode *string
	DecisionAt   *time.Time

	// EvaluatedCID is the exact content CID the AppView has indexed for this
	// post — what the next decision will judge, and what an acceptance's pinned
	// CID is compared against.
	EvaluatedCID *string

	// Redrivable is false for terminal policy rejections, so the dead-letter
	// redrive pass does not retry a decision that will never change. Transient
	// evaluation failures stay redrivable.
	Redrivable bool

	// LastCommunityEvent is the §5.2 watermark, nil until the first community
	// event about this subject applies. Author-repo events never set it.
	LastCommunityEvent *CommunityWatermark

	CreatedAt time.Time
	UpdatedAt time.Time
}

// AdmissionResult pairs what a mutation did with the row it left behind.
//
// The row is returned even for a skip: a caller that has just declined an event
// still needs the current state to decide whether to notify, re-emit, or do
// nothing, and making it fetch that separately would reintroduce the read-then-
// write race the single-statement CAS exists to avoid.
//
// Admission is nil in exactly one case: RepinAcceptedCID meeting a subject
// that has no row. The five event mutations insert when the subject is absent,
// so they always have a row to report; RecordRejection, the other mutation
// that may not create one, treats an absent subject as an ERROR rather than a
// result (the engine rejects rows it read from its own queue). That leaves the
// repin, where there is genuinely no row to describe: the outcome is
// skipped_terminal and the caller has nothing to reconcile.
type AdmissionResult struct {
	Outcome   AdmissionOutcome
	Admission *Admission
}

// UpsertPendingCommand records an AUTHOR-repo observation of a post's content:
// the post was created, edited, or re-indexed, and this is the CID now held.
//
// It never carries a watermark because author events must not advance the
// community watermark — they are ordered by migration 033's per-record gate,
// which is a different clock.
type UpsertPendingCommand struct {
	CommunityDID string
	PostURI      string
	EvaluatedCID string

	// IsSeed marks the WRITE PATH's own seed of a post it has just written,
	// rather than an observation decoded off the firehose. It narrows the guard:
	// a seed may create the row or re-affirm the CID already there, and may
	// never overwrite a different one.
	//
	// THE DEFAULT GUARD IS RIGHT FOR EVENTS AND WRONG FOR A SEED, and the
	// difference is which direction time runs. A firehose observation is always
	// the newest thing the AppView has seen about the record, so "the CID
	// differs, therefore it is newer" holds. A seed carries the CID the write
	// path committed a moment ago, and the firehose is live in that moment: it
	// can already have delivered the create AND a subsequent edit. Then the CIDs
	// differ because the row is NEWER, and writing it would move the AppView's
	// belief about the post backwards.
	//
	// What that costs is not a stale column. AcceptSubmission's guard is "the
	// row is pending and still holds the CID I am accepting", so a rewound row
	// makes that guard pass against superseded content: the community publishes
	// an acceptance pinning a version the author has already replaced, and the
	// author is told "accepted" for content this AppView never evaluated. That
	// is the outcome §5.5 exists to prevent, reached with nobody being wrong
	// about anything.
	IsSeed bool
}

// ApplyAcceptanceCommand applies a community acceptance record write.
//
// PinnedCID is the CID the acceptance names. It matching the indexed content is
// what separates `accepted` from `pending_reacceptance`; the acceptance fields
// are persisted either way, so an acceptance that arrives before its subject's
// edit converges when the post event lands instead of livelocking.
type ApplyAcceptanceCommand struct {
	CommunityDID   string
	PostURI        string
	AcceptanceURI  string
	AcceptanceRkey string
	PinnedCID      string
	Watermark      CommunityWatermark
}

// ApplyRemovalCommand applies a community removal record write. A removal with
// no prior acceptance is valid and indexes normally — communities may remove
// pre-emptively.
type ApplyRemovalCommand struct {
	CommunityDID string
	PostURI      string
	DecisionCode string
	Watermark    CommunityWatermark
}

// RepinAcceptanceCommand moves a standing acceptance onto a new content CID
// without re-deciding anything.
//
// It exists for the bridgedStats exception of §5.5. Bridges refresh their
// records frequently and specifically to update origin-platform vote counts, so
// every refresh changes the record's CID while changing nothing a moderator
// would judge. Routing those through the ordinary edit path would strobe
// accepted bridge posts out of feeds and spam the community's repo with
// re-acceptance commits. A repin is the community's acceptance record being
// updated in place — same rkey, new pinned CID — so it carries a watermark like
// any other community event, but no status transition.
//
// The caller is responsible for having established that the diff touches ONLY
// bridgedStats and that the author passes the bridge-trust gate. Any diff
// touching title, content, facets, embed, labels, tags or community requires
// full re-admission and must not come through here.
type RepinAcceptanceCommand struct {
	CommunityDID string
	PostURI      string
	PinnedCID    string
	Watermark    CommunityWatermark
}

// RecordRejectionCommand records an AppView-LOCAL rejection.
//
// JudgedCID is the exact content CID the decision judged, and it is part of
// the guard: a rejection lands only on a `pending` row still holding this CID.
// The engine reads a pending row, evaluates its content, and writes the
// verdict — if the author edited in between, the verdict judged content the
// row no longer holds and must not land on the new content (§5.5: new content
// is judged fresh, and re-acceptance failure is a removal, not a rejection).
//
// Redrivable is the caller's classification of WHY: a policy rejection is
// terminal and must not be retried by the dead-letter redrive pass, while a
// transient evaluation failure has to stay retryable. Getting it backwards
// either retries a decision that will never change or abandons one that would
// have succeeded on the next attempt.
type RecordRejectionCommand struct {
	CommunityDID string
	PostURI      string
	DecisionCode string
	JudgedCID    string
	Redrivable   bool
}

// CommunityDeleteCommand applies the deletion of a community record about this
// subject — an acceptance or a removal. Which record was deleted is expressed by
// the method called, not by a field, because the two deletions mean opposite
// things and a boolean would make the call sites unreadable.
type CommunityDeleteCommand struct {
	CommunityDID string
	PostURI      string
	Watermark    CommunityWatermark
}

// AdmissionRepository stores the per-(community, post) admission decisions.
//
// Every mutation is a single-statement compare-and-swap: the ordering test and
// the write happen in one statement, so concurrent consumers of overlapping
// feeds cannot interleave a read and a write, and a replayed event is a no-op
// rather than a re-stamped decision timestamp. An error return means a genuine
// failure — a skip is an outcome.
type AdmissionRepository interface {
	// UpsertPending records the author-repo content observation. On a new
	// subject it creates the pending row; on an accepted subject whose content
	// has changed it moves to pending_reacceptance; on a removed subject it
	// updates audit metadata only, so that editing cannot launder a removed post
	// back through auto-acceptance.
	UpsertPending(ctx context.Context, cmd UpsertPendingCommand) (AdmissionResult, error)

	// ApplyAcceptance applies an acceptance write under the watermark rule.
	ApplyAcceptance(ctx context.Context, cmd ApplyAcceptanceCommand) (AdmissionResult, error)

	// ApplyAcceptanceDelete applies the deletion of the acceptance record.
	ApplyAcceptanceDelete(ctx context.Context, cmd CommunityDeleteCommand) (AdmissionResult, error)

	// ApplyRemoval applies a removal write under the watermark rule.
	ApplyRemoval(ctx context.Context, cmd ApplyRemovalCommand) (AdmissionResult, error)

	// ApplyRemovalDelete applies the deletion of the removal record.
	ApplyRemovalDelete(ctx context.Context, cmd CommunityDeleteCommand) (AdmissionResult, error)

	// RepinAcceptedCID moves a standing acceptance onto new content without a
	// status transition — the §5.5 bridgedStats exception. It applies only to a
	// row that is `accepted` NOW: any other status (or an absent row) refuses
	// with skipped_terminal, because a repin re-decides nothing and so has no
	// business changing what a non-accepted row would need decided.
	RepinAcceptedCID(ctx context.Context, cmd RepinAcceptanceCommand) (AdmissionResult, error)

	// RecordRejection records the AppView's own decision not to admit a post.
	// It is not a community-repo record, so it must NOT advance the community
	// watermark: a local decision that could outrank a genuine community event
	// would suppress the very acceptance that overrules it.
	//
	// It lands only on a `pending` row still holding cmd.JudgedCID — rejection's
	// one legal source state (§5.5: re-acceptance failure is a removal, and a
	// rejection overwriting an accepted row would suppress a live acceptance).
	// Any other standing row refuses it with a skip outcome; a subject with NO
	// row is an ERROR, because the engine rejects rows it read from its own
	// queue and their absence is a caller bug, not delivery skew.
	RecordRejection(ctx context.Context, cmd RecordRejectionCommand) (AdmissionResult, error)

	// Get returns the admission row for one subject, or ErrNotFound if the
	// community has never seen the post.
	Get(ctx context.Context, communityDID, postURI string) (*Admission, error)

	// GetByPostURIs returns every community's decision about each of the given
	// posts, keyed by post URI. Posts with no admission row anywhere are absent
	// from the map rather than present-and-empty.
	//
	// The map is per-URI rather than flat because one post genuinely has several
	// decisions: an author may submit the same post to several communities, and
	// the author's own view of it has to show each community's answer separately.
	GetByPostURIs(ctx context.Context, postURIs []string) (map[string][]*Admission, error)

	// ListByStatusForCommunity pages one community's admissions in a single
	// status — the moderation queue. Order is oldest first, matching the
	// (community_did, status, created_at) index.
	//
	// The cursor is opaque and must be treated as such by callers; a malformed
	// one is an error rather than a silent reset to the first page, which would
	// make a moderator re-review what they had already cleared.
	ListByStatusForCommunity(ctx context.Context, communityDID string, status AdmissionStatus, limit int, cursor *string) ([]*Admission, *string, error)

	// ListPendingSubjects returns the work the acceptance engine still owes a
	// decision on, ACROSS communities, oldest first and bounded by limit.
	//
	// It is a different question from ListByStatusForCommunity, which serves a
	// moderator looking at one community. This serves the engine's driver, which
	// has no community in hand — it is asking "what is undecided anywhere that I
	// can actually decide", and the two halves of that qualification are why this
	// is a query rather than a filter over the other one:
	//
	//   - ONLY COMMUNITIES THIS APPVIEW HOSTS. Hosting means holding the
	//     community's PDS credentials, because writing the acceptance is the
	//     whole point and a community whose keys we do not have can never be
	//     satisfied. Every pass would re-list it, hand it to the engine, and
	//     collect the same credential failure forever.
	//   - ONLY SUBJECTS WHOSE POST STILL STANDS. An admission whose post row is
	//     tombstoned or absent must not be handed to the engine: accepting a
	//     deleted post writes an acceptance for content that no longer exists,
	//     and the tombstone sweep would then delete it again — a resurrection
	//     loop between two components each behaving correctly on its own.
	//
	// Both exclusions are also enforced by the decider (a driver is a queue, not
	// a security boundary), but doing it here is what keeps the queue's DEPTH an
	// honest signal: a backlog full of subjects nothing can ever settle looks
	// identical, from the outside, to an engine that has stopped working.
	ListPendingSubjects(ctx context.Context, limit int) ([]PendingSubject, error)

	// CountRecentAdmissions counts how many posts this author has had ADMITTED
	// to this community since a point in time — accepted and still-pending rows
	// together.
	//
	// It is the firehose path's quota substrate, and it has to be a different
	// one from the write path's. post_submissions (migration 035) is written by
	// CreatePost, so it only ever sees submissions this AppView handled; a post
	// that arrived over the firehose from an author on another server has no
	// ledger row and never will. Counting the ledger would therefore apply the
	// quota to local users and exempt precisely the remote ones §8 is about.
	//
	// Rejected and removed rows are excluded deliberately: §8 is explicit that a
	// refusal consumes no quota, and counting them would let an author extend
	// their own lockout by continuing to post.
	CountRecentAdmissions(ctx context.Context, communityDID, authorDID string, since time.Time) (int, error)
}

// PendingSubject is one (community, post) pair the engine still owes a decision.
//
// It carries CreatedAt because the driver's health surface reports the age of
// the oldest undecided subject, and that number is the queue's only early
// warning: a backlog that is merely BIG is a busy instance, while a backlog
// whose oldest entry keeps getting older is an engine that has stopped settling
// anything. The ordering the query already applies makes carrying it free.
type PendingSubject struct {
	CommunityDID string
	PostURI      string
	CreatedAt    time.Time
}

// DecisionCode is the reason a post was refused or removed — the value stored
// in community_post_admissions.decision_code and, for the subset that a
// community publishes, in a social.coves.community.removal record's `code`.
//
// It is a plain named string with NO validation function, deliberately. The
// removal lexicon spells `code` as `knownValues`, which is an OPEN set by
// definition — "values are not limited to this set" (§3.3) — so that new codes
// ship without a lexicon break. A Go-side IsValid would re-close what the
// lexicon deliberately left open, and would start rejecting perfectly legal
// codes minted by a remote community running a newer build than ours.
type DecisionCode string

// The six codes the removal lexicon names (§3.3). These are the vocabulary a
// COMMUNITY publishes: they appear in removal records, so a client reading the
// firehose meets them. Kebab-case is the lexicon style guide's convention for
// fixed strings.
const (
	DecisionRuleViolation       DecisionCode = "rule-violation"
	DecisionSpam                DecisionCode = "spam"
	DecisionOffTopic            DecisionCode = "off-topic"
	DecisionIllegalContent      DecisionCode = "illegal-content"
	DecisionAuthorBanned        DecisionCode = "author-banned"
	DecisionModeratorDiscretion DecisionCode = "moderator-discretion"
)

// The admission-time codes. These never reach a community repo: §3.3 is
// explicit that a submission refused before it was ever accepted writes NO
// record, because spam must not bloat the community's repository. They live in
// the same vocabulary anyway — the admissions table's decision_code column
// stores both kinds, and getStatus serves both to the author who asked why.
const (
	// DecisionRateLimitExceeded: the author is over their per-community
	// submission quota (§8).
	DecisionRateLimitExceeded DecisionCode = "rate-limit-exceeded"

	// DecisionDuplicateSubmission: an identical submission from this author to
	// this community is already on the ledger for the current window.
	DecisionDuplicateSubmission DecisionCode = "duplicate-submission"

	// DecisionCommunityNotFound: the at-identifier names no community this
	// AppView has indexed.
	DecisionCommunityNotFound DecisionCode = "community-not-found"

	// DecisionCommunityPrivate: a private community refusing a regular user.
	//
	// It is deliberately the answer for a BANNED user of a private community
	// too — see admitPost's check order, which explains why a ban must not be
	// disclosed through a privacy wall.
	DecisionCommunityPrivate DecisionCode = "community-private"

	// DecisionAggregatorNotAuthorized: a registered aggregator the community
	// has not authorized (or whose authorization it has revoked).
	DecisionAggregatorNotAuthorized DecisionCode = "aggregator-not-authorized"
)
