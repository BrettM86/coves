//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_StoredSelfLabelsAtEveryPosition(t *testing.T) {
	f := newListHandlerFixture(t)
	thread := f.seedThread("Root", "Body")
	at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
	postReply := "at://" + thread.actor + "/social.coves.community.comment/labeledpostreply"
	parent := "at://" + thread.caller + "/social.coves.community.comment/labeledparent"
	commentReply := "at://" + thread.actor + "/social.coves.community.comment/unlabeledreply"
	commentMention := "at://" + thread.actor + "/social.coves.community.comment/labeledmention"
	postMention := "at://" + thread.actor + "/social.coves.community.postv2/labeledmention"
	f.addComment(postReply, "bafypr", "labeledpostreply", thread.actor, thread.root, "bafyroot", thread.root, "bafyroot", "Spoiler", at)
	f.addComment(parent, "bafyparent", "labeledparent", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", "Parent", at)
	f.addComment(commentReply, "bafycr", "unlabeledreply", thread.actor, thread.root, "bafyroot", parent, "bafyparent", "Unlabeled", at)
	f.addComment(commentMention, "bafycmention", "labeledmention", thread.actor, thread.root, "bafyroot", thread.root, "bafyroot", "Mention", at)
	f.addPost(postMention, "bafypmention", "labeledmention", thread.actor, thread.community, "Mention post", "Body", at)
	for _, seed := range []struct{ table, uri, labels string }{
		{"posts", thread.root, `{"values":[{"val":"nsfw"}]}`},
		{"posts", postMention, `{"values":[{"val":"nsfw"}]}`},
		{"comments", postReply, `{"values":[{"val":"spoiler"}]}`},
		{"comments", parent, `{"values":[{"val":"violence"}]}`},
		{"comments", commentMention, `{"values":[{"val":"spoiler"}]}`},
	} {
		// Both consumers store the full selfLabels object in the JSONB column.
		query := "UPDATE " + seed.table + " SET content_labels = $1 WHERE uri = $2"
		_, err := f.db.ExecContext(context.Background(), query, seed.labels, seed.uri)
		require.NoError(t, err)
	}
	f.addReply(thread.caller, notifications.ReasonPostReply, postReply, "bafypr", thread.actor, thread.root, thread.root, at, at)
	f.addReply(thread.caller, notifications.ReasonCommentReply, commentReply, "bafycr", thread.actor, parent, thread.root, at, at.Add(time.Second))
	f.addMention(thread.caller, commentMention, "bafycmention", thread.actor, thread.root, at, at.Add(2*time.Second))
	f.addMention(thread.caller, postMention, "bafypmention", thread.actor, postMention, at, at.Add(3*time.Second))
	f.addUpvoteGroup(thread.caller, thread.root, thread.root, at.Add(4*time.Second))
	f.addUpvoteGroup(thread.caller, parent, thread.root, at.Add(5*time.Second))
	voter := "did:plc:labelvoter" + f.id
	f.addUser(voter, "labelvoter"+f.id+".test", "Voter")
	f.addUpvoteVote(voter, thread.root, "up", at)
	f.addUpvoteVote(voter, parent, "up", at.Add(time.Second))

	postViews, err := postgres.NewPostRepository(f.db).GetViewsByURIs(context.Background(), []string{thread.root, postMention}, "")
	require.NoError(t, err)
	for uri, want := range map[string]string{thread.root: `{"values":[{"val":"nsfw"}]}`, postMention: `{"values":[{"val":"nsfw"}]}`} {
		require.NotNil(t, postViews[uri], "post fixture %s", uri)
		labels, err := json.Marshal(postViews[uri].Record.(map[string]interface{})["labels"])
		require.NoError(t, err)
		require.JSONEq(t, want, string(labels), "feed record.labels for %s", uri)
	}
	commentViews, err := postgres.NewCommentRepository(f.db).GetByURIsBatch(context.Background(), []string{postReply, parent, commentReply, commentMention})
	require.NoError(t, err)
	for uri, want := range map[string]string{postReply: `{"values":[{"val":"spoiler"}]}`, parent: `{"values":[{"val":"violence"}]}`, commentMention: `{"values":[{"val":"spoiler"}]}`} {
		require.NotNil(t, commentViews[uri])
		require.NotNil(t, commentViews[uri].ContentLabels)
		require.JSONEq(t, want, *commentViews[uri].ContentLabels, "comment ContentLabels for %s", uri)
	}
	require.NotNil(t, commentViews[commentReply])
	require.Nil(t, commentViews[commentReply].ContentLabels)

	response := f.request(thread.caller, "")
	require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
	rows := listRawRows(t, response)
	require.Len(t, rows, 6)
	seen := make(map[string]bool)
	for _, row := range rows {
		reason := string(row["reason"])
		root := placeholderObject(t, row, "rootPost")
		require.JSONEq(t, `{"values":[{"val":"nsfw"}]}`, string(root["labels"]), "rootPost.labels for %s", row)
		switch reason {
		case `"postReply"`, `"commentReply"`, `"mention"`:
			record := placeholderObject(t, row, "record")
			uri := string(record["uri"])
			require.False(t, seen[uri], "duplicate notification %s", uri)
			seen[uri] = true
			switch uri {
			case `"` + postReply + `"`:
				require.JSONEq(t, `{"values":[{"val":"spoiler"}]}`, string(record["labels"]))
				require.JSONEq(t, `{"values":[{"val":"nsfw"}]}`, string(placeholderObject(t, row, "subject")["labels"]))
			case `"` + commentReply + `"`:
				require.NotContains(t, record, "labels", "unlabeled reply does not inherit from its subject or root")
				require.JSONEq(t, `{"values":[{"val":"violence"}]}`, string(placeholderObject(t, row, "subject")["labels"]))
			case `"` + commentMention + `"`:
				require.JSONEq(t, `{"values":[{"val":"spoiler"}]}`, string(record["labels"]))
				require.NotContains(t, row, "subject")
			case `"` + postMention + `"`:
				require.JSONEq(t, `{"values":[{"val":"nsfw"}]}`, string(record["labels"]))
				require.NotContains(t, row, "subject")
			default:
				t.Fatalf("unexpected record %s", uri)
			}
		case `"upvote"`:
			require.NotContains(t, row, "record")
			subject := placeholderObject(t, row, "subject")
			uri := string(subject["uri"])
			require.False(t, seen[uri], "duplicate upvote group %s", uri)
			seen[uri] = true
			switch uri {
			case `"` + thread.root + `"`:
				require.JSONEq(t, `{"values":[{"val":"nsfw"}]}`, string(subject["labels"]))
			case `"` + parent + `"`:
				require.JSONEq(t, `{"values":[{"val":"violence"}]}`, string(subject["labels"]))
			default:
				t.Fatalf("unexpected upvote subject %s", uri)
			}
		default:
			t.Fatalf("unexpected reason %s", reason)
		}
	}
	require.Len(t, seen, 6)
}
