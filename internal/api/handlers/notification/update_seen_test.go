//go:build integration

package notification_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func updateSeenRequest(did, body string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.notification.updateSeen", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), did))
	return httptest.NewRecorder(), req
}

func updateSeenHandler(db *sql.DB) *notification.Handler {
	return notification.NewHandler(notifications.NewService(postgres.NewNotificationRepository(db).(notifications.ReadRepository)))
}

func requireUpdateSeenError(t *testing.T, rec *httptest.ResponseRecorder, status int, name string) {
	t.Helper()
	require.Equal(t, status, rec.Code, "updateSeen response: %s", rec.Body.String())
	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, name, response["error"])
}

func requireNoNotificationState(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	var rows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM notification_state WHERE did = $1`, did).Scan(&rows))
	require.Zero(t, rows, "updateSeen must not create notification state for %s", did)
}

func TestUpdateSeen_StoresCallersSeenAt(t *testing.T) {
	db := testkit.DB(t)
	caller, other := preferenceHandlerUsers(t, db)
	handler := updateSeenHandler(db)
	rec, req := updateSeenRequest(caller, `{"seenAt":"2026-09-30T14:34:56.123456+02:00"}`)
	handler.HandleUpdateSeen(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "updateSeen response: %s", rec.Body.String())
	var seenAt time.Time
	require.NoError(t, db.QueryRow(`SELECT seen_at FROM notification_state WHERE did = $1`, caller).Scan(&seenAt))
	require.True(t, seenAt.Equal(time.Date(2026, 9, 30, 12, 34, 56, 123456000, time.UTC)), "stored seen_at: %s", seenAt)
	requireNoNotificationState(t, db, other)
}

// Postgres accepts zone offsets only up to ±15:59; datetime syntax allows up
// to ±23:59, so the instant must reach the database independent of its zone.
func TestUpdateSeen_StoresSeenAtWithOffsetBeyondPostgresZoneRange(t *testing.T) {
	db := testkit.DB(t)
	caller, _ := preferenceHandlerUsers(t, db)
	handler := updateSeenHandler(db)
	rec, req := updateSeenRequest(caller, `{"seenAt":"2026-09-30T12:00:00+16:00"}`)
	handler.HandleUpdateSeen(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "updateSeen response: %s", rec.Body.String())
	var seenAt time.Time
	require.NoError(t, db.QueryRow(`SELECT seen_at FROM notification_state WHERE did = $1`, caller).Scan(&seenAt))
	require.True(t, seenAt.Equal(time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)), "stored seen_at: %s", seenAt)
}

func TestUpdateSeen_RejectsMissingOrInvalidSeenAt(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing", `{}`},
		{"null", `{"seenAt":null}`},
		{"unparseable", `{"seenAt":"yesterday"}`},
		{"without zone", `{"seenAt":"2026-09-30T12:00:00"}`},
		{"negative zero offset", `{"seenAt":"2026-09-30T12:00:00-00:00"}`},
		{"non-string", `{"seenAt":1727697600}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			caller, _ := preferenceHandlerUsers(t, db)
			handler := updateSeenHandler(db)
			rec, req := updateSeenRequest(caller, tc.body)
			handler.HandleUpdateSeen(rec, req)

			requireUpdateSeenError(t, rec, http.StatusBadRequest, "InvalidRequest")
			requireNoNotificationState(t, db, caller)
		})
	}
}

func TestUpdateSeen_UnindexedCallerGetsAccountNotIndexed(t *testing.T) {
	db := testkit.DB(t)
	caller := "did:plc:seenunindexed" + testkit.UniqueID(t)
	handler := updateSeenHandler(db)
	rec, req := updateSeenRequest(caller, `{"seenAt":"2026-09-30T12:00:00Z"}`)
	handler.HandleUpdateSeen(rec, req)

	requireUpdateSeenError(t, rec, http.StatusBadRequest, "AccountNotIndexed")
	requireNoNotificationState(t, db, caller)
}

func TestUpdateSeen_DatabaseFailureIsInternalError(t *testing.T) {
	db := testkit.DB(t)
	caller, _ := preferenceHandlerUsers(t, db)
	var name string
	require.NoError(t, db.QueryRow(`SELECT current_database()`).Scan(&name))
	closedDB, err := sql.Open("postgres", testkit.Endpoints().Postgres.URL(name))
	require.NoError(t, err)
	require.NoError(t, closedDB.Close())
	handler := updateSeenHandler(closedDB)
	rec, req := updateSeenRequest(caller, `{"seenAt":"2026-09-30T12:00:00Z"}`)
	handler.HandleUpdateSeen(rec, req)

	requireUpdateSeenError(t, rec, http.StatusInternalServerError, "InternalServerError")
}
