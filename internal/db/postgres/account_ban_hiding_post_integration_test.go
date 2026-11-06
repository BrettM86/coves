//go:build integration

package postgres_test

import (
	"database/sql"
	"testing"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountRestrictionPostRead is one post read path that applies account
// restrictions, read as one viewer.
type accountRestrictionPostRead struct {
	name string
	read func(*testing.T) []string
}

// accountRestrictionPostReads are the community feed and actor.getPosts of
// authorDID as viewerDID ("" for anonymous), each returning the served URIs.
func accountRestrictionPostReads(db *sql.DB, communityDID, authorDID, viewerDID string) []accountRestrictionPostRead {
	postRepo := postgres.NewPostRepository(db, postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	feedRepo := postgres.NewCommunityFeedRepository(db, "account-ban-item-removal-secret",
		postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID()))
	return []accountRestrictionPostRead{
		{"community feed", func(t *testing.T) []string {
			feed, _, err := feedRepo.GetCommunityFeed(t.Context(), communityFeeds.GetCommunityFeedRequest{
				Community: communityDID, ViewerDID: viewerDID, Sort: "new", Timeframe: "all", Limit: 50,
			})
			require.NoError(t, err)
			var uris []string
			for _, item := range feed {
				uris = append(uris, item.Post.URI)
			}
			return uris
		}},
		{"actor.getPosts", func(t *testing.T) []string {
			views, _, err := postRepo.GetByAuthor(t.Context(), posts.GetAuthorPostsRequest{ActorDID: authorDID, ViewerDID: viewerDID, Limit: 50})
			require.NoError(t, err)
			var uris []string
			for _, view := range views {
				uris = append(uris, view.URI)
			}
			return uris
		}},
	}
}

// TestAccountBanUnbanKeepsPerItemRemovals bans A, whose post P1 an instance
// removal took down before the ban and whose post P2 is removed while banned.
// While banned every post of A is hidden from another viewer; unbanning
// returns only P0, the post no per-item removal touched.
func TestAccountBanUnbanKeepsPerItemRemovals(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	viewerName := testkit.UniqueIDWithPrefix(t, "banviewer")
	viewerDID := fixtures.DID(viewerName)
	fixtures.User(t, db, viewerName+".test", viewerDID)
	untouched := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "untouched post", "")
	removedBeforeBan := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "removed before the ban", "")
	removedWhileBanned := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "removed while banned", "")
	service := newPostgresModerationService(db)
	accountModerator := accountbanfixture.New(db)
	check := func(stage string, want []string) {
		t.Helper()
		for _, read := range accountRestrictionPostReads(db, communityDID, authorDID, viewerDID) {
			t.Run(stage+"/"+read.name, func(t *testing.T) {
				assert.ElementsMatch(t, want, read.read(t))
			})
		}
	}

	removeIndexedModerationPostWithKey(t, service, removedBeforeBan, "remove-before-ban")
	accountModerator.Ban(t, authorDID, nil)
	removeIndexedModerationPostWithKey(t, service, removedWhileBanned, "remove-while-banned")
	check("while banned", nil)
	accountModerator.Unban(t, authorDID)
	check("after unban", []string{untouched.URI})
}

// TestAccountStatusAndBanAreIndependentRestrictions restricts A by an instance
// account ban, a stored #account status restriction, or both. A has post P0
// and post P1, which an instance removal took down first. While either
// restriction is present every post of A is hidden from another viewer and
// from the anonymous public, and A still sees P0; lifting one restriction
// while the other stays changes nothing; once both are gone P0 is served
// again and P1 stays removed.
func TestAccountStatusAndBanAreIndependentRestrictions(t *testing.T) {
	type stage struct {
		name   string
		apply  func(t *testing.T, db *sql.DB, moderator *accountbanfixture.Moderator, authorDID string)
		hidden bool
	}
	ban := func(t *testing.T, _ *sql.DB, moderator *accountbanfixture.Moderator, authorDID string) {
		moderator.Ban(t, authorDID, nil)
	}
	unban := func(t *testing.T, _ *sql.DB, moderator *accountbanfixture.Moderator, authorDID string) {
		moderator.Unban(t, authorDID)
	}
	setStatus := func(t *testing.T, db *sql.DB, _ *accountbanfixture.Moderator, authorDID string) {
		accountstatusfixture.Set(t, db, authorDID, accountstatus.Takendown)
	}
	clearStatus := func(t *testing.T, db *sql.DB, _ *accountbanfixture.Moderator, authorDID string) {
		accountstatusfixture.Clear(t, db, authorDID)
	}
	both := func(t *testing.T, db *sql.DB, moderator *accountbanfixture.Moderator, authorDID string) {
		ban(t, db, moderator, authorDID)
		setStatus(t, db, moderator, authorDID)
	}

	for _, ordering := range []struct {
		name   string
		stages []stage
	}{
		{
			name: "unban first",
			stages: []stage{
				{name: "banned and status restricted", apply: both, hidden: true},
				{name: "unbanned, status still restricted", apply: unban, hidden: true},
				{name: "status cleared", apply: clearStatus, hidden: false},
			},
		},
		{
			name: "status cleared first",
			stages: []stage{
				{name: "banned and status restricted", apply: both, hidden: true},
				{name: "status cleared, still banned", apply: clearStatus, hidden: true},
				{name: "unbanned", apply: unban, hidden: false},
			},
		},
		{
			name: "status only",
			stages: []stage{
				{name: "status restricted", apply: setStatus, hidden: true},
				{name: "status cleared", apply: clearStatus, hidden: false},
			},
		},
	} {
		t.Run(ordering.name, func(t *testing.T) {
			db := testkit.DB(t)
			communityDID, authorDID := moderationPostActors(t, db)
			viewerName := testkit.UniqueIDWithPrefix(t, "statusviewer")
			viewerDID := fixtures.DID(viewerName)
			fixtures.User(t, db, viewerName+".test", viewerDID)
			untouched := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "untouched post", "")
			removed := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "removed post", "")
			removeIndexedModerationPostWithKey(t, newPostgresModerationService(db), removed, "remove-before-restriction")
			moderator := accountbanfixture.New(db)
			viewers := map[string]string{"other viewer": viewerDID, "anonymous": "", "author": authorDID}

			for _, stage := range ordering.stages {
				stage.apply(t, db, moderator, authorDID)
				for viewerLabel, viewer := range viewers {
					want := []string{untouched.URI}
					if stage.hidden && viewerLabel != "author" {
						want = nil
					}
					for _, read := range accountRestrictionPostReads(db, communityDID, authorDID, viewer) {
						t.Run(stage.name+"/"+read.name+"/"+viewerLabel, func(t *testing.T) {
							assert.ElementsMatch(t, want, read.read(t))
						})
					}
				}
			}
		})
	}
}
