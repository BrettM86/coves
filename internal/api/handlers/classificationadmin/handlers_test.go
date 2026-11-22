package classificationadmin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"Coves/internal/api/handlers/classificationadmin"
	"Coves/internal/api/middleware"
	"Coves/internal/core/classification"
	"Coves/internal/validation"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	listNSID      = "social.coves.moderation.listClassificationResults"
	verdictNSID   = "social.coves.moderation.recordClassificationVerdict"
	precisionNSID = "social.coves.moderation.getClassificationPrecision"
	adminDID      = "did:plc:contextadmin"
)

// reviewServiceFake records the arguments each handler decoded and answers
// with the configured value or error.
type reviewServiceFake struct {
	page          classification.ResultPage
	verdict       classification.Verdict
	report        classification.PrecisionReport
	err           error
	lastFilter    *classification.ResultFilter
	lastVerdict   *classification.VerdictRequest
	lastPrecision *classification.PrecisionRequest
}

func (service *reviewServiceFake) ListResults(_ context.Context, filter classification.ResultFilter) (classification.ResultPage, error) {
	service.lastFilter = &filter
	return service.page, service.err
}

func (service *reviewServiceFake) RecordVerdict(_ context.Context, request classification.VerdictRequest) (classification.Verdict, error) {
	service.lastVerdict = &request
	return service.verdict, service.err
}

func (service *reviewServiceFake) Precision(_ context.Context, request classification.PrecisionRequest) (classification.PrecisionReport, error) {
	service.lastPrecision = &request
	return service.report, service.err
}

func adminRequest(method, target string, body []byte) *http.Request {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request.WithContext(middleware.SetTestUserDID(request.Context(), adminDID))
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func listRequest(query url.Values) *http.Request {
	return adminRequest(http.MethodGet, "/xrpc/"+listNSID+"?"+query.Encode(), nil)
}

func precisionRequest(query url.Values) *http.Request {
	return adminRequest(http.MethodGet, "/xrpc/"+precisionNSID+"?"+query.Encode(), nil)
}

func verdictRequest(body string) *http.Request {
	return adminRequest(http.MethodPost, "/xrpc/"+verdictNSID, []byte(body))
}

func assertXRPCError(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	assert.Equal(t, http.StatusBadRequest, response.Code)
	var body map[string]any
	if assert.NoError(t, json.Unmarshal(response.Body.Bytes(), &body), "XRPC error must be a JSON body: %q", response.Body.String()) {
		assert.Equal(t, code, body["error"])
	}
}

// requireLexiconOutput checks a 200 response validates against nsid#output
// and returns its decoded generic form.
func requireLexiconOutput(t *testing.T, response *httptest.ResponseRecorder, nsid string) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../../atproto/lexicon"))
	decoded, err := atdata.UnmarshalJSON(response.Body.Bytes())
	require.NoError(t, err)
	assert.NoError(t, validation.ValidateData(catalog, decoded, nsid+"#output", 0), "output must validate against %s#output", nsid)
	var generic map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &generic))
	return generic
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err, "datetime %q", value)
	return parsed
}

var (
	postCreatedAt    = time.Date(2026, 11, 10, 9, 15, 30, 250000000, time.UTC)
	commentCreatedAt = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
)

