//go:build integration

package postgres

import (
	"bytes"
	"database/sql"
	"log"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nestedBackfillFirstCID  = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	nestedBackfillSecondCID = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	nestedBackfillThirdCID  = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
)

func nestedBackfillDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := testkit.DB(t)
	require.EqualValues(t, 66, testkit.MigrateDownOne(t, db, 66))
	require.EqualValues(t, 65, testkit.MigrateDownOne(t, db, 65))
	// Keep 062-064 applied: these are rows an existing deployment missed.
	return db
}

func nestedBackfillEmbed(gallery []string, attachment string) string {
	return `{"$type":"social.coves.embed.imagesWithAttachment","images":` +
		contentBackfillImagesEmbed(gallery) + `,"attachment":` + attachment + `}`
}

func nestedBackfillQuote() string {
	return `{"$type":"social.coves.embed.post","post":{"uri":"at://did:plc:quoted/social.coves.community.postv2/quoted","cid":"` + nestedBackfillFirstCID + `"}}`
}

func nestedBackfillSetEmbed(t *testing.T, db *sql.DB, table, uri, embed string) {
	t.Helper()
	require.Contains(t, []string{"posts", "comments"}, table)
	_, err := db.Exec(`UPDATE `+table+` SET embed = $2::jsonb WHERE uri = $1`, uri, embed)
	require.NoError(t, err)
}

func TestNestedBlobReferencesBackfill065_ExistingRowsOwnersAndKinds(t *testing.T) {
	db := nestedBackfillDatabase(t)
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author, commenter := "did:plc:"+testkit.UniqueID(t), "did:plc:"+testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	external := seedVisibilityPost(t, db, community, author, testkit.TID(), "nested external", createdAt)
	quoted := seedVisibilityPost(t, db, community, author, testkit.TID(), "nested quote", createdAt)
	comment := seedActorComment(t, db, commenter, quoted, testkit.TID(), createdAt)
	nestedBackfillSetEmbed(t, db, "posts", external, nestedBackfillEmbed([]string{nestedBackfillFirstCID},
		`{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/article","thumb":{"ref":{"$link":"`+nestedBackfillFirstCID+`"}},"images":`+contentBackfillImageEntries([]string{nestedBackfillSecondCID})+`}}`))
	nestedBackfillSetEmbed(t, db, "posts", quoted, nestedBackfillEmbed([]string{nestedBackfillSecondCID}, nestedBackfillQuote()))
	nestedBackfillSetEmbed(t, db, "comments", comment, nestedBackfillEmbed([]string{nestedBackfillThirdCID}, nestedBackfillQuote()))
	require.Empty(t, contentBackfillRows(t, db, external, quoted, comment))

	testkit.MigrateUp(t, db)

	assert.ElementsMatch(t, []contentBackfillReference{
		{author, nestedBackfillFirstCID, external, "post_image", true},
		{author, nestedBackfillFirstCID, external, "external_thumb", true},
		{author, nestedBackfillSecondCID, external, "post_image", true},
		{author, nestedBackfillSecondCID, quoted, "post_image", true},
		{commenter, nestedBackfillThirdCID, comment, "comment_image", true},
	}, contentBackfillRows(t, db, external, quoted, comment))
}

func TestNestedBlobReferencesBackfill065_LegacyDeletedRemovedAndUnknownSubjects(t *testing.T) {
	db := nestedBackfillDatabase(t)
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author, commenter := "did:plc:"+testkit.UniqueID(t), "did:plc:"+testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	root := seedVisibilityPost(t, db, community, author, testkit.TID(), "root", createdAt)
	legacy := seedLegacyPost(t, db, community, author, testkit.TID(), "legacy", createdAt)
	subjects := []string{legacy}
	want := []contentBackfillReference{{community, nestedBackfillFirstCID, legacy, "post_image", true}}
	nestedBackfillSetEmbed(t, db, "posts", legacy, nestedBackfillEmbed([]string{nestedBackfillFirstCID}, nestedBackfillQuote()))
	legacyAction := nestedBackfillRemoval(t, db, legacy)
	for _, fixture := range []struct{ table, state, owner, kind string }{
		{"posts", "deleted", author, "post_image"},
		{"comments", "deleted", commenter, "comment_image"},
		{"posts", "removed", author, "post_image"},
		{"comments", "removed", commenter, "comment_image"},
	} {
		var uri string
		if fixture.table == "posts" {
			uri = seedVisibilityPost(t, db, community, author, testkit.TID(), fixture.state, createdAt)
		} else {
			uri = seedActorComment(t, db, commenter, root, testkit.TID(), createdAt)
		}
		nestedBackfillSetEmbed(t, db, fixture.table, uri, nestedBackfillEmbed([]string{nestedBackfillFirstCID}, nestedBackfillQuote()))
		if fixture.state == "deleted" {
			_, err := db.Exec(`UPDATE `+fixture.table+` SET deleted_at = $2 WHERE uri = $1`, uri, createdAt)
			require.NoError(t, err)
		} else {
			seedModerationDecision(t, db, uri, "removal", "", true)
		}
		subjects = append(subjects, uri)
		want = append(want, contentBackfillReference{fixture.owner, nestedBackfillFirstCID, uri, fixture.kind, fixture.state != "deleted"})
	}
	unknown := seedVisibilityPost(t, db, community, author, testkit.TID(), "unknown collection", createdAt)
	unknownURI := strings.Replace(unknown, "/social.coves.community.postv2/", "/app.bsky.feed.post/", 1)
	_, err := db.Exec(`UPDATE posts SET uri = $2 WHERE uri = $1`, unknown, unknownURI)
	require.NoError(t, err)
	nestedBackfillSetEmbed(t, db, "posts", unknownURI, nestedBackfillEmbed([]string{nestedBackfillSecondCID}, nestedBackfillQuote()))
	subjects = append(subjects, unknownURI)

	testkit.MigrateUp(t, db)

	assert.ElementsMatch(t, want, contentBackfillRows(t, db, subjects...))
	var legacyBlocks []nestedBackfillBlock
	for _, block := range nestedBackfillBlocks(t, db) {
		if block.ActionID == legacyAction {
			legacyBlocks = append(legacyBlocks, block)
		}
	}
	assert.Equal(t, []nestedBackfillBlock{
		{sql.NullString{String: community, Valid: true}, nestedBackfillFirstCID, legacyAction, true},
	}, legacyBlocks, "legacy removal blocks must belong to the community")
	for _, block := range legacyBlocks {
		assert.NotEqual(t, sql.NullString{String: author, Valid: true}, block.OwnerDID, "legacy removal must not block the author's image")
	}
}

