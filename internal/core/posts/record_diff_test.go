package posts

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The bridgedStats exception of docs/PRD_AUTHOR_OWNED_POSTS.md §5.5.
//
// A bridge rewrites its records constantly and specifically to refresh
// origin-platform vote counts. Every rewrite changes the record's CID, so
// without an exception every accepted bridge post would drop out of feeds into
// pending_reacceptance several times an hour and the community's repo would
// fill with re-acceptance commits that decided nothing.
//
// The exception is narrow and the narrowness is the security property. What is
// waved through is a re-decision, so anything that slips through unexamined is
// content a moderator accepted once and never looked at again. The function
// therefore answers RecordDiffBridgedStatsOnly for exactly one shape —
// bridgedStats differs, nothing else does — and RecordDiffPolicyRelevant for
// everything it does not fully understand, INCLUDING fields it has never heard
// of. That is why it reads decoded maps rather than PostRecord values: a typed
// struct discards unknown fields, so an author who added one would produce two
// structs comparing equal and an edit classified as "nothing changed".
//
// classifyRecordDiff is not called by anything yet. It ships with the engine so
// that the repin path has a decision procedure to call when it lands, and it is
// specified here so that path cannot be written against a guess.

// basePostRecord is the fully-populated record every case below mutates. It
// carries every property social.coves.community.postv2 declares, so a case that
// changes one field is changing it in the presence of all the others.
func basePostRecord() map[string]any {
	return map[string]any{
		"$type":     "social.coves.community.postv2",
		"community": "did:plc:cccccccccccccccccccccccc",
		"title":     "a post with every field",
		"content":   "the body",
		"facets": []any{
			map[string]any{
				"index":    map[string]any{"byteStart": float64(0), "byteEnd": float64(1)},
				"features": []any{map[string]any{"$type": "social.coves.richtext.facet#bold"}},
			},
		},
		"embed": map[string]any{
			"$type": "social.coves.embed.external",
			"uri":   "https://example.com/a",
		},
		"langs":  []any{"en"},
		"labels": map[string]any{"$type": "com.atproto.label.defs#selfLabels", "values": []any{map[string]any{"val": "spoiler"}}},
		"tags":   []any{"gardening"},
		"crosspostOf": map[string]any{
			"uri": "at://did:plc:abc123/social.coves.community.postv2/3kjzl5kcb2s2v",
			"cid": "bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		"crosspostChain": []any{"did:plc:cccccccccccccccccccccccc"},
		"createdAt":      "2026-07-01T12:00:00Z",
		"bridgedStats": map[string]any{
			"upvotes":   float64(10),
			"downvotes": float64(2),
			"asOf":      "2026-07-01T12:00:00Z",
		},
	}
}

// withField returns the base record with one field set, or removed when value
// is nil.
func withField(field string, value any) map[string]any {
	record := basePostRecord()
	if value == nil {
		delete(record, field)
		return record
	}
	record[field] = value
	return record
}

func TestClassifyRecordDiff(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change map[string]any
		want   RecordDiffClass
		why    string
	}{
		{
			name:   "identical records",
			change: basePostRecord(),
			want:   RecordDiffNone,
			why:    "a redelivery of the same record is not an edit and must not cost a re-decision",
		},
		{
			name: "bridgedStats counts refreshed",
			change: withField("bridgedStats", map[string]any{
				"upvotes":   float64(97),
				"downvotes": float64(3),
				"asOf":      "2026-07-01T13:00:00Z",
			}),
			want: RecordDiffBridgedStatsOnly,
			why:  "this is the exception itself: the vote counts a bridge exists to refresh",
		},
		{
			name:   "bridgedStats added where there was none",
			change: withField("bridgedStats", map[string]any{"upvotes": float64(1), "downvotes": float64(0)}),
			want:   RecordDiffBridgedStatsOnly,
			why:    "the first refresh after a bridge starts reporting is still only a stats change",
		},
		{
			name:   "bridgedStats removed",
			change: withField("bridgedStats", nil),
			want:   RecordDiffBridgedStatsOnly,
			why:    "a bridge that stops reporting counts has still changed nothing a moderator judges",
		},
		{
			name:   "title changed",
			change: withField("title", "a completely different post"),
			want:   RecordDiffPolicyRelevant,
			why:    "the headline is the single most re-decidable thing about a post",
		},
		{
			name:   "content changed",
			change: withField("content", "something a moderator never saw"),
			want:   RecordDiffPolicyRelevant,
			why:    "the body is what acceptance accepted",
		},
		{
			name:   "content removed",
			change: withField("content", nil),
			want:   RecordDiffPolicyRelevant,
			why:    "a field vanishing is as much an edit as a field changing",
		},
		{
			name: "facets changed",
			change: withField("facets", []any{
				map[string]any{
					"index":    map[string]any{"byteStart": float64(0), "byteEnd": float64(5)},
					"features": []any{map[string]any{"$type": "social.coves.richtext.facet#link", "uri": "https://elsewhere.example/"}},
				},
			}),
			want: RecordDiffPolicyRelevant,
			why:  "facets carry links; repointing one changes where readers land without touching the text",
		},
		{
			name:   "embed changed",
			change: withField("embed", map[string]any{"$type": "social.coves.embed.external", "uri": "https://elsewhere.example/"}),
			want:   RecordDiffPolicyRelevant,
			why:    "swapping the embedded media is the classic bait-and-switch against a stale acceptance",
		},
		{
			name:   "labels changed",
			change: withField("labels", map[string]any{"$type": "com.atproto.label.defs#selfLabels", "values": []any{}}),
			want:   RecordDiffPolicyRelevant,
			why:    "self-labels are how content warnings are declared; dropping one un-warns the post",
		},
		{
			name:   "tags changed",
			change: withField("tags", []any{"politics"}),
			want:   RecordDiffPolicyRelevant,
			why:    "tags steer distribution",
		},
		{
			name:   "community changed",
			change: withField("community", "did:plc:dddddddddddddddddddddddd"),
			want:   RecordDiffPolicyRelevant,
			why:    "the post claims to belong somewhere else entirely",
		},
		{
			name:   "langs changed",
			change: withField("langs", []any{"ru"}),
			want:   RecordDiffPolicyRelevant,
			why:    "not on §5.5's enumerated list, and everything not on the list is re-decided",
		},
		{
			name:   "crosspostOf changed",
			change: withField("crosspostOf", map[string]any{"uri": "at://did:plc:abc123/social.coves.community.postv2/other", "cid": "bafyreibbbb"}),
			want:   RecordDiffPolicyRelevant,
			why:    "the post now claims to mirror something else",
		},
		{
			name:   "createdAt changed",
			change: withField("createdAt", "2026-07-02T12:00:00Z"),
			want:   RecordDiffPolicyRelevant,
			why:    "a record rewritten with a new timestamp is not the record that was accepted",
		},
		{
			name:   "an unknown field appeared",
			change: withField("summary", "a field this build has never heard of"),
			want:   RecordDiffPolicyRelevant,
			why: "FAIL CLOSED. A bridge or a client running a newer lexicon than this AppView will " +
				"send fields this function cannot name, and treating an unrecognised difference as " +
				"harmless would wave through exactly the edits nobody has looked at yet",
		},
		{
			name:   "an unknown field changed value",
			change: withField("$type", "social.coves.community.postv3"),
			want:   RecordDiffPolicyRelevant,
			why:    "even the record's own type is a difference that has to be re-decided, not assumed benign",
		},
		{
			name: "bridgedStats changed AND a policy field changed",
			change: func() map[string]any {
				record := withField("title", "a completely different post")
				record["bridgedStats"] = map[string]any{"upvotes": float64(99), "downvotes": float64(0)}
				return record
			}(),
			want: RecordDiffPolicyRelevant,
			why: "the exception is bridgedStats AND NOTHING ELSE; a real edit smuggled alongside a " +
				"stats refresh is the obvious way to launder one past re-admission",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equalf(t, tc.want, classifyRecordDiff(basePostRecord(), tc.change), "%s", tc.why)
		})
	}
}

