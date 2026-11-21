//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// TestCommunityReportNotificationContract is the production wiring of report
// notifications. The owner creates a hosted community and puts a moderator on
// its roster; an author's post in it is indexed; a separate account reports
// the post as spam through social.coves.admin.submitReport. The moderator's
// social.coves.notification.listNotifications then serves exactly one
// communityReport notification naming the community with reportCount 1, the
// response never names the reporter, and the reporter receives none.
func TestCommunityReportNotificationContract(t *testing.T) {
	p := newPipeline(t)
	owner := p.IndexedAccount(t, "rnco")
	moderator := p.IndexedAccount(t, "rncm")
	author := p.IndexedAccount(t, "rnca")
	reporter := p.IndexedAccount(t, "rncr")
	ownerClient := p.AppView.As(p.AppView.SignIn(t, owner))
	moderatorClient := p.AppView.As(p.AppView.SignIn(t, moderator))
	authorClient := p.AppView.As(p.AppView.SignIn(t, author))
	reporterClient := p.AppView.As(p.AppView.SignIn(t, reporter))
	ctx, cancel := context.WithTimeout(context.Background(), 6*contractBudget)
	defer cancel()

	var created struct {
		DID string `json:"did"`
	}
	require.NoError(t, ownerClient.Procedure(ctx, "social.coves.community.create", map[string]any{
		"name": testkit.UniqueIDWithPrefix(t, "rnc"), "description": "a community whose moderators are told about reports",
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
	}, nil), "the reporter reports the post as spam")

	awaitNotificationContractRow(t, p, moderatorClient, "the moderator's communityReport notification",
		func(row notificationContractRow) bool { return row.Reason == "communityReport" })

	var raw json.RawMessage
	require.NoError(t, moderatorClient.Query(ctx, "social.coves.notification.listNotifications",
		url.Values{"limit": {"100"}}, &raw), "the moderator lists notifications")
	var page notificationContractPage
	require.NoError(t, json.Unmarshal(raw, &page))
	var reports []notificationContractRow
	for _, row := range page.Notifications {
		if row.Reason == "communityReport" {
			reports = append(reports, row)
		}
	}
	require.Len(t, reports, 1, "the moderator holds exactly one communityReport notification: %s", raw)
	require.Equal(t, communityDID, reports[0].Community.DID, "the notification names the community: %s", raw)
	require.Equal(t, 1, reports[0].ReportCount, "the notification counts the one open report: %s", raw)
	require.False(t, strings.Contains(string(raw), reporter.DID), "the notification never names the reporter: %s", raw)

	reporterPage, err := notificationContractList(reporterClient)
	require.NoError(t, err, "the reporter lists notifications")
	for _, row := range reporterPage.Notifications {
		require.NotEqual(t, "communityReport", row.Reason, "the reporter receives no communityReport notification")
	}
}
