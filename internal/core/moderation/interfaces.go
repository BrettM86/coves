package moderation

import (
	"context"

	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
)

// Authority answers whether a DID may act as an instance admin.
type Authority interface {
	IsInstanceAdmin(did string) bool
}

// SubjectReader reports what the AppView has indexed for a subject URI.
// It returns ErrSubjectNotIndexed for a URI the AppView has never seen.
type SubjectReader interface {
	ReadSubject(ctx context.Context, uri string) (*IndexedRecord, error)
}

// PostReader is the ungated post lookup the reader adapter needs.
type PostReader interface {
	GetRawIndexedRow(ctx context.Context, uri string) (*posts.Post, error)
}

// CommentReader is the admission-blind comment lookup the reader adapter needs.
type CommentReader interface {
	GetByURI(ctx context.Context, uri string) (*comments.Comment, error)
}

// Service is the moderation domain's read and mutation surface.
type Service interface {
	GetSubjectState(ctx context.Context, subject string) (*SubjectState, error)
	RemoveContent(ctx context.Context, actorDID string, request RemoveContentRequest) (*MutationResult, error)
	RestoreContent(ctx context.Context, actorDID string, request RestoreContentRequest) (*MutationResult, error)
}
