//go:build integration

package postgres_test

import (
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/discover"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func labelViewURIs(views []*posts.PostView) []string {
	uris := make([]string, 0, len(views))
	for _, view := range views {
		uris = append(uris, view.URI)
	}
	return uris
}

func labelViewByURI(t *testing.T, views []*posts.PostView, uri string) *posts.PostView {
	t.Helper()
	for _, view := range views {
		if view.URI == uri {
			return view
		}
	}
	t.Fatalf("post %s absent from read path", uri)
	return nil
}

func labelViewJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

// labelViewPages is one paged walk of a read path: the URIs in served order and
// the cursor returned after each page.
type labelViewPages struct {
	uris    []string
	cursors []string
}

func walkLabelViewPages(t *testing.T, page func(*testing.T, *string) ([]*posts.PostView, *string)) labelViewPages {
	t.Helper()
	var walked labelViewPages
	var cursor *string
	for range 10 {
		views, next := page(t, cursor)
		walked.uris = append(walked.uris, labelViewURIs(views)...)
		if next == nil {
			return walked
		}
		walked.cursors = append(walked.cursors, *next)
		cursor = next
	}
	t.Fatalf("paged walk did not end after 10 pages: %v", walked.uris)
	return walked
}

func labelViewExpected(sources ...posts.ModerationSourceView) *posts.ModerationView {
	return &posts.ModerationView{State: moderation.ModerationStateClear, ContentLabels: []posts.ContentLabelView{{
		Value: moderation.LabelNSFW, Sources: sources,
	}}}
}