func populatedPage() classification.ResultPage {
	return classification.ResultPage{
		Cursor: "next-page-cursor",
		Results: []classification.ResultView{
			{
				ID: 42, SubjectKind: classification.SubjectKindPost,
				SubjectURI:   "at://did:plc:author/social.coves.community.post/3kpost",
				ScoredCID:    "bafyreiscoredpost",
				CommunityDID: "did:plc:community", AuthorDID: "did:plc:author",
				ModelID: "text-model-a", PolicyVersion: "v1",
				Probabilities: map[string]float64{"spam_scam": 0.8734567, "hate_harassment": 0.25},
				ImagesSent:    1, ImagesSkipped: 2, TextTruncated: true, Partial: true,
				CreatedAt: postCreatedAt, ScoredCIDIsCurrent: true,
				Subject: classification.SubjectView{
					Visibility: classification.VisibilityAwaitingAdmission,
					CurrentCID: "bafyreiscoredpost",
					Post:       &classification.PostFields{Title: "Post title", Body: "Post body", ExternalURI: "https://example.com/article", ExternalTitle: "Article"},
					Images:     []classification.ImageRef{{OwnerDID: "did:plc:author", CID: "bafkreiimage"}},
				},
			},
			{
				ID: 41, SubjectKind: classification.SubjectKindComment,
				SubjectURI:   "at://did:plc:commenter/social.coves.community.comment/3kcomment",
				ScoredCID:    "bafyreiscoredcomment",
				CommunityDID: "did:plc:community", AuthorDID: "did:plc:commenter",
				ModelID: "text-model-a", PolicyVersion: "v1",
				Probabilities: map[string]float64{"spam_scam": 0.1},
				CreatedAt:     commentCreatedAt,
				Subject: classification.SubjectView{
					Visibility: classification.VisibilityHidden,
					Deleted:    true,
					Comment:    &classification.CommentFields{Body: "Comment body"},
				},
			},
			{
				ID: 40, SubjectKind: classification.SubjectKindCommunity,
				SubjectURI:   "did:plc:community",
				ScoredCID:    "bafyreiscoredcommunity",
				CommunityDID: "did:plc:community", AuthorDID: "did:plc:community",
				ModelID: "text-model-a", PolicyVersion: "v1",
				Probabilities: map[string]float64{"spam_scam": 0},
				CreatedAt:     commentCreatedAt,
				Subject: classification.SubjectView{
					Visibility: classification.VisibilityVisible,
					CurrentCID: "bafyreieditedcommunity",
					Community:  &classification.CommunityFields{Name: "gardening", DisplayName: "Gardening", Description: "Plants"},
				},
			},
		},
	}
}

type wireProbability struct {
	Question    string `json:"question"`
	Probability int64  `json:"probability"`
}

type wireImage struct {
	OwnerDID string `json:"ownerDid"`
	CID      string `json:"cid"`
}

type wireSubject struct {
	Visibility string `json:"visibility"`
	Deleted    bool   `json:"deleted"`
	CurrentCID string `json:"currentCid"`
	Post       *struct {
		Title         string `json:"title"`
		Body          string `json:"body"`
		ExternalURI   string `json:"externalUri"`
		ExternalTitle string `json:"externalTitle"`
	} `json:"post"`
	Comment *struct {
		Body string `json:"body"`
	} `json:"comment"`
	Community *struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
	} `json:"community"`
	Images []wireImage `json:"images"`
}

type wireResult struct {
	ID                 int64             `json:"id"`
	SubjectKind        string            `json:"subjectKind"`
	SubjectURI         string            `json:"subjectUri"`
	ScoredCID          string            `json:"scoredCid"`
	CommunityDID       string            `json:"communityDid"`
	AuthorDID          string            `json:"authorDid"`
	ModelID            string            `json:"modelId"`
	PolicyVersion      string            `json:"policyVersion"`
	Probabilities      []wireProbability `json:"probabilities"`
	ImagesSent         int               `json:"imagesSent"`
	ImagesSkipped      int               `json:"imagesSkipped"`
	TextTruncated      bool              `json:"textTruncated"`
	Partial            *bool             `json:"partial"`
	CreatedAt          string            `json:"createdAt"`
	ScoredCIDIsCurrent bool              `json:"scoredCidIsCurrent"`
	Subject            wireSubject       `json:"subject"`
}

