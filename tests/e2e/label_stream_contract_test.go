//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"testing"
	"time"

	"Coves/tests/testkit"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/labeling"
	"github.com/stretchr/testify/require"
)

// labelStreamFrameWait bounds each wait for the next stream frame. The stack is
// shared, so a cursor-0 replay may carry labels other contracts produced first.
const labelStreamFrameWait = 60 * time.Second

// TestLabelStreamRemovalContract follows an instance removal and its
// restoration out to an independent subscriber of the public
// com.atproto.label.subscribeLabels stream. The subscriber knows only the
// instance DID: it resolves the DID document through the stack's configured
// resolver, subscribes from cursor 0 at the advertised #atproto_labeler
// endpoint before the removal, and must then receive the post's !takedown
// label followed by its negation, in increasing seq order, each signed by the
// advertised #atproto_label key.
func TestLabelStreamRemovalContract(t *testing.T) {
	labelKey, endpoint := resolveInstanceLabeler(t)

	p := newPipeline(t)
	author := p.IndexedAccount(t, "lsr")
	community := indexedCommunity(t, p, "lsr", author.DID)
	admin := testkit.ModerationAdmin(t, 1)
	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	createdCID := author.PutRecord(t, postV2Collection, rkey, postV2Record(community.DID,
		"label stream removal "+testkit.UniqueID(t), "label stream content "+testkit.UniqueID(t))).CID
	awaitStatus(t, p, uri, community.DID, "pending", "the author's post to be indexed")

	readState := func() moderationSubjectStateResponse {
		t.Helper()
		var state moderationSubjectStateResponse
		err := p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)).Query(
			context.Background(), subjectStateMethod, url.Values{"subject": {uri}}, &state)
		require.NoError(t, err)
		return state
	}
	state := readState()
	require.Equal(t, createdCID, state.State.CurrentSubject.CID)
	require.Equal(t, "clear", state.State.Moderation.State)

	dialContext, cancelDial := context.WithTimeout(t.Context(), labelStreamFrameWait)
	defer cancelDial()
	cursor := int64(0)
	stream, err := testkit.DialLabelStream(t, dialContext, endpoint, &cursor)
	require.NoError(t, err, "subscribing to subscribeLabels at the advertised labeler endpoint %s", endpoint)

	lastSeq := int64(0)
	// nextLabelForPost reads frames until one carries a label for uri, skipping
	// frames for other subjects. Every frame's seq must exceed the previous one.
	nextLabelForPost := func(description string) (int64, comatproto.LabelDefs_Label) {
		t.Helper()
		for {
			waitContext, cancel := context.WithTimeout(t.Context(), labelStreamFrameWait)
			frame, err := stream.Next(waitContext)
			cancel()
			require.NoError(t, err, "waiting for %s", description)
			require.Equal(t, int64(1), frame.Header.Op, "every frame on a healthy stream is op 1")
			require.Equal(t, "#labels", frame.Header.T)
			require.Greater(t, frame.Seq, lastSeq, "frame seqs must strictly increase")
			lastSeq = frame.Seq
			var matched []comatproto.LabelDefs_Label
			for _, label := range frame.Labels {
				if label.Uri == uri {
					matched = append(matched, label)
				}
			}
			if len(matched) == 0 {
				continue
			}
			require.Len(t, matched, 1, "one label per frame for the post")
			return frame.Seq, matched[0]
		}
	}

	var removal struct {
		Outcome string `json:"outcome"`
		Action  struct {
			Action struct {
				Ref struct {
					ActionID string `json:"actionId"`
				} `json:"ref"`
			} `json:"action"`
		} `json:"action"`
	}
	err = p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, removeContentMethod)).Procedure(
		t.Context(), removeContentMethod, map[string]any{
			"subject":         map[string]any{"uri": uri, "cid": state.State.CurrentSubject.CID},
			"expectedVersion": state.State.Version,
			"idempotencyKey":  testkit.UniqueID(t),
			"reason":          "social.coves.moderation.defs#reasonSpam",
		}, &removal)
	require.NoError(t, err)
	require.Equal(t, "applied", removal.Outcome)
	require.NotEmpty(t, removal.Action.Action.Ref.ActionID)

	takedownSeq, takedown := nextLabelForPost("the removed post's !takedown label")
	require.Equal(t, "!takedown", takedown.Val)
	require.Equal(t, communityInstanceDID, takedown.Src)
	require.Nil(t, takedown.Cid, "a removal label is URI-scoped")
	require.False(t, takedown.Neg != nil && *takedown.Neg, "a removal label is not a negation")
	verifiableTakedown := labeling.FromLexicon(&takedown)
	require.NoError(t, verifiableTakedown.VerifySignature(labelKey),
		"the streamed !takedown label must verify with the advertised #atproto_label key")

	removedState := readState()
	require.Equal(t, "removed", removedState.State.Moderation.State)
	var restoration struct {
		Outcome string `json:"outcome"`
	}
	err = p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, restoreContentMethod)).Procedure(
		t.Context(), restoreContentMethod, map[string]any{
			"actionId":        removal.Action.Action.Ref.ActionID,
			"reviewedSubject": map[string]any{"uri": uri, "cid": createdCID},
			"expectedVersion": removedState.State.Version,
			"idempotencyKey":  testkit.UniqueID(t),
			"reason":          "social.coves.moderation.defs#reasonModeratorDiscretion",
		}, &restoration)
	require.NoError(t, err)
	require.Equal(t, "applied", restoration.Outcome)

	negationSeq, negation := nextLabelForPost("the restored post's !takedown negation")
	require.Greater(t, negationSeq, takedownSeq)
	require.Equal(t, "!takedown", negation.Val)
	require.Equal(t, communityInstanceDID, negation.Src)
	require.Nil(t, negation.Cid, "a restoration negation is URI-scoped")
	require.NotNil(t, negation.Neg, "a restoration label must be a negation")
	require.True(t, *negation.Neg, "a restoration label must be a negation")
	verifiableNegation := labeling.FromLexicon(&negation)
	require.NoError(t, verifiableNegation.VerifySignature(labelKey),
		"the streamed negation must verify with the advertised #atproto_label key")
}
