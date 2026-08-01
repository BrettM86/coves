//go:build integration

package postgres

import (
	"Coves/internal/core/comments"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"
	"context"
	"fmt"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// The comment repository's four listing queries, against a real comment tree.
//
// ListByRoot, ListByParent, CountByParent and ListByCommenter each slice the
// same table differently — whole thread, direct children, child count, one
// author's comments — and the only thing that can tell them apart is SQL that
// runs. They are exercised here rather than through the consumer or the query
// service because neither of those calls all four: a service test that happened
// to cover ListByParent would say nothing about ListByCommenter, and a
// regression in the WHERE clause of one query is invisible from the others.
//
// The tree is written through commentRepo.Create rather than through the
// firehose consumer on purpose. What is under test is the reading side; going
// in through the consumer would make these four assertions fail whenever the
// consumer broke, which is what comment_consumer_test.go in internal/core/comments
// is for.

func TestCommentRepository_Queries(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	ctx := context.Background()
	commentRepo := NewCommentRepository(db)

	testUser := fixtures.User(t, db, "query.test", "did:plc:query123")
	testCommunity, err := fixtures.Community(ctx, db, "querycommunity", "owner6.test")
	if err != nil {
		t.Fatalf("Failed to create test community: %v", err)
	}
	postURI := fixtures.Post(t, db, testCommunity, testUser.DID, "Query Test", 0, time.Now())

	// Create a comment tree
	// Post
	//  |- Comment 1
	//      |- Comment 2
	//      |- Comment 3
	//  |- Comment 4
	//
	// The shape is what makes the four queries distinguishable: the post has two
	// direct replies but four descendants, so a ListByParent that quietly
	// behaved like ListByRoot would be caught.

	comment1 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.community.comment/1", testUser.DID),
		CID:          "bafyc1",
		RKey:         "1",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    postURI,
		ParentCID:    "bafypost",
		Content:      "Comment 1",
		Langs:        []string{},
		CreatedAt:    time.Now(),
	}

	comment2 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.community.comment/2", testUser.DID),
		CID:          "bafyc2",
		RKey:         "2",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    comment1.URI,
		ParentCID:    "bafyc1",
		Content:      "Comment 2 (reply to 1)",
		Langs:        []string{},
		CreatedAt:    time.Now().Add(1 * time.Second),
	}

	comment3 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.community.comment/3", testUser.DID),
		CID:          "bafyc3",
		RKey:         "3",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    comment1.URI,
		ParentCID:    "bafyc1",
		Content:      "Comment 3 (reply to 1)",
		Langs:        []string{},
		CreatedAt:    time.Now().Add(2 * time.Second),
	}

	comment4 := &comments.Comment{
		URI:          fmt.Sprintf("at://%s/social.coves.community.comment/4", testUser.DID),
		CID:          "bafyc4",
		RKey:         "4",
		CommenterDID: testUser.DID,
		RootURI:      postURI,
		RootCID:      "bafypost",
		ParentURI:    postURI,
		ParentCID:    "bafypost",
		Content:      "Comment 4",
		Langs:        []string{},
		CreatedAt:    time.Now().Add(3 * time.Second),
	}

	// Create all comments
	for i, c := range []*comments.Comment{comment1, comment2, comment3, comment4} {
		if err := commentRepo.Create(ctx, c); err != nil {
			t.Fatalf("Failed to create comment %d: %v", i+1, err)
		}
	}

	t.Run("ListByRoot returns all comments in thread", func(t *testing.T) {
		thread, err := commentRepo.ListByRoot(ctx, postURI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list by root: %v", err)
		}

		if len(thread) != 4 {
			t.Errorf("Expected 4 comments, got %d", len(thread))
		}
	})

	t.Run("ListByParent returns direct replies", func(t *testing.T) {
		// Direct replies to post
		postReplies, err := commentRepo.ListByParent(ctx, postURI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list post replies: %v", err)
		}

		if len(postReplies) != 2 {
			t.Errorf("Expected 2 direct replies to post, got %d", len(postReplies))
		}

		// Direct replies to comment1
		comment1Replies, err := commentRepo.ListByParent(ctx, comment1.URI, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list comment1 replies: %v", err)
		}

		if len(comment1Replies) != 2 {
			t.Errorf("Expected 2 direct replies to comment1, got %d", len(comment1Replies))
		}
	})

	t.Run("CountByParent returns correct counts", func(t *testing.T) {
		postCount, err := commentRepo.CountByParent(ctx, postURI)
		if err != nil {
			t.Fatalf("Failed to count post replies: %v", err)
		}

		if postCount != 2 {
			t.Errorf("Expected 2 direct replies to post, got %d", postCount)
		}

		comment1Count, err := commentRepo.CountByParent(ctx, comment1.URI)
		if err != nil {
			t.Fatalf("Failed to count comment1 replies: %v", err)
		}

		if comment1Count != 2 {
			t.Errorf("Expected 2 direct replies to comment1, got %d", comment1Count)
		}
	})

	t.Run("ListByCommenter returns user's comments", func(t *testing.T) {
		userComments, err := commentRepo.ListByCommenter(ctx, testUser.DID, 100, 0)
		if err != nil {
			t.Fatalf("Failed to list by commenter: %v", err)
		}

		if len(userComments) != 4 {
			t.Errorf("Expected 4 comments by user, got %d", len(userComments))
		}
	})
}