// TestListResultsHandlerOutput serves a populated page that validates against
// the lexicon, with probabilities as parts per million sorted by question,
// lowerCamel visibility, and the current subject view of each kind (B3).
func TestListResultsHandlerOutput(t *testing.T) {
	service := &reviewServiceFake{page: populatedPage()}
	response := serve(classificationadmin.NewListResultsHandler(service), listRequest(url.Values{}))
	generic := requireLexiconOutput(t, response, listNSID)
	assert.Equal(t, "next-page-cursor", generic["cursor"])

	var output struct {
		Results []wireResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &output))
	require.Len(t, output.Results, 3)

	post := output.Results[0]
	assert.Equal(t, int64(42), post.ID)
	assert.Equal(t, "post", post.SubjectKind)
	assert.Equal(t, "at://did:plc:author/social.coves.community.post/3kpost", post.SubjectURI)
	assert.Equal(t, "bafyreiscoredpost", post.ScoredCID)
	assert.Equal(t, "did:plc:community", post.CommunityDID)
	assert.Equal(t, "did:plc:author", post.AuthorDID)
	assert.Equal(t, "text-model-a", post.ModelID)
	assert.Equal(t, "v1", post.PolicyVersion)
	assert.Equal(t, []wireProbability{{"hate_harassment", 250000}, {"spam_scam", 873457}}, post.Probabilities)
	assert.Equal(t, 1, post.ImagesSent)
	assert.Equal(t, 2, post.ImagesSkipped)
	assert.True(t, post.TextTruncated)
	if assert.NotNil(t, post.Partial, "partial is always present") {
		assert.True(t, *post.Partial, "a result stored from the batches of a failed job is partial")
	}
	if assert.NotNil(t, output.Results[1].Partial, "partial is always present") {
		assert.False(t, *output.Results[1].Partial)
	}
	assert.True(t, postCreatedAt.Equal(mustTime(t, post.CreatedAt)), "createdAt %s", post.CreatedAt)
	assert.True(t, post.ScoredCIDIsCurrent)
	assert.Equal(t, "awaitingAdmission", post.Subject.Visibility)
	assert.False(t, post.Subject.Deleted)
	assert.Equal(t, "bafyreiscoredpost", post.Subject.CurrentCID)
	if assert.NotNil(t, post.Subject.Post) {
		assert.Equal(t, "Post title", post.Subject.Post.Title)
		assert.Equal(t, "Post body", post.Subject.Post.Body)
		assert.Equal(t, "https://example.com/article", post.Subject.Post.ExternalURI)
		assert.Equal(t, "Article", post.Subject.Post.ExternalTitle)
	}
	assert.Equal(t, []wireImage{{OwnerDID: "did:plc:author", CID: "bafkreiimage"}}, post.Subject.Images)

	comment := output.Results[1]
	assert.Equal(t, int64(41), comment.ID)
	assert.Equal(t, "comment", comment.SubjectKind)
	assert.Equal(t, []wireProbability{{"spam_scam", 100000}}, comment.Probabilities)
	assert.False(t, comment.ScoredCIDIsCurrent)
	assert.Equal(t, "hidden", comment.Subject.Visibility)
	assert.True(t, comment.Subject.Deleted)
	assert.Nil(t, comment.Subject.Post)
	if assert.NotNil(t, comment.Subject.Comment) {
		assert.Equal(t, "Comment body", comment.Subject.Comment.Body)
	}
	results, ok := generic["results"].([]any)
	require.True(t, ok)
	require.Len(t, results, 3)
	commentSubject, ok := results[1].(map[string]any)["subject"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, commentSubject, "currentCid", "a deleted subject has no current CID")

	community := output.Results[2]
	assert.Equal(t, "community", community.SubjectKind)
	assert.Equal(t, []wireProbability{{"spam_scam", 0}}, community.Probabilities)
	assert.False(t, community.ScoredCIDIsCurrent)
	assert.Equal(t, "visible", community.Subject.Visibility)
	assert.Equal(t, "bafyreieditedcommunity", community.Subject.CurrentCID)
	if assert.NotNil(t, community.Subject.Community) {
		assert.Equal(t, "gardening", community.Subject.Community.Name)
		assert.Equal(t, "Gardening", community.Subject.Community.DisplayName)
		assert.Equal(t, "Plants", community.Subject.Community.Description)
	}
}

