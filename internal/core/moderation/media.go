package moderation

import (
	"context"
	"database/sql"
	"log/slog"
)

// MediaTransaction provides the operations needed to reconcile a subject's images.
type MediaTransaction interface {
	ActiveRemoval(ctx context.Context, authorityDID, subjectURI string) (*Action, error)
	ReadIndexedComment(ctx context.Context, subjectURI string) (*IndexedComment, error)
	ReadIndexedPost(ctx context.Context, subjectURI string) (*IndexedPost, error)
	// InsertNewMediaBlocks inserts only blocks not already active for the action
	// and returns the blocks it inserted.
	InsertNewMediaBlocks(ctx context.Context, blocks []MediaBlock) ([]MediaBlock, error)
}

// TransactionBinder binds media operations to a caller's transaction.
type TransactionBinder interface {
	BindTransaction(tx *sql.Tx) MediaTransaction
}

// MediaReconciler keeps media blocks in step with a removed subject's
// indexed images when a consumer rewrites the subject.
type MediaReconciler struct {
	binder      TransactionBinder
	instanceDID string
	purger      MediaPurger
}

// NewMediaReconciler builds a MediaReconciler.
func NewMediaReconciler(binder TransactionBinder, instanceDID string, purger MediaPurger) *MediaReconciler {
	return &MediaReconciler{binder: binder, instanceDID: instanceDID, purger: purger}
}

// ReconcileTx blocks images newly present on a subject with an active
// removal, inside the caller's transaction, and returns the new blocks.
func (r *MediaReconciler) ReconcileTx(ctx context.Context, tx *sql.Tx, subjectURI string) ([]MediaBlock, error) {
	bound := r.binder.BindTransaction(tx)
	action, err := bound.ActiveRemoval(ctx, r.instanceDID, subjectURI)
	if err != nil || action == nil {
		return nil, err
	}
	subject, err := readIndexedSubject(ctx, bound, subjectURI)
	if err != nil {
		return nil, err
	}
	if subject == nil {
		return nil, ErrSubjectNotIndexed
	}
	return bound.InsertNewMediaBlocks(ctx, imageMediaBlocks(subject, action))
}

// ReconcileIncomingTx blocks blobs of incoming content the consumer did not
// index, such as a recreate of a deleted post whose tombstone is kept. The
// owner's repository still serves those blobs, and the stored row does not
// name them, so the caller passes the owner and CIDs from the incoming record.
func (r *MediaReconciler) ReconcileIncomingTx(ctx context.Context, tx *sql.Tx, subjectURI, ownerDID string, blobCIDs []string) ([]MediaBlock, error) {
	if len(blobCIDs) == 0 {
		return nil, nil
	}
	bound := r.binder.BindTransaction(tx)
	action, err := bound.ActiveRemoval(ctx, r.instanceDID, subjectURI)
	if err != nil || action == nil {
		return nil, err
	}
	return bound.InsertNewMediaBlocks(ctx, imageMediaBlocks(&indexedSubject{OwnerDID: ownerDID, BlobCIDs: blobCIDs}, action))
}

// Purge removes cached bytes of newly blocked blobs after commit.
func (r *MediaReconciler) Purge(blocks []MediaBlock) {
	purgeMediaBlocks(r.purger, blocks)
}

func purgeMediaBlocks(purger MediaPurger, blocks []MediaBlock) {
	if purger == nil {
		return
	}
	for _, block := range blocks {
		var err error
		if block.OwnerDID == "" {
			err = purger.PurgeBlob(block.BlobCID)
		} else {
			err = purger.PurgeOwnerBlob(block.OwnerDID, block.BlobCID)
		}
		// The block is committed and serving is refused, but the bytes stay on
		// disk until the image proxy's blocked media sweep retries the purge.
		if err != nil {
			if block.OwnerDID == "" {
				slog.Error("moderation media cache purge failed", "action_id", block.ActionID, "cid", block.BlobCID, "error", err)
			} else {
				slog.Error("moderation media cache purge failed", "action_id", block.ActionID, "did", block.OwnerDID, "cid", block.BlobCID, "error", err)
			}
		}
	}
}

func imageMediaBlocks(subject *indexedSubject, action *Action) []MediaBlock {
	var blocks []MediaBlock
	seen := make(map[string]bool)
	for _, cid := range subject.BlobCIDs {
		if seen[cid] {
			continue
		}
		seen[cid] = true
		blocks = append(blocks, MediaBlock{OwnerDID: subject.OwnerDID, BlobCID: cid, ActionID: action.ID})
		if action.Reason == illegalContentReason {
			blocks = append(blocks, MediaBlock{BlobCID: cid, ActionID: action.ID})
		}
	}
	return blocks
}
