package notifications_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/require"
)

type recordingPlaceholderPosts struct {
	views     anonymousPosts
	requested []string
}

func (p *recordingPlaceholderPosts) GetViewsByURIs(_ context.Context, uris []string, viewer string) (map[string]*posts.PostView, error) {
	p.requested = append(p.requested, uris...)
	if viewer != "" {
		return nil, nil
	}
	return p.views, nil
}

type recordingPlaceholderComments struct {
	views     cannedComments
	requested []string
}

func (c *recordingPlaceholderComments) GetByURIsBatch(_ context.Context, uris []string) (map[string]*comments.Comment, error) {
	c.requested = append(c.requested, uris...)
	return c.views, nil
}

func TestListNotifications_PlaceholderReferences(t *testing.T) {
	const rootURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const parentURI = "at://did:plc:owner/social.coves.community.comment/parent"
	const replyURI = "at://did:plc:actor/social.coves.community.comment/reply"
	const actorDID = "did:plc:actor"
	at := time.Date(2026, 9, 20, 9, 0, 0, 123456000, time.UTC)
	deletedAt := at
	live := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylisedroot"}
	parentLive := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedparent"}
	replyLive := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedreply"}
	post := &posts.PostView{URI: rootURI, CID: "bafylookuproot", Record: map[string]interface{}{"title": "Root title", "content": "Private body"}, Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.coves.social", Name: "Community"}}
	parent := &comments.Comment{URI: parentURI, CID: "bafylookupparent", Content: "Parent preview"}
	reply := &comments.Comment{URI: replyURI, CID: "bafylookupreply", Content: "Reply excerpt"}
	rootLiveJSON := `{"uri":"` + rootURI + `","cid":"bafylisedroot","title":"Root title","community":{"did":"did:plc:community","handle":"community.coves.social","name":"Community"}}`
	subjectLiveJSON := `{"uri":"` + rootURI + `","cid":"bafylisedroot","preview":"Root title"}`
	parentLiveJSON := `{"uri":"` + parentURI + `","cid":"bafylistedparent","preview":"Parent preview"}`
	recordLiveJSON := `{"uri":"` + replyURI + `","cid":"bafylistedreply","excerpt":"Reply excerpt","createdAt":"2026-09-20T09:00:00.123456Z"}`
	rootDeletedJSON := `{"uri":"` + rootURI + `","cid":"bafylistedrootdeleted","status":"deleted"}`
	rootRemovedJSON := `{"uri":"` + rootURI + `","cid":"bafylistedrootremoved","status":"removedByModerator"}`
	parentDeletedJSON := `{"uri":"` + parentURI + `","cid":"bafylistedparentdeleted","status":"deleted"}`
	recordDeletedJSON := `{"uri":"` + replyURI + `","cid":"bafylistedreplydeleted","createdAt":"2026-09-20T09:00:00.123456Z","status":"deleted"}`
	adminRemovedRoot := notifications.ListedReference{State: notifications.ReferenceRemovedByServerAdmin, CID: "bafylistedrootadmin"}
	rootAdminRemovedJSON := `{"uri":"` + rootURI + `","cid":"bafylistedrootadmin","status":"removedByServerAdmin"}`
	parentAdminRemovedJSON := `{"uri":"` + parentURI + `","cid":"bafylistedparentadmin","status":"removedByServerAdmin"}`
	recordAdminRemovedJSON := `{"uri":"` + replyURI + `","cid":"bafylistedreplyadmin","createdAt":"2026-09-20T09:00:00.123456Z","status":"removedByServerAdmin"}`
	cases := []struct {
		name                              string
		reason                            notifications.Reason
		root, subject, record             notifications.ListedReference
		posts                             anonymousPosts
		comments                          cannedComments
		wantRoot, wantSubject, wantRecord string
		wantPosts, wantComments           []string
	}{
		{"missing deleted record", notifications.ReasonPostReply, live, live, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedreplydeleted"}, anonymousPosts{rootURI: post}, cannedComments{}, rootLiveJSON, subjectLiveJSON, recordDeletedJSON, []string{rootURI}, nil},
		{"missing deleted parent and removed root", notifications.ReasonCommentReply, notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafylistedrootremoved"}, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedparentdeleted"}, replyLive, anonymousPosts{}, cannedComments{replyURI: reply}, rootRemovedJSON, parentDeletedJSON, recordLiveJSON, nil, []string{replyURI}},
		{"missing deleted subject and root post", notifications.ReasonPostReply, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedrootdeleted"}, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedrootdeleted"}, replyLive, anonymousPosts{}, cannedComments{replyURI: reply}, rootDeletedJSON, `{"uri":"` + rootURI + `","cid":"bafylistedrootdeleted","status":"deleted"}`, recordLiveJSON, nil, []string{replyURI}},
		{"deleted record returned by comment lookup", notifications.ReasonPostReply, live, live, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedreplydeleted"}, anonymousPosts{rootURI: post}, cannedComments{replyURI: {URI: replyURI, CID: "bafyleakedreply", Content: "Must not leak", DeletedAt: &deletedAt}}, rootLiveJSON, subjectLiveJSON, recordDeletedJSON, []string{rootURI}, nil},
		{"deleted subject returned by comment lookup", notifications.ReasonCommentReply, live, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedparentdeleted"}, replyLive, anonymousPosts{rootURI: post}, cannedComments{parentURI: {URI: parentURI, CID: "bafyleakedparent", Content: "Must not leak", DeletedAt: &deletedAt}, replyURI: reply}, rootLiveJSON, parentDeletedJSON, recordLiveJSON, []string{rootURI}, []string{replyURI}},
		{"deleted post returned by post lookup", notifications.ReasonPostReply, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedrootdeleted"}, notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafylistedrootdeleted"}, replyLive, anonymousPosts{rootURI: post}, cannedComments{replyURI: reply}, rootDeletedJSON, `{"uri":"` + rootURI + `","cid":"bafylistedrootdeleted","status":"deleted"}`, recordLiveJSON, nil, []string{replyURI}},
		{"server admin removed root and subject post with live record", notifications.ReasonPostReply, adminRemovedRoot, adminRemovedRoot, replyLive, anonymousPosts{rootURI: post}, cannedComments{replyURI: reply}, rootAdminRemovedJSON, `{"uri":"` + rootURI + `","cid":"bafylistedrootadmin","status":"removedByServerAdmin"}`, recordLiveJSON, nil, []string{replyURI}},
		{"server admin removed subject comment and record with live root", notifications.ReasonCommentReply, live, notifications.ListedReference{State: notifications.ReferenceRemovedByServerAdmin, CID: "bafylistedparentadmin"}, notifications.ListedReference{State: notifications.ReferenceRemovedByServerAdmin, CID: "bafylistedreplyadmin"}, anonymousPosts{rootURI: post}, cannedComments{parentURI: {URI: parentURI, CID: "bafyleakedparent", Content: "Must not leak"}, replyURI: {URI: replyURI, CID: "bafyleakedreply", Content: "Must not leak"}}, rootLiveJSON, parentAdminRemovedJSON, recordAdminRemovedJSON, []string{rootURI}, nil},
		{"live reference CIDs come from List", notifications.ReasonCommentReply, live, parentLive, replyLive, anonymousPosts{rootURI: post}, cannedComments{parentURI: parent, replyURI: reply}, rootLiveJSON, parentLiveJSON, recordLiveJSON, []string{rootURI}, []string{replyURI, parentURI}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subjectURI := rootURI
			if tc.reason == notifications.ReasonCommentReply {
				subjectURI = parentURI
			}
			row := notifications.ListedNotification{Reason: tc.reason, ActorDID: actorDID, RootPostURI: rootURI, SubjectURI: subjectURI, RecordURI: replyURI, RootPost: tc.root, Subject: tc.subject, Record: tc.record, RecordCreatedAt: at, SortAt: at}
			postLookup := &recordingPlaceholderPosts{views: tc.posts}
			commentLookup := &recordingPlaceholderComments{views: tc.comments}
			service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: []notifications.ListedNotification{row}}}, cannedProfiles{actorDID: &users.User{DID: actorDID, Handle: "actor.test"}}, postLookup, commentLookup)
			got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
			require.NoError(t, err)
			require.Len(t, got.Notifications, 1, "placeholder reply must remain listed")
			require.Equal(t, &notifications.ProfileView{DID: actorDID, Handle: "actor.test"}, got.Notifications[0].Author)
			for _, reference := range []struct {
				name  string
				value any
				want  string
			}{
				{"rootPost", got.Notifications[0].RootPost, tc.wantRoot},
				{"subject", got.Notifications[0].Subject, tc.wantSubject},
				{"record", got.Notifications[0].Record, tc.wantRecord},
			} {
				encoded, err := json.Marshal(reference.value)
				require.NoError(t, err)
				require.JSONEq(t, reference.want, string(encoded), reference.name)
			}
			require.ElementsMatch(t, tc.wantPosts, postLookup.requested, "only live post URIs may be looked up")
			require.ElementsMatch(t, tc.wantComments, commentLookup.requested, "only live comment URIs may be looked up")
		})
	}
}