// TestListResultsHandlerDecodesFilter passes every query parameter to the
// service, minProbability from parts per million, and leaves absent
// parameters unset (B3).
func TestListResultsHandlerDecodesFilter(t *testing.T) {
	half := 0.5
	for _, test := range []struct {
		name     string
		query    url.Values
		expected classification.ResultFilter
	}{
		{
			name: "every parameter",
			query: url.Values{
				"question": {"spam_scam"}, "minProbability": {"500000"}, "subjectKind": {"comment"},
				"community": {"did:plc:community"}, "modelId": {"text-model-a"}, "policyVersion": {"v1"},
				"since": {"2026-11-01T00:00:00Z"}, "until": {"2026-11-02T12:30:00.5Z"},
				"limit": {"25"}, "cursor": {"opaque-cursor"},
			},
			expected: classification.ResultFilter{
				Question: "spam_scam", MinProbability: &half, SubjectKind: classification.SubjectKindComment,
				CommunityDID: "did:plc:community", ModelID: "text-model-a", PolicyVersion: "v1",
				Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 2, 12, 30, 0, 500000000, time.UTC),
				Limit: 25, Cursor: "opaque-cursor",
			},
		},
		{name: "no parameters", query: url.Values{}, expected: classification.ResultFilter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &reviewServiceFake{}
			response := serve(classificationadmin.NewListResultsHandler(service), listRequest(test.query))
			assert.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
			require.NotNil(t, service.lastFilter, "a valid query must reach the service")
			got := *service.lastFilter
			assert.True(t, test.expected.Since.Equal(got.Since), "since %s", got.Since)
			assert.True(t, test.expected.Until.Equal(got.Until), "until %s", got.Until)
			got.Since, got.Until = test.expected.Since, test.expected.Until
			assert.Equal(t, test.expected, got)
		})
	}
}