func TestNestedBlobReferencesBackfill065_CanonicalizationLimitsAndCensus(t *testing.T) {
	db := nestedBackfillDatabase(t)
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author, commenter := "did:plc:"+testkit.UniqueID(t), "did:plc:"+testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	parsed, err := cid.Decode(nestedBackfillFirstCID)
	require.NoError(t, err)
	alias, err := parsed.StringOfBase(multibase.Base58BTC)
	require.NoError(t, err)
	repeated := func(value string, count int) []string {
		result := make([]string, count)
		for index := range result {
			result[index] = value
		}
		return result
	}
	external := func(images []string) string {
		return `{"$type":"social.coves.embed.external","external":{"uri":"https://example.com/article","images":` + contentBackfillImageEntries(images) + `}}`
	}
	var subjects []string
	for _, fixture := range []struct{ name, embed string }{
		{"malformed", nestedBackfillEmbed([]string{alias, nestedBackfillFirstCID, "invalid-private-blob"}, nestedBackfillQuote())},
		{"eight", nestedBackfillEmbed(repeated(nestedBackfillFirstCID, 4), external(repeated(nestedBackfillSecondCID, 4)))},
		{"nine combined", nestedBackfillEmbed(repeated(nestedBackfillFirstCID, 5), external(repeated(nestedBackfillSecondCID, 4)))},
		{"nine gallery", nestedBackfillEmbed(repeated(nestedBackfillFirstCID, 9), nestedBackfillQuote())},
		{"already referenced", nestedBackfillEmbed([]string{nestedBackfillSecondCID}, nestedBackfillQuote())},
	} {
		uri := seedVisibilityPost(t, db, community, author, testkit.TID(), fixture.name, createdAt)
		nestedBackfillSetEmbed(t, db, "posts", uri, fixture.embed)
		subjects = append(subjects, uri)
	}
	comment := seedActorComment(t, db, commenter, subjects[0], testkit.TID(), createdAt)
	// A historical legacy-CID blob and a malformed entry exercise comment wiring too.
	nestedBackfillSetEmbed(t, db, "comments", comment, `{"$type":"social.coves.embed.imagesWithAttachment","images":{"$type":"social.coves.embed.images","images":[{"image":{"cid":"`+nestedBackfillThirdCID+`"}},null]},"attachment":`+nestedBackfillQuote()+`}`)
	_, err = db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active) VALUES ($1, $2, $3, 'post_image', true)`, author, nestedBackfillSecondCID, subjects[4])
	require.NoError(t, err)

	var captured bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	testkit.MigrateUp(t, db)
	log.SetOutput(previousOutput)

	assert.ElementsMatch(t, []contentBackfillReference{
		{author, nestedBackfillFirstCID, subjects[0], "post_image", true},
		{author, nestedBackfillFirstCID, subjects[1], "post_image", true},
		{author, nestedBackfillSecondCID, subjects[1], "post_image", true},
		{author, nestedBackfillSecondCID, subjects[4], "post_image", true},
		{commenter, nestedBackfillThirdCID, comment, "comment_image", true},
	}, contentBackfillRows(t, db, append(subjects, comment)...))
	var census string
	for _, line := range strings.Split(captured.String(), "\n") {
		if strings.Contains(line, "nested blob references backfill:") {
			census = line
		}
	}
	require.NotEmpty(t, census, "missing nested backfill census")
	for label, expected := range map[string]int{
		"posts scanned": 5, "comments scanned": 1, "records over image limit": 2,
		"records with skipped or duplicate blob entries": 3, "references extracted": 5, "references inserted": 4,
	} {
		match := regexp.MustCompile(regexp.QuoteMeta(label) + `=(\d+)`).FindStringSubmatch(census)
		require.Len(t, match, 2, "missing count %s", label)
		actual, err := strconv.Atoi(match[1])
		require.NoError(t, err)
		assert.Equal(t, expected, actual, "%s", label)
	}
	for _, sensitive := range append(subjects, comment, author, commenter, community, alias, nestedBackfillFirstCID, nestedBackfillSecondCID, nestedBackfillThirdCID, "invalid-private-blob") {
		assert.NotContains(t, captured.String(), sensitive, "migration census must contain counts only")
	}
}
