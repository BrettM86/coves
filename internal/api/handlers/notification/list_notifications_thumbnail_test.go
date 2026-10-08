//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_StoredPostThumbnailsMatchFeedProjection(t *testing.T) {
	blobs.ResetImageURLConfigForTesting()
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
	f := newListHandlerFixture(t)
	thread := f.seedThread("Base", "Body")
	at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
	const firstCID = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	const legacyCID = "bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm"
	const imageCID = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	postsToCheck := []struct {
		key, uri, embed, literal string
	}{
		{"link", "at://" + thread.caller + "/social.coves.community.postv2/thumb-link", `{"$type":"social.coves.embed.external","external":{"uri":"https://article.example.test","title":"Article","description":"Summary","thumb":{"$type":"blob","ref":{"$link":"` + firstCID + `"},"mimeType":"image/jpeg","size":123}}}`, "https://img.example.test/img/embed_thumbnail/plain/" + thread.caller + "/" + firstCID},
		{"legacy", "at://" + thread.community + "/social.coves.community.post/thumb-legacy", `{"$type":"social.coves.embed.external","external":{"uri":"https://article.example.test","title":"Article","description":"Summary","thumb":{"$type":"blob","ref":{"$link":"` + legacyCID + `"},"mimeType":"image/jpeg","size":123}}}`, "https://img.example.test/img/embed_thumbnail/plain/" + thread.community + "/" + legacyCID},
		{"image", "at://" + thread.caller + "/social.coves.community.postv2/thumb-image", `{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"` + imageCID + `"},"mimeType":"image/jpeg","size":123},"alt":"First image"},{"image":{"$type":"blob","ref":{"$link":"` + firstCID + `"},"mimeType":"image/jpeg","size":123},"alt":"Second image"}]}`, "https://img.example.test/img/content_preview/plain/" + thread.caller + "/" + imageCID},
		{"text", "at://" + thread.caller + "/social.coves.community.postv2/thumb-text", "", ""},
	}
	voter := "did:plc:thumbnailvoter" + f.id
	f.addUser(voter, "thumbnailvoter"+f.id+".test", "Voter")
	for index, post := range postsToCheck {
		f.addPost(post.uri, "bafy"+post.key, "thumb-"+post.key, thread.caller, thread.community, "Title "+post.key, "Body", at)
		if post.embed != "" {
			_, err := f.db.ExecContext(context.Background(), `UPDATE posts SET embed = $1 WHERE uri = $2`, post.embed, post.uri)
			require.NoError(t, err)
		}
		replyURI := "at://" + thread.actor + "/social.coves.community.comment/thumb-reply-" + post.key
		f.addComment(replyURI, "bafyre"+post.key, "thumb-reply-"+post.key, thread.actor, post.uri, "bafy"+post.key, post.uri, "bafy"+post.key, "Reply", at)
		f.addReply(thread.caller, notifications.ReasonPostReply, replyURI, "bafyre"+post.key, thread.actor, post.uri, post.uri, at, at.Add(time.Duration(index*3)*time.Second))
		f.addUpvoteGroup(thread.caller, post.uri, post.uri, at.Add(time.Duration(index*3+1)*time.Second))
		f.addUpvoteVote(voter, post.uri, "up", at)
		f.addMention(thread.caller, post.uri, "bafy"+post.key, thread.caller, post.uri, at, at.Add(time.Duration(index*3+2)*time.Second))
	}
	// Comments can store the very same images embed, but neither a comment
	// subject nor a comment record may acquire a thumbnail.
	commentSubject := "at://" + thread.caller + "/social.coves.community.comment/thumb-comment-subject"
	commentRecord := "at://" + thread.actor + "/social.coves.community.comment/thumb-comment-record"
	f.addComment(commentSubject, "bafycs", "thumb-comment-subject", thread.caller, postsToCheck[0].uri, "bafylink", postsToCheck[0].uri, "bafylink", "Subject", at)
	f.addComment(commentRecord, "bafycr", "thumb-comment-record", thread.actor, postsToCheck[0].uri, "bafylink", commentSubject, "bafycs", "Reply", at)
	for _, uri := range []string{commentSubject, commentRecord} {
		_, err := f.db.ExecContext(context.Background(), `UPDATE comments SET embed = $1 WHERE uri = $2`, postsToCheck[2].embed, uri)
		require.NoError(t, err)
	}
	f.addReply(thread.caller, notifications.ReasonCommentReply, commentRecord, "bafycr", thread.actor, commentSubject, postsToCheck[0].uri, at, at.Add(13*time.Second))
	f.addMention(thread.caller, commentRecord, "bafycr", thread.actor, postsToCheck[0].uri, at, at.Add(14*time.Second))
	f.addUpvoteGroup(thread.caller, commentSubject, postsToCheck[0].uri, at.Add(15*time.Second))
	f.addUpvoteVote(voter, commentSubject, "up", at)

	uris := make([]string, 0, len(postsToCheck))
	for _, post := range postsToCheck {
		uris = append(uris, post.uri)
	}
	views, err := postgres.NewPostRepository(f.db).GetViewsByURIs(context.Background(), uris, "")
	require.NoError(t, err)
	feedURLs := map[string]string{}
	for _, post := range postsToCheck {
		view := views[post.uri]
		require.NotNil(t, view, "feed view for %s", post.uri)
		posts.TransformBlobRefsToURLs(view)
		if post.embed == "" {
			require.Nil(t, view.Embed)
			continue
		}
		embed, ok := view.Embed.(map[string]interface{})
		require.True(t, ok)
		var url string
		if post.key == "image" {
			require.Equal(t, "social.coves.embed.images#view", embed["$type"])
			images, ok := embed["images"].([]interface{})
			require.True(t, ok)
			require.Len(t, images, 2)
			url, ok = images[0].(map[string]interface{})["thumb"].(string)
			require.True(t, ok)
		} else {
			require.Equal(t, "social.coves.embed.external#view", embed["$type"])
			url, ok = embed["external"].(map[string]interface{})["thumb"].(string)
			require.True(t, ok)
		}
		require.Equal(t, post.literal, url, "literal proxy URL and owner for %s", post.key)
		feedURLs[post.uri] = url
	}

	response := f.request(thread.caller, "")
	require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
	rows := listRawRows(t, response)
	require.Len(t, rows, 15)
	counts := map[string]map[string]int{}
	for _, post := range postsToCheck {
		counts[post.uri] = map[string]int{}
	}
	commentPositions := 0
	for _, row := range rows {
		root := placeholderObject(t, row, "rootPost")
		var rootURI string
		require.NoError(t, json.Unmarshal(root["uri"], &rootURI))
		_, known := counts[rootURI]
		require.True(t, known, "unexpected root %s", rootURI)
		check := func(position string, object map[string]json.RawMessage, uri string) {
			t.Helper()
			if uri == commentSubject || uri == commentRecord {
				require.NotContains(t, object, "thumbnail", "comment %s %s", uri, position)
				commentPositions++
				return
			}
			if uri == postsToCheck[3].uri {
				require.NotContains(t, object, "thumbnail", "text post at %s", position)
			} else {
				require.JSONEq(t, `"`+feedURLs[uri]+`"`, string(object["thumbnail"]), "%s %s matches feed projection", uri, position)
			}
			counts[uri][position]++
		}
		check("rootPost", root, rootURI)
		if string(row["reason"]) == `"mention"` {
			record := placeholderObject(t, row, "record")
			var uri string
			require.NoError(t, json.Unmarshal(record["uri"], &uri))
			check("record", record, uri)
		} else {
			subject := placeholderObject(t, row, "subject")
			var uri string
			require.NoError(t, json.Unmarshal(subject["uri"], &uri))
			check("subject", subject, uri)
			if string(row["reason"]) == `"commentReply"` {
				check("record", placeholderObject(t, row, "record"), commentRecord)
			}
		}
	}
	for _, post := range postsToCheck {
		rootCount := 3
		if post.key == "link" {
			rootCount = 6 // The three comment notifications share this root.
		}
		require.Equal(t, map[string]int{"rootPost": rootCount, "subject": 2, "record": 1}, counts[post.uri], post.key)
	}
	require.Equal(t, 4, commentPositions, "comment reply subject and record, mention record, and upvote subject")
}
