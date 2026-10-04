package moderation

import (
	"context"
	"errors"
	"fmt"

	"Coves/internal/core/comments"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// NewRepositorySubjectReader reads subjects from the existing post and
// comment repositories, dispatching on the URI's collection.
func NewRepositorySubjectReader(postReader PostReader, commentReader CommentReader) SubjectReader {
	return &repositorySubjectReader{postReader: postReader, commentReader: commentReader}
}

type repositorySubjectReader struct {
	postReader    PostReader
	commentReader CommentReader
}

func (r *repositorySubjectReader) ReadSubject(ctx context.Context, uri string) (*IndexedRecord, error) {
	parsed, err := syntax.ParseATURI(uri)
	if err != nil || !parsed.Authority().IsDID() || parsed.RecordKey().String() == "" {
		return nil, fmt.Errorf("%w: expected a record URI with a DID authority", ErrInvalidSubject)
	}

	switch parsed.Collection().String() {
	case PostV2Collection, LegacyPostCollection:
		post, err := r.postReader.GetRawIndexedRow(ctx, uri)
		if errors.Is(err, posts.ErrNotFound) {
			return nil, ErrSubjectNotIndexed
		}
		if err != nil {
			return nil, fmt.Errorf("reading post: %w", err)
		}
		return &IndexedRecord{URI: post.URI, CID: post.CID, Deleted: post.DeletedAt != nil}, nil
	case CommentCollection:
		comment, err := r.commentReader.GetByURI(ctx, uri)
		if errors.Is(err, comments.ErrCommentNotFound) {
			return nil, ErrSubjectNotIndexed
		}
		if err != nil {
			return nil, fmt.Errorf("reading comment: %w", err)
		}
		return &IndexedRecord{
			URI: comment.URI, CID: comment.CID,
			Deleted: comment.DeletedAt != nil && comment.DeletionReason != nil && *comment.DeletionReason == comments.DeletionReasonAuthor,
		}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported collection", ErrInvalidSubject)
	}
}
