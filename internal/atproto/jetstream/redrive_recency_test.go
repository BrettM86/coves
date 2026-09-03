//go:build integration

package jetstream

import (
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests prove the stale-redrive recency guard: the DeadLetterRedriver
// replays failed events up to ~50 minutes later, so an OLD update (v1) that
// failed transiently can be re-delivered AFTER a NEWER update (v2) was already
// indexed. Without the guard, replaying v1 would silently revert content to the
// stale version. The guard compares the event's Jetstream time_us against the
// row's indexed_at watermark (advanced to the event time on every applied
// write) and skips — successfully, without error — any event that is not
// strictly newer.

const (
	recencyTestPrefix    = "did:plc:jsrcy"
	recencyTestCommunity = recencyTestPrefix + "community"
	recencyTestAuthor    = recencyTestPrefix + "author"
	recencyTestCommenter = recencyTestPrefix + "commenter"
)

// recencyCommentEvent builds a comment commit event with an explicit Jetstream time_us.
func recencyCommentEvent(op, rkey, cid, content, postURI, postCID string, timeUS int64) *JetstreamEvent {
	var record map[string]interface{}
	if op != "delete" {
		record = map[string]interface{}{
			"$type":   CommentCollection,
			"content": content,
			"reply": map[string]interface{}{
				"root":   map[string]interface{}{"uri": postURI, "cid": postCID},
				"parent": map[string]interface{}{"uri": postURI, "cid": postCID},
			},
			"createdAt": "2026-03-01T00:01:00Z",
		}
	}
	return &JetstreamEvent{
		Kind:   "commit",
		Did:    recencyTestCommenter,
		TimeUS: timeUS,
		Commit: &CommitEvent{
			Operation:  op,
			Collection: CommentCollection,
			RKey:       rkey,
			CID:        cid,
			Record:     record,
		},
	}
}

func setupRecencyFixtures(t *testing.T, db *sql.DB) *PostEventConsumer {
	t.Helper()
	insertBridgedUser(t, db, recencyTestAuthor, "rcyauthor.test")
	insertBridgedUser(t, db, recencyTestCommenter, "rcycommenter.test")
	insertBridgedCommunity(t, db, recencyTestCommunity, "rcycommunity.test", recencyTestAuthor)

	us := newMockUserService()
	us.users[recencyTestAuthor] = &users.User{DID: recencyTestAuthor, Handle: "rcyauthor.test"}
	return NewPostEventConsumer(
		postgres.NewPostRepository(db), postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), us, db,
		WithAdmissions(postgres.NewAdmissionRepository(db)),
	)
}

