package jetstream

import (
	"context"
	"fmt"
	"log"

	"Coves/internal/core/moderation"
)

// WithPostCommunityCommentRemovals routes community removal records whose
// subject is a comment to the store, and lets a newly indexed root post apply
// removals held for comments under it. Without it, comment-subject removals
// are not indexed.
func WithPostCommunityCommentRemovals(store moderation.CommunityCommentRemovalStore) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.communityCommentRemovals = store }
}

// WithCommentCommunityRemovals lets a newly indexed comment apply community
// removals held for it. Production passes the same store to both consumers.
func WithCommentCommunityRemovals(store moderation.CommunityCommentRemovalStore) CommentEventConsumerOption {
	return func(c *CommentEventConsumer) { c.communityRemovals = store }
}

// recordCommunityCommentRemoval indexes a community's removal of a comment.
// Whether the community may remove it is decided by the store, against the
// community of the comment's thread root.
func (c *PostEventConsumer) recordCommunityCommentRemoval(
	ctx context.Context, communityDID string, commit *CommitEvent, decision *communityDecisionRecord,
) error {
	outcome, err := c.communityCommentRemovals.RecordRemoval(ctx, moderation.CommunityCommentRemoval{
		RepositoryDID: communityDID, RecordKey: commit.RKey, RecordCID: commit.CID, Rev: commit.Rev,
		SubjectURI: decision.Subject.URI, SubjectCID: decision.Subject.CID,
	})
	if err != nil {
		return fmt.Errorf("recording the removal of comment %s by %s: %w", decision.Subject.URI, communityDID, err)
	}
	log.Printf("INFO: community comment removal %s/%s of %s: %s", communityDID, commit.RKey, decision.Subject.URI, outcome)
	return nil
}

// deleteCommunityCommentRemoval handles a removal delete commit whose key
// names an indexed comment removal. handled is false when the key is unknown,
// so the record may have been about a post.
func (c *PostEventConsumer) deleteCommunityCommentRemoval(
	ctx context.Context, communityDID string, commit *CommitEvent,
) (bool, error) {
	outcome, err := c.communityCommentRemovals.DeleteRemoval(ctx, communityDID, commit.RKey, commit.Rev)
	if err != nil {
		return false, fmt.Errorf("withdrawing comment removal %s/%s: %w", communityDID, commit.RKey, err)
	}
	if outcome == moderation.CommunityCommentRemovalNotFound {
		return false, nil
	}
	log.Printf("INFO: community comment removal %s/%s deleted: %s", communityDID, commit.RKey, outcome)
	return true, nil
}
