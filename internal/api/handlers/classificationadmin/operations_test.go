package classificationadmin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"Coves/internal/api/handlers/classificationadmin"
	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	enqueueNSID = "social.coves.moderation.enqueueClassification"
	summaryNSID = "social.coves.moderation.getCommunityClassificationSummary"
)

// operationsServiceFake records the request each operations handler decoded
// and answers with the configured value or error.
type operationsServiceFake struct {
	report        classification.ReenqueueReport
	summary       classification.CommunitySummary
	err           error
	lastReenqueue *classification.ReenqueueRequest
	lastSummary   *classification.SummaryRequest
}

func (service *operationsServiceFake) Reenqueue(_ context.Context, request classification.ReenqueueRequest) (classification.ReenqueueReport, error) {
	service.lastReenqueue = &request
	return service.report, service.err
}

func (service *operationsServiceFake) Summary(_ context.Context, request classification.SummaryRequest) (classification.CommunitySummary, error) {
	service.lastSummary = &request
	return service.summary, service.err
}

func enqueueRequest(body string) *http.Request {
	return adminRequest(http.MethodPost, "/xrpc/"+enqueueNSID, []byte(body))
}

func summaryRequest(query url.Values) *http.Request {
	return adminRequest(http.MethodGet, "/xrpc/"+summaryNSID+"?"+query.Encode(), nil)
}

func validSummaryQuery() url.Values {
	return url.Values{"community": {"did:plc:community"}, "question": {"sexual_explicit"}, "minProbability": {"500000"}}
}

// TestEnqueueHandlerDecodesRequest passes the target and the RFC 3339 range
// to the service, leaves an absent range zero, and ignores unknown fields
// (B3).
func TestEnqueueHandlerDecodesRequest(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		expected classification.ReenqueueRequest
	}{
		{
			name: "posts and comments with range",
			body: `{"target":"postsAndComments","since":"2026-11-01T00:00:00Z","until":"2026-11-02T12:30:00.5Z"}`,
			expected: classification.ReenqueueRequest{
				Target: classification.ReenqueuePostsAndComments,
				Since:  time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
				Until:  time.Date(2026, 11, 2, 12, 30, 0, 500000000, time.UTC),
			},
		},
		{
			name:     "all communities",
			body:     `{"target":"allCommunities"}`,
			expected: classification.ReenqueueRequest{Target: classification.ReenqueueAllCommunities},
		},
		{
			name:     "unknown field ignored",
			body:     `{"target":"allCommunities","dryRun":true}`,
			expected: classification.ReenqueueRequest{Target: classification.ReenqueueAllCommunities},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &operationsServiceFake{}
			response := serve(classificationadmin.NewEnqueueHandler(service), enqueueRequest(test.body))
			assert.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
			require.NotNil(t, service.lastReenqueue, "a valid body must reach the service")
			got := *service.lastReenqueue
			assert.Equal(t, test.expected.Target, got.Target)
			assert.True(t, test.expected.Since.Equal(got.Since), "since %s", got.Since)
			assert.True(t, test.expected.Until.Equal(got.Until), "until %s", got.Until)
		})
	}
}

// TestEnqueueHandlerOutput answers the report as a lexicon-valid
// {created, revived, alreadyPresent}, zero counts included (B3).
func TestEnqueueHandlerOutput(t *testing.T) {
	for _, test := range []struct {
		name                             string
		report                           classification.ReenqueueReport
		created, revived, alreadyPresent float64
	}{
		{
			name:    "counts",
			report:  classification.ReenqueueReport{Created: 3, Revived: 4, AlreadyPresent: 2},
			created: 3, revived: 4, alreadyPresent: 2,
		},
		{name: "nothing in scope", report: classification.ReenqueueReport{}, created: 0, revived: 0, alreadyPresent: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &operationsServiceFake{report: test.report}
			response := serve(classificationadmin.NewEnqueueHandler(service), enqueueRequest(`{"target":"allCommunities"}`))
			generic := requireLexiconOutput(t, response, enqueueNSID)
			assert.Equal(t, map[string]any{
				"created": test.created, "revived": test.revived, "alreadyPresent": test.alreadyPresent,
			}, generic)
		})
	}
}

