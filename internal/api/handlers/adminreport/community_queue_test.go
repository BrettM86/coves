package adminreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/api/middleware"
	"Coves/internal/core/adminreports"
	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
	"Coves/internal/validation"
)

const (
	queueCommunityDID   = "did:plc:queuecommunity"
	queueModeratorDID   = "did:plc:queuemoderator"
	queuePostURI        = "at://did:plc:queueauthor/social.coves.community.post/3kpost"
	queueCommentURI     = "at://did:plc:queueauthor/social.coves.community.comment/3kcomment"
	queueUnhydratedURI  = "at://did:plc:queueauthor/social.coves.community.comment/3kgone"
	queueViewCID        = "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	queueIdempotencyKey = "dismiss-key-1"
)

// queueServiceFake is the community queue service: it serves page or err and
// stores the requests it was given.
type queueServiceFake struct {
	page          adminreports.CommunityQueuePage
	dismissResult adminreports.DismissResult
	err           error
	listRequests  []adminreports.ListCommunityQueueRequest
	dismissals    []adminreports.DismissCommunityReportsRequest
}

func (f *queueServiceFake) ListQueue(_ context.Context, req adminreports.ListCommunityQueueRequest) (adminreports.CommunityQueuePage, error) {
	f.listRequests = append(f.listRequests, req)
	if f.err != nil {
		return adminreports.CommunityQueuePage{}, f.err
	}
	return f.page, nil
}

func (f *queueServiceFake) Dismiss(_ context.Context, req adminreports.DismissCommunityReportsRequest) (adminreports.DismissResult, error) {
	f.dismissals = append(f.dismissals, req)
	if f.err != nil {
		return adminreports.DismissResult{}, f.err
	}
	return f.dismissResult, nil
}

// queueViewerFake hydrates only the URIs it holds results or views for, and
// reports the comment URIs in blocked as blocked.
type queueViewerFake struct {
	posts    map[string]*posts.PostResult
	comments map[string]*comments.CommentView
	blocked  map[string]bool
}

func (f *queueViewerFake) PostResults(_ context.Context, _ string, uris []string) (map[string]*posts.PostResult, error) {
	out := map[string]*posts.PostResult{}
	for _, uri := range uris {
		if view, ok := f.posts[uri]; ok {
			out[uri] = view
		}
	}
	return out, nil
}

func (f *queueViewerFake) CommentViews(_ context.Context, _ string, uris []string) (map[string]*comments.CommentView, map[string]bool, error) {
	out := map[string]*comments.CommentView{}
	blocked := map[string]bool{}
	for _, uri := range uris {
		if f.blocked[uri] {
			blocked[uri] = true
		} else if view, ok := f.comments[uri]; ok {
			out[uri] = view
		}
	}
	return out, blocked, nil
}

func queuePostView(uri string) *posts.PostView {
	at := time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)
	return &posts.PostView{
		URI:       uri,
		CID:       queueViewCID,
		RKey:      uri[strings.LastIndex(uri, "/")+1:],
		Author:    &posts.AuthorView{DID: "did:plc:queueauthor", Handle: "author.coves.test"},
		Community: &posts.CommunityRef{DID: queueCommunityDID, Handle: "queue.coves.test", Name: "queue"},
		Record: map[string]any{
			"$type":     "social.coves.community.post",
			"community": queueCommunityDID,
			"title":     "Reported post",
			"createdAt": "2026-11-09T07:00:00Z",
		},
		CreatedAt: at,
		IndexedAt: at,
	}
}

func queueCommentView(uri string) *comments.CommentView {
	return &comments.CommentView{
		URI:       uri,
		CID:       queueViewCID,
		Author:    &posts.AuthorView{DID: "did:plc:queueauthor", Handle: "author.coves.test"},
		Post:      &comments.CommentRef{URI: queuePostURI, CID: queueViewCID},
		Stats:     &comments.CommentStats{},
		CreatedAt: "2026-11-09T07:05:00Z",
		IndexedAt: "2026-11-09T07:05:00Z",
		Record: map[string]any{
			"$type":     "social.coves.community.comment",
			"content":   "Reported comment",
			"createdAt": "2026-11-09T07:05:00Z",
		},
	}
}

