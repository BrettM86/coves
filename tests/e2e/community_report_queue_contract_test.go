//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// putOnRoster has the owner invite the account behind moderatorClient as a
// moderator of communityDID and the invitee accept, through the AppView.
func putOnRoster(t *testing.T, ctx context.Context, ownerClient, moderatorClient *testkit.AppView, communityDID, moderatorDID string) {
	t.Helper()
	var state struct {
		Version int64 `json:"version"`
	}
	require.NoError(t, ownerClient.Query(ctx, "social.coves.moderation.getCommunityState",
		url.Values{"subject": {communityDID}, "community": {communityDID}}, &state))
	var invited struct {
		Invitation struct {
			ID string `json:"id"`
		} `json:"invitation"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.inviteModerator", map[string]any{
		"community": communityDID, "subject": moderatorDID,
		"expectedVersion": state.Version, "idempotencyKey": testkit.UniqueID(t),
	}, &invited), "the owner invites the moderator")
	require.NotEmpty(t, invited.Invitation.ID)
	var inbox struct {
		Invitations []struct {
			Invitation struct {
				ID string `json:"id"`
			} `json:"invitation"`
			RosterVersion int64 `json:"rosterVersion"`
		} `json:"invitations"`
	}
	require.NoError(t, moderatorClient.Query(ctx, "social.coves.community.listViewerInvitations", url.Values{}, &inbox))
	require.Len(t, inbox.Invitations, 1, "the moderator's inbox holds the invitation")
	require.Equal(t, invited.Invitation.ID, inbox.Invitations[0].Invitation.ID)
	require.NoError(t, moderatorClient.Procedure(ctx, "social.coves.community.acceptInvitation", map[string]any{
		"invitation": invited.Invitation.ID, "expectedVersion": inbox.Invitations[0].RosterVersion,
		"idempotencyKey": testkit.UniqueID(t),
	}, nil), "the moderator accepts the invitation")
}

// TestCommunityReportQueueContract is the production wiring of the community
// report queue. The owner creates a hosted community and puts a moderator on
// its roster; an author's post in it is indexed; a second account reports the
// post as spam through social.coves.admin.submitReport. The moderator's
// social.coves.community.listReports then serves one entry for the post with
// the post's view, and the response never names the reporter. An account not
// on the roster is refused with 403 Forbidden.
func TestCommunityReportQueueContract(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "rqco")
	moderator := p.IndexedAccount(t, "rqcm")
	author := p.IndexedAccount(t, "rqca")
	reporter := p.IndexedAccount(t, "rqcr")
	outsider := p.IndexedAccount(t, "rqcx")
	ownerClient := p.AppView.As(p.AppView.SignIn(t, owner))
	moderatorClient := p.AppView.As(p.AppView.SignIn(t, moderator))
	authorClient := p.AppView.As(p.AppView.SignIn(t, author))
	reporterClient := p.AppView.As(p.AppView.SignIn(t, reporter))
	outsiderClient := p.AppView.As(p.AppView.SignIn(t, outsider))
	ctx, cancel := context.WithTimeout(context.Background(), 6*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "rqc"), "description": "a community whose moderators read reports",
	}, &created), "the owner creates a community through the AppView, which hosts it")
	require.NotEmpty(t, created.DID)
	communityDID := created.DID
	putOnRoster(t, ctx, ownerClient, moderatorClient, communityDID, moderator.DID)

	var post struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	require.NoError(t, authorClient.Procedure(ctx, "social.coves.community.post.create", map[string]any{
		"community": communityDID,
		"title":     "a post someone will report " + testkit.UniqueID(t),
		"content":   "a post in a hosted community",
	}, &post), "the author posts in the hosted community")
	require.NotEmpty(t, post.URI)
	awaitStatus(t, p, post.URI, communityDID, "accepted", "the hosted community to accept the author's post")
	p.Await(t, "the author's post to be indexed", func() (bool, error) {
		var response struct {
			Posts []map[string]any `json:"posts"`
		}
		if err := authorClient.Query(context.Background(), "social.coves.community.post.get",
			url.Values{"uris": {post.URI}}, &response); err != nil {
			return false, err
		}
		return len(response.Posts) == 1 && response.Posts[0]["uri"] == post.URI && response.Posts[0]["record"] != nil, nil
	})

	require.NoError(t, reporterClient.Procedure(ctx, "social.coves.admin.submitReport", map[string]any{
		"targetUri": post.URI, "reason": "spam",
	}, nil), "the second account reports the post as spam")

	var raw json.RawMessage
	require.NoError(t, moderatorClient.Query(ctx, "social.coves.community.listReports",
		url.Values{"community": {communityDID}}, &raw), "the moderator reads the community's report queue")
	var queue struct {
		Reports []struct {
			Subject     string         `json:"subject"`
			SubjectType string         `json:"subjectType"`
			Reasons     []string       `json:"reasons"`
			ReportCount int            `json:"reportCount"`
			Post        map[string]any `json:"post"`
		} `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(raw, &queue))
	require.Len(t, queue.Reports, 1, "the queue holds one entry: %s", raw)
	entry := queue.Reports[0]
	require.Equal(t, post.URI, entry.Subject, "the entry is the reported post")
	require.Equal(t, "post", entry.SubjectType)
	require.Equal(t, []string{"spam"}, entry.Reasons)
	require.Equal(t, 1, entry.ReportCount)
	require.NotNil(t, entry.Post, "the entry carries the post's view: %s", raw)
	require.Equal(t, post.URI, entry.Post["uri"], "the view is the reported post's")
	require.False(t, strings.Contains(string(raw), reporter.DID), "the queue never names the reporter: %s", raw)

	err := outsiderClient.Query(ctx, "social.coves.community.listReports", url.Values{"community": {communityDID}}, nil)
	_ = requireXRPCRefusal(t, err, http.StatusForbidden, "Forbidden", "listReports by an account not on the roster")
}
