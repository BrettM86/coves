package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"Coves/internal/core/classification"
)

// ClassificationEnqueuer enqueues a classification job inside the
// indexing transaction.
type ClassificationEnqueuer interface {
	EnqueueInTx(ctx context.Context, tx *sql.Tx, job classification.NewJob, classifiedFieldsChanged bool) error
	// ErasureGateTx takes authorDID's shared erasure lock in tx and reports
	// whether the author has an erasure marker. tx must be READ COMMITTED.
	ErasureGateTx(ctx context.Context, tx *sql.Tx, authorDID string) (bool, error)
}

// WithClassificationEnqueuer enqueues a classification job, under evaluation,
// for every post the consumer indexes or whose content update applies.
func WithClassificationEnqueuer(e ClassificationEnqueuer, evaluation classification.Evaluation) PostEventConsumerOption {
	return func(c *PostEventConsumer) {
		c.classificationEnqueuer = e
		c.classificationEvaluation = evaluation
	}
}

// enqueuePostClassificationTx enqueues a post classification job in the
// indexing transaction tx. Without an enqueuer it does nothing.
func (c *PostEventConsumer) enqueuePostClassificationTx(ctx context.Context, tx *sql.Tx, uri, cid, authorDID, communityDID string, classifiedFieldsChanged bool) error {
	if c.classificationEnqueuer == nil {
		return nil
	}
	err := c.classificationEnqueuer.EnqueueInTx(ctx, tx, classification.NewJob{
		SubjectURI:   uri,
		SubjectKind:  classification.SubjectKindPost,
		RecordCID:    cid,
		AuthorDID:    authorDID,
		CommunityDID: communityDID,
		Evaluation:   c.classificationEvaluation,
	}, classifiedFieldsChanged)
	if err != nil {
		return fmt.Errorf("enqueue classification for post %s: %w", uri, err)
	}
	return nil
}

// authorErasureGateTx takes authorDID's shared erasure lock in tx and reports
// whether the author is erased, through the notifications repository or, when
// only classification is wired, the classification enqueuer. With neither
// wired nothing in the transaction depends on erasure, so it reports false.
// Callers take it before locking the post row, the lock order account
// deletion uses.
func (c *PostEventConsumer) authorErasureGateTx(ctx context.Context, tx *sql.Tx, authorDID string) (bool, error) {
	if c.notifications != nil {
		return c.notifications.ErasureGateTx(ctx, tx, authorDID)
	}
	if c.classificationEnqueuer != nil {
		return c.classificationEnqueuer.ErasureGateTx(ctx, tx, authorDID)
	}
	return false, nil
}

// WithCommentClassificationEnqueuer enqueues a classification job, under
// evaluation, for every comment the consumer indexes or updates.
func WithCommentClassificationEnqueuer(e ClassificationEnqueuer, evaluation classification.Evaluation) CommentEventConsumerOption {
	return func(c *CommentEventConsumer) {
		c.classificationEnqueuer = e
		c.classificationEvaluation = evaluation
	}
}

// enqueueCommentClassificationTx enqueues a comment classification job in the
// indexing transaction tx. The job's community is the root post's community,
// empty while the root post is not indexed. Without an enqueuer it does nothing.
func (c *CommentEventConsumer) enqueueCommentClassificationTx(ctx context.Context, tx *sql.Tx, uri, cid, commenterDID, rootURI string, classifiedFieldsChanged bool) error {
	if c.classificationEnqueuer == nil {
		return nil
	}
	var communityDID string
	err := tx.QueryRowContext(ctx, `SELECT community_did FROM posts WHERE uri = $1`, rootURI).Scan(&communityDID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read root post community for comment %s classification: %w", uri, err)
	}
	err = c.classificationEnqueuer.EnqueueInTx(ctx, tx, classification.NewJob{
		SubjectURI:   uri,
		SubjectKind:  classification.SubjectKindComment,
		RecordCID:    cid,
		AuthorDID:    commenterDID,
		CommunityDID: communityDID,
		Evaluation:   c.classificationEvaluation,
	}, classifiedFieldsChanged)
	if err != nil {
		return fmt.Errorf("enqueue classification for comment %s: %w", uri, err)
	}
	return nil
}