// TestListResultsHandlerRejectsInvalidParameters answers 400 InvalidRequest
// for parameters that do not decode or are out of range (B3).
func TestListResultsHandlerRejectsInvalidParameters(t *testing.T) {
	for _, test := range []struct {
		name  string
		query url.Values
	}{
		{name: "minProbability above one million", query: url.Values{"question": {"spam_scam"}, "minProbability": {"1000001"}}},
		{name: "negative minProbability", query: url.Values{"question": {"spam_scam"}, "minProbability": {"-1"}}},
		{name: "fractional minProbability", query: url.Values{"question": {"spam_scam"}, "minProbability": {"0.5"}}},
		{name: "non-integer limit", query: url.Values{"limit": {"ten"}}},
		{name: "malformed since", query: url.Values{"since": {"yesterday"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := serve(classificationadmin.NewListResultsHandler(&reviewServiceFake{}), listRequest(test.query))
			assertXRPCError(t, response, "InvalidRequest")
		})
	}
}

// TestRecordVerdictHandlerOutput passes the result ID, every answer (an
// explicit false as false) and the authenticated admin DID from the request
// context to the service, and answers with a lexicon-valid verdict (B3).
func TestRecordVerdictHandlerOutput(t *testing.T) {
	recordedAt := time.Date(2026, 11, 12, 10, 30, 0, 123000000, time.UTC)
	service := &reviewServiceFake{verdict: classification.Verdict{
		ResultID: 7, AdminDID: adminDID, RecordedAt: recordedAt,
		Present: map[string]bool{"spam_scam": true, "hate_harassment": false},
	}}
	response := serve(classificationadmin.NewRecordVerdictHandler(service), verdictRequest(
		`{"resultId":7,"answers":[{"question":"spam_scam","present":true},{"question":"hate_harassment","present":false}]}`))

	require.NotNil(t, service.lastVerdict, "a valid verdict must reach the service")
	assert.Equal(t, classification.VerdictRequest{
		ResultID: 7, AdminDID: adminDID,
		Present: map[string]bool{"spam_scam": true, "hate_harassment": false},
	}, *service.lastVerdict)

	requireLexiconOutput(t, response, verdictNSID)
	var output struct {
		ResultID int64 `json:"resultId"`
		Answers  []struct {
			Question string `json:"question"`
			Present  bool   `json:"present"`
		} `json:"answers"`
		RecordedAt string `json:"recordedAt"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &output))
	assert.Equal(t, int64(7), output.ResultID)
	require.Len(t, output.Answers, 2)
	assert.Equal(t, "hate_harassment", output.Answers[0].Question, "answers are sorted by question")
	assert.False(t, output.Answers[0].Present)
	assert.Equal(t, "spam_scam", output.Answers[1].Question)
	assert.True(t, output.Answers[1].Present)
	assert.True(t, recordedAt.Equal(mustTime(t, output.RecordedAt)), "recordedAt %s", output.RecordedAt)
}

// TestRecordVerdictHandlerRejectsInvalidBodies answers 400 InvalidRequest
// without calling the service for a body that does not decode to a result ID
// and answers each naming a question once with a boolean present (B3).
func TestRecordVerdictHandlerRejectsInvalidBodies(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "answer without present", body: `{"resultId":7,"answers":[{"question":"spam_scam"}]}`},
		{name: "question named twice", body: `{"resultId":7,"answers":[{"question":"spam_scam","present":true},{"question":"spam_scam","present":false}]}`},
		{name: "string resultId", body: `{"resultId":"7","answers":[{"question":"spam_scam","present":true}]}`},
		{name: "fractional resultId", body: `{"resultId":7.5,"answers":[{"question":"spam_scam","present":true}]}`},
		{name: "missing resultId", body: `{"answers":[{"question":"spam_scam","present":true}]}`},
		{name: "non-boolean present", body: `{"resultId":7,"answers":[{"question":"spam_scam","present":"yes"}]}`},
		{name: "not JSON", body: `resultId=7`},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &reviewServiceFake{}
			response := serve(classificationadmin.NewRecordVerdictHandler(service), verdictRequest(test.body))
			assertXRPCError(t, response, "InvalidRequest")
			assert.Nil(t, service.lastVerdict, "an invalid body must not reach the service")
		})
	}
}

type wireQuestionPrecision struct {
	Question     string `json:"question"`
	AtOrAbove    int    `json:"atOrAbove"`
	Present      int    `json:"present"`
	TruePositive int    `json:"truePositive"`
	Precision    *int64 `json:"precision"`
	Recall       *int64 `json:"recall"`
}

// TestPrecisionHandlerOutput answers a lexicon-valid report with ratios as
// parts per million and undefined ratios omitted (B3).
func TestPrecisionHandlerOutput(t *testing.T) {
	twoThirds, half, zero := 2.0/3.0, 0.5, 0.0
	service := &reviewServiceFake{report: classification.PrecisionReport{
		ModelID: "text-model-a", PolicyVersion: "v1", Threshold: 0.5,
		VerdictedCount: 6, UnreviewableCount: 2,
		Questions: []classification.QuestionPrecision{
			{Question: "hate_harassment", AtOrAbove: 2, Present: 1, TruePositive: 0, Precision: &zero, Recall: &zero},
			{Question: "sexual_explicit"},
			{Question: "spam_scam", AtOrAbove: 3, Present: 4, TruePositive: 2, Precision: &twoThirds, Recall: &half},
		},
	}}
	response := serve(classificationadmin.NewPrecisionHandler(service), precisionRequest(url.Values{
		"modelId": {"text-model-a"}, "policyVersion": {"v1"}, "threshold": {"500000"},
	}))
	generic := requireLexiconOutput(t, response, precisionNSID)

	var output struct {
		ModelID           string                  `json:"modelId"`
		PolicyVersion     string                  `json:"policyVersion"`
		Threshold         int64                   `json:"threshold"`
		VerdictedCount    int                     `json:"verdictedCount"`
		UnreviewableCount int                     `json:"unreviewableCount"`
		Questions         []wireQuestionPrecision `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &output))
	assert.Equal(t, "text-model-a", output.ModelID)
	assert.Equal(t, "v1", output.PolicyVersion)
	assert.Equal(t, int64(500000), output.Threshold)
	assert.Equal(t, 6, output.VerdictedCount)
	assert.Equal(t, 2, output.UnreviewableCount)
	ppm := func(value int64) *int64 { return &value }
	assert.Equal(t, []wireQuestionPrecision{
		{Question: "hate_harassment", AtOrAbove: 2, Present: 1, TruePositive: 0, Precision: ppm(0), Recall: ppm(0)},
		{Question: "sexual_explicit"},
		{Question: "spam_scam", AtOrAbove: 3, Present: 4, TruePositive: 2, Precision: ppm(666667), Recall: ppm(500000)},
	}, output.Questions)

	questions, ok := generic["questions"].([]any)
	require.True(t, ok)
	require.Len(t, questions, 3)
	undefined, ok := questions[1].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, undefined, "precision", "an undefined precision is omitted")
	assert.NotContains(t, undefined, "recall", "an undefined recall is omitted")
}

