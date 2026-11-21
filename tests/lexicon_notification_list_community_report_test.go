package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

type communityReportListService struct {
	output notifications.ListNotificationsOutput
}

func (s communityReportListService) ListNotifications(context.Context, string, string, int) (notifications.ListNotificationsOutput, error) {
	return s.output, nil
}

func TestNotificationListLexicon_ServedCommunityReportValidates(t *testing.T) {
	catalog, recordID, _ := placeholderListLexicon(t)
	service := communityReportListService{output: notifications.ListNotificationsOutput{Notifications: []notifications.NotificationView{{
		Reason:      notifications.ReasonCommunityReport,
		SortAt:      "2026-11-16T09:00:00Z",
		Community:   &posts.CommunityRef{DID: "did:plc:reportedcommunity", Handle: "reported.coves.social", Name: "Reported"},
		ReportCount: 5,
	}}}}
	request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.notification.listNotifications", nil)
	request = request.WithContext(middleware.SetTestUserDID(request.Context(), "did:plc:reportmoderator"))
	recorder := httptest.NewRecorder()
	notification.NewListHandler(service).HandleListNotifications(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, "response: %s", recorder.Body.String())

	var body struct {
		Notifications []map[string]json.RawMessage `json:"notifications"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body.Notifications, 1)
	served := body.Notifications[0]
	require.JSONEq(t, `"communityReport"`, string(served["reason"]))
	require.JSONEq(t, `5`, string(served["reportCount"]))
	var community struct {
		DID string `json:"did"`
	}
	require.NoError(t, json.Unmarshal(served["community"], &community))
	require.Equal(t, "did:plc:reportedcommunity", community.DID)
	require.NotContains(t, served, "author", "a communityReport must not carry an author")

	data, err := atdata.UnmarshalJSON(recorder.Body.Bytes())
	require.NoError(t, err)
	data["$type"] = recordID
	require.NoError(t, lexicon.ValidateRecord(catalog, data, recordID, 0), "the served communityReport must validate against listNotifications")

	// The count is a positive integer: a group with no open reports is not served.
	invalid, err := atdata.UnmarshalJSON([]byte(`{"notifications":[{"reason":"communityReport","sortAt":"2026-11-16T09:00:00Z","isRead":false,"community":{"did":"did:plc:reportedcommunity","handle":"reported.coves.social","name":"Reported"},"reportCount":0}]}`))
	require.NoError(t, err)
	invalid["$type"] = recordID
	require.Error(t, lexicon.ValidateRecord(catalog, invalid, recordID, 0), "reportCount 0 must be rejected by the notification lexicon")
}