func TestCommentConsumer_StaleRedrivenUpdate_CannotRevertNewerContent(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)

	pc := setupRecencyFixtures(t, db)
	cc := NewCommentEventConsumer(postgres.NewCommentRepository(db), db)
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC).UnixMicro()
	postURI := pv2URI(recencyTestAuthor, "rcyc1")
	postCID := "bafrcyc1"
	require.NoError(t, pc.HandleEvent(ctx, pv2Event(
		recencyTestAuthor, "create", "rcyc1", "", postCID, base,
		pv2Record(recencyTestCommunity, "thread", "body"),
	)))

	uri := "at://" + recencyTestCommenter + "/social.coves.community.comment/rcycc1"
	t0, t1, t2 := base+1_000_000, base+2_000_000, base+3_000_000

	require.NoError(t, cc.HandleEvent(ctx, recencyCommentEvent("create", "rcycc1", "bafcc0", "v0 comment", postURI, postCID, t0)))

	// v2 (the newer edit) is indexed first — v1 failed transiently and went to the DLQ.
	require.NoError(t, cc.HandleEvent(ctx, recencyCommentEvent("update", "rcycc1", "bafcc2", "v2 comment", postURI, postCID, t2)))

	readComment := func() (content string) {
		require.NoError(t, db.QueryRow(`SELECT content FROM comments WHERE uri=$1`, uri).Scan(&content))
		return
	}
	require.Equal(t, "v2 comment", readComment())

	// The DeadLetterRedriver replays the stale v1: skipped without error, no revert.
	err := cc.HandleEvent(ctx, recencyCommentEvent("update", "rcycc1", "bafcc1", "v1 comment", postURI, postCID, t1))
	require.NoError(t, err, "stale redriven comment update must be skipped as success")
	assert.Equal(t, "v2 comment", readComment(), "stale redriven update must not revert comment content")

	// A rewind duplicate of v2 itself (equal time_us) is skipped idempotently.
	require.NoError(t, cc.HandleEvent(ctx, recencyCommentEvent("update", "rcycc1", "bafcc2", "v2 comment", postURI, postCID, t2)))
	assert.Equal(t, "v2 comment", readComment())

	// A genuinely newer update still applies.
	require.NoError(t, cc.HandleEvent(ctx, recencyCommentEvent("update", "rcycc1", "bafcc3", "v3 comment", postURI, postCID, t2+1_000_000)))
	assert.Equal(t, "v3 comment", readComment(), "newer update must still apply")

	// Backward compatibility: TimeUS == 0 applies unconditionally.
	require.NoError(t, cc.HandleEvent(ctx, recencyCommentEvent("update", "rcycc1", "bafcc4", "v4 comment", postURI, postCID, 0)))
	assert.Equal(t, "v4 comment", readComment(), "TimeUS=0 event applies unconditionally")
}

// TestUserConsumer_StaleRedrivenProfileUpdate_Skipped covers the check-then-skip
// guard on user profile events: an event older than the user row's last
// successful write (users.updated_at) is skipped as success.
func TestUserConsumer_StaleRedrivenProfileUpdate_Skipped(t *testing.T) {
	t.Parallel()
	mockService := newMockUserService()
	lastWrite := time.Now()
	mockService.users["did:plc:rcyprofile"] = &users.User{
		DID:         "did:plc:rcyprofile",
		Handle:      "rcyprofile.test",
		DisplayName: "Newer Name",
		UpdatedAt:   lastWrite,
	}
	consumer := NewUserEventConsumer(mockService, &mockIdentityResolverForUser{})
	ctx := context.Background()

	profileEvent := func(timeUS int64, displayName string) *JetstreamEvent {
		return &JetstreamEvent{
			Did:    "did:plc:rcyprofile",
			Kind:   "commit",
			TimeUS: timeUS,
			Commit: &CommitEvent{
				Operation:  "update",
				Collection: CovesProfileCollection,
				RKey:       "self",
				CID:        "bafrcyp",
				Record:     map[string]interface{}{"displayName": displayName},
			},
		}
	}

	// Redriven event from an hour BEFORE the row's last write: skipped, no error.
	err := consumer.HandleEvent(ctx, profileEvent(lastWrite.Add(-time.Hour).UnixMicro(), "Stale Name"))
	require.NoError(t, err, "stale redriven profile update must be skipped as success")
	assert.Empty(t, mockService.updatedCalls, "stale profile event must not reach UpdateProfile")
	assert.Equal(t, "Newer Name", mockService.users["did:plc:rcyprofile"].DisplayName)

	// An event NEWER than the last write applies normally.
	err = consumer.HandleEvent(ctx, profileEvent(lastWrite.Add(time.Hour).UnixMicro(), "Even Newer Name"))
	require.NoError(t, err)
	require.Len(t, mockService.updatedCalls, 1, "newer profile event must apply")
	assert.Equal(t, "Even Newer Name", mockService.users["did:plc:rcyprofile"].DisplayName)

	// Backward compatibility: TimeUS == 0 applies unconditionally.
	err = consumer.HandleEvent(ctx, profileEvent(0, "Untimed Name"))
	require.NoError(t, err)
	require.Len(t, mockService.updatedCalls, 2, "TimeUS=0 profile event must apply")
}
