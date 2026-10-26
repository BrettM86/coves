//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/require"
)

const (
	communityAvatarA = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	communityBannerB = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	communityAvatarC = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
)

func communityProfileSubject(did string) string {
	return "at://" + did + "/social.coves.community.profile/self"
}

func seedCommunityProfileRow(t *testing.T, db *sql.DB, did string, avatar, banner any) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO communities
		(did, handle, name, owner_did, created_by_did, hosted_by_did, avatar_cid, banner_cid, created_at)
		VALUES ($1, $2, 'Community', $1, $1, $1, $3, $4, NOW())`, did, did+".test", avatar, banner)
	require.NoError(t, err)
}

func communityProfileSnapshot(t *testing.T, db *sql.DB, did string) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(c)::text FROM communities c WHERE did = $1`, did).Scan(&snapshot))
	return snapshot
}

func communityProfileReferencesSnapshot(t *testing.T, db *sql.DB, subject string) string {
	t.Helper()
	var snapshot string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.owner_did, r.blob_cid, r.kind), '[]'::jsonb)::text
		FROM blob_references r WHERE subject_uri = $1`, subject).Scan(&snapshot))
	return snapshot
}

func activeCommunityProfileRows(t *testing.T, db *sql.DB, subject string) []contentBackfillReference {
	t.Helper()
	active := []contentBackfillReference{}
	for _, row := range contentBackfillRows(t, db, subject) {
		if row.Active {
			active = append(active, row)
		}
	}
	return active
}

func TestCommunityProfileBlobReferences_WritesRollBackWhenReferencesFail(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		seed    bool
		trigger string
		write   func(context.Context, communities.Repository, string) error
	}{
		{"create", false, `CREATE TRIGGER reject_community_reference BEFORE INSERT ON blob_references
			FOR EACH ROW WHEN (NEW.blob_cid = 'bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q')
			EXECUTE FUNCTION reject_community_reference()`, func(ctx context.Context, repo communities.Repository, did string) error {
			_, err := repo.Create(ctx, &communities.Community{
				DID: did, Handle: did + ".test", Name: "Community", OwnerDID: did,
				CreatedByDID: did, HostedByDID: did, Visibility: "public",
				AvatarCID: communityAvatarC, BannerCID: communityBannerB,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			})
			return err
		}},
		{"update", true, `CREATE TRIGGER reject_community_reference BEFORE INSERT ON blob_references
			FOR EACH ROW WHEN (NEW.blob_cid = 'bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q')
			EXECUTE FUNCTION reject_community_reference()`, func(ctx context.Context, repo communities.Repository, did string) error {
			community, err := repo.GetByDID(ctx, did)
			if err != nil {
				return err
			}
			community.AvatarCID = communityAvatarC
			community.DisplayName = "Changed name"
			_, err = repo.Update(ctx, community)
			return err
		}},
		{"delete", true, `CREATE TRIGGER reject_community_reference BEFORE UPDATE ON blob_references
			FOR EACH ROW WHEN (NEW.subject_uri = '%s')
			EXECUTE FUNCTION reject_community_reference()`, func(ctx context.Context, repo communities.Repository, did string) error {
			return repo.Delete(ctx, did)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewCommunityRepository(db, credentialciphertest.Fixed())
			did := "did:plc:" + testkit.UniqueID(t)
			subject := communityProfileSubject(did)
			var communityBefore string
			if scenario.seed {
				seedCommunityProfileRow(t, db, did, communityAvatarA, communityBannerB)
				for _, reference := range []struct{ cid, kind string }{{communityAvatarA, "avatar"}, {communityBannerB, "banner"}} {
					_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
						VALUES ($1, $2, $3, $4, true)`, did, reference.cid, subject, reference.kind)
					require.NoError(t, err)
				}
				communityBefore = communityProfileSnapshot(t, db, did)
			}
			var post, postBefore, commentsBefore string
			contentReferencesBefore := map[string]string{}
			if scenario.name == "delete" {
				createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				author := "did:plc:" + testkit.UniqueID(t)
				commenter := "did:plc:" + testkit.UniqueID(t)
				replier := "did:plc:" + testkit.UniqueID(t)
				post = seedVisibilityPost(t, db, did, author, testkit.TID(), "rollback content", createdAt)
				comment := seedActorComment(t, db, commenter, post, testkit.TID(), createdAt)
				reply := seedActorComment(t, db, replier, post, testkit.TID(), createdAt)
				_, err := db.Exec(`UPDATE comments SET parent_uri = $2,
					parent_cid = (SELECT cid FROM comments WHERE uri = $2) WHERE uri = $1`, reply, comment)
				require.NoError(t, err)
				for _, reference := range []contentBackfillReference{
					{author, communityAvatarA, post, "post_image", true},
					{commenter, communityBannerB, comment, "comment_image", true},
					{replier, communityAvatarC, reply, "comment_image", true},
				} {
					_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
						VALUES ($1, $2, $3, $4, $5)`, reference.OwnerDID, reference.BlobCID,
						reference.SubjectURI, reference.Kind, reference.Active)
					require.NoError(t, err)
					contentReferencesBefore[reference.SubjectURI] = communityProfileReferencesSnapshot(t, db, reference.SubjectURI)
				}
				require.NoError(t, db.QueryRow(`SELECT to_jsonb(p)::text FROM posts p WHERE uri = $1`, post).Scan(&postBefore))
				require.NoError(t, db.QueryRow(`SELECT jsonb_agg(to_jsonb(c) ORDER BY uri)::text
					FROM comments c WHERE root_uri = $1`, post).Scan(&commentsBefore))
			}
			referencesBefore := communityProfileReferencesSnapshot(t, db, subject)
			if scenario.seed {
				require.NotEqual(t, "[]", referencesBefore)
			}
			_, err := db.Exec(`CREATE FUNCTION reject_community_reference() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'reject reference write'; END; $$`)
			require.NoError(t, err)
			trigger := scenario.trigger
			if scenario.name == "delete" {
				trigger = fmt.Sprintf(trigger, subject)
			}
			_, err = db.Exec(trigger)
			require.NoError(t, err)

			require.Error(t, scenario.write(context.Background(), repo, did))
			if scenario.seed {
				require.Equal(t, communityBefore, communityProfileSnapshot(t, db, did))
			} else {
				var count int
				require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM communities WHERE did = $1`, did).Scan(&count))
				require.Zero(t, count)
			}
			require.Equal(t, referencesBefore, communityProfileReferencesSnapshot(t, db, subject))
			if scenario.name == "delete" {
				var postAfter, commentsAfter string
				require.NoError(t, db.QueryRow(`SELECT to_jsonb(p)::text FROM posts p WHERE uri = $1`, post).Scan(&postAfter))
				require.Equal(t, postBefore, postAfter, "profile cleanup failure must restore the cascaded post")
				require.NoError(t, db.QueryRow(`SELECT jsonb_agg(to_jsonb(c) ORDER BY uri)::text
					FROM comments c WHERE root_uri = $1`, post).Scan(&commentsAfter))
				require.Equal(t, commentsBefore, commentsAfter, "profile cleanup failure must leave comments unchanged")
				for contentSubject, before := range contentReferencesBefore {
					require.Equal(t, before, communityProfileReferencesSnapshot(t, db, contentSubject),
						"profile cleanup failure must restore the complete content reference snapshot, including active and timestamps")
				}
			}
		})
	}
}

