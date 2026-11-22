package classification_test

import (
	"testing"

	"Coves/internal/core/classification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A probability at its threshold meets it, one below does not, and a
// question the result has no probability for never meets its threshold (B1).
func TestMetScoreAlerts(t *testing.T) {
	probabilities := map[string]float64{
		"sexual_minors":    0.3,
		"graphic_violence": 0.89,
		"spam_scam":        0.95,
	}
	thresholds := []classification.ScoreAlertThreshold{
		{QuestionKey: "sexual_minors", Threshold: 0.3},
		{QuestionKey: "graphic_violence", Threshold: 0.9},
		{QuestionKey: "spam_scam", Threshold: 0.5},
		{QuestionKey: "hate_harassment", Threshold: 0.1},
	}

	assert.Equal(t, []string{"sexual_minors", "spam_scam"}, classification.MetScoreAlerts(probabilities, thresholds))
}

// The alert message carries the result ID, the question key and the subject
// kind and nothing else: no alert ID, job ID or attempt count (B2).
func TestRenderScoreAlert(t *testing.T) {
	message := classification.RenderScoreAlert(classification.ScoreAlert{
		ID:          9,
		ResultID:    4217,
		JobID:       77,
		QuestionKey: "sexual_minors",
		SubjectKind: classification.SubjectKindComment,
		Attempts:    3,
	})

	assert.Equal(t, "Classifier score alert: result 4217, question sexual_minors, subject comment", message)
}

// CLASSIFIER_SCORE_ALERTS is key:threshold pairs over the policy's question
// keys, each threshold in (0, 1]; anything else is an error naming the
// setting and the offending entry (B3).
func TestParseScoreAlerts(t *testing.T) {
	policy, err := classification.LoadPolicy()
	require.NoError(t, err)

	t.Run("valid", func(t *testing.T) {
		for _, test := range []struct {
			name string
			raw  string
			want []classification.ScoreAlertThreshold
		}{
			{
				name: "two pairs with spaces",
				raw:  "sexual_minors:0.3, spam_scam:0.9",
				want: []classification.ScoreAlertThreshold{
					{QuestionKey: "sexual_minors", Threshold: 0.3},
					{QuestionKey: "spam_scam", Threshold: 0.9},
				},
			},
			{
				name: "threshold of exactly one",
				raw:  "sexual_minors:1",
				want: []classification.ScoreAlertThreshold{{QuestionKey: "sexual_minors", Threshold: 1}},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				thresholds, err := classification.ParseScoreAlerts(test.raw, policy)
				require.NoError(t, err)
				assert.Equal(t, test.want, thresholds)
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			raw      string
			wantName string
		}{
			{name: "unknown key", raw: "sexual_minor:0.3", wantName: "sexual_minor"},
			{name: "no threshold", raw: "sexual_minors", wantName: "sexual_minors"},
			{name: "zero threshold", raw: "sexual_minors:0", wantName: "sexual_minors"},
			{name: "threshold above one", raw: "sexual_minors:1.5", wantName: "sexual_minors"},
			{name: "threshold not a number", raw: "sexual_minors:x", wantName: "sexual_minors"},
			{name: "duplicate key", raw: "sexual_minors:0.3,sexual_minors:0.5", wantName: "sexual_minors"},
			{name: "empty", raw: "", wantName: ""},
		} {
			t.Run(test.name, func(t *testing.T) {
				thresholds, err := classification.ParseScoreAlerts(test.raw, policy)
				require.Error(t, err)
				assert.Nil(t, thresholds)
				assert.ErrorContains(t, err, "CLASSIFIER_SCORE_ALERTS")
				assert.ErrorContains(t, err, test.wantName)
			})
		}
		t.Run("unknown key names the key as given", func(t *testing.T) {
			_, err := classification.ParseScoreAlerts("sexual_minor:0.3", policy)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "sexual_minors", "the error names the unknown key, not a policy key")
		})
	})
}