func queueEntry(uri string, subjectType adminreports.TargetType, count int, reasons ...adminreports.Reason) adminreports.CommunityQueueEntry {
	return adminreports.CommunityQueueEntry{
		SubjectURI:      uri,
		SubjectType:     subjectType,
		Reasons:         reasons,
		ReportCount:     count,
		FirstReportedAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC),
		LastReportedAt:  time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC),
	}
}

func requestListReports(handler *ListCommunityReportsHandler, viewerDID, query string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.listReports?"+query, nil)
	if viewerDID != "" {
		request = request.WithContext(middleware.SetTestUserDID(request.Context(), viewerDID))
	}
	recorder := httptest.NewRecorder()
	handler.HandleListReports(recorder, request)
	return recorder
}

func requestDismissReports(handler *DismissCommunityReportsHandler, viewerDID, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.community.dismissReports", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if viewerDID != "" {
		request = request.WithContext(middleware.SetTestUserDID(request.Context(), viewerDID))
	}
	recorder := httptest.NewRecorder()
	handler.HandleDismissReports(recorder, request)
	return recorder
}

func queueLexiconCatalog(t *testing.T) *lexicon.BaseCatalog {
	t.Helper()
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))
	return catalog
}

func requireValidQueueOutput(t *testing.T, body []byte, ref string) {
	t.Helper()
	decoded, err := atdata.UnmarshalJSON(body)
	require.NoError(t, err)
	assert.NoError(t, validation.ValidateData(queueLexiconCatalog(t), decoded, ref, 0), "%s must validate", ref)
}

