package moderation

import (
	"context"
	"database/sql"
	"fmt"
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

// MediaReconciler adds owner-scoped blocks for images introduced after a
// subject is removed. Ownerless (every-owner) blocks exist only for an
// illegal-content removal and cover only the images indexed when the admin
// removed the subject; reconciliation never adds one. Both reconcile methods
// therefore refuse an empty owner DID, which the store records as ownerless.
type MediaReconciler struct {
	binder      TransactionBinder
	instanceDID string
	purger      MediaPurger
	cdnPurger   CDNPurger
}

// NewMediaReconciler builds a MediaReconciler.
func NewMediaReconciler(binder TransactionBinder, instanceDID string, purger MediaPurger, options ...MediaReconcilerOption) *MediaReconciler {
	reconciler := &MediaReconciler{binder: binder, instanceDID: instanceDID, purger: purger}
	for _, option := range options {
		option(reconciler)
	}
	return reconciler
}

// ReconcileTx blocks images newly present on a subject with an active
// removal for that subject's owner, inside the caller's transaction, and
// returns the new blocks.
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
	if subject.OwnerDID == "" {
		return nil, fmt.Errorf("%w: indexed subject has no owner DID", ErrInvalidSubject)
	}
	return bound.InsertNewMediaBlocks(ctx, ownerImageMediaBlocks(subject, action))
}

// ReconcileIncomingTx blocks blobs of incoming content the consumer did not
// index, such as a recreate of a deleted post whose tombstone is kept. The
// owner's repository still serves those blobs, and the stored row does not
// name them, so the caller passes the owner and CIDs from the incoming record.
// These new blocks apply only to that owner, regardless of removal reason.
func (r *MediaReconciler) ReconcileIncomingTx(ctx context.Context, tx *sql.Tx, subjectURI, ownerDID string, blobCIDs []string) ([]MediaBlock, error) {
	if len(blobCIDs) == 0 {
		return nil, nil
	}
	bound := r.binder.BindTransaction(tx)
	action, err := bound.ActiveRemoval(ctx, r.instanceDID, subjectURI)
	if err != nil || action == nil {
		return nil, err
	}
	if ownerDID == "" {
		return nil, fmt.Errorf("%w: incoming content has no owner DID", ErrInvalidSubject)
	}
	return bound.InsertNewMediaBlocks(ctx, ownerImageMediaBlocks(&indexedSubject{OwnerDID: ownerDID, BlobCIDs: blobCIDs}, action))
}

// Purge removes cached bytes of newly blocked blobs after commit.
func (r *MediaReconciler) Purge(blocks []MediaBlock) {
	purgeMediaBlocks(r.purger, blocks)
	purgeCDNMediaBlocks(context.Background(), r.cdnPurger, blocks)
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

// ownerImageMediaBlocks returns one owner-scoped block per distinct CID.
// Reconciliation uses it directly, and imageMediaBlocks builds on it.
func ownerImageMediaBlocks(subject *indexedSubject, action *Action) []MediaBlock {
	var blocks []MediaBlock
	seen := make(map[string]bool)
	for _, cid := range subject.BlobCIDs {
		if seen[cid] {
			continue
		}
		seen[cid] = true
		blocks = append(blocks, MediaBlock{OwnerDID: subject.OwnerDID, BlobCID: cid, ActionID: action.ID})
	}
	return blocks
}

// imageMediaBlocks returns the blocks an admin removal installs: the owner
// blocks plus, for illegal content, an every-owner block per CID.
func imageMediaBlocks(subject *indexedSubject, action *Action) []MediaBlock {
	ownerBlocks := ownerImageMediaBlocks(subject, action)
	if action.Reason != illegalContentReason {
		return ownerBlocks
	}
	var blocks []MediaBlock
	for _, ownerBlock := range ownerBlocks {
		blocks = append(blocks, ownerBlock, MediaBlock{BlobCID: ownerBlock.BlobCID, ActionID: action.ID})
	}
	return blocks
}
