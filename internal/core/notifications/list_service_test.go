package notifications_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/require"
)

type cannedListReader struct {
	notifications.ReadRepository
	page notifications.ListPage
}

func (r cannedListReader) List(context.Context, string, string, int) (notifications.ListPage, error) {
	return r.page, nil
}

type cannedProfiles map[string]*users.User

func (p cannedProfiles) GetByDIDs(context.Context, []string) (map[string]*users.User, error) {
	return p, nil
}

type anonymousPosts map[string]*posts.PostView

func (p anonymousPosts) GetViewsByURIs(_ context.Context, _ []string, viewerDID string) (map[string]*posts.PostView, error) {
	if viewerDID != "" {
		return map[string]*posts.PostView{}, nil
	}
	return p, nil
}

type cannedComments map[string]*comments.Comment

func (c cannedComments) GetByURIsBatch(context.Context, []string) (map[string]*comments.Comment, error) {
	return c, nil
}

func TestListNotifications_OmitsRowsWithUnavailableReferences(t *testing.T) {
	seen := time.Date(2026, time.September, 20, 9, 0, 0, 500000000, time.UTC)
	deleted := seen
	const postURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const parentURI = "at://did:plc:owner/social.coves.community.comment/parent"
	const actorDID = "did:plc:replier"
	post := &posts.PostView{URI: postURI, CID: "bafycurrentroot", Record: map[string]interface{}{"title": "Root title", "content": "Root content"}, Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.coves.social", Name: "Community"}}
	baseComments := cannedComments{parentURI: {URI: parentURI, CID: "bafyparent", Content: "Parent reply"}}
	basePosts := anonymousPosts{postURI: post}
	for _, tc := range []struct {
		name                           string
		reason                         notifications.Reason
		recordURI, subjectURI, rootURI string
		posts                          anonymousPosts
		comments                       cannedComments
	}{
		{"missing record", notifications.ReasonPostReply, "at://did:plc:replier/social.coves.community.comment/missing", postURI, postURI, basePosts, baseComments},
		{"missing subject post", notifications.ReasonPostReply, "at://did:plc:replier/social.coves.community.comment/reply", "at://did:plc:owner/social.coves.community.postv2/missing", postURI, basePosts, baseComments},
		{"missing subject comment", notifications.ReasonCommentReply, "at://did:plc:replier/social.coves.community.comment/reply", "at://did:plc:owner/social.coves.community.comment/missing", postURI, basePosts, baseComments},
		{"missing root post", notifications.ReasonCommentReply, "at://did:plc:replier/social.coves.community.comment/reply", parentURI, "at://did:plc:owner/social.coves.community.postv2/missing", basePosts, baseComments},
		{"deleted record", notifications.ReasonPostReply, "at://did:plc:replier/social.coves.community.comment/deleted", postURI, postURI, basePosts, cannedComments{parentURI: baseComments[parentURI], "at://did:plc:replier/social.coves.community.comment/deleted": {URI: "at://did:plc:replier/social.coves.community.comment/deleted", CID: "bafydeleted", DeletedAt: &deleted}}},
		{"deleted subject comment", notifications.ReasonCommentReply, "at://did:plc:replier/social.coves.community.comment/reply", parentURI, postURI, basePosts, cannedComments{parentURI: {URI: parentURI, CID: "bafyparent", DeletedAt: &deleted}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const healthyURI = "at://did:plc:replier/social.coves.community.comment/healthy"
			const replyURI = "at://did:plc:replier/social.coves.community.comment/reply"
			cs := cannedComments{healthyURI: {URI: healthyURI, CID: "bafyhealthy", Content: "Healthy reply"}, replyURI: {URI: replyURI, CID: "bafyreply", Content: "Reply"}}
			for uri, comment := range tc.comments {
				cs[uri] = comment
			}
			rows := []notifications.ListedNotification{
				{Reason: notifications.ReasonPostReply, RecordURI: healthyURI, SubjectURI: postURI, RootPostURI: postURI, ActorDID: actorDID, SortAt: seen.Add(time.Minute), RecordCreatedAt: seen},
				{Reason: tc.reason, RecordURI: tc.recordURI, SubjectURI: tc.subjectURI, RootPostURI: tc.rootURI, ActorDID: actorDID, SortAt: seen, RecordCreatedAt: seen},
				{Reason: notifications.ReasonPostReply, RecordURI: replyURI, SubjectURI: postURI, RootPostURI: postURI, ActorDID: actorDID, SortAt: seen.Add(-time.Minute), RecordCreatedAt: seen},
			}
			service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: rows, Cursor: "next-page", SeenAt: &seen}}, cannedProfiles{actorDID: {DID: actorDID, Handle: "replier.test"}}, tc.posts, cs)
			got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
			require.NoError(t, err)
			require.Len(t, got.Notifications, 2)
			require.Equal(t, []string{healthyURI, replyURI}, []string{got.Notifications[0].Record.URI, got.Notifications[1].Record.URI})
			require.Equal(t, "next-page", got.Cursor)
			require.Equal(t, "2026-09-20T09:00:00.5Z", got.SeenAt)
		})
	}
	t.Run("all omitted keeps page metadata and empty array", func(t *testing.T) {
		row := notifications.ListedNotification{Reason: notifications.ReasonPostReply, RecordURI: "at://did:plc:replier/social.coves.community.comment/missing", SubjectURI: postURI, RootPostURI: postURI, ActorDID: actorDID, SortAt: seen}
		service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: []notifications.ListedNotification{row}, Cursor: "next-page", SeenAt: &seen}}, cannedProfiles{actorDID: {DID: actorDID, Handle: "replier.test"}}, basePosts, baseComments)
		got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
		require.NoError(t, err)
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		require.JSONEq(t, `{"notifications":[],"cursor":"next-page","seenAt":"2026-09-20T09:00:00.5Z"}`, string(raw))
	})
}

type recordingHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, record.Clone())
	return nil
}

func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// captureDefaultLogs routes the default slog logger into a recorder for the
// rest of the test. Callers must not use t.Parallel.
func captureDefaultLogs(t *testing.T) func() []slog.Record {
	t.Helper()
	handler := recordingHandler{mu: &sync.Mutex{}, records: &[]slog.Record{}}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []slog.Record {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return append([]slog.Record(nil), *handler.records...)
	}
}

func flattenLogAttrs(prefix string, attrs []slog.Attr, into map[string]slog.Value) {
	for _, attr := range attrs {
		value := attr.Value.Resolve()
		if value.Kind() == slog.KindGroup {
			flattenLogAttrs(prefix+attr.Key+".", value.Group(), into)
			continue
		}
		into[prefix+attr.Key] = value
	}
}

func TestListNotifications_LogsOmittedRowCountsWithoutIdentifiers(t *testing.T) {
	seen := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	deleted := seen
	const postURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const parentURI = "at://did:plc:owner/social.coves.community.comment/parent"
	const healthyURI = "at://did:plc:replier/social.coves.community.comment/healthy"
	const replyURI = "at://did:plc:replier/social.coves.community.comment/reply"
	const actorDID = "did:plc:replier"
	post := &posts.PostView{URI: postURI, CID: "bafyroot", Record: map[string]interface{}{"title": "Root title"}}
	profiles := cannedProfiles{actorDID: {DID: actorDID, Handle: "replier.test"}}
	healthy := notifications.ListedNotification{Reason: notifications.ReasonPostReply, RecordURI: healthyURI, SubjectURI: postURI, RootPostURI: postURI, ActorDID: actorDID, SortAt: seen, RecordCreatedAt: seen}

	t.Run("omitted rows produce one warning with counts only", func(t *testing.T) {
		captured := captureDefaultLogs(t)
		rows := []notifications.ListedNotification{
			healthy,
			{Reason: notifications.ReasonPostReply, RecordURI: "at://did:plc:replier/social.coves.community.comment/missing", SubjectURI: postURI, RootPostURI: postURI, ActorDID: actorDID, SortAt: seen, RecordCreatedAt: seen},
			{Reason: notifications.ReasonCommentReply, RecordURI: replyURI, SubjectURI: parentURI, RootPostURI: postURI, ActorDID: actorDID, SortAt: seen, RecordCreatedAt: seen},
		}
		commentViews := cannedComments{
			healthyURI: {URI: healthyURI, CID: "bafyhealthy", Content: "Healthy reply"},
			replyURI:   {URI: replyURI, CID: "bafyreply", Content: "Reply"},
			parentURI:  {URI: parentURI, CID: "bafyparent", DeletedAt: &deleted},
		}
		service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: rows, Cursor: "next-page"}}, profiles, anonymousPosts{postURI: post}, commentViews)

		got, err := service.ListNotifications(context.Background(), "did:plc:owner", "cursor-in", 50)
		require.NoError(t, err)
		require.Len(t, got.Notifications, 1)

		records := captured()
		require.Len(t, records, 1, "expected exactly one log record for a page with omitted rows")
		record := records[0]
		require.Equal(t, slog.LevelWarn, record.Level)
		attrs := []slog.Attr{}
		record.Attrs(func(attr slog.Attr) bool {
			attrs = append(attrs, attr)
			return true
		})
		flat := map[string]slog.Value{}
		flattenLogAttrs("", attrs, flat)
		counts := map[string]int64{}
		for key, value := range flat {
			require.Equal(t, slog.KindInt64, value.Kind(), "attribute %q must be a count", key)
			counts[key] = value.Int64()
		}
		require.Equal(t, map[string]int64{
			"omitted":                      2,
			"by_reason.postReply":          1,
			"by_reason.commentReply":       1,
			"by_reason.other":              0,
			"by_cause.missing_record":      1,
			"by_cause.deleted_record":      0,
			"by_cause.missing_root":        0,
			"by_cause.missing_subject":     0,
			"by_cause.deleted_subject":     1,
			"by_cause.unrecognized_reason": 0,
			"by_cause.malformed_labels":    0,
		}, counts)
		for _, text := range append([]string{record.Message}, func() []string {
			values := []string{}
			for key, value := range flat {
				values = append(values, key, value.String())
			}
			return values
		}()...) {
			require.NotContains(t, text, "at://")
			require.NotContains(t, text, "did:")
			require.False(t, strings.Contains(text, "cursor-in") || strings.Contains(text, "next-page"), "log must not carry cursors: %q", text)
		}
	})

	t.Run("page without omissions logs nothing", func(t *testing.T) {
		captured := captureDefaultLogs(t)
		service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: []notifications.ListedNotification{healthy}}}, profiles, anonymousPosts{postURI: post}, cannedComments{healthyURI: {URI: healthyURI, CID: "bafyhealthy", Content: "Healthy reply"}})

		got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
		require.NoError(t, err)
		require.Len(t, got.Notifications, 1)
		require.Empty(t, captured())
	})
}