func decodeQueueReports(t *testing.T, body []byte) map[string]map[string]json.RawMessage {
	t.Helper()
	var page struct {
		Reports []map[string]json.RawMessage `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(body, &page), "body: %s", body)
	bySubject := map[string]map[string]json.RawMessage{}
	for _, report := range page.Reports {
		var subject string
		require.NoError(t, json.Unmarshal(report["subject"], &subject))
		bySubject[subject] = report
	}
	return bySubject
}

// TestListReportsSerializesQueueEntries: post and comment entries carry their
// hydrated views, reasons, count and timestamps; an entry the viewer cannot
// hydrate omits its view; the body names no reporter and matches the lexicon.
func TestListReportsSerializesQueueEntries(t *testing.T) {
	service := &queueServiceFake{page: adminreports.CommunityQueuePage{
		Entries: []adminreports.CommunityQueueEntry{
			queueEntry(queuePostURI, adminreports.TargetTypePost, 4, adminreports.ReasonHarassment, adminreports.ReasonSpam),
			queueEntry(queueCommentURI, adminreports.TargetTypeComment, 1, adminreports.ReasonDoxing),
			queueEntry(queueUnhydratedURI, adminreports.TargetTypeComment, 2, adminreports.ReasonOther),
		},
		Cursor: "next-page",
	}}
	viewer := &queueViewerFake{
		posts:    map[string]*posts.PostResult{queuePostURI: {Post: queuePostView(queuePostURI)}},
		comments: map[string]*comments.CommentView{queueCommentURI: queueCommentView(queueCommentURI)},
	}
	response := requestListReports(NewListCommunityReportsHandler(service, viewer), queueModeratorDID,
		"community="+queueCommunityDID+"&limit=10&cursor=page-two")

	require.Equal(t, http.StatusOK, response.Code)
	require.NotEmpty(t, response.Body.Bytes(), "handler must write the queue page")
	assert.Equal(t, []adminreports.ListCommunityQueueRequest{{
		CommunityDID: queueCommunityDID, CallerDID: queueModeratorDID, Limit: 10, Cursor: "page-two",
	}}, service.listRequests, "the query must reach the service as the authenticated caller")

	var page map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	assert.JSONEq(t, `"next-page"`, string(page["cursor"]))

	reports := decodeQueueReports(t, response.Body.Bytes())
	require.Len(t, reports, 3)

	post := reports[queuePostURI]
	assert.JSONEq(t, `"post"`, string(post["subjectType"]))
	assert.JSONEq(t, `["harassment","spam"]`, string(post["reasons"]))
	assert.JSONEq(t, `4`, string(post["reportCount"]))
	assert.JSONEq(t, `"2026-11-10T08:00:00Z"`, string(post["firstReportedAt"]))
	assert.JSONEq(t, `"2026-11-11T09:30:00Z"`, string(post["lastReportedAt"]))
	require.Contains(t, post, "post")
	var postView map[string]any
	require.NoError(t, json.Unmarshal(post["post"], &postView))
	assert.Equal(t, queuePostURI, postView["uri"])
	assert.NotContains(t, post, "comment")
	assert.NotContains(t, post, "postState", "a visible post carries its view, not a state")

	comment := reports[queueCommentURI]
	assert.JSONEq(t, `"comment"`, string(comment["subjectType"]))
	assert.JSONEq(t, `["doxing"]`, string(comment["reasons"]))
	assert.JSONEq(t, `1`, string(comment["reportCount"]))
	require.Contains(t, comment, "comment")
	var commentView map[string]any
	require.NoError(t, json.Unmarshal(comment["comment"], &commentView))
	assert.Equal(t, queueCommentURI, commentView["uri"])
	assert.NotContains(t, comment, "post")
	assert.NotContains(t, comment, "commentState", "a visible comment carries its view, not a state")

	unhydrated := reports[queueUnhydratedURI]
	require.NotNil(t, unhydrated, "an entry with no view must still be listed")
	assert.NotContains(t, unhydrated, "post", "an unhydrated subject must omit its view")
	assert.NotContains(t, unhydrated, "comment", "an unhydrated subject must omit its view")

	assert.NotContains(t, strings.ToLower(response.Body.String()), "reporter", "the queue must not name reporters")
	requireValidQueueOutput(t, response.Body.Bytes(), "social.coves.community.listReports#output")
}

// TestListReportsServesUnavailablePostState: a reported post the moderator's
// view cannot show is listed with an explicit postState naming why, and no
// post view. The body still matches the lexicon.
func TestListReportsServesUnavailablePostState(t *testing.T) {
	uri := func(rkey string) string { return "at://did:plc:queueauthor/social.coves.community.postv2/" + rkey }
	tests := []struct {
		rkey      string
		result    *posts.PostResult
		wantState string
	}{
		{
			rkey: "3kpending", wantState: "notFound",
			result: &posts.PostResult{NotFound: &posts.NotFoundPost{URI: uri("3kpending"), NotFound: true}},
		},
		{
			rkey: "3kblocked", wantState: "blocked",
			result: &posts.PostResult{Blocked: &posts.BlockedPost{
				URI: uri("3kblocked"), Blocked: true, BlockedBy: "author",
				Author: &posts.BlockedAuthor{DID: "did:plc:queueauthor"},
			}},
		},
		{
			rkey: "3kremoved", wantState: "removed",
			result: &posts.PostResult{Removed: &posts.RemovedPost{URI: uri("3kremoved"), Removed: true, Code: "spam"}},
		},
		{
			rkey: "3kmoderated", wantState: "moderated",
			result: &posts.PostResult{Moderated: &posts.ModeratedPost{URI: uri("3kmoderated"), AuthorDID: "did:plc:queueauthor"}},
		},
	}
	entries := make([]adminreports.CommunityQueueEntry, 0, len(tests))
	results := map[string]*posts.PostResult{}
	for _, tt := range tests {
		entries = append(entries, queueEntry(uri(tt.rkey), adminreports.TargetTypePost, 1, adminreports.ReasonSpam))
		results[uri(tt.rkey)] = tt.result
	}
	service := &queueServiceFake{page: adminreports.CommunityQueuePage{Entries: entries}}
	response := requestListReports(NewListCommunityReportsHandler(service, &queueViewerFake{posts: results}), queueModeratorDID,
		"community="+queueCommunityDID)

	require.Equal(t, http.StatusOK, response.Code)
	reports := decodeQueueReports(t, response.Body.Bytes())
	require.Len(t, reports, len(tests))
	for _, tt := range tests {
		entry := reports[uri(tt.rkey)]
		require.NotNil(t, entry, tt.rkey)
		assert.NotContains(t, entry, "post", "%s: a post the moderator cannot see carries no view", tt.rkey)
		if assert.Contains(t, entry, "postState", "%s: the entry names the post's state", tt.rkey) {
			assert.JSONEq(t, `"`+tt.wantState+`"`, string(entry["postState"]), tt.rkey)
		}
	}
	requireValidQueueOutput(t, response.Body.Bytes(), "social.coves.community.listReports#output")
}

// TestListReportsServesBlockedCommentState: a reported comment whose author
// the moderator blocked is listed with commentState "blocked" and no comment
// view, as a blocked author's post is. The body still matches the lexicon.
func TestListReportsServesBlockedCommentState(t *testing.T) {
	service := &queueServiceFake{page: adminreports.CommunityQueuePage{Entries: []adminreports.CommunityQueueEntry{
		queueEntry(queueCommentURI, adminreports.TargetTypeComment, 1, adminreports.ReasonDoxing),
	}}}
	viewer := &queueViewerFake{
		comments: map[string]*comments.CommentView{queueCommentURI: queueCommentView(queueCommentURI)},
		blocked:  map[string]bool{queueCommentURI: true},
	}
	response := requestListReports(NewListCommunityReportsHandler(service, viewer), queueModeratorDID,
		"community="+queueCommunityDID)

	require.Equal(t, http.StatusOK, response.Code)
	entry := decodeQueueReports(t, response.Body.Bytes())[queueCommentURI]
	require.NotNil(t, entry)
	assert.NotContains(t, entry, "comment", "a comment by a blocked author carries no view")
	if assert.Contains(t, entry, "commentState") {
		assert.JSONEq(t, `"blocked"`, string(entry["commentState"]))
	}
	requireValidQueueOutput(t, response.Body.Bytes(), "social.coves.community.listReports#output")
}

// TestListReportsEmptyPage: an empty queue serializes as an empty array.
func TestListReportsEmptyPage(t *testing.T) {
	service := &queueServiceFake{}
	response := requestListReports(NewListCommunityReportsHandler(service, &queueViewerFake{}), queueModeratorDID,
		"community="+queueCommunityDID)
	require.Equal(t, http.StatusOK, response.Code)
	require.NotEmpty(t, response.Body.Bytes(), "handler must write the queue page")
	assert.JSONEq(t, `{"reports":[]}`, response.Body.String())
	requireValidQueueOutput(t, response.Body.Bytes(), "social.coves.community.listReports#output")
}

type queueErrorCase struct {
	name      string
	viewerDID string
	err       error
	status    int
	errorName string
}

func assertQueueError(t *testing.T, response *httptest.ResponseRecorder, test queueErrorCase) {
	t.Helper()
	require.Equal(t, test.status, response.Code, "body: %s", response.Body.String())
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, test.errorName, body.Error)
}

// TestListReportsErrors maps the service's refusals and a missing caller.
func TestListReportsErrors(t *testing.T) {
	for _, test := range []queueErrorCase{
		{name: "forbidden", viewerDID: queueModeratorDID, err: fmt.Errorf("%w: not a moderator", adminreports.ErrCommunityQueueForbidden), status: http.StatusForbidden, errorName: "Forbidden"},
		{name: "invalid request", viewerDID: queueModeratorDID, err: fmt.Errorf("%w: limit", adminreports.ErrInvalidQueueRequest), status: http.StatusBadRequest, errorName: "InvalidRequest"},
		{name: "unauthenticated", status: http.StatusUnauthorized, errorName: "AuthRequired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &queueServiceFake{err: test.err}
			response := requestListReports(NewListCommunityReportsHandler(service, &queueViewerFake{}), test.viewerDID,
				"community="+queueCommunityDID)
			assertQueueError(t, response, test)
		})
	}
}

// TestDismissReportsHandler is the dismissReports wiring table.
func TestDismissReportsHandler(t *testing.T) {
	validBody := `{"community":"` + queueCommunityDID + `","subject":"` + queuePostURI + `","idempotencyKey":"` + queueIdempotencyKey + `"}`

	t.Run("dismisses as the caller", func(t *testing.T) {
		service := &queueServiceFake{dismissResult: adminreports.DismissResult{DismissedCount: 3}}
		response := requestDismissReports(NewDismissCommunityReportsHandler(service), queueModeratorDID, validBody)
		require.Equal(t, http.StatusOK, response.Code)
		require.NotEmpty(t, response.Body.Bytes(), "handler must write the dismissal result")
		assert.JSONEq(t, `{"dismissedCount":3}`, response.Body.String())
		assert.Equal(t, []adminreports.DismissCommunityReportsRequest{{
			CommunityDID: queueCommunityDID, CallerDID: queueModeratorDID,
			SubjectURI: queuePostURI, IdempotencyKey: queueIdempotencyKey,
		}}, service.dismissals)
		requireValidQueueOutput(t, response.Body.Bytes(), "social.coves.community.dismissReports#output")
	})

	for _, test := range []struct {
		queueErrorCase
		body string
	}{
		{queueErrorCase: queueErrorCase{
			name: "idempotency conflict", viewerDID: queueModeratorDID,
			err: fmt.Errorf("%w: key reused", adminreports.ErrIdempotencyConflict), status: http.StatusConflict, errorName: "IdempotencyConflict",
		}, body: validBody},
		{queueErrorCase: queueErrorCase{
			name: "forbidden", viewerDID: queueModeratorDID,
			err: fmt.Errorf("%w: not a moderator", adminreports.ErrCommunityQueueForbidden), status: http.StatusForbidden, errorName: "Forbidden",
		}, body: validBody},
		{queueErrorCase: queueErrorCase{
			name: "malformed body", viewerDID: queueModeratorDID,
			status: http.StatusBadRequest, errorName: "InvalidRequest",
		}, body: `{"community":`},
		{queueErrorCase: queueErrorCase{
			name:   "unauthenticated",
			status: http.StatusUnauthorized, errorName: "AuthRequired",
		}, body: validBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &queueServiceFake{err: test.err}
			response := requestDismissReports(NewDismissCommunityReportsHandler(service), test.viewerDID, test.body)
			assertQueueError(t, response, test.queueErrorCase)
		})
	}
}

// queuePostsFake is posts.GetPosts: it refuses more than 25 URIs, as the real
// service does, and finds every post it is asked for.
type queuePostsFake struct{}

func (queuePostsFake) GetPosts(_ context.Context, req posts.GetPostsRequest) ([]*posts.PostResult, error) {
	if len(req.URIs) == 0 || len(req.URIs) > 25 {
		return nil, errors.New("too many URIs (max 25)")
	}
	results := make([]*posts.PostResult, 0, len(req.URIs))
	for _, uri := range req.URIs {
		results = append(results, &posts.PostResult{Post: queuePostView(uri)})
	}
	return results, nil
}

type queueCommentsFake struct{}

func (queueCommentsFake) GetCommentViewsByURIs(_ context.Context, uris []string, _ string) (map[string]*comments.CommentView, map[string]bool, error) {
	out := map[string]*comments.CommentView{}
	for _, uri := range uris {
		out[uri] = queueCommentView(uri)
	}
	return out, map[string]bool{}, nil
}

// TestReportSubjectViewerBatchesPosts: 26 reported posts hydrate even though a
// single GetPosts call accepts at most 25.
func TestReportSubjectViewerBatchesPosts(t *testing.T) {
	viewer := NewReportSubjectViewer(queuePostsFake{}, queueCommentsFake{})
	uris := make([]string, 0, 26)
	for i := 0; i < 26; i++ {
		uris = append(uris, fmt.Sprintf("at://did:plc:queueauthor/social.coves.community.post/3kpost%02d", i))
	}
	results, err := viewer.PostResults(context.Background(), queueModeratorDID, uris)
	require.NoError(t, err)
	got := make([]string, 0, len(results))
	for uri, result := range results {
		require.NotNil(t, result, uri)
		require.NotNil(t, result.Post, uri)
		assert.Equal(t, uri, result.Post.URI)
		got = append(got, uri)
	}
	sort.Strings(got)
	assert.Equal(t, uris, got, "every one of the 26 posts must hydrate")
}
