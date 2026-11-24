//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"testing"

	"Coves/internal/validation"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	listClassificationResultsMethod   = "social.coves.moderation.listClassificationResults"
	recordClassificationVerdictMethod = "social.coves.moderation.recordClassificationVerdict"
	getClassificationPrecisionMethod  = "social.coves.moderation.getClassificationPrecision"
)

// requireClassificationLexiconOutput checks a response body validates
// against nsid#output in the repository's lexicon directory.
func requireClassificationLexiconOutput(t *testing.T, body json.RawMessage, nsid string) {
	t.Helper()
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../internal/atproto/lexicon"))
	decoded, err := atdata.UnmarshalJSON(body)
	require.NoError(t, err)
	assert.NoError(t, validation.ValidateData(catalog, decoded, nsid+"#output", 0), "output must validate against %s#output", nsid)
}

type classificationPrecisionResponse struct {
	ModelID           string `json:"modelId"`
	PolicyVersion     string `json:"policyVersion"`
	Threshold         int64  `json:"threshold"`
	VerdictedCount    int    `json:"verdictedCount"`
	UnreviewableCount int    `json:"unreviewableCount"`
	Questions         []struct {
		Question     string `json:"question"`
		AtOrAbove    int    `json:"atOrAbove"`
		Present      int    `json:"present"`
		TruePositive int    `json:"truePositive"`
		Precision    *int64 `json:"precision"`
		Recall       *int64 `json:"recall"`
	} `json:"questions"`
}

// TestClassificationReviewAPIContract: on the hermetic stack an instance
// admin lists no results, is told an unknown result is not found, and reads
// the current policy questions' empty precision, all lexicon-valid; a non-admin is
// forbidden all three (B10).
func TestClassificationReviewAPIContract(t *testing.T) {
	p := newPipeline(t)
	admin := testkit.ModerationAdmin(t, 1)
	nonAdmin := p.IndexedAccount(t, "clsoutsider")

	precisionParams := url.Values{"modelId": {"@cf/test/model"}, "policyVersion": {"v3"}, "threshold": {"500000"}}
	verdictInput := map[string]any{
		"resultId": 999999999,
		"answers":  []map[string]any{{"question": "spam_scam", "present": true}},
	}

	t.Run("admin lists no results", func(t *testing.T) {
		token := admin.ServiceAuth(t, communityInstanceDID, listClassificationResultsMethod)
		var body json.RawMessage
		require.NoError(t, p.AppView.As(token).Query(t.Context(), listClassificationResultsMethod, nil, &body))
		requireClassificationLexiconOutput(t, body, listClassificationResultsMethod)
		var response struct {
			Results []json.RawMessage `json:"results"`
			Cursor  *string           `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		assert.NotNil(t, response.Results, "results is present: %s", body)
		assert.Empty(t, response.Results)
		assert.Nil(t, response.Cursor, "an empty listing has no cursor")
	})

	t.Run("admin verdict on an unknown result", func(t *testing.T) {
		token := admin.ServiceAuth(t, communityInstanceDID, recordClassificationVerdictMethod)
		err := p.AppView.As(token).Procedure(t.Context(), recordClassificationVerdictMethod, verdictInput, nil)
		_ = requireXRPCRefusal(t, err, http.StatusBadRequest, "ResultNotFound", "a verdict on an unknown result")
	})

	t.Run("admin precision with nothing verdicted", func(t *testing.T) {
		token := admin.ServiceAuth(t, communityInstanceDID, getClassificationPrecisionMethod)
		var body json.RawMessage
		require.NoError(t, p.AppView.As(token).Query(t.Context(), getClassificationPrecisionMethod, precisionParams, &body))
		requireClassificationLexiconOutput(t, body, getClassificationPrecisionMethod)
		var response classificationPrecisionResponse
		require.NoError(t, json.Unmarshal(body, &response))
		assert.Equal(t, "@cf/test/model", response.ModelID)
		assert.Equal(t, "v3", response.PolicyVersion)
		assert.Equal(t, int64(500000), response.Threshold)
		assert.Equal(t, 0, response.VerdictedCount)
		assert.Equal(t, 0, response.UnreviewableCount)
		questions := make([]string, 0, len(response.Questions))
		for _, question := range response.Questions {
			questions = append(questions, question.Question)
			assert.Zero(t, question.AtOrAbove, "%s at or above", question.Question)
			assert.Zero(t, question.Present, "%s present", question.Question)
			assert.Zero(t, question.TruePositive, "%s true positives", question.Question)
			assert.Nil(t, question.Precision, "%s precision is undefined", question.Question)
			assert.Nil(t, question.Recall, "%s recall is undefined", question.Question)
		}
		sort.Strings(questions)
		assert.Equal(t, []string{
			"graphic_violence", "hate_harassment", "sexual_explicit", "sexual_minors", "sexual_suggestive", "spam_scam",
		}, questions)
	})

	for _, call := range []struct {
		name   string
		method string
		invoke func(ctx context.Context, client *testkit.AppView) error
	}{
		{
			name:   "list",
			method: listClassificationResultsMethod,
			invoke: func(ctx context.Context, client *testkit.AppView) error {
				return client.Query(ctx, listClassificationResultsMethod, nil, nil)
			},
		},
		{
			name:   "verdict",
			method: recordClassificationVerdictMethod,
			invoke: func(ctx context.Context, client *testkit.AppView) error {
				return client.Procedure(ctx, recordClassificationVerdictMethod, verdictInput, nil)
			},
		},
		{
			name:   "precision",
			method: getClassificationPrecisionMethod,
			invoke: func(ctx context.Context, client *testkit.AppView) error {
				return client.Query(ctx, getClassificationPrecisionMethod, precisionParams, nil)
			},
		},
	} {
		t.Run("non-admin "+call.name+" is forbidden", func(t *testing.T) {
			token := nonAdmin.ServiceAuth(t, communityInstanceDID, call.method)
			err := call.invoke(t.Context(), p.AppView.As(token))
			_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Forbidden", "a non-admin's "+call.name+" call")
		})
	}
}
