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

// The alert message's first line carries the result ID, the question key and
// the subject kind; no alert ID, job ID or attempt count. A URI line follows
// with the subject identifier verbatim when there is one, and an Open line
// with the subject's web link when one can be built from the base URL, the
// community DID and the subject (and, for a comment, its root post) URI.
// Each link path segment is escaped.
func TestRenderScoreAlert(t *testing.T) {
	const (
		base         = "https://coves.social"
		communityDID = "did:plc:alertcommunity"
		postURI      = "at://did:plc:alertauthor/social.coves.community.postv2/3kalertpost1"
		commentURI   = "at://did:plc:alertcommenter/social.coves.community.comment/3kalertcmt1"
	)
	post := func(subjectURI string) classification.ScoreAlert {
		return classification.ScoreAlert{
			ID: 9, ResultID: 4217, JobID: 77, QuestionKey: "sexual_minors", Attempts: 3,
			SubjectKind: classification.SubjectKindPost, SubjectURI: subjectURI, CommunityDID: communityDID,
		}
	}
	comment := func(rootURI string) classification.ScoreAlert {
		return classification.ScoreAlert{
			ID: 9, ResultID: 4218, JobID: 77, QuestionKey: "spam_scam", Attempts: 3,
			SubjectKind: classification.SubjectKindComment, SubjectURI: commentURI, CommunityDID: communityDID, RootURI: rootURI,
		}
	}
	for _, row := range []struct {
		name    string
		alert   classification.ScoreAlert
		baseURL string
		want    string
	}{
		{
			name:    "post",
			alert:   post(postURI),
			baseURL: base,
			want: "Classifier score alert: result 4217, question sexual_minors, subject post\n" +
				"URI: at://did:plc:alertauthor/social.coves.community.postv2/3kalertpost1\n" +
				"Open: https://coves.social/c/did:plc:alertcommunity/post/did:plc:alertauthor/3kalertpost1",
		},
		{
			name:    "comment with its root post",
			alert:   comment(postURI),
			baseURL: base,
			want: "Classifier score alert: result 4218, question spam_scam, subject comment\n" +
				"URI: at://did:plc:alertcommenter/social.coves.community.comment/3kalertcmt1\n" +
				"Open: https://coves.social/c/did:plc:alertcommunity/post/did:plc:alertauthor/3kalertpost1/comment/did:plc:alertcommenter/3kalertcmt1",
		},
		{
			name: "community, whose subject is its DID",
			alert: classification.ScoreAlert{
				ID: 9, ResultID: 4219, JobID: 77, QuestionKey: "hate_harassment", Attempts: 3,
				SubjectKind: classification.SubjectKindCommunity, SubjectURI: communityDID, CommunityDID: communityDID,
			},
			baseURL: base,
			want: "Classifier score alert: result 4219, question hate_harassment, subject community\n" +
				"URI: did:plc:alertcommunity\n" +
				"Open: https://coves.social/c/did:plc:alertcommunity",
		},
		{
			name:    "comment with no root post has no link",
			alert:   comment(""),
			baseURL: base,
			want: "Classifier score alert: result 4218, question spam_scam, subject comment\n" +
				"URI: at://did:plc:alertcommenter/social.coves.community.comment/3kalertcmt1",
		},
		{
			name:    "comment with an unparsable root post has no link",
			alert:   comment("not an at uri"),
			baseURL: base,
			want: "Classifier score alert: result 4218, question spam_scam, subject comment\n" +
				"URI: at://did:plc:alertcommenter/social.coves.community.comment/3kalertcmt1",
		},
		{
			name:    "base URL with a trailing slash",
			alert:   post(postURI),
			baseURL: "https://coves.social/",
			want: "Classifier score alert: result 4217, question sexual_minors, subject post\n" +
				"URI: at://did:plc:alertauthor/social.coves.community.postv2/3kalertpost1\n" +
				"Open: https://coves.social/c/did:plc:alertcommunity/post/did:plc:alertauthor/3kalertpost1",
		},
		{
			name:    "no base URL has no link",
			alert:   post(postURI),
			baseURL: "",
			want: "Classifier score alert: result 4217, question sexual_minors, subject post\n" +
				"URI: at://did:plc:alertauthor/social.coves.community.postv2/3kalertpost1",
		},
		{
			name:    "unparsable subject URI is shown verbatim with no link",
			alert:   post("not an at uri"),
			baseURL: base,
			want: "Classifier score alert: result 4217, question sexual_minors, subject post\n" +
				"URI: not an at uri",
		},
		{
			name:    "subject URI with no record key is shown verbatim with no link",
			alert:   post("at://did:plc:alertauthor/social.coves.community.postv2"),
			baseURL: base,
			want: "Classifier score alert: result 4217, question sexual_minors, subject post\n" +
				"URI: at://did:plc:alertauthor/social.coves.community.postv2",
		},
		{
			name:    "no subject URI has neither line",
			alert:   post(""),
			baseURL: base,
			want:    "Classifier score alert: result 4217, question sexual_minors, subject post",
		},
		{
			name:    "link path segments are escaped",
			alert:   post("at://did:web:example.com%3A8443/social.coves.community.postv2/3kalertpost2"),
			baseURL: base,
			want: "Classifier score alert: result 4217, question sexual_minors, subject post\n" +
				"URI: at://did:web:example.com%3A8443/social.coves.community.postv2/3kalertpost2\n" +
				"Open: https://coves.social/c/did:plc:alertcommunity/post/did:web:example.com%253A8443/3kalertpost2",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			assert.Equal(t, row.want, classification.RenderScoreAlert(row.alert, row.baseURL))
		})
	}
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