func TestModerationLabelServedPostViews(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	viewerName := testkit.UniqueIDWithPrefix(t, "labelviewer")
	viewerDID := fixtures.DID(viewerName)
	fixtures.User(t, db, viewerName+".test", viewerDID)
	_, err := db.ExecContext(t.Context(), `INSERT INTO community_subscriptions (user_did, community_did, subscribed_at) VALUES ($1, $2, NOW())`, viewerDID, communityDID)
	require.NoError(t, err)
	// Large score gaps keep the Hot order stable across snapshot reuse; all
	// three posts are accepted and old enough to contribute to Hot history.
	// Distinct creation times make every ordering, not only tie-breaks, count.
	controlFirst := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "label view search control first", "")
	labelled := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "label view search target", "")
	controlLast := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "label view search control last", "")
	for _, entry := range []struct {
		uri   string
		score int
		age   time.Duration
	}{{controlFirst.URI, 900, 4 * time.Hour}, {labelled.URI, 500, 5 * time.Hour}, {controlLast.URI, 100, 6 * time.Hour}} {
		_, err := db.ExecContext(t.Context(), `UPDATE posts SET score = $2, upvote_count = $2, created_at = $3 WHERE uri = $1`, entry.uri, entry.score, time.Now().Add(-entry.age).Truncate(time.Second))
		require.NoError(t, err)
	}
	_, err = db.ExecContext(t.Context(), `UPDATE posts SET content_labels = '{"values":[{"val":"spoiler"}]}'::jsonb WHERE uri = $1`, labelled.URI)
	require.NoError(t, err)
	postRepo := postgres.NewPostRepository(db)
	feedRepo := postgres.NewCommunityFeedRepository(db, "label-views-feed-secret")
	timelineRepo := postgres.NewTimelineRepository(db, "label-views-timeline-secret")
	discoverRepo := postgres.NewDiscoverRepository(db, "label-views-discover-secret")
	service := newPostgresModerationService(db)
	allURIs := []string{controlFirst.URI, labelled.URI, controlLast.URI}
	reads := []struct {
		name string
		read func(*testing.T) []*posts.PostView
	}{
		{"post.get / GetViewsByURIs", func(t *testing.T) []*posts.PostView {
			found, err := postRepo.GetViewsByURIs(t.Context(), allURIs, "")
			require.NoError(t, err)
			views := make([]*posts.PostView, 0, len(allURIs))
			for _, uri := range allURIs {
				require.Contains(t, found, uri)
				views = append(views, found[uri])
			}
			return views
		}},
		{"actor.getPosts / GetByAuthor", func(t *testing.T) []*posts.PostView {
			views, _, err := postRepo.GetByAuthor(t.Context(), posts.GetAuthorPostsRequest{ActorDID: authorDID, Limit: 50})
			require.NoError(t, err)
			return views
		}},
		{"community feed", func(t *testing.T) []*posts.PostView {
			feed, _, err := feedRepo.GetCommunityFeed(t.Context(), communityFeeds.GetCommunityFeedRequest{Community: communityDID, Sort: "new", Timeframe: "all", Limit: 50})
			require.NoError(t, err)
			views := make([]*posts.PostView, 0, len(feed))
			for _, entry := range feed {
				views = append(views, entry.Post)
			}
			return views
		}},
		{"searchPosts", func(t *testing.T) []*posts.PostView {
			feed, _, err := feedRepo.SearchPosts(t.Context(), communityFeeds.SearchPostsRequest{Query: "label view search", Community: communityDID, Sort: "relevance", Timeframe: "all", Limit: 50})
			require.NoError(t, err)
			views := make([]*posts.PostView, 0, len(feed))
			for _, entry := range feed {
				views = append(views, entry.Post)
			}
			return views
		}},
		{"timeline", func(t *testing.T) []*posts.PostView {
			feed, _, err := timelineRepo.GetTimeline(t.Context(), timeline.GetTimelineRequest{UserDID: viewerDID, Sort: "new", Limit: 50})
			require.NoError(t, err)
			views := make([]*posts.PostView, 0, len(feed))
			for _, entry := range feed {
				views = append(views, entry.Post)
			}
			return views
		}},
		{"Discover new", func(t *testing.T) []*posts.PostView {
			feed, _, err := discoverRepo.GetDiscover(t.Context(), discover.GetDiscoverRequest{Sort: "new", Timeframe: "all", Limit: 50})
			require.NoError(t, err)
			views := make([]*posts.PostView, 0, len(feed))
			for _, entry := range feed {
				views = append(views, entry.Post)
			}
			return views
		}},
		{"getComments / VisibleHeaderView", func(t *testing.T) []*posts.PostView {
			views := make([]*posts.PostView, 0, len(allURIs))
			for _, uri := range allURIs {
				view, err := postRepo.VisibleHeaderView(t.Context(), uri, "")
				require.NoError(t, err)
				require.NotNil(t, view)
				views = append(views, view)
			}
			return views
		}},
	}
	// Walk each paged path one post per page, so a label-driven change to rank
	// or cursor building moves a post across a page boundary.
	pagedReads := []struct {
		name string
		// stableCursor is false for hot sort, whose cursor embeds the query time.
		stableCursor bool
		page         func(*testing.T, *string) ([]*posts.PostView, *string)
	}{
		{"community feed hot", false, func(t *testing.T, cursor *string) ([]*posts.PostView, *string) {
			feed, next, err := feedRepo.GetCommunityFeed(t.Context(), communityFeeds.GetCommunityFeedRequest{Community: communityDID, Sort: "hot", Timeframe: "all", Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			views := make([]*posts.PostView, 0, len(feed))
			for _, entry := range feed {
				views = append(views, entry.Post)
			}
			return views, next
		}},
		{"searchPosts relevance", true, func(t *testing.T, cursor *string) ([]*posts.PostView, *string) {
			feed, next, err := feedRepo.SearchPosts(t.Context(), communityFeeds.SearchPostsRequest{Query: "label view search", Community: communityDID, Sort: "relevance", Timeframe: "all", Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			views := make([]*posts.PostView, 0, len(feed))
			for _, entry := range feed {
				views = append(views, entry.Post)
			}
			return views, next
		}},
		{"actor.getPosts", true, func(t *testing.T, cursor *string) ([]*posts.PostView, *string) {
			views, next, err := postRepo.GetByAuthor(t.Context(), posts.GetAuthorPostsRequest{ActorDID: authorDID, Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			return views, next
		}},
	}
	baselinePages := make(map[string]labelViewPages, len(pagedReads))
	for _, read := range pagedReads {
		walked := walkLabelViewPages(t, read.page)
		require.ElementsMatch(t, allURIs, walked.uris, "%s must page every accepted post once", read.name)
		require.GreaterOrEqual(t, len(walked.cursors), 2, "%s must be compared across several pages", read.name)
		baselinePages[read.name] = walked
	}
	require.Equal(t, []string{controlFirst.URI, labelled.URI, controlLast.URI}, baselinePages["community feed hot"].uris, "Hot must rank the labelled post between the controls")
	checkPages := func(stage string) {
		t.Helper()
		for _, read := range pagedReads {
			t.Run(stage+"/paged "+read.name, func(t *testing.T) {
				walked := walkLabelViewPages(t, read.page)
				assert.Equal(t, baselinePages[read.name].uris, walked.uris, "label must not move a post across pages")
				if read.stableCursor {
					assert.Equal(t, baselinePages[read.name].cursors, walked.cursors, "label must not change cursors")
				} else {
					assert.Len(t, walked.cursors, len(baselinePages[read.name].cursors))
				}
			})
		}
	}

	baselineOrder := make(map[string][]string, len(reads))
	baselineRecord := make(map[string][]byte, len(reads))
	for _, read := range reads {
		// The after-apply and after-retract checks compare against this baseline.
		if !t.Run("before/"+read.name, func(t *testing.T) {
			views := read.read(t)
			baselineOrder[read.name] = labelViewURIs(views)
			require.Len(t, views, 3, "all three accepted posts must be visible before moderation")
			require.ElementsMatch(t, allURIs, baselineOrder[read.name])
			baselineRecord[read.name] = labelViewJSON(t, labelViewByURI(t, views, labelled.URI).Record)
			require.Contains(t, string(baselineRecord[read.name]), `"labels"`, "the author self-label must be present so an overwritten record.labels cannot pass")
			for _, view := range views {
				require.Nil(t, view.Moderation, "no decisions exist before apply")
				require.NotContains(t, string(labelViewJSON(t, view)), `"moderation"`)
			}
		}) {
			t.Fatalf("baseline read %s failed", read.name)
		}
	}

	readHot := func(t *testing.T, limit int, cursor *string) ([]*posts.PostView, *string) {
		t.Helper()
		feed, next, err := discoverRepo.GetDiscover(t.Context(), discover.GetDiscoverRequest{Sort: "hot", Limit: limit, Cursor: cursor})
		require.NoError(t, err)
		views := make([]*posts.PostView, 0, len(feed))
		for _, entry := range feed {
			views = append(views, entry.Post)
		}
		return views, next
	}
	first, preLabelCursor := readHot(t, 1, nil)
	require.Equal(t, []string{controlFirst.URI}, labelViewURIs(first), "Hot must page the unlabelled high-ranked control first")
	require.NotNil(t, preLabelCursor)
	hotRefreshBaseline, _ := readHot(t, 2, nil)
	hotContinuationBaseline, _ := readHot(t, 2, preLabelCursor)
	require.Equal(t, []string{controlFirst.URI, labelled.URI}, labelViewURIs(hotRefreshBaseline))
	require.Equal(t, []string{labelled.URI, controlLast.URI}, labelViewURIs(hotContinuationBaseline))
	for _, view := range append(hotRefreshBaseline, hotContinuationBaseline...) {
		require.Nil(t, view.Moderation)
	}
	hotRecord := labelViewJSON(t, labelViewByURI(t, hotContinuationBaseline, labelled.URI).Record)

	label := labelPost(t, service, fixtures.DID("labelviewsadmin"), labelled, "v0", "views-apply")
	want := labelViewExpected(posts.ModerationSourceView{AuthorityDID: fixtures.InstanceDID(), Scope: posts.ModerationScopeView{Kind: moderation.ScopeInstance}})
	for _, read := range reads {
		t.Run("after apply/"+read.name, func(t *testing.T) {
			views := read.read(t)
			assert.Equal(t, baselineOrder[read.name], labelViewURIs(views), "label must not change ordering or membership")
			target := labelViewByURI(t, views, labelled.URI)
			assert.Equal(t, baselineRecord[read.name], labelViewJSON(t, target.Record), "record.labels must be served verbatim")
			assert.Equal(t, want, target.Moderation)
			assert.Contains(t, string(labelViewJSON(t, target)), `"moderation":{"state":"clear","contentLabels":[{"value":"nsfw","sources":[{"authorityDid":"`+fixtures.InstanceDID()+`","scope":{"kind":"instance"}}]}]}`)
			for _, control := range []string{controlFirst.URI, controlLast.URI} {
				view := labelViewByURI(t, views, control)
				assert.Nil(t, view.Moderation)
				assert.NotContains(t, string(labelViewJSON(t, view)), `"moderation"`)
			}
		})
	}
	checkHot := func(stage string, expected *posts.ModerationView) {
		t.Helper()
		for _, page := range []struct {
			name   string
			cursor *string
			want   []string
		}{
			{"refreshed first page", nil, labelViewURIs(hotRefreshBaseline)},
			{"continuation from pre-label cursor", preLabelCursor, labelViewURIs(hotContinuationBaseline)},
		} {
			t.Run(stage+"/Discover hot/"+page.name, func(t *testing.T) {
				views, _ := readHot(t, 2, page.cursor)
				assert.Equal(t, page.want, labelViewURIs(views), "reused snapshot must keep page order and membership")
				target := labelViewByURI(t, views, labelled.URI)
				assert.Equal(t, hotRecord, labelViewJSON(t, target.Record))
				assert.Equal(t, expected, target.Moderation)
				for _, view := range views {
					if view.URI != labelled.URI {
						assert.Nil(t, view.Moderation)
					}
				}
			})
		}
	}
	checkHot("after apply", want)
	checkPages("after apply")
	_, err = service.RetractContentLabel(t.Context(), fixtures.DID("labelviewsadmin"), moderation.RetractContentLabelRequest{
		ActionID: label.Action.ID, ReviewedSubject: &labelled, ExpectedVersion: "v1", IdempotencyKey: "views-retract",
	})
	require.NoError(t, err)
	for _, read := range reads {
		t.Run("after retract/"+read.name, func(t *testing.T) {
			views := read.read(t)
			assert.Equal(t, baselineOrder[read.name], labelViewURIs(views))
			assert.Equal(t, baselineRecord[read.name], labelViewJSON(t, labelViewByURI(t, views, labelled.URI).Record))
			for _, view := range views {
				assert.Nil(t, view.Moderation)
				assert.NotContains(t, string(labelViewJSON(t, view)), `"moderation"`)
			}
		})
	}
	checkHot("after retract", nil)
	checkPages("after retract")
}

func TestModerationLabelDecisionSourcesInServedViews(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	local := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "multi-source", "")
	inactive := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "inactive label", "")
	foreign := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "foreign label", "")
	repo := postgres.NewPostRepository(db)
	for _, uri := range []string{local.URI, inactive.URI, foreign.URI} {
		views, err := repo.GetViewsByURIs(t.Context(), []string{uri}, "")
		require.NoError(t, err)
		require.Contains(t, views, uri)
		require.Nil(t, views[uri].Moderation, "raw fixtures have not been inserted yet")
	}
	labelPost(t, newPostgresModerationService(db), fixtures.DID("labelsourcesadmin"), local, "v0", "sources-local")
	foreignAuthority := fixtures.DID("foreignlabels")
	insertDecision := func(subject moderation.StrongRef, authority, scopeKind, scopeCommunity string, active bool) {
		t.Helper()
		id := testkit.TID()
		_, err := db.ExecContext(t.Context(), `
			INSERT INTO moderation_actions
				(id, actor_did, authority_did, scope_kind, scope_community_did, subject_uri, subject_collection,
				 subject_community_did, observed_cid, action, label_value, origin, created_at)
			VALUES ($1, $2, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, 'label', 'nsfw', 'inherited', NOW())
		`, id, authority, scopeKind, scopeCommunity, subject.URI, moderation.PostV2Collection, communityDID, subject.CID)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `
			INSERT INTO moderation_decisions
				(authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
			VALUES ($1, $2, NULLIF($3, ''), $4, 'label', 'nsfw', $5, $6)
		`, authority, scopeKind, scopeCommunity, subject.URI, id, active)
		require.NoError(t, err)
	}
	insertDecision(inactive, foreignAuthority, moderation.ScopeInstance, "", false)
	insertDecision(foreign, foreignAuthority, "community", communityDID, true)
	insertDecision(local, foreignAuthority, moderation.ScopeInstance, "", true)
	views, err := repo.GetViewsByURIs(t.Context(), []string{inactive.URI, foreign.URI, local.URI}, "")
	require.NoError(t, err)
	require.Len(t, views, 3)
	assert.Nil(t, views[inactive.URI].Moderation, "an inactive decision must not produce a label")
	assert.Equal(t, labelViewExpected(posts.ModerationSourceView{
		AuthorityDID: foreignAuthority, Scope: posts.ModerationScopeView{Kind: "community", CommunityDID: communityDID},
	}), views[foreign.URI].Moderation, "a foreign decision must retain its own authority and community scope")
	assert.Equal(t, labelViewExpected(
		posts.ModerationSourceView{AuthorityDID: foreignAuthority, Scope: posts.ModerationScopeView{Kind: moderation.ScopeInstance}},
		posts.ModerationSourceView{AuthorityDID: fixtures.InstanceDID(), Scope: posts.ModerationScopeView{Kind: moderation.ScopeInstance}},
	), views[local.URI].Moderation, "the same value from two authorities must aggregate into one label with sorted sources")
}
