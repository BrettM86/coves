package tests

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

type mentionLexiconReader struct {
	notifications.ReadRepository
	page notifications.ListPage
}

func (r mentionLexiconReader) List(context.Context, string, string, int) (notifications.ListPage, error) {
	return r.page, nil
}

type mentionLexiconProfiles struct{}

func (mentionLexiconProfiles) GetByDIDs(context.Context, []string) (map[string]*users.User, error) {
	return map[string]*users.User{}, nil
}

type mentionLexiconPosts map[string]*posts.PostView

func (p mentionLexiconPosts) GetViewsByURIs(context.Context, []string, string) (map[string]*posts.PostView, error) {
	return p, nil
}

type mentionLexiconComments map[string]*comments.Comment

func (c mentionLexiconComments) GetByURIsBatch(context.Context, []string) (map[string]*comments.Comment, error) {
	return c, nil
}

func TestNotificationListLexicon_RenderedMentionsValidate(t *testing.T) {
	catalog, recordID, _ := placeholderListLexicon(t)
	const root = "at://did:plc:owner/social.coves.community.postv2/root"
	const comment = "at://did:plc:actor/social.coves.community.comment/mention"
	const post = "at://did:plc:actor/social.coves.community.postv2/mention"
	const deleted = "at://did:plc:actor/social.coves.community.postv2/deleted"
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	rows := []struct {
		uri, root             string
		record, rootReference notifications.ListedReference
	}{
		{comment, root, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafycomment"}, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyroot"}},
		{post, post, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafypost"}, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafypost"}},
		{deleted, deleted, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeleted"}, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeleted"}},
	}
	listed := make([]notifications.ListedNotification, 0, len(rows))
	for index, row := range rows {
		listed = append(listed, notifications.ListedNotification{Reason: notifications.ReasonMention, ActorDID: "did:plc:actor", RecordURI: row.uri, RootPostURI: row.root, Record: row.record, RootPost: row.rootReference, RecordCreatedAt: at, SortAt: at.Add(time.Duration(index) * time.Second)})
	}
	service := notifications.NewListService(mentionLexiconReader{page: notifications.ListPage{Notifications: listed}}, mentionLexiconProfiles{}, mentionLexiconPosts{
		root: {URI: root, CID: "bafyroot", Record: map[string]interface{}{"title": "Root"}},
		post: {URI: post, CID: "bafypost", Record: map[string]interface{}{"title": "Mentioned"}},
	}, mentionLexiconComments{comment: {URI: comment, CID: "bafycomment", Content: "Hello"}})
	output, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
	require.NoError(t, err)
	require.Len(t, output.Notifications, 3, "live comment, live post, and post placeholder mentions must be rendered")
	for index, row := range output.Notifications {
		require.Equal(t, notifications.ReasonMention, row.Reason)
		require.Equal(t, rows[index].uri, row.Record.URI)
		require.Nil(t, row.Subject)
	}
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	data, err := atdata.UnmarshalJSON(encoded)
	require.NoError(t, err)
	data["$type"] = recordID
	require.NoError(t, lexicon.ValidateRecord(catalog, data, recordID, 0))
}
