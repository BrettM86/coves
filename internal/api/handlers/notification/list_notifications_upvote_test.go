//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func (f *listHandlerFixture) addUpvoteGroup(recipient, subject, root string, sortAt time.Time) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		VALUES ($1, 'upvote', NULL, NULL, NULL, $2, $3, NULL, $4)`, recipient, subject, root, sortAt.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
}

// Both timestamps order votes identically; the assertion does not pick a clock.
func (f *listHandlerFixture) addUpvoteVote(voter, subject, direction string, at time.Time) string {
	f.t.Helper()
	key := testkit.TID()
	uri := "at://" + voter + "/social.coves.feed.vote/" + key
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO votes
		(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, 'bafyvote', $2, $3, $4, 'bafystalesubject', $5, $6, $6)`,
		uri, key, voter, subject, direction, at.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
	return uri
}

func upvoteVoters(t *testing.T, raw map[string]json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var voters []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["recentUpvoters"], &voters))
	return voters
}

func upvoteVoterDIDs(t *testing.T, raw map[string]json.RawMessage) []string {
	t.Helper()
	var dids []string
	for _, voter := range upvoteVoters(t, raw) {
		var did string
		require.NoError(t, json.Unmarshal(voter["did"], &did))
		dids = append(dids, did)
	}
	return dids
}

func TestListNotifications_UpvoteFullShape(t *testing.T) {
	for _, tc := range []struct {
		name, kind, title, content, preview, subjectCID, rootCID string
	}{
		{"post title", "post", "A complete title", "Body", "A complete title", "bafyroot", "bafyroot"},
		{"untitled post body", "post", "", strings.Repeat("b", 139) + "👩‍👩‍👧‍👦" + strings.Repeat("c", 160), strings.Repeat("b", 139) + "👩‍👩‍👧‍👦", "bafyroot", "bafyroot"},
		{"comment excerpt on another author's root", "comment", "Other root", strings.Repeat("a", 139) + "🇺🇸" + strings.Repeat("z", 160), strings.Repeat("a", 139) + "🇺🇸", "bafycurrentcomment", "bafyotherroot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Seed root", "Seed body")
			at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
			root, subject := thread.root, thread.root
			if tc.kind == "post" {
				_, err := f.db.ExecContext(context.Background(), `UPDATE posts SET title = $1, content = $2 WHERE uri = $3`, tc.title, tc.content, thread.root)
				require.NoError(t, err)
			} else {
				root = "at://" + thread.actor + "/social.coves.community.postv2/other"
				f.addPost(root, tc.rootCID, "other", thread.actor, thread.community, tc.title, "Other body", at)
				subject = "at://" + thread.caller + "/social.coves.community.comment/own"
				f.addComment(subject, tc.subjectCID, "own", thread.caller, root, "bafystaleroot", root, "bafystaleparent", tc.content, at)
			}
			f.addUpvoteGroup(thread.caller, subject, root, at)
			older, newest := "did:plc:older"+f.id, "did:plc:newest"+f.id
			f.addUser(older, "older"+f.id+".test", "Older")
			f.addUser(newest, "newest"+f.id+".test", "Newest")
			f.addUpvoteVote(older, subject, "up", at.Add(time.Second))
			f.addUpvoteVote(newest, subject, "up", at.Add(2*time.Second))
			response := f.placeholderRequest(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Len(t, rows, 1, "upvote group must be listed")
			row := rows[0]
			require.JSONEq(t, `"upvote"`, string(row["reason"]))
			require.NotContains(t, row, "author")
			require.NotContains(t, row, "record")
			require.JSONEq(t, `{"uri":"`+subject+`","cid":"`+tc.subjectCID+`","preview":"`+tc.preview+`"}`, string(row["subject"]))
			rootView := placeholderObject(t, row, "rootPost")
			require.JSONEq(t, `"`+root+`"`, string(rootView["uri"]))
			require.JSONEq(t, `"`+tc.rootCID+`"`, string(rootView["cid"]))
			if tc.title == "" {
				require.NotContains(t, rootView, "title")
			} else {
				require.JSONEq(t, `"`+tc.title+`"`, string(rootView["title"]))
			}
			require.JSONEq(t, `{"did":"`+thread.community+`","handle":"listcommunity`+f.id+`.coves.social","name":"Thread Community"}`, string(rootView["community"]))
			require.JSONEq(t, `2`, string(row["upvoteCount"]))
			require.Equal(t, []string{newest, older}, upvoteVoterDIDs(t, row))
			voters := upvoteVoters(t, row)
			require.JSONEq(t, `{"did":"`+newest+`","handle":"newest`+f.id+`.test","displayName":"Newest"}`, mustJSON(t, voters[0]))
			require.JSONEq(t, `{"did":"`+older+`","handle":"older`+f.id+`.test","displayName":"Older"}`, mustJSON(t, voters[1]))
		})
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func TestListNotifications_UpvoteVoterExclusions(t *testing.T) {
	for _, tc := range []struct {
		name, exclusion string
		count           int
		want            []int
	}{
		{"control", "", 5, []int{5, 4, 3}},
		{"recipient blocks V4", "recipientBlock", 4, []int{5, 3, 2}},
		{"V4 blocks recipient", "voterBlock", 4, []int{5, 3, 2}},
		{"V4 erased", "erased", 4, []int{5, 3, 2}},
		{"V4 aggregator", "aggregator", 4, []int{5, 3, 2}},
		{"V4 retracted", "retracted", 4, []int{5, 3, 2}},
		{"V4 downvote", "down", 4, []int{5, 3, 2}},
		{"V4 replaced by self vote", "self", 4, []int{5, 3, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Root", "Body")
			at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
			f.addUpvoteGroup(thread.caller, thread.root, thread.root, at)
			voters := make([]string, 6)
			for i := 1; i <= 5; i++ {
				voters[i] = fmt.Sprintf("did:plc:voter%d%s", i, f.id)
				f.addUser(voters[i], fmt.Sprintf("voter%d%s.test", i, f.id), fmt.Sprintf("Voter %d", i))
				if i == 4 && tc.exclusion == "self" {
					voters[i] = thread.caller
				}
				direction := "up"
				if i == 4 && tc.exclusion == "down" {
					direction = "down"
				}
				vote := f.addUpvoteVote(voters[i], thread.root, direction, at.Add(time.Duration(i)*time.Second))
				if i == 4 && tc.exclusion == "retracted" {
					_, err := f.db.ExecContext(context.Background(), `UPDATE votes SET deleted_at = $1 WHERE uri = $2`, at.Add(10*time.Second), vote)
					require.NoError(t, err)
				}
			}
			switch tc.exclusion {
			case "recipientBlock", "voterBlock":
				blocker, blocked := thread.caller, voters[4]
				if tc.exclusion == "voterBlock" {
					blocker, blocked = blocked, blocker
				}
				_, err := f.db.ExecContext(context.Background(), `INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, 'bafyblock')`, blocker, blocked, "at://"+blocker+"/social.coves.actor.block/"+testkit.TID())
				require.NoError(t, err)
			case "erased":
				_, err := f.db.ExecContext(context.Background(), `INSERT INTO deleted_accounts (did) VALUES ($1)`, voters[4])
				require.NoError(t, err)
			case "aggregator":
				_, err := f.db.ExecContext(context.Background(), `INSERT INTO aggregators (did, display_name, record_uri, record_cid) VALUES ($1, 'Voter aggregator', $2, 'bafyservice')`, voters[4], "at://"+voters[4]+"/social.coves.aggregator.service/self")
				require.NoError(t, err)
			}
			response := f.placeholderRequest(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Len(t, rows, 1, "group must remain visible with four qualifying voters")
			require.JSONEq(t, fmt.Sprintf("%d", tc.count), string(rows[0]["upvoteCount"]))
			require.Equal(t, []string{voters[tc.want[0]], voters[tc.want[1]], voters[tc.want[2]]}, upvoteVoterDIDs(t, rows[0]))
		})
	}
}

func TestListNotifications_UnindexedNewestUpvoter(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		name := "unindexed newest"
		if indexed {
			name = "indexed newest"
		}
		t.Run(name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Root", "Body")
			at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
			f.addUpvoteGroup(thread.caller, thread.root, thread.root, at)
			voters := make([]string, 6)
			for i := 1; i <= 5; i++ {
				voters[i] = fmt.Sprintf("did:plc:voter%d%s", i, f.id)
				if i != 5 || indexed {
					f.addUser(voters[i], fmt.Sprintf("voter%d%s.test", i, f.id), fmt.Sprintf("Voter %d", i))
				}
				f.addUpvoteVote(voters[i], thread.root, "up", at.Add(time.Duration(i)*time.Second))
			}
			response := f.placeholderRequest(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Len(t, rows, 1)
			require.JSONEq(t, `5`, string(rows[0]["upvoteCount"]))
			require.Equal(t, []string{voters[5], voters[4], voters[3]}, upvoteVoterDIDs(t, rows[0]))
			if !indexed {
				require.JSONEq(t, `{"did":"`+voters[5]+`"}`, mustJSON(t, upvoteVoters(t, rows[0])[0]))
			}
		})
	}
}

func TestListNotifications_UpvotePlaceholders(t *testing.T) {
	for _, tc := range []struct {
		name, subjectKind, withdrawal, subjectStatus, rootStatus string
	}{
		{"deleted comment subject", "comment", "", "deleted", ""},
		{"removed post subject and root", "post", "communityWithdrawal", "removedByModerator", "removedByModerator"},
		{"live comment under deleted root", "comment", "authorDelete", "", "deleted"},
		{"live comment under removed root", "comment", "communityWithdrawal", "", "removedByModerator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Root title", "Private root body")
			at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
			subject, subjectCID := thread.root, "bafyroot"
			if tc.subjectKind == "comment" {
				subject, subjectCID = "at://"+thread.caller+"/social.coves.community.comment/own", "bafyowncomment"
				f.addComment(subject, subjectCID, "own", thread.caller, thread.root, "bafystaleroot", thread.root, "bafystaleroot", "Visible own comment", at)
			}
			f.addUpvoteGroup(thread.caller, subject, thread.root, at)
			v1, v2 := "did:plc:first"+f.id, "did:plc:second"+f.id
			f.addUser(v1, "first"+f.id+".test", "First")
			f.addUser(v2, "second"+f.id+".test", "Second")
			f.addUpvoteVote(v1, subject, "up", at.Add(time.Second))
			f.addUpvoteVote(v2, subject, "up", at.Add(2*time.Second))
			if tc.subjectStatus == "deleted" {
				f.softDeleteComment(subject)
			}
			if tc.withdrawal != "" {
				f.withdrawPost(thread.root, tc.withdrawal)
			}
			response := f.placeholderRequest(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Len(t, rows, 1, "upvote group survives withdrawal")
			row := rows[0]
			require.NotContains(t, row, "record")
			require.NotContains(t, row, "author")
			require.JSONEq(t, `2`, string(row["upvoteCount"]))
			require.Equal(t, []string{v2, v1}, upvoteVoterDIDs(t, row))
			subjectView := placeholderObject(t, row, "subject")
			rootView := placeholderObject(t, row, "rootPost")
			require.JSONEq(t, `"`+subject+`"`, string(subjectView["uri"]))
			require.JSONEq(t, `"`+subjectCID+`"`, string(subjectView["cid"]))
			require.JSONEq(t, `"`+thread.root+`"`, string(rootView["uri"]))
			require.JSONEq(t, `"bafyroot"`, string(rootView["cid"]))
			if tc.subjectStatus != "" {
				require.JSONEq(t, `"`+tc.subjectStatus+`"`, string(subjectView["status"]))
				require.NotContains(t, subjectView, "preview")
			} else {
				require.NotContains(t, subjectView, "status")
				preview := "Visible own comment"
				if tc.subjectKind == "post" {
					preview = "Root title"
				}
				require.JSONEq(t, `"`+preview+`"`, string(subjectView["preview"]))
			}
			if tc.rootStatus != "" {
				require.JSONEq(t, `"`+tc.rootStatus+`"`, string(rootView["status"]))
				require.NotContains(t, rootView, "title")
				require.NotContains(t, rootView, "community")
			} else {
				require.NotContains(t, rootView, "status")
				require.JSONEq(t, `"Root title"`, string(rootView["title"]))
				require.Contains(t, rootView, "community")
			}
		})
	}
}

type upvoteBatchProfiles struct {
	*countedProfiles
	dids []string
}

func (p *upvoteBatchProfiles) GetByDIDs(ctx context.Context, dids []string) (map[string]*users.User, error) {
	p.dids = append([]string(nil), dids...)
	return p.countedProfiles.GetByDIDs(ctx, dids)
}

func TestListNotifications_UpvoteMixedPageBatchesHydration(t *testing.T) {
	for _, count := range []int{50, 8} {
		t.Run(fmt.Sprintf("%d rows", count), func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Batch root", "Batch body")
			at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
			parent := "at://" + thread.caller + "/social.coves.community.comment/batchparent"
			f.addComment(parent, "bafybatchparent", "batchparent", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", "Parent comment", at)
			authors := []string{thread.actor, "did:plc:second" + f.id, "did:plc:third" + f.id}
			for i, author := range authors[1:] {
				f.addUser(author, fmt.Sprintf("author%d%s.test", i, f.id), fmt.Sprintf("Author %d", i))
			}
			wantDIDs := map[string]bool{}
			groups := map[string][]string{}
			wantRecords := map[string]bool{}
			for i := 0; i < count; i++ {
				key := fmt.Sprintf("mixed%d", i)
				moment := at.Add(time.Duration(i) * time.Second)
				if i%4 == 0 {
					root := "at://" + thread.caller + "/social.coves.community.postv2/" + key
					f.addPost(root, "bafy"+key, key, thread.caller, thread.community, "Title "+key, "Body", at)
					subject := root
					if i%8 == 4 {
						subject = "at://" + thread.caller + "/social.coves.community.comment/" + key
						f.addComment(subject, "bafycomment"+key, key, thread.caller, root, "bafy"+key, root, "bafy"+key, "Own comment", at)
					}
					f.addUpvoteGroup(thread.caller, subject, root, moment)
					for j := 0; j < 2; j++ {
						voter := fmt.Sprintf("did:plc:voter%d_%d%s", i, j, f.id)
						f.addUser(voter, fmt.Sprintf("voter%d_%d%s.test", i, j, f.id), "Batch voter")
						f.addUpvoteVote(voter, subject, "up", moment.Add(time.Duration(j+1)*time.Millisecond))
						wantDIDs[voter] = true
						groups[subject] = append([]string{voter}, groups[subject]...)
					}
					continue
				}
				actor := authors[i%len(authors)]
				wantDIDs[actor] = true
				uri := "at://" + actor + "/social.coves.community.comment/" + key
				f.addComment(uri, "bafy"+key, key, actor, thread.root, "bafyroot", thread.root, "bafyroot", "Text "+key, moment)
				switch i % 4 {
				case 1:
					f.addReply(thread.caller, notifications.ReasonPostReply, uri, "bafystored"+key, actor, thread.root, thread.root, moment, moment)
				case 2:
					f.addReply(thread.caller, notifications.ReasonCommentReply, uri, "bafystored"+key, actor, parent, thread.root, moment, moment)
				case 3:
					f.addMention(thread.caller, uri, "bafystored"+key, actor, thread.root, moment, moment)
				}
				wantRecords[uri] = true
			}
			repo := &countedNotificationReads{ReadRepository: f.repo}
			profiles := &upvoteBatchProfiles{countedProfiles: &countedProfiles{ProfileLookup: postgres.NewUserRepository(f.db)}}
			posts := &countedPosts{PostViewLookup: postgres.NewPostRepository(f.db)}
			comments := &countedComments{CommentLookup: postgres.NewCommentRepository(f.db)}
			response := f.requestWithListDependencies(thread.caller, fmt.Sprintf("limit=%d", count), repo, profiles, posts, comments)
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Equal(t, count, len(rows), "no live row may be omitted")
			for _, row := range rows {
				root := placeholderObject(t, row, "rootPost")
				require.Contains(t, root, "title")
				if string(row["reason"]) == `"upvote"` {
					subject := placeholderObject(t, row, "subject")
					var uri string
					require.NoError(t, json.Unmarshal(subject["uri"], &uri))
					require.Contains(t, groups, uri)
					require.Contains(t, subject, "preview")
					require.JSONEq(t, `2`, string(row["upvoteCount"]))
					require.Equal(t, groups[uri], upvoteVoterDIDs(t, row))
					delete(groups, uri)
				} else {
					var record map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(row["record"], &record))
					var uri string
					require.NoError(t, json.Unmarshal(record["uri"], &uri))
					require.True(t, wantRecords[uri], "unexpected record %s", uri)
					require.Contains(t, record, "excerpt")
					require.Contains(t, row, "author")
					delete(wantRecords, uri)
				}
			}
			require.Empty(t, groups)
			require.Empty(t, wantRecords)
			require.Equal(t, 1, repo.lists)
			require.Equal(t, 1, profiles.calls)
			require.Len(t, profiles.dids, len(wantDIDs))
			for did := range wantDIDs {
				require.Contains(t, profiles.dids, did)
			}
			require.LessOrEqual(t, posts.calls, 1)
			require.LessOrEqual(t, comments.calls, 1)
		})
	}
}
