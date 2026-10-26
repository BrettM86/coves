//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestUserProfileBlobReferences_UpdateRollsBackWhenReferenceInsertFails(t *testing.T) {
	db := testkit.DB(t)
	repo := NewUserRepository(db)
	did := "did:plc:" + testkit.UniqueID(t)
	createTestUser(t, db, testkit.UniqueID(t)+".test", did)
	subject := "at://" + did + "/social.coves.actor.profile/self"
	const (
		avatarA = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
		bannerB = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
		avatarC = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
	)
	_, err := db.Exec(`UPDATE users SET avatar_cid = $2, banner_cid = $3 WHERE did = $1`, did, avatarA, bannerB)
	require.NoError(t, err)
	for _, reference := range []struct{ cid, kind string }{{avatarA, "avatar"}, {bannerB, "banner"}} {
		_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind)
			VALUES ($1, $2, $3, $4)`, did, reference.cid, subject, reference.kind)
		require.NoError(t, err)
	}

	var userBefore, referencesBefore string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(u)::text FROM users u WHERE did = $1`, did).Scan(&userBefore))
	require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.owner_did, r.blob_cid, r.kind), '[]'::jsonb)::text
		FROM blob_references r WHERE subject_uri = $1`, subject).Scan(&referencesBefore))
	require.NotEqual(t, "[]", referencesBefore)
	_, err = db.Exec(`CREATE FUNCTION reject_profile_avatar_c() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'reject avatar C'; END; $$`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reject_profile_avatar_c BEFORE INSERT ON blob_references
		FOR EACH ROW WHEN (NEW.blob_cid = 'bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q')
		EXECUTE FUNCTION reject_profile_avatar_c()`)
	require.NoError(t, err)

	newName := "Changed name"
	newAvatar := avatarC
	_, err = repo.UpdateProfile(context.Background(), did, users.UpdateProfileInput{
		AvatarCID: &newAvatar, DisplayName: &newName,
	})
	require.Error(t, err)
	var userAfter, referencesAfter string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(u)::text FROM users u WHERE did = $1`, did).Scan(&userAfter))
	require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.owner_did, r.blob_cid, r.kind), '[]'::jsonb)::text
		FROM blob_references r WHERE subject_uri = $1`, subject).Scan(&referencesAfter))
	require.Equal(t, userBefore, userAfter)
	require.Equal(t, referencesBefore, referencesAfter)
}

func TestUserProfileBlobReferences_DisplayNameOnlyKeepsStoredImageReferences(t *testing.T) {
	db := testkit.DB(t)
	repo := NewUserRepository(db)
	did := "did:plc:" + testkit.UniqueID(t)
	createTestUser(t, db, testkit.UniqueID(t)+".test", did)
	subject := "at://" + did + "/social.coves.actor.profile/self"
	const (
		avatar = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
		banner = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	)
	avatarCID, bannerCID := avatar, banner
	_, err := repo.UpdateProfile(context.Background(), did, users.UpdateProfileInput{
		AvatarCID: &avatarCID, BannerCID: &bannerCID,
	})
	require.NoError(t, err)
	name := "New display name"
	_, err = repo.UpdateProfile(context.Background(), did, users.UpdateProfileInput{DisplayName: &name})
	require.NoError(t, err)
	var storedAvatar, storedBanner sql.NullString
	require.NoError(t, db.QueryRow(`SELECT avatar_cid, banner_cid FROM users WHERE did = $1`, did).Scan(&storedAvatar, &storedBanner))
	require.Equal(t, sql.NullString{String: avatar, Valid: true}, storedAvatar)
	require.Equal(t, sql.NullString{String: banner, Valid: true}, storedBanner)
	require.Equal(t, []contentBackfillReference{
		{did, avatar, subject, "avatar", true},
		{did, banner, subject, "banner", true},
	}, contentBackfillRows(t, db, subject))
}

// Delete is paused at step 12 by a fixture transaction holding the account's
// notification_state row: by then it holds the exclusive erasure lock and has
// deleted the account's blob references (10a), but not yet the users row (13).
// A profile write arriving in that window must wait for the erasure and then
// find no user, rather than deadlock with it or leave an active reference
// owned by an erased DID.
func TestUserProfileBlobReferences_UpdateWaitsForAccountErasure(t *testing.T) {
	const (
		oldAvatar = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
		newAvatar = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
	)
	for _, testCase := range []struct {
		name           string
		existingAvatar bool
	}{
		{"replaces existing avatar", true},
		{"sets first avatar", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo := NewUserRepository(db)
			did := "did:plc:" + testkit.UniqueID(t)
			createTestUser(t, db, testkit.UniqueID(t)+".test", did)
			if testCase.existingAvatar {
				avatar := oldAvatar
				_, err := repo.UpdateProfile(ctx, did, users.UpdateProfileInput{AvatarCID: &avatar})
				require.NoError(t, err)
			}
			_, err := db.ExecContext(ctx, `INSERT INTO notification_state (did) VALUES ($1)`, did)
			require.NoError(t, err)

			gate, gateProcessID := notificationRaceTransaction(t, db, ctx)
			_, err = gate.ExecContext(ctx, `SELECT 1 FROM notification_state WHERE did = $1 FOR UPDATE`, did)
			require.NoError(t, err)

			deleteResult := make(chan error, 1)
			go func() { deleteResult <- repo.Delete(ctx, did) }()
			var deleteProcessID int
			testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
				err := db.QueryRowContext(ctx, `
					SELECT pid FROM pg_stat_activity
					WHERE datname = current_database()
						AND pid <> pg_backend_pid()
						AND $1 = ANY(pg_blocking_pids(pid))
						AND query ILIKE '%DELETE FROM notification_state%'
					LIMIT 1`, gateProcessID).Scan(&deleteProcessID)
				if errors.Is(err, sql.ErrNoRows) {
					return false, nil
				}
				return err == nil, err
			}, testkit.WithDescription("Delete paused at its notification_state step"))

			updateResult := make(chan error, 1)
			go func() {
				avatar := newAvatar
				_, err := repo.UpdateProfile(ctx, did, users.UpdateProfileInput{AvatarCID: &avatar})
				updateResult <- err
			}()
			var updateErr error
			updateFinished := false
			testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
				select {
				case updateErr = <-updateResult:
					updateFinished = true
					return true, nil
				default:
				}
				// Probed on the gate's own connection: the clone pool's three
				// connections are held by the gate, Delete and UpdateProfile.
				var blocked bool
				err := gate.QueryRowContext(ctx, `
					SELECT EXISTS (SELECT 1 FROM pg_locks
						WHERE NOT granted AND $1 = ANY(pg_blocking_pids(pid)))`, deleteProcessID).Scan(&blocked)
				return blocked, err
			}, testkit.WithDescription("UpdateProfile blocked by the paused Delete or finished"))

			require.NoError(t, gate.Commit())
			deleteErr := <-deleteResult
			if !updateFinished {
				updateErr = <-updateResult
			}
			require.NoError(t, deleteErr, "account erasure must not fail because of a concurrent profile write")
			require.ErrorIs(t, updateErr, users.ErrUserNotFound, "a profile write that waited for erasure must find no user")
			var userCount, referenceCount int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE did = $1`, did).Scan(&userCount))
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob_references WHERE owner_did = $1`, did).Scan(&referenceCount))
			require.Zero(t, userCount)
			require.Zero(t, referenceCount, "no blob reference may outlive its erased owner")
		})
	}
}