// TestPrecisionHandlerDecodesRequest passes the evaluation and the threshold
// converted from parts per million to the service (B3).
func TestPrecisionHandlerDecodesRequest(t *testing.T) {
	for _, test := range []struct {
		threshold string
		expected  float64
	}{
		{threshold: "500000", expected: 0.5},
		{threshold: "333333", expected: 0.333333},
		{threshold: "1000000", expected: 1},
		{threshold: "0", expected: 0},
	} {
		t.Run(test.threshold, func(t *testing.T) {
			service := &reviewServiceFake{report: classification.PrecisionReport{ModelID: "text-model-a", PolicyVersion: "v1"}}
			response := serve(classificationadmin.NewPrecisionHandler(service), precisionRequest(url.Values{
				"modelId": {"text-model-a"}, "policyVersion": {"v1"}, "threshold": {test.threshold},
			}))
			assert.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
			require.NotNil(t, service.lastPrecision, "a valid query must reach the service")
			assert.Equal(t, "text-model-a", service.lastPrecision.ModelID)
			assert.Equal(t, "v1", service.lastPrecision.PolicyVersion)
			assert.InDelta(t, test.expected, service.lastPrecision.Threshold, 1e-9)
		})
	}
}

// TestPrecisionHandlerRejectsInvalidThreshold answers 400 InvalidRequest for a
// threshold outside 0..1000000 or not an integer (B3).
func TestPrecisionHandlerRejectsInvalidThreshold(t *testing.T) {
	for _, threshold := range []string{"1000001", "-1", "0.5", "half"} {
		t.Run(threshold, func(t *testing.T) {
			response := serve(classificationadmin.NewPrecisionHandler(&reviewServiceFake{}), precisionRequest(url.Values{
				"modelId": {"text-model-a"}, "policyVersion": {"v1"}, "threshold": {threshold},
			}))
			assertXRPCError(t, response, "InvalidRequest")
		})
	}
}

// TestHandlersMapServiceErrors maps each review error to its 400 XRPC error
// name on the endpoint that can produce it (B3).
func TestHandlersMapServiceErrors(t *testing.T) {
	list := func(service *reviewServiceFake) *httptest.ResponseRecorder {
		return serve(classificationadmin.NewListResultsHandler(service), listRequest(url.Values{"cursor": {"opaque"}}))
	}
	verdict := func(service *reviewServiceFake) *httptest.ResponseRecorder {
		return serve(classificationadmin.NewRecordVerdictHandler(service), verdictRequest(`{"resultId":7,"answers":[{"question":"spam_scam","present":true}]}`))
	}
	precision := func(service *reviewServiceFake) *httptest.ResponseRecorder {
		return serve(classificationadmin.NewPrecisionHandler(service), precisionRequest(url.Values{
			"modelId": {"text-model-a"}, "policyVersion": {"v1"}, "threshold": {"500000"},
		}))
	}
	for _, test := range []struct {
		name     string
		endpoint func(*reviewServiceFake) *httptest.ResponseRecorder
		err      error
		code     string
	}{
		{name: "list invalid cursor", endpoint: list, err: classification.ErrInvalidCursor, code: "InvalidCursor"},
		{name: "list invalid request", endpoint: list, err: classification.ErrInvalidRequest, code: "InvalidRequest"},
		{name: "verdict result not found", endpoint: verdict, err: classification.ErrResultNotFound, code: "ResultNotFound"},
		{name: "verdict unreviewable", endpoint: verdict, err: classification.ErrUnreviewable, code: "Unreviewable"},
		{name: "verdict invalid request", endpoint: verdict, err: classification.ErrInvalidRequest, code: "InvalidRequest"},
		{name: "precision invalid request", endpoint: precision, err: classification.ErrInvalidRequest, code: "InvalidRequest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := test.endpoint(&reviewServiceFake{err: fmt.Errorf("wrapped: %w", test.err)})
			assertXRPCError(t, response, test.code)
		})
	}
}

// TestHandlersReturnServiceRefusalReason answers a service refusal with its
// specific reason, so an admin can tell which question key was wrong (B3).
func TestHandlersReturnServiceRefusalReason(t *testing.T) {
	service := &reviewServiceFake{err: fmt.Errorf("%w: verdict does not answer question %q", classification.ErrInvalidRequest, "hate_harassment")}
	response := serve(classificationadmin.NewRecordVerdictHandler(service), verdictRequest(`{"resultId":7,"answers":[{"question":"spam_scam","present":true}]}`))

	assertXRPCError(t, response, "InvalidRequest")
	var body struct {
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Contains(t, body.Message, `verdict does not answer question "hate_harassment"`)
}