func TestCommunityProfileBlobReferences_StoresCanonicalCIDs(t *testing.T) {
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	did := "did:plc:" + testkit.UniqueID(t)
	subject := communityProfileSubject(did)
	alias := func(raw string) string {
		t.Helper()
		parsed, err := cid.Decode(raw)
		require.NoError(t, err)
		encoded, err := parsed.StringOfBase(multibase.Base58BTC)
		require.NoError(t, err)
		require.NotEqual(t, raw, encoded)
		return encoded
	}
	community := &communities.Community{
		DID: did, Handle: did + ".test", Name: "Community", OwnerDID: did,
		CreatedByDID: did, HostedByDID: did, Visibility: "public",
		AvatarCID: alias(communityAvatarA), BannerCID: "bafynotacid",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	_, err := repo.Create(context.Background(), community)
	require.NoError(t, err)
	assertStored := func(avatar, banner sql.NullString, rows []contentBackfillReference) {
		t.Helper()
		var storedAvatar, storedBanner sql.NullString
		require.NoError(t, db.QueryRow(`SELECT avatar_cid, banner_cid FROM communities WHERE did = $1`, did).Scan(&storedAvatar, &storedBanner))
		require.Equal(t, avatar, storedAvatar)
		require.Equal(t, banner, storedBanner)
		require.ElementsMatch(t, rows, activeCommunityProfileRows(t, db, subject))
	}
	assertStored(sql.NullString{String: communityAvatarA, Valid: true}, sql.NullString{},
		[]contentBackfillReference{{did, communityAvatarA, subject, "avatar", true}})

	community, err = repo.GetByDID(context.Background(), did)
	require.NoError(t, err)
	community.AvatarCID = alias(communityAvatarC)
	community.BannerCID = alias(communityBannerB)
	_, err = repo.Update(context.Background(), community)
	require.NoError(t, err)
	assertStored(sql.NullString{String: communityAvatarC, Valid: true}, sql.NullString{String: communityBannerB, Valid: true},
		[]contentBackfillReference{{did, communityAvatarC, subject, "avatar", true}, {did, communityBannerB, subject, "banner", true}})

	community, err = repo.GetByDID(context.Background(), did)
	require.NoError(t, err)
	community.AvatarCID = "bafynotacid"
	community.BannerCID = ""
	_, err = repo.Update(context.Background(), community)
	require.NoError(t, err)
	assertStored(sql.NullString{}, sql.NullString{}, []contentBackfillReference{})
}

func TestCommunityProfileBlobReferences_RebuildUnknownCommunityReturnsNotFound(t *testing.T) {
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	did := "did:plc:" + testkit.UniqueID(t)

	err := repo.RebuildProfileBlobReferences(context.Background(), did)
	require.True(t, errors.Is(err, communities.ErrCommunityNotFound), "want ErrCommunityNotFound, got %v", err)
	require.Equal(t, "[]", communityProfileReferencesSnapshot(t, db, communityProfileSubject(did)))
}

func TestCommunityProfileBlobReferences_RebuildWaitsForConcurrentProfileUpdate(t *testing.T) {
	ctx := context.Background()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	did := "did:plc:" + testkit.UniqueID(t)
	subject := communityProfileSubject(did)
	seedCommunityProfileRow(t, db, did, communityAvatarA, communityBannerB)
	for _, reference := range []struct{ cid, kind string }{{communityAvatarA, "avatar"}, {communityBannerB, "banner"}} {
		_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
			VALUES ($1, $2, $3, $4, true)`, did, reference.cid, subject, reference.kind)
		require.NoError(t, err)
	}

	holder, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close() })
	_, err = holder.ExecContext(ctx, `BEGIN`)
	require.NoError(t, err)
	holderOpen := true
	t.Cleanup(func() {
		if holderOpen {
			_, _ = holder.ExecContext(context.Background(), `ROLLBACK`)
		}
	})
	var holderPID int
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID))
	_, err = holder.ExecContext(ctx, `UPDATE communities SET avatar_cid = $2 WHERE did = $1`, did, communityAvatarC)
	require.NoError(t, err)

	rebuildResults := make(chan error, 1)
	rebuildReturned := false
	t.Cleanup(func() {
		if holderOpen {
			_, _ = holder.ExecContext(context.Background(), `ROLLBACK`)
			holderOpen = false
		}
		if !rebuildReturned {
			select {
			case err := <-rebuildResults:
				t.Logf("RebuildProfileBlobReferences returned during cleanup: %v", err)
			case <-time.After(5 * time.Second):
				t.Errorf("RebuildProfileBlobReferences did not return during cleanup")
			}
		}
	})
	go func() { rebuildResults <- repo.RebuildProfileBlobReferences(ctx, did) }()

	testkit.WaitFor(t, 3*time.Second, func() (bool, error) {
		select {
		case rebuildErr := <-rebuildResults:
			rebuildReturned = true
			return false, fmt.Errorf("rebuild returned before the holder committed (err=%v); it did not wait on the community row lock", rebuildErr)
		default:
		}
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity activity
			WHERE activity.datname = current_database() AND activity.pid <> $1
				AND $1 = ANY(pg_blocking_pids(activity.pid)))`, holderPID).Scan(&blocked)
		return blocked, err
	}, testkit.WithDescription("rebuild waits on the community row locked by an uncommitted profile update"))

	_, err = holder.ExecContext(ctx, `COMMIT`)
	holderOpen = false
	require.NoError(t, err)
	select {
	case err = <-rebuildResults:
		rebuildReturned = true
	case <-time.After(5 * time.Second):
		t.Fatal("RebuildProfileBlobReferences did not return after the holder committed")
	}
	require.NoError(t, err)

	var storedAvatar sql.NullString
	require.NoError(t, db.QueryRow(`SELECT avatar_cid FROM communities WHERE did = $1`, did).Scan(&storedAvatar))
	require.Equal(t, sql.NullString{String: communityAvatarC, Valid: true}, storedAvatar)
	require.ElementsMatch(t, []contentBackfillReference{
		{did, communityAvatarC, subject, "avatar", true},
		{did, communityBannerB, subject, "banner", true},
	}, activeCommunityProfileRows(t, db, subject))
}