func TestClassifyRecordDiff_TreatsAMissingRecordAsPolicyRelevant(t *testing.T) {
	t.Parallel()

	// The engine reaches this function with whatever it managed to decode. A
	// nil old record means the AppView has no prior version to compare against
	// — it cannot know the change is only stats, so it must not say so.
	assert.Equal(t, RecordDiffPolicyRelevant, classifyRecordDiff(nil, basePostRecord()),
		"with no prior version there is nothing to prove the change is stats-only")

	assert.Equal(t, RecordDiffPolicyRelevant, classifyRecordDiff(basePostRecord(), nil),
		"a record that decoded to nothing is not a stats refresh")
}

func TestClassifyRecordDiff_IsSymmetricAboutBridgedStats(t *testing.T) {
	t.Parallel()

	// Direction must not matter. Records arrive out of order from overlapping
	// feeds, and a classifier that answered differently depending on which copy
	// it called "old" would repin in one delivery order and re-decide in the
	// other.
	withStats := basePostRecord()
	withoutStats := withField("bridgedStats", nil)

	assert.Equal(t, RecordDiffBridgedStatsOnly, classifyRecordDiff(withStats, withoutStats))
	assert.Equal(t, RecordDiffBridgedStatsOnly, classifyRecordDiff(withoutStats, withStats))

	edited := withField("title", "edited")
	assert.Equal(t, RecordDiffPolicyRelevant, classifyRecordDiff(withStats, edited))
	assert.Equal(t, RecordDiffPolicyRelevant, classifyRecordDiff(edited, withStats))
}
