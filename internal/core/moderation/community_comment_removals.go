package moderation

import "context"

// CommunityCommentRemoval is a social.coves.community.removal record whose
// subject is a comment, as it arrives from the community's repo.
type CommunityCommentRemoval struct {
	RepositoryDID string // the community repo that holds the record
	RecordKey     string
	RecordCID     string
	Rev           string
	SubjectURI    string
	SubjectCID    string
}

// ReasonSubjectMoved is the reason of the system actions that end and restore
// a community's comment removal when the comment is re-created outside the
// community's thread and back. Moderators cannot choose it.
const ReasonSubjectMoved = "social.coves.moderation.defs#reasonSubjectMoved"

// CommunityCommentRemovalOutcome says what one store call did.
type CommunityCommentRemovalOutcome string

const (
	// CommunityCommentRemovalApplied: the community-scope decision is active.
	CommunityCommentRemovalApplied CommunityCommentRemovalOutcome = "applied"
	// CommunityCommentRemovalHeld: the comment or its thread root is not
	// indexed yet; the removal waits for them.
	CommunityCommentRemovalHeld CommunityCommentRemovalOutcome = "held"
	// CommunityCommentRemovalDemoted: an applied removal whose comment's
	// thread moved to another community was retracted by a system action and
	// returned to held.
	CommunityCommentRemovalDemoted CommunityCommentRemovalOutcome = "demoted"
	// CommunityCommentRemovalRejected: the record does not apply. Either the
	// record's repo is not the community of the post the comment is shown
	// under, and the removal is kept held since the comment may be re-created
	// back under that community's post; or
	// an update under the same record key names a different subject, which is
	// refused and not indexed.
	CommunityCommentRemovalRejected CommunityCommentRemovalOutcome = "rejected"
	// CommunityCommentRemovalStale: the rev gate refused an older event.
	CommunityCommentRemovalStale CommunityCommentRemovalOutcome = "stale"
	// CommunityCommentRemovalRetracted: a deleted record ended an applied decision.
	CommunityCommentRemovalRetracted CommunityCommentRemovalOutcome = "retracted"
	// CommunityCommentRemovalDiscarded: a deleted record had not been applied yet.
	CommunityCommentRemovalDiscarded CommunityCommentRemovalOutcome = "discarded"
	// CommunityCommentRemovalNotFound: no comment removal is indexed under the key.
	CommunityCommentRemovalNotFound CommunityCommentRemovalOutcome = "not-found"
)

// CommunityCommentRemovalStore indexes community comment removal records by
// (repository DID, record key) and turns them into community-scope removal
// decisions once the comment's parent chain is indexed up to the post it is
// shown under (its thread root) and that post's community is the record's repo.
//
// Every entry point commits its own write before reconciling the removals it
// may affect, so whichever of the removal, the comment and the root commits
// last sees the other two. Reconciling one removal locks it, then resolves the
// comment's thread: a held removal whose thread resolves to the record's repo
// is applied; an applied one whose thread resolves to another community is
// demoted to held by a system retract-removal (no actor, ReasonSubjectMoved)
// and re-applied by a system apply-removal if the comment comes back; an
// unresolved thread changes nothing.
type CommunityCommentRemovalStore interface {
	// RecordRemoval indexes a removal record and reconciles it, so a
	// redelivery also demotes an applied removal whose comment moved.
	RecordRemoval(ctx context.Context, removal CommunityCommentRemoval) (CommunityCommentRemovalOutcome, error)
	// DeleteRemoval handles the delete commit of a removal record: a held
	// removal is discarded, an applied one is retracted. NotFound means the key
	// names no comment removal, so the record may have been about a post.
	DeleteRemoval(ctx context.Context, repositoryDID, recordKey, rev string) (CommunityCommentRemovalOutcome, error)
	// ApplyHeldForComment reconciles every removal, held or applied, of a
	// newly indexed comment and of comments already indexed beneath it.
	ApplyHeldForComment(ctx context.Context, commentURI string) error
	// ApplyHeldForRoot reconciles every removal, held or applied, of comments
	// already indexed beneath a newly indexed post.
	ApplyHeldForRoot(ctx context.Context, rootURI string) error
}
