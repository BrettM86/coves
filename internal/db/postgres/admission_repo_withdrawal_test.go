//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

var withdrawalHistoricalTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

type withdrawalMarkerRow struct {
	kind       string
	rev        sql.NullString
	recordedAt time.Time
}

func withdrawalMarker(t *testing.T, db *sql.DB, postURI, kind string) *withdrawalMarkerRow {
	t.Helper()
	var row withdrawalMarkerRow
	err := db.QueryRowContext(context.Background(), `SELECT kind, community_rev, recorded_at
		FROM notification_public_post_withdrawals WHERE post_uri = $1 AND kind = $2`, postURI, kind).
		Scan(&row.kind, &row.rev, &row.recordedAt)
	if err == sql.ErrNoRows {
		return nil
	}
	require.NoError(t, err)
	return &row
}

func seedWithdrawalMarker(t *testing.T, db *sql.DB, postURI, kind string, rev any) *withdrawalMarkerRow {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO notification_public_post_withdrawals
		(post_uri, kind, community_rev, recorded_at) VALUES ($1, $2, $3, $4)`,
		postURI, kind, rev, withdrawalHistoricalTime)
	require.NoError(t, err)
	return withdrawalMarker(t, db, postURI, kind)
}

func requireCommunityWithdrawal(t *testing.T, db *sql.DB, postURI, rev string) *withdrawalMarkerRow {
	t.Helper()
	marker := withdrawalMarker(t, db, postURI, "communityWithdrawal")
	require.NotNil(t, marker, "communityWithdrawal marker for %s", postURI)
	require.Equal(t, "communityWithdrawal", marker.kind)
	require.Equal(t, sql.NullString{String: rev, Valid: true}, marker.rev)
	return marker
}

func newWithdrawalSubject(t *testing.T, db *sql.DB) (admissionSubject, string) {
	t.Helper()
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:withdrawal" + testkit.UniqueID(t)
	postURI := seedVisibilityPost(t, db, community, author, testkit.TID(), "withdrawal candidate", time.Now())
	return admissionSubject{CommunityDID: community, PostURI: postURI}, postContentCID(t, db, postURI)
}

func acceptWithdrawalSubject(t *testing.T, repo posts.AdmissionRepository, subject admissionSubject, cid, rev string) {
	t.Helper()
	uri, rkey := acceptanceRecord(t, subject.CommunityDID)
	result, err := repo.ApplyAcceptance(context.Background(), posts.ApplyAcceptanceCommand{
		CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
		AcceptanceURI: uri, AcceptanceRkey: rkey, PinnedCID: cid,
		Watermark: posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome)
	require.Equal(t, posts.AdmissionStatusAccepted, result.Admission.Status)
}

func removalWithdrawal(t *testing.T, repo posts.AdmissionRepository, subject admissionSubject, rev string) posts.AdmissionResult {
	t.Helper()
	result, err := repo.ApplyRemoval(context.Background(), posts.ApplyRemovalCommand{
		CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
		DecisionCode: "rule_violation",
		Watermark:    posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpPut},
	})
	require.NoError(t, err)
	return result
}

func acceptanceDeleteWithdrawal(t *testing.T, repo posts.AdmissionRepository, subject admissionSubject, rev string) posts.AdmissionResult {
	t.Helper()
	result, err := repo.ApplyAcceptanceDelete(context.Background(), posts.CommunityDeleteCommand{
		CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
		Watermark: posts.CommunityWatermark{Rev: rev, OpRank: posts.CommunityOpDelete},
	})
	require.NoError(t, err)
	return result
}

func requireWithdrawalStatus(t *testing.T, repo posts.AdmissionRepository, subject admissionSubject, status posts.AdmissionStatus, rev string, rank posts.CommunityOpRank) {
	t.Helper()
	row, err := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
	require.NoError(t, err)
	require.Equal(t, status, row.Status)
	assertWatermark(t, rev, rank, row.LastCommunityEvent)
}

func TestAdmissionRepo_CommunityWithdrawal_RemovalFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{"removal then refused acceptance delete", false},
		{"refresh stale marker", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 2)
			subject, cid := newWithdrawalSubject(t, db)
			acceptWithdrawalSubject(t, repo, subject, cid, revs[0])
			if tc.stale {
				seedWithdrawalMarker(t, db, subject.PostURI, "communityWithdrawal", revs[0])
			}
			require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[1]).Outcome)
			requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[1], posts.CommunityOpPut)
			marker := requireCommunityWithdrawal(t, db, subject.PostURI, revs[1])
			if tc.stale {
				require.True(t, marker.recordedAt.After(withdrawalHistoricalTime), "upsert refreshes recorded_at")
				return
			}
			require.Equal(t, posts.AdmissionSkippedStale, acceptanceDeleteWithdrawal(t, repo, subject, revs[1]).Outcome)
			requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[1], posts.CommunityOpPut)
			require.Equal(t, marker, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
		})
	}
}

// Both foreign-community and unindexed admissions deliberately exist: a marker
// must not be cleared just because a decision for the URI happened to exist.
func setupWithdrawalNotVisible(t *testing.T, db *sql.DB, repo posts.AdmissionRepository, kind string, rev string) admissionSubject {
	t.Helper()
	if kind == "unindexed" {
		community := visibilityCommunity(t, db, testkit.UniqueID(t))
		subject := admissionSubject{CommunityDID: community, PostURI: postV2URI("did:plc:unindexed"+testkit.UniqueID(t), testkit.TID())}
		_, err := repo.UpsertPending(context.Background(), posts.UpsertPendingCommand{CommunityDID: community, PostURI: subject.PostURI, EvaluatedCID: "bafyunindexed"})
		require.NoError(t, err)
		return subject
	}
	if kind == "legacy" {
		subject := newAdmissionSubject(t, db)
		acceptWithdrawalSubject(t, repo, subject, postContentCID(t, db, subject.PostURI), rev)
		return subject
	}
	subject, cid := newWithdrawalSubject(t, db)
	switch kind {
	case "pending":
		_, err := repo.UpsertPending(context.Background(), posts.UpsertPendingCommand{CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: cid})
		require.NoError(t, err)
	case "pending reacceptance":
		acceptWithdrawalSubject(t, repo, subject, cid, rev)
		_, err := repo.UpsertPending(context.Background(), posts.UpsertPendingCommand{CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: contentCID(t, "edit")})
		require.NoError(t, err)
	case "drifted CID":
		acceptWithdrawalSubject(t, repo, subject, cid, rev)
		_, err := db.ExecContext(context.Background(), `UPDATE posts SET cid = $2 WHERE uri = $1`, subject.PostURI, contentCID(t, "edit"))
		require.NoError(t, err)
	case "fork":
		other := visibilityCommunity(t, db, testkit.UniqueID(t))
		acceptWithdrawalSubject(t, repo, subject, cid, rev)
		subject.CommunityDID = other
		acceptWithdrawalSubject(t, repo, subject, cid, rev)
	case "no admission":
	default:
		t.Fatalf("unknown non-visible fixture %q", kind)
	}
	return subject
}

func TestAdmissionRepo_CommunityWithdrawal_NotVisibleRemoval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fixture string
		keep          bool
	}{
		{"pending clears stale marker", "pending", false},
		{"pending reacceptance clears stale marker", "pending reacceptance", false},
		{"drifted CID clears stale marker", "drifted CID", false},
		{"preemptive removal clears stale marker", "no admission", false},
		{"fork keeps marker", "fork", true},
		{"unindexed keeps marker", "unindexed", true},
		{"legacy does not mark", "legacy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 3)
			subject := setupWithdrawalNotVisible(t, db, repo, tc.fixture, revs[1])
			var before *withdrawalMarkerRow
			if tc.fixture != "legacy" {
				before = seedWithdrawalMarker(t, db, subject.PostURI, "communityWithdrawal", revs[0])
			}
			require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[2]).Outcome)
			requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[2], posts.CommunityOpPut)
			if tc.fixture == "fork" {
				var ownStatus posts.AdmissionStatus
				err := db.QueryRowContext(context.Background(), `SELECT a.status FROM community_post_admissions a
					JOIN posts p ON p.uri = a.post_uri AND p.community_did = a.community_did
					WHERE a.post_uri = $1`, subject.PostURI).Scan(&ownStatus)
				require.NoError(t, err)
				require.Equal(t, posts.AdmissionStatusAccepted, ownStatus)
			}
			if tc.keep {
				require.Equal(t, before, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			} else {
				require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			}
		})
	}
}

func TestAdmissionRepo_CommunityWithdrawal_AcceptanceDeleteFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fixture string
		stale         bool
	}{
		{"acceptance delete then removal", "visible", false},
		{"refresh stale marker", "visible", true},
		{"pending keeps marker", "pending", true},
		{"pending reacceptance keeps marker", "pending reacceptance", true},
		{"drifted CID keeps marker", "drifted CID", true},
		{"fork keeps marker", "fork", true},
		{"unindexed keeps marker", "unindexed", true},
		{"legacy does not mark", "legacy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 3)
			var subject admissionSubject
			if tc.fixture == "visible" {
				var cid string
				subject, cid = newWithdrawalSubject(t, db)
				acceptWithdrawalSubject(t, repo, subject, cid, revs[1])
			} else {
				subject = setupWithdrawalNotVisible(t, db, repo, tc.fixture, revs[1])
			}
			var before *withdrawalMarkerRow
			if tc.stale {
				before = seedWithdrawalMarker(t, db, subject.PostURI, "communityWithdrawal", revs[0])
			}
			require.Equal(t, posts.AdmissionApplied, acceptanceDeleteWithdrawal(t, repo, subject, revs[2]).Outcome)
			requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusPending, revs[2], posts.CommunityOpDelete)
			if tc.fixture != "visible" {
				if tc.stale {
					require.Equal(t, before, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
				} else {
					require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
				}
				return
			}
			marker := requireCommunityWithdrawal(t, db, subject.PostURI, revs[2])
			if tc.stale {
				require.True(t, marker.recordedAt.After(withdrawalHistoricalTime), "upsert refreshes recorded_at")
				return
			}
			require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[2]).Outcome)
			requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[2], posts.CommunityOpPut)
			require.Equal(t, marker, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
		})
	}
}

func TestAdmissionRepo_CommunityWithdrawal_StaleMarkersCleared(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		firstRemove bool
	}{
		{"acceptance delete then later removal", false},
		{"removal withdrawn then later removal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 4)
			subject, cid := newWithdrawalSubject(t, db)
			acceptWithdrawalSubject(t, repo, subject, cid, revs[0])
			if tc.firstRemove {
				require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[1]).Outcome)
			} else {
				require.Equal(t, posts.AdmissionApplied, acceptanceDeleteWithdrawal(t, repo, subject, revs[1]).Outcome)
			}
			requireCommunityWithdrawal(t, db, subject.PostURI, revs[1])
			if tc.firstRemove {
				result, err := repo.ApplyRemovalDelete(context.Background(), posts.CommunityDeleteCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
					Watermark: posts.CommunityWatermark{Rev: revs[2], OpRank: posts.CommunityOpDelete},
				})
				require.NoError(t, err)
				require.Equal(t, posts.AdmissionApplied, result.Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusPending, revs[2], posts.CommunityOpDelete)
			}
			finalRev := revs[2]
			if tc.firstRemove {
				finalRev = revs[3]
			}
			require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, finalRev).Outcome)
			requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, finalRev, posts.CommunityOpPut)
			require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
		})
	}
}

func TestAdmissionRepo_CommunityWithdrawal_MarkersLeftAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name string }{
		{"already removed"},
		{"stale removal on accepted"},
		{"stale acceptance delete on accepted"},
		{"stale removal on pending"},
		{"author delete survives acceptance delete"},
		{"author delete survives stale marker removal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 4)
			subject, cid := newWithdrawalSubject(t, db)
			switch tc.name {
			case "already removed":
				acceptWithdrawalSubject(t, repo, subject, cid, revs[0])
				require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[1]).Outcome)
				before := requireCommunityWithdrawal(t, db, subject.PostURI, revs[1])
				require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[2]).Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[2], posts.CommunityOpPut)
				require.Equal(t, before, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			case "stale removal on accepted", "stale acceptance delete on accepted":
				acceptWithdrawalSubject(t, repo, subject, cid, revs[2])
				if tc.name == "stale removal on accepted" {
					require.Equal(t, posts.AdmissionSkippedStale, removalWithdrawal(t, repo, subject, revs[1]).Outcome)
				} else {
					require.Equal(t, posts.AdmissionSkippedStale, acceptanceDeleteWithdrawal(t, repo, subject, revs[1]).Outcome)
				}
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusAccepted, revs[2], posts.CommunityOpPut)
				require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			case "stale removal on pending":
				acceptWithdrawalSubject(t, repo, subject, cid, revs[0])
				require.Equal(t, posts.AdmissionApplied, acceptanceDeleteWithdrawal(t, repo, subject, revs[2]).Outcome)
				before := requireCommunityWithdrawal(t, db, subject.PostURI, revs[2])
				require.Equal(t, posts.AdmissionSkippedStale, removalWithdrawal(t, repo, subject, revs[1]).Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusPending, revs[2], posts.CommunityOpDelete)
				require.Equal(t, before, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			case "author delete survives acceptance delete":
				acceptWithdrawalSubject(t, repo, subject, cid, revs[0])
				author := seedWithdrawalMarker(t, db, subject.PostURI, "authorDelete", nil)
				require.Equal(t, posts.AdmissionApplied, acceptanceDeleteWithdrawal(t, repo, subject, revs[1]).Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusPending, revs[1], posts.CommunityOpDelete)
				requireCommunityWithdrawal(t, db, subject.PostURI, revs[1])
				require.Equal(t, author, withdrawalMarker(t, db, subject.PostURI, "authorDelete"))
			case "author delete survives stale marker removal":
				_, err := repo.UpsertPending(context.Background(), posts.UpsertPendingCommand{CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: cid})
				require.NoError(t, err)
				author := seedWithdrawalMarker(t, db, subject.PostURI, "authorDelete", nil)
				seedWithdrawalMarker(t, db, subject.PostURI, "communityWithdrawal", revs[0])
				require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[2]).Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[2], posts.CommunityOpPut)
				require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
				require.Equal(t, author, withdrawalMarker(t, db, subject.PostURI, "authorDelete"))
			}
		})
	}
}

func TestAdmissionRepo_CommunityWithdrawal_SoftDeleted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		deleteFirst    bool
		softDeleteLate bool
	}{
		{"removal first on deleted post", false, false},
		{"acceptance delete first on deleted post", true, false},
		{"deletion between acceptance delete and removal", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 3)
			subject, cid := newWithdrawalSubject(t, db)
			acceptWithdrawalSubject(t, repo, subject, cid, revs[1])
			var visibleMarker *withdrawalMarkerRow
			if tc.softDeleteLate {
				require.Equal(t, posts.AdmissionApplied, acceptanceDeleteWithdrawal(t, repo, subject, revs[2]).Outcome)
				visibleMarker = requireCommunityWithdrawal(t, db, subject.PostURI, revs[2])
			} else {
				seedWithdrawalMarker(t, db, subject.PostURI, "communityWithdrawal", revs[0])
			}
			_, err := db.ExecContext(context.Background(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, subject.PostURI)
			require.NoError(t, err)
			if tc.deleteFirst && !tc.softDeleteLate {
				before := withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal")
				require.Equal(t, posts.AdmissionApplied, acceptanceDeleteWithdrawal(t, repo, subject, revs[2]).Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusPending, revs[2], posts.CommunityOpDelete)
				require.Equal(t, before, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			}
			require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[2]).Outcome)
			requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[2], posts.CommunityOpPut)
			if tc.softDeleteLate {
				require.Equal(t, visibleMarker, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			} else {
				require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			}
			if !tc.deleteFirst {
				require.Equal(t, posts.AdmissionSkippedStale, acceptanceDeleteWithdrawal(t, repo, subject, revs[2]).Outcome)
				require.Nil(t, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			}
		})
	}
}

func TestAdmissionRepo_CommunityWithdrawal_RollsBackWithMarkerFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		initialAccepted  bool
		acceptanceDelete bool
	}{
		{"acceptance delete marker insert", true, true},
		{"removal marker insert", true, false},
		{"removal marker delete", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 3)
			subject, cid := newWithdrawalSubject(t, db)
			if tc.initialAccepted {
				acceptWithdrawalSubject(t, repo, subject, cid, revs[0])
			} else {
				_, err := repo.UpsertPending(context.Background(), posts.UpsertPendingCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: cid,
				})
				require.NoError(t, err)
				seedWithdrawalMarker(t, db, subject.PostURI, "communityWithdrawal", revs[1])
			}
			before, err := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
			require.NoError(t, err)
			beforeCommunity := withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal")
			beforeAuthor := withdrawalMarker(t, db, subject.PostURI, "authorDelete")

			_, err = db.ExecContext(context.Background(), `CREATE FUNCTION fail_community_withdrawal_marker() RETURNS trigger
				LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'marker write rejected'; END; $$`)
			require.NoError(t, err)
			_, err = db.ExecContext(context.Background(), `CREATE TRIGGER fail_community_withdrawal_marker
				BEFORE INSERT OR UPDATE OR DELETE ON notification_public_post_withdrawals
				FOR EACH ROW EXECUTE FUNCTION fail_community_withdrawal_marker()`)
			require.NoError(t, err)

			if tc.acceptanceDelete {
				_, err = repo.ApplyAcceptanceDelete(context.Background(), posts.CommunityDeleteCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
					Watermark: posts.CommunityWatermark{Rev: revs[2], OpRank: posts.CommunityOpDelete},
				})
			} else {
				_, err = repo.ApplyRemoval(context.Background(), posts.ApplyRemovalCommand{
					CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, DecisionCode: "rule_violation",
					Watermark: posts.CommunityWatermark{Rev: revs[2], OpRank: posts.CommunityOpPut},
				})
			}
			require.ErrorContains(t, err, "record post community withdrawal")
			after, getError := repo.Get(context.Background(), subject.CommunityDID, subject.PostURI)
			require.NoError(t, getError)
			require.Equal(t, before, after, "admission and watermark must roll back with the marker")
			require.Equal(t, beforeCommunity, withdrawalMarker(t, db, subject.PostURI, "communityWithdrawal"))
			require.Equal(t, beforeAuthor, withdrawalMarker(t, db, subject.PostURI, "authorDelete"))
		})
	}
}

// An active admin removal hides the post from the read path, but the post was
// still admitted to the public by its community; the community's own withdrawal
// must leave the marker so the notification keeps its removed placeholder.
func TestAdmissionRepo_CommunityWithdrawal_AdminRemovedPost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		communityScope   bool
		acceptanceDelete bool
	}{
		{"instance removal then community removal", false, false},
		{"community-scope removal then community removal", true, false},
		{"instance removal then acceptance delete", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			repo := NewAdmissionRepository(db)
			revs := increasingRevs(t, 2)
			subject, cid := newWithdrawalSubject(t, db)
			acceptWithdrawalSubject(t, repo, subject, cid, revs[0])
			scope := ""
			if tc.communityScope {
				scope = subject.CommunityDID
			}
			seedModerationDecision(t, db, subject.PostURI, "removal", scope, true)
			if tc.acceptanceDelete {
				require.Equal(t, posts.AdmissionApplied, acceptanceDeleteWithdrawal(t, repo, subject, revs[1]).Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusPending, revs[1], posts.CommunityOpDelete)
			} else {
				require.Equal(t, posts.AdmissionApplied, removalWithdrawal(t, repo, subject, revs[1]).Outcome)
				requireWithdrawalStatus(t, repo, subject, posts.AdmissionStatusRemoved, revs[1], posts.CommunityOpPut)
			}
			requireCommunityWithdrawal(t, db, subject.PostURI, revs[1])
		})
	}
}
