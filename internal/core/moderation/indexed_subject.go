package moderation

import (
	"context"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

type indexedSubject struct {
	URI           string
	CID           string
	Collection    string
	AuthorDeleted bool
	OwnerDID      string
	CommunityDID  string
	BlobCIDs      []string
}

type indexedSubjectReader interface {
	ReadIndexedComment(ctx context.Context, subjectURI string) (*IndexedComment, error)
	ReadIndexedPost(ctx context.Context, subjectURI string) (*IndexedPost, error)
}

func readIndexedSubject(ctx context.Context, reader indexedSubjectReader, subjectURI string) (*indexedSubject, error) {
	uri, err := syntax.ParseATURI(subjectURI)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid subject URI: %w", ErrInvalidSubject, err)
	}
	collection := uri.Collection().String()
	switch collection {
	case CommentCollection:
		comment, err := reader.ReadIndexedComment(ctx, subjectURI)
		if err != nil || comment == nil {
			return nil, err
		}
		return &indexedSubject{
			URI: comment.URI, CID: comment.CID, Collection: collection,
			AuthorDeleted: comment.AuthorDeleted, OwnerDID: comment.OwnerDID,
			CommunityDID: comment.CommunityDID, BlobCIDs: comment.ImageCIDs,
		}, nil
	case PostV2Collection, LegacyPostCollection:
		post, err := reader.ReadIndexedPost(ctx, subjectURI)
		if err != nil || post == nil {
			return nil, err
		}
		return &indexedSubject{
			URI: post.URI, CID: post.CID, Collection: collection,
			AuthorDeleted: post.AuthorDeleted, OwnerDID: post.OwnerDID,
			CommunityDID: post.CommunityDID, BlobCIDs: post.BlobCIDs,
		}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported subject collection", ErrInvalidSubject)
	}
}
