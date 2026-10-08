//go:build integration

package postgres

import (
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestNotificationList_AgreesWithCountForMentionRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, position string
		status               posts.AdmissionStatus
		drifted              bool
		wantLabels           []string
		wantCount            int
	}{
		{"post source pending", "post", "", posts.AdmissionStatusPending, false, []string{"control"}, 1},
		{"post source rejected", "post", "", posts.AdmissionStatusRejected, false, []string{"control"}, 1},
		{"post source pending reacceptance", "post", "", posts.AdmissionStatusPendingReacceptance, false, []string{"control"}, 1},
		{"post source no admission", "post", "", "", false, []string{"control"}, 1},
		{"post source CID drifted", "post", "", "", true, []string{"control"}, 1},
		{"post source deleted without marker", "deletedPost", "", posts.AdmissionStatusAccepted, false, []string{"control"}, 1},
		{"post source removed without marker", "post", "", posts.AdmissionStatusRemoved, false, []string{"control"}, 1},
		{"post source unindexed", "unindexedPost", "", "", false, []string{"control"}, 1},
		{"comment source unindexed", "unindexedComment", "", "", false, []string{"control"}, 1},
		{"mention record in unsupported collection", "unsupportedCollection", "", "", false, []string{"control"}, 1},
		{"comment root pending", "commentRoot", "", posts.AdmissionStatusPending, false, []string{"control"}, 1},
		{"post source author-deleted with marker", "deletedPostMarked", "", posts.AdmissionStatusAccepted, false, []string{"case", "control"}, 2},
		{"post source community-removed with marker", "removedPostMarked", "", posts.AdmissionStatusRemoved, false, []string{"case", "control"}, 2},
		{"comment source deleted", "deletedComment", "", "", false, []string{"case", "control"}, 2},
		{"comment root author-deleted with marker", "deletedRootMarked", "", posts.AdmissionStatusAccepted, false, []string{"case", "control"}, 2},
		{"comment root community-removed with marker", "removedRootMarked", "", posts.AdmissionStatusRemoved, false, []string{"case", "control"}, 2},
		{"post source live", "post", "", posts.AdmissionStatusAccepted, false, []string{"case", "control"}, 2},
		{"comment source live", "comment", "", "", false, []string{"case", "control"}, 2},
		{"recipient blocks mention actor", "block", "recipient", "", false, []string{"control"}, 1},
		{"mention actor blocks recipient", "block", "actor", "", false, []string{"control"}, 1},
		{"disabled mention", "disabled", "", "", false, []string{"control"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnreadVisibilityFixture(t)
			control := f.listControlRecord(t) // A non-mention reply survives even when mentions are disabled.
			labels := map[string]string{control: "control"}
			actor := f.actor
			if tc.kind == "block" {
				actor = "did:plc:listmentioncase" + testkit.UniqueID(t)
			}
			root := f.root
			var record string
			switch tc.kind {
			case "post", "deletedPost", "deletedPostMarked", "removedPostMarked":
				rkey := testkit.TID()
				record = seedVisibilityPost(t, f.db, f.community, actor, rkey, "mention post", f.sortAt)
				if tc.status != "" {
					seedVisibilityAdmission(t, f.db, f.community, record, tc.status, "", "")
				}
				if tc.drifted {
					seedVisibilityAdmissionDriftedCID(t, f.db, f.community, record)
				}
				if tc.kind == "deletedPost" || tc.kind == "deletedPostMarked" {
					f.deletePost(t, record)
				}
				if tc.kind == "deletedPostMarked" {
					seedWithdrawalMarker(t, f.db, record, "authorDelete", nil)
				}
				if tc.kind == "removedPostMarked" {
					seedWithdrawalMarker(t, f.db, record, "communityWithdrawal", "3lqqqqqqqqqq1")
				}
				root = record // A post mention points to itself as both record and root.
			case "unindexedPost":
				record = postV2URI(actor, testkit.TID())
				root = record
			case "commentRoot", "deletedRootMarked", "removedRootMarked":
				root = f.post(t, tc.status, false)
				if tc.kind == "deletedRootMarked" {
					f.deletePost(t, root)
					seedWithdrawalMarker(t, f.db, root, "authorDelete", nil)
				}
				if tc.kind == "removedRootMarked" {
					seedWithdrawalMarker(t, f.db, root, "communityWithdrawal", "3lqqqqqqqqqq1")
				}
				record = seedActorComment(t, f.db, actor, root, testkit.TID(), f.sortAt)
			case "unindexedComment":
				record = "at://" + actor + "/social.coves.community.comment/" + testkit.TID()
			case "unsupportedCollection":
				record = "at://" + actor + "/social.coves.feed.vote/" + testkit.TID()
			default:
				record = seedActorComment(t, f.db, actor, root, testkit.TID(), f.sortAt)
				if tc.kind == "deletedComment" {
					f.deleteComment(t, record)
				}
				if tc.kind == "block" {
					if tc.position == "recipient" {
						f.insertBlock(t, f.recipient, actor)
					} else {
						f.insertBlock(t, actor, f.recipient)
					}
				}
				if tc.kind == "disabled" {
					f.setDisabledReasons(t, f.recipient, []string{"mention"})
				}
			}
			f.listNotificationAt(t, f.recipient, actor, "mention", record, "", root, f.sortAt.Add(time.Second))
			labels[record] = "case"
			f.requireCount(t, tc.wantCount)
			page := f.listPage(t, f.recipient, "", 10)
			require.Equal(t, tc.wantLabels, f.recordLabels(t, page, labels))
			require.Empty(t, page.Cursor)
		})
	}
}
