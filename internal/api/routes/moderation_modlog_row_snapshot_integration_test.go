//go:build integration

package routes_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Acceptance: a restore row carries the snapshot of the removal it reverses in
// both logs, and an NSFW label row carries the labelled post's current text in
// the admin log.
func TestModerationModlogRestoreAndLabelRowsCarrySnapshots(t *testing.T) {
	f := newModlogFixture(t)
	commentURI, commentCID := f.comment(t)
	removalID, version := f.remove(t, commentURI, commentCID, f.tokenA, modlogSpam, "comment removal note")
	restoreID := f.restore(t, commentURI, commentCID, f.tokenA, removalID, version, modlogDiscretion, "restore note")
	labelID, _ := f.label(t, f.labelPost(t), modlogRuleViolation, "label note")
	removedComment := map[string]any{"text": "modlog comment", "capturedCid": commentCID}

	_, publicItems := modlogPage(t, f.list(t, false, "", nil), false)
	public := map[string]map[string]any{}
	for _, item := range publicItems {
		action := modlogAction(t, item, false)
		public[modlogID(t, action)] = action
	}
	require.Contains(t, public, restoreID)
	assert.Equal(t, removedComment, public[restoreID]["contentSnapshot"], "the public restore row shows the reversed removal's snapshot")

	for _, test := range []struct {
		name     string
		actionID string
		snapshot map[string]any
	}{
		{name: "restore row", actionID: restoreID, snapshot: removedComment},
		{
			name: "label row", actionID: labelID,
			snapshot: map[string]any{"title": "modlog labelled post", "text": "indexed body", "capturedCid": "bafytest"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, adminItems := modlogPage(t, f.list(t, true, f.tokenB, url.Values{"actionId": {test.actionID}}), true)
			require.Len(t, adminItems, 1)
			entry := moderationAcceptanceObject(t, adminItems[0])
			assert.Equal(t, test.snapshot, entry["contentSnapshot"])
		})
	}
}