// TestEnqueueHandlerRejectsInvalidBodies answers 400 InvalidRequest without
// calling the service for a body that does not decode (B3).
func TestEnqueueHandlerRejectsInvalidBodies(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "not JSON", body: `target=allCommunities`},
		{name: "numeric target", body: `{"target":5}`},
		{name: "malformed since", body: `{"target":"postsAndComments","since":"yesterday","until":"2026-11-02T00:00:00Z"}`},
		{name: "malformed until", body: `{"target":"postsAndComments","since":"2026-11-01T00:00:00Z","until":"tomorrow"}`},
		{name: "numeric since", body: `{"target":"postsAndComments","since":1730419200,"until":"2026-11-02T00:00:00Z"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &operationsServiceFake{}
			response := serve(classificationadmin.NewEnqueueHandler(service), enqueueRequest(test.body))
			assertXRPCError(t, response, "InvalidRequest")
			assert.Nil(t, service.lastReenqueue, "an invalid body must not reach the service")
		})
	}
}

// TestSummaryHandlerDecodesRequest passes the community, the question and
// minProbability converted from parts per million to the service (B4).
func TestSummaryHandlerDecodesRequest(t *testing.T) {
	for _, test := range []struct {
		minProbability string
		expected       float64
	}{
		{minProbability: "500000", expected: 0.5},
		{minProbability: "0", expected: 0},
		{minProbability: "1000000", expected: 1},
	} {
		t.Run(test.minProbability, func(t *testing.T) {
			service := &operationsServiceFake{summary: classification.CommunitySummary{CommunityDID: "did:plc:community", Question: "sexual_explicit"}}
			response := serve(classificationadmin.NewSummaryHandler(service), summaryRequest(url.Values{
				"community": {"did:plc:community"}, "question": {"sexual_explicit"}, "minProbability": {test.minProbability},
			}))
			assert.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
			require.NotNil(t, service.lastSummary, "a valid query must reach the service")
			assert.Equal(t, "did:plc:community", service.lastSummary.CommunityDID)
			assert.Equal(t, "sexual_explicit", service.lastSummary.Question)
			assert.InDelta(t, test.expected, service.lastSummary.MinProbability, 1e-9)
		})
	}
}

// TestSummaryHandlerRejectsInvalidMinProbability answers 400 InvalidRequest
// without calling the service when minProbability is missing, not an integer
// or outside 0..1000000 (B4).
func TestSummaryHandlerRejectsInvalidMinProbability(t *testing.T) {
	for _, test := range []struct {
		name           string
		minProbability []string
	}{
		{name: "missing"},
		{name: "fractional", minProbability: []string{"0.5"}},
		{name: "word", minProbability: []string{"half"}},
		{name: "above one million", minProbability: []string{"1000001"}},
		{name: "negative", minProbability: []string{"-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := url.Values{"community": {"did:plc:community"}, "question": {"sexual_explicit"}}
			if test.minProbability != nil {
				query["minProbability"] = test.minProbability
			}
			service := &operationsServiceFake{}
			response := serve(classificationadmin.NewSummaryHandler(service), summaryRequest(query))
			assertXRPCError(t, response, "InvalidRequest")
			assert.Nil(t, service.lastSummary, "an invalid query must not reach the service")
		})
	}
}

type wireFlagEvent struct {
	OldNSFW    bool   `json:"oldNsfw"`
	NewNSFW    bool   `json:"newNsfw"`
	OccurredAt string `json:"occurredAt"`
	Delivered  bool   `json:"delivered"`
}

type wireKindSummary struct {
	Scored int `json:"scored"`
	Above  int `json:"above"`
}

type wireSummary struct {
	Community      string          `json:"community"`
	Question       string          `json:"question"`
	MinProbability int64           `json:"minProbability"`
	Since          string          `json:"since"`
	Until          string          `json:"until"`
	Posts          wireKindSummary `json:"posts"`
	Comments       wireKindSummary `json:"comments"`
	Share          *int64          `json:"share"`
	FlagEvents     []wireFlagEvent `json:"flagEvents"`
}

var (
	summarySince   = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	summaryUntil   = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	newerFlagEvent = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	olderFlagEvent = time.Date(2026, 11, 10, 12, 0, 0, 250000000, time.UTC)
)

// TestSummaryHandlerOutput answers a lexicon-valid summary with
// minProbability and share in parts per million and the flag events in the
// order the service gave them (B4).
func TestSummaryHandlerOutput(t *testing.T) {
	service := &operationsServiceFake{summary: classification.CommunitySummary{
		CommunityDID: "did:plc:community", Question: "sexual_explicit", MinProbability: 0.5,
		Since: summarySince, Until: summaryUntil,
		Posts:    classification.KindSummary{Scored: 3, Above: 1},
		Comments: classification.KindSummary{Scored: 1, Above: 1},
		FlagEvents: []classification.FlagEvent{
			{OldNSFW: true, NewNSFW: false, OccurredAt: newerFlagEvent, Delivered: false},
			{OldNSFW: false, NewNSFW: true, OccurredAt: olderFlagEvent, Delivered: true},
		},
	}}
	response := serve(classificationadmin.NewSummaryHandler(service), summaryRequest(validSummaryQuery()))
	requireLexiconOutput(t, response, summaryNSID)

	var output wireSummary
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &output))
	assert.Equal(t, "did:plc:community", output.Community)
	assert.Equal(t, "sexual_explicit", output.Question)
	assert.Equal(t, int64(500000), output.MinProbability)
	assert.True(t, summarySince.Equal(mustTime(t, output.Since)), "since %s", output.Since)
	assert.True(t, summaryUntil.Equal(mustTime(t, output.Until)), "until %s", output.Until)
	assert.Equal(t, wireKindSummary{Scored: 3, Above: 1}, output.Posts)
	assert.Equal(t, wireKindSummary{Scored: 1, Above: 1}, output.Comments)
	if assert.NotNil(t, output.Share, "share is defined when something was scored") {
		assert.Equal(t, int64(500000), *output.Share)
	}
	require.Len(t, output.FlagEvents, 2)
	assert.True(t, output.FlagEvents[0].OldNSFW)
	assert.False(t, output.FlagEvents[0].NewNSFW)
	assert.False(t, output.FlagEvents[0].Delivered)
	assert.True(t, newerFlagEvent.Equal(mustTime(t, output.FlagEvents[0].OccurredAt)), "occurredAt %s", output.FlagEvents[0].OccurredAt)
	assert.False(t, output.FlagEvents[1].OldNSFW)
	assert.True(t, output.FlagEvents[1].NewNSFW)
	assert.True(t, output.FlagEvents[1].Delivered)
	assert.True(t, olderFlagEvent.Equal(mustTime(t, output.FlagEvents[1].OccurredAt)), "occurredAt %s", output.FlagEvents[1].OccurredAt)
}

// TestSummaryHandlerOmitsUndefinedShare answers a lexicon-valid summary with
// no share and an empty flagEvents array when nothing was scored (B4).
func TestSummaryHandlerOmitsUndefinedShare(t *testing.T) {
	service := &operationsServiceFake{summary: classification.CommunitySummary{
		CommunityDID: "did:plc:community", Question: "sexual_explicit", MinProbability: 0.5,
		Since: summarySince, Until: summaryUntil,
	}}
	response := serve(classificationadmin.NewSummaryHandler(service), summaryRequest(validSummaryQuery()))
	generic := requireLexiconOutput(t, response, summaryNSID)
	assert.NotContains(t, generic, "share", "an undefined share is omitted")
	assert.Equal(t, []any{}, generic["flagEvents"], "flagEvents is an empty array, never null")
	assert.Equal(t, map[string]any{"scored": float64(0), "above": float64(0)}, generic["posts"])
	assert.Equal(t, map[string]any{"scored": float64(0), "above": float64(0)}, generic["comments"])
}

// TestOperationsHandlersMapServiceErrors maps each named service error to its
// 400 XRPC error name on the endpoint that can produce it (B3, B4).
func TestOperationsHandlersMapServiceErrors(t *testing.T) {
	enqueue := func(service *operationsServiceFake) *httptest.ResponseRecorder {
		return serve(classificationadmin.NewEnqueueHandler(service), enqueueRequest(`{"target":"allCommunities"}`))
	}
	summary := func(service *operationsServiceFake) *httptest.ResponseRecorder {
		return serve(classificationadmin.NewSummaryHandler(service), summaryRequest(validSummaryQuery()))
	}
	for _, test := range []struct {
		name     string
		endpoint func(*operationsServiceFake) *httptest.ResponseRecorder
		err      error
		code     string
	}{
		{name: "enqueue invalid request", endpoint: enqueue, err: classification.ErrInvalidRequest, code: "InvalidRequest"},
		{name: "enqueue classifier not configured", endpoint: enqueue, err: classification.ErrClassifierNotConfigured, code: "ClassifierNotConfigured"},
		{name: "summary invalid request", endpoint: summary, err: classification.ErrInvalidRequest, code: "InvalidRequest"},
		{name: "summary community not found", endpoint: summary, err: classification.ErrCommunityNotFound, code: "CommunityNotFound"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := test.endpoint(&operationsServiceFake{err: fmt.Errorf("wrapped: %w", test.err)})
			assertXRPCError(t, response, test.code)
		})
	}
}

// TestOperationsHandlersAnswerUnknownErrorsWith500 answers an unnamed service
// error with 500 InternalServerError on both endpoints (B3, B4).
func TestOperationsHandlersAnswerUnknownErrorsWith500(t *testing.T) {
	storeFailure := errors.New("store unavailable")
	for _, test := range []struct {
		name    string
		handler http.Handler
		request *http.Request
	}{
		{name: "enqueue", handler: classificationadmin.NewEnqueueHandler(&operationsServiceFake{err: storeFailure}), request: enqueueRequest(`{"target":"allCommunities"}`)},
		{name: "summary", handler: classificationadmin.NewSummaryHandler(&operationsServiceFake{err: storeFailure}), request: summaryRequest(validSummaryQuery())},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := serve(test.handler, test.request)
			assert.Equal(t, http.StatusInternalServerError, response.Code)
			var body map[string]any
			if assert.NoError(t, json.Unmarshal(response.Body.Bytes(), &body), "XRPC error must be a JSON body: %q", response.Body.String()) {
				assert.Equal(t, "InternalServerError", body["error"])
				assert.NotContains(t, body["message"], "store unavailable", "an internal error's detail stays out of the response")
			}
		})
	}
}
