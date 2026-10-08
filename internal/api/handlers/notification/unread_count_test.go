//go:build integration

package notification_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	os.Exit(testkit.Main(m, testkit.RequirePostgres))
}

// Seed the reader's two recipients with the same accepted root; only the
// authenticated caller's three notifications may contribute to the response.
func seedUnreadHandlerFixture(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	ctx := context.Background()
	id := testkit.UniqueID(t)
	caller, other := "did:plc:unreadcaller"+id, "did:plc:unreadother"+id
	for _, user := range []struct{ did, handle string }{
		{caller, "unreadcaller" + id + ".test"},
		{other, "unreadother" + id + ".test"},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`,
			user.did, user.handle, "https://pds.test")
		require.NoError(t, err)
	}
	base := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	for _, did := range []string{caller, other} {
		_, err := db.ExecContext(ctx, `INSERT INTO notification_state (did, seen_at) VALUES ($1, $2)`, did, base.Add(-time.Second))
		require.NoError(t, err)
	}
	community := "did:plc:unreadcommunity" + id
	_, err := db.ExecContext(ctx, `INSERT INTO communities
		(did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		VALUES ($1, $2, $3, $4, $4, $4, $5)`, community, "unread"+id+".coves.social", "unread", caller, base)
	require.NoError(t, err)
	root := "at://" + caller + "/social.coves.community.postv2/" + id
	rootCID := "bafyunreadpost" + id
	_, err = db.ExecContext(ctx, `INSERT INTO posts
		(uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, 'unread root', $6, 1, 1, 0)`, root, rootCID, id, caller, community, base)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO community_post_admissions
		(community_did, post_uri, status, acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid,
		 last_community_rev, last_community_op_rank, created_at, updated_at)
		VALUES ($1, $2, 'accepted', $3, $4, $5, $5, '3lqqqqqqqqqq2', $7, $6, $6)`,
		community, root, "at://"+community+"/social.coves.community.acceptance/"+id, id, rootCID, base, int16(posts.CommunityOpPut))
	require.NoError(t, err)

	seedComment := func(rkey, parent string, at time.Time) string {
		t.Helper()
		uri := "at://" + other + "/social.coves.community.comment/" + rkey
		_, err := db.ExecContext(ctx, `INSERT INTO comments
			(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $6, 'reply', $8)`,
			uri, "bafyunreadcomment"+rkey, rkey, other, root, rootCID, parent, at)
		require.NoError(t, err)
		return uri
	}
	subject := seedComment(id+"subject", root, base)
	for _, recipient := range []struct {
		did   string
		count int
	}{
		{caller, 3}, {other, 5},
	} {
		for i := 0; i < recipient.count; i++ {
			rkey := testkit.UniqueID(t)
			at := base.Add(time.Duration(i+1) * time.Second)
			reason, target := "postReply", root
			if i%2 == 0 {
				reason, target = "commentReply", subject
			}
			record := seedComment(rkey, target, at)
			_, err := db.ExecContext(ctx, `INSERT INTO notifications
				(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
				recipient.did, reason, record, "bafyunreadcomment"+rkey, other, target, root, at)
			require.NoError(t, err)
		}
	}
	return caller, other
}

func TestGetUnreadCount_UsesAuthenticatedRecipient(t *testing.T) {
	db := testkit.DB(t)
	caller, other := seedUnreadHandlerFixture(t, db)
	repo := postgres.NewNotificationRepository(db).(notifications.ReadRepository)
	handler := notification.NewHandler(notifications.NewService(repo))
	req := httptest.NewRequest(http.MethodGet,
		"/xrpc/social.coves.notification.getUnreadCount?recipient="+other+"&did="+other, nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), caller))
	rec := httptest.NewRecorder()
	handler.HandleGetUnreadCount(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "getUnreadCount response: %s", rec.Body.String())
	require.JSONEq(t, `{"count":3}`, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}
