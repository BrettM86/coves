//go:build integration

package postgres_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"Coves/internal/api/routes"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/labeling"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queriedLabel is the identity of one served label: every label these tests
// issue is a !takedown, so subject and negation tell them apart.
type queriedLabel struct {
	uri string
	neg bool
}

// getQueryLabels calls queryLabels and returns the status and body.
// repeatedValues returns count copies of value as one repeated query parameter.
func repeatedValues(value string, count int) []string {
	values := make([]string, count)
	for index := range values {
		values[index] = value
	}
	return values
}

func getQueryLabels(t *testing.T, serverURL string, query url.Values) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		serverURL+"/xrpc/com.atproto.label.queryLabels?"+query.Encode(), nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, body
}

// decodeVerifiedLabels decodes a queryLabels page, verifies every label's
// signature on the wire path with the configured public key, and returns the
// labels' identities in served order.
func decodeVerifiedLabels(t *testing.T, body []byte) (comatproto.LabelQueryLabels_Output, []queriedLabel) {
	t.Helper()
	var output comatproto.LabelQueryLabels_Output
	require.NoError(t, json.Unmarshal(body, &output), string(body))
	var served []queriedLabel
	for _, label := range output.Labels {
		require.NotNil(t, label)
		assert.Equal(t, fixtures.InstanceDID(), label.Src)
		assert.Equal(t, "!takedown", label.Val)
		assert.Nil(t, label.Cid)
		decoded := labeling.FromLexicon(label)
		require.NoError(t, decoded.VerifySignature(issuanceLabelPublicKey(t)), "served label signature must verify")
		served = append(served, queriedLabel{uri: label.Uri, neg: label.Neg != nil && *label.Neg})
	}
	return output, served
}

