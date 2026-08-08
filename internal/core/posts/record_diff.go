package posts

import "reflect"

// bridgedStatsField is the ONE field §5.5 lets through unexamined. It is named
// once, here, so that widening the exception is a visible edit to this file
// rather than a condition that grew a second disjunct.
const bridgedStatsField = "bridgedStats"

// RecordDiffClass says what kind of change an author made to a post record —
// the classification the bridgedStats exception of §5.5 turns on.
type RecordDiffClass string

const (
	// RecordDiffNone means the two records are the same content.
	RecordDiffNone RecordDiffClass = "none"

	// RecordDiffBridgedStatsOnly means the ONLY field that differs is
	// bridgedStats: a bridge refreshing origin-platform vote counts. Such an
	// edit is repinned in place — the acceptance moves onto the new CID with no
	// status transition, no feed removal and no re-decision.
	RecordDiffBridgedStatsOnly RecordDiffClass = "bridged-stats-only"

	// RecordDiffPolicyRelevant means something a moderator would judge changed,
	// so the post needs full re-admission.
	//
	// It is also the answer for any difference this function does not
	// recognise. See classifyRecordDiff.
	RecordDiffPolicyRelevant RecordDiffClass = "policy-relevant"
)

// classifyRecordDiff reports whether the change between two versions of a post
// record is the bridgedStats refresh of §5.5 or an edit needing re-admission.
//
// IT CLASSIFIES THE DIFF ONLY. §5.5 conditions the repin on the bridge-trust
// gate as well — "the record diff touches only bridgedStats AND the author
// passes the bridge-trust gate" — and the gate is the CALLER's to apply. A
// RecordDiffBridgedStatsOnly answer is necessary for a repin, never sufficient.
//
// NOT CALLED BY ANYTHING YET. It ships with the engine so that the repin path
// has a decision procedure to call when it lands (task 5's consumer wiring,
// alongside EngineRepinned and CommunityRecordWriter.RepinAcceptance), and it
// is specified now so that path cannot be written against a guess.
//
// IT TAKES DECODED RECORDS, NOT PostRecord VALUES, AND THAT IS THE WHOLE POINT.
// A typed struct silently drops every field it does not know about, so an
// author who added an unmodelled field — or a bridge running a newer lexicon
// than this build — would produce two structs that compare equal and a diff
// classified as "nothing changed". The classification would then wave through
// an edit nobody looked at. The raw maps keep unknown fields visible.
//
// IT FAILS CLOSED. Only one specific shape of difference earns the exception:
// bridgedStats differs and NOTHING else does. Every other answer — a known
// policy field changed, a field this function has never heard of changed, a
// field appeared or vanished — is RecordDiffPolicyRelevant. The cost of
// getting that wrong in the safe direction is one unnecessary re-admission of a
// bridge post; in the unsafe direction it is edited content rendering under an
// acceptance granted to different content.
func classifyRecordDiff(oldRecord, newRecord map[string]any) RecordDiffClass {
	// A version that is not there proves nothing about the version that is. The
	// engine reaches this with whatever it managed to decode, and "I could not
	// read the old record" must not read as "the change was only stats".
	if oldRecord == nil || newRecord == nil {
		return RecordDiffPolicyRelevant
	}

	// EVERYTHING EXCEPT bridgedStats, COMPARED WHOLE. Comparing the remainder as
	// one value rather than field by field is what makes the exception fail
	// closed against fields this build has never heard of: an unknown key is
	// part of the remainder, so it is compared like any other, and a bridge
	// running a newer lexicon cannot smuggle one past.
	oldRest := withoutBridgedStats(oldRecord)
	newRest := withoutBridgedStats(newRecord)
	if !reflect.DeepEqual(oldRest, newRest) {
		return RecordDiffPolicyRelevant
	}

	// The remainder is identical, so bridgedStats is the only thing left that
	// can differ. Presence is part of the comparison: a bridge that started or
	// stopped reporting counts changed its stats, not its content.
	oldStats, oldHasStats := oldRecord[bridgedStatsField]
	newStats, newHasStats := newRecord[bridgedStatsField]
	if oldHasStats == newHasStats && reflect.DeepEqual(oldStats, newStats) {
		return RecordDiffNone
	}
	return RecordDiffBridgedStatsOnly
}

// withoutBridgedStats copies a record with the excepted field removed.
//
// The copy is not an optimisation to skip: the caller owns these maps — they
// are decoded event payloads that the engine goes on to index — and a
// classifier that deleted a field from them would silently strip bridged vote
// counts out of everything downstream of the decision.
func withoutBridgedStats(record map[string]any) map[string]any {
	rest := make(map[string]any, len(record))
	for field, value := range record {
		if field == bridgedStatsField {
			continue
		}
		rest[field] = value
	}
	return rest
}
