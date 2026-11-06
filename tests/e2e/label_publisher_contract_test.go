//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/config"
	"Coves/tests/testkit"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/labeling"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/require"
)

// TestLabelPublisherRemovalContract follows an instance removal out to an
// independent label consumer. The consumer knows only the instance DID: it
// resolves the DID document through the stack's configured resolver, reads the
// #atproto_label key and the #atproto_labeler endpoint from it, queries
// com.atproto.label.queryLabels there, and verifies the !takedown label's
// signature with the advertised key.
func TestLabelPublisherRemovalContract(t *testing.T) {
	p := newPipeline(t)
	author := p.IndexedAccount(t, "lpr")
	community := indexedCommunity(t, p, "lpr", author.DID)
	admin := testkit.ModerationAdmin(t, 1)
	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	createdCID := author.PutRecord(t, postV2Collection, rkey, postV2Record(community.DID,
		"label publisher removal "+testkit.UniqueID(t), "label publisher content "+testkit.UniqueID(t))).CID
	awaitStatus(t, p, uri, community.DID, "pending", "the author's post to be indexed")

	var state moderationSubjectStateResponse
	err := p.AppView.As(admin.ServiceAuth(t, communityInstanceDID, subjectStateMethod)).Query(
		context.Background(), subjectStateMethod, url.Values{"subject": {uri}}, &state)
	require.NoError(t, err)
	require.Equal(t, createdCID, state.State.CurrentSubject.CID)
	require.Equal(t, "clear", state.State.Moderation.State)

	var removal struct {
		Outcome string `json:"outcome"`
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

	labelKey, endpoint := resolveInstanceLabeler(t)

	var takedowns []*comatproto.LabelDefs_Label
	for _, label := range queryLabels(t, endpoint, uri) {
		if label.Val == "!takedown" {
			takedowns = append(takedowns, label)
		}
	}
	require.Len(t, takedowns, 1, "exactly one !takedown label for the removed post")
	label := takedowns[0]
	require.Equal(t, communityInstanceDID, label.Src)
	require.Equal(t, uri, label.Uri)
	require.Nil(t, label.Cid, "a removal label is URI-scoped")
	require.Nil(t, label.Neg, "a removal label is not a negation")
	verifiable := labeling.FromLexicon(label)
	require.NoError(t, verifiable.VerifySignature(labelKey), "the label must verify with the advertised #atproto_label key")
}

// queryLabels asks the labeler at endpoint for the labels matching uriPattern,
// the way an independent label consumer would.
func queryLabels(t *testing.T, endpoint, uriPattern string) []*comatproto.LabelDefs_Label {
	t.Helper()
	queryURL := strings.TrimSuffix(endpoint, "/") + "/xrpc/com.atproto.label.queryLabels?" +
		url.Values{"uriPatterns": {uriPattern}}.Encode()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, queryURL, nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode, "queryLabels at the advertised labeler endpoint")
	var output comatproto.LabelQueryLabels_Output
	require.NoError(t, json.NewDecoder(response.Body).Decode(&output))
	return output.Labels
}

// resolveInstanceLabeler resolves the instance DID document the way an
// independent label consumer would, through the stack's configured resolver
// (PLC_DIRECTORY_URL and DID_WEB_HOSTS), and returns the advertised
// #atproto_label key and #atproto_labeler service endpoint.
func resolveInstanceLabeler(t *testing.T) (atcrypto.PublicKey, string) {
	t.Helper()
	plcURL := os.Getenv("PLC_DIRECTORY_URL")
	require.NotEmpty(t, plcURL, "PLC_DIRECTORY_URL must be set in the hermetic stack")
	rawDIDWebHosts := os.Getenv("DID_WEB_HOSTS")
	require.NotEmpty(t, rawDIDWebHosts, "DID_WEB_HOSTS must be set in the hermetic stack")

	didWebHosts, err := config.ParseDIDWebHosts(rawDIDWebHosts)
	require.NoError(t, err)
	directory := identity.NewIndigoDirectory(plcURL, true, didWebHosts)
	instance, err := directory.LookupDID(t.Context(), syntax.DID(communityInstanceDID))
	require.NoError(t, err, "resolving the instance DID document")
	labelKey, err := instance.GetPublicKey("atproto_label")
	require.NoError(t, err, "the instance DID document must advertise #atproto_label")
	endpoint := instance.GetServiceEndpoint("atproto_labeler")
	require.NotEmpty(t, endpoint, "the instance DID document must advertise #atproto_labeler")
	return labelKey, endpoint
}