func TestQueryLabelsServesStoredLabels(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	post := func(rkey string) moderation.StrongRef {
		return indexedModerationPostWithRecordKey(t, db, moderation.PostV2Collection, communityDID, authorDID, "query "+rkey, "", rkey)
	}
	// "querya_1" and "querya_2" share the prefix "querya_"; "queryab1" differs
	// only where the underscore is, so it matches that prefix only if the
	// underscore is treated as a wildcard.
	first, second, wildcardDistractor, other := post("querya_1"), post("querya_2"), post("queryab1"), post("queryzz1")
	collectionPrefix := "at://" + authorDID + "/" + moderation.PostV2Collection + "/"

	service := newLabelPublishingModerationService(t, db)
	actor := fixtures.DID("labelqueryadmin")
	var firstRemovalID string
	for index, subject := range []moderation.StrongRef{first, second, wildcardDistractor, other} {
		removed, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: fmt.Sprintf("query-remove-%d", index), Reason: moderationConcurrencyReason,
		})
		require.NoError(t, err)
		if index == 0 {
			firstRemovalID = removed.Action.ID
		}
	}
	_, err := service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
		ActionID: firstRemovalID, ReviewedSubject: &first, ExpectedVersion: "v1",
		IdempotencyKey: "query-restore-first", Reason: issuanceRestoreReason,
	})
	require.NoError(t, err)
	// Issued in seq order: first, second, wildcardDistractor, other removed; first restored.

	router := chi.NewRouter()
	routes.RegisterLabelRoutes(router, postgres.NewLabelRepository(db))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name  string
		query url.Values
		want  []queriedLabel
	}{
		{
			name: "exact uri", query: url.Values{"uriPatterns": {first.URI}},
			want: []queriedLabel{{first.URI, false}, {first.URI, true}},
		},
		{
			name: "prefix pattern", query: url.Values{"uriPatterns": {collectionPrefix + "querya*"}},
			want: []queriedLabel{{first.URI, false}, {second.URI, false}, {wildcardDistractor.URI, false}, {first.URI, true}},
		},
		{
			name: "two disjoint patterns return their union", query: url.Values{"uriPatterns": {second.URI, collectionPrefix + "queryz*"}},
			want: []queriedLabel{{second.URI, false}, {other.URI, false}},
		},
		{
			name: "underscore in a prefix matches only an underscore", query: url.Values{"uriPatterns": {collectionPrefix + "querya_*"}},
			want: []queriedLabel{{first.URI, false}, {second.URI, false}, {first.URI, true}},
		},
		{
			name: "sources naming this instance", query: url.Values{"uriPatterns": {other.URI}, "sources": {fixtures.InstanceDID()}},
			want: []queriedLabel{{other.URI, false}},
		},
		{
			name: "sources naming another labeler", query: url.Values{"uriPatterns": {other.URI}, "sources": {"did:plc:otherlabeler"}},
			want: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := getQueryLabels(t, server.URL, test.query)
			require.Equal(t, http.StatusOK, status, string(body))
			_, served := decodeVerifiedLabels(t, body)
			assert.Equal(t, test.want, served)
		})
	}

	t.Run("limit and cursor page through every match once in seq order", func(t *testing.T) {
		var served []queriedLabel
		var pageSizes []int
		cursor := ""
		exhausted := false
		for range 10 {
			query := url.Values{"uriPatterns": {collectionPrefix + "query*"}, "limit": {"2"}}
			if cursor != "" {
				query.Set("cursor", cursor)
			}
			status, body := getQueryLabels(t, server.URL, query)
			require.Equal(t, http.StatusOK, status, string(body))
			output, page := decodeVerifiedLabels(t, body)
			if len(page) == 0 {
				exhausted = true
				break
			}
			require.NotNil(t, output.Cursor, "a non-empty page carries a cursor")
			cursor = *output.Cursor
			pageSizes = append(pageSizes, len(page))
			served = append(served, page...)
		}
		require.True(t, exhausted, "the final page's cursor must yield no further labels")
		assert.Equal(t, []int{2, 2, 1}, pageSizes)
		assert.Equal(t, []queriedLabel{
			{first.URI, false}, {second.URI, false}, {wildcardDistractor.URI, false}, {other.URI, false}, {first.URI, true},
		}, served)
	})

	for _, test := range []struct {
		name  string
		query url.Values
	}{
		{name: "limit zero", query: url.Values{"uriPatterns": {first.URI}, "limit": {"0"}}},
		{name: "limit above 250", query: url.Values{"uriPatterns": {first.URI}, "limit": {"251"}}},
		{name: "missing uriPatterns", query: url.Values{"limit": {"10"}}},
		{name: "non-numeric limit", query: url.Values{"uriPatterns": {first.URI}, "limit": {"x"}}},
		{name: "non-numeric cursor", query: url.Values{"uriPatterns": {first.URI}, "cursor": {"abc"}}},
		{name: "negative cursor", query: url.Values{"uriPatterns": {first.URI}, "cursor": {"-1"}}},
		{name: "51 uriPatterns", query: url.Values{"uriPatterns": repeatedValues(first.URI, 51)}},
		{name: "51 sources", query: url.Values{"uriPatterns": {first.URI}, "sources": repeatedValues(fixtures.InstanceDID(), 51)}},
		{name: "uriPattern over the byte cap", query: url.Values{"uriPatterns": {first.URI + strings.Repeat("a", 2049-len(first.URI))}}},
		{name: "source over the byte cap", query: url.Values{"uriPatterns": {first.URI}, "sources": {"did:web:" + strings.Repeat("a", 2041)}}},
		{name: "uriPattern with invalid UTF-8", query: url.Values{"uriPatterns": {first.URI + "\xff"}}},
		{name: "uriPattern with NUL", query: url.Values{"uriPatterns": {first.URI + "\x00"}}},
		{name: "source with NUL", query: url.Values{"uriPatterns": {first.URI}, "sources": {fixtures.InstanceDID() + "\x00"}}},
	} {
		t.Run("rejects "+test.name, func(t *testing.T) {
			status, body := getQueryLabels(t, server.URL, test.query)
			require.Equal(t, http.StatusBadRequest, status, string(body))
			var failure struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(body, &failure), string(body))
			assert.Equal(t, "InvalidRequest", failure.Error)
		})
	}
}
