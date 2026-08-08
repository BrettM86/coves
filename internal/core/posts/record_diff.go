package posts

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
	return ""
}
