//go:build integration

package jetstream

import (
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/stretchr/testify/require"
)

const (
	profileAvatarA = "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm"
	profileBannerB = "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"
	profileAvatarC = "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q"
)

type profileBlobRow struct {
	ownerDID string
	blobCID  string
	kind     string
	active   bool
}

func readProfileBlobRows(t *testing.T, db *sql.DB, subjectURI string) []profileBlobRow {
	t.Helper()
	rows, err := db.Query(`SELECT owner_did, blob_cid, kind, active FROM blob_references
		WHERE subject_uri = $1 ORDER BY kind, blob_cid`, subjectURI)
	require.NoError(t, err)
	defer rows.Close()
	var found []profileBlobRow
	for rows.Next() {
		var row profileBlobRow
		require.NoError(t, rows.Scan(&row.ownerDID, &row.blobCID, &row.kind, &row.active))
		found = append(found, row)
	}
	require.NoError(t, rows.Err())
	return found
}

type indexedProfileBlobFixture struct {
	db       *sql.DB
	did      string
	uri      string
	consumer *UserEventConsumer
}

func profileBlobRecord(avatar, banner interface{}) map[string]interface{} {
	record := map[string]interface{}{"$type": CovesProfileCollection, "displayName": "Profile with images"}
	if avatar != nil {
		record["avatar"] = avatar
	}
	if banner != nil {
		record["banner"] = banner
	}
	return record
}

func profileBlob(cid string) map[string]interface{} {
	return map[string]interface{}{
		"$type": "blob", "ref": map[string]interface{}{"$link": cid}, "mimeType": "image/png", "size": 1,
	}
}

func profileBlobColumns(t *testing.T, fixture indexedProfileBlobFixture) (string, string) {
	t.Helper()
	var avatar, banner sql.NullString
	require.NoError(t, fixture.db.QueryRow(`SELECT avatar_cid, banner_cid FROM users WHERE did = $1`, fixture.did).Scan(&avatar, &banner))
	return avatar.String, banner.String
}

func profileBlobUpdatedAt(t *testing.T, fixture indexedProfileBlobFixture) time.Time {
	t.Helper()
	var updatedAt time.Time
	require.NoError(t, fixture.db.QueryRow(`SELECT updated_at FROM users WHERE did = $1`, fixture.did).Scan(&updatedAt))
	return updatedAt
}

func indexedProfileWithAvatarAndBanner(t *testing.T, gated bool) indexedProfileBlobFixture {
	t.Helper()
	db := testkit.DB(t)
	id := testkit.UniqueID(t)
	did := "did:plc:" + id + "profile"
	_, err := db.Exec(`INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`, did, id+"profile.test", bridgedTestNativePDS)
	require.NoError(t, err)
	resolver := &mockIdentityResolverForUser{}
	service := users.NewUserService(postgres.NewUserRepository(db), resolver, "", nil, "")
	var options []ConsumerOption
	rev := ""
	if gated {
		options = append(options, WithUserRevGate(NewRevGate(db)))
		rev = revB
	}
	fixture := indexedProfileBlobFixture{
		db: db, did: did, uri: "at://" + did + "/social.coves.actor.profile/self",
		consumer: NewUserEventConsumer(service, resolver, options...),
	}
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), revCommitEvent(
		did, CovesProfileCollection, "create", "self", rev, profileAvatarA,
		profileBlobUpdatedAt(t, fixture).Add(time.Minute).UnixMicro(),
		profileBlobRecord(profileBlob(profileAvatarA), profileBlob(profileBannerB)),
	)))
	avatar, banner := profileBlobColumns(t, fixture)
	require.Equal(t, profileAvatarA, avatar, "fixture: avatar indexed")
	require.Equal(t, profileBannerB, banner, "fixture: banner indexed")
	return fixture
}

func TestUserProfileBlobReferences_Create(t *testing.T) {
	fixture := indexedProfileWithAvatarAndBanner(t, false)
	avatar, banner := profileBlobColumns(t, fixture)
	require.Equal(t, profileAvatarA, avatar)
	require.Equal(t, profileBannerB, banner)
	require.Equal(t, []profileBlobRow{
		{fixture.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
		{fixture.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
	}, readProfileBlobRows(t, fixture.db, fixture.uri))
}

func TestUserProfileBlobReferences_UpdateReplacesAvatar(t *testing.T) {
	fixture := indexedProfileWithAvatarAndBanner(t, false)
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), revCommitEvent(
		fixture.did, CovesProfileCollection, "update", "self", "", profileAvatarC,
		profileBlobUpdatedAt(t, fixture).Add(time.Minute).UnixMicro(),
		profileBlobRecord(profileBlob(profileAvatarC), profileBlob(profileBannerB)),
	)))
	avatar, banner := profileBlobColumns(t, fixture)
	require.Equal(t, profileAvatarC, avatar)
	require.Equal(t, profileBannerB, banner)
	require.Equal(t, []profileBlobRow{
		{fixture.did, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "avatar", true},
		{fixture.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
		{fixture.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
	}, readProfileBlobRows(t, fixture.db, fixture.uri))
}

func TestUserProfileBlobReferences_UpdateClearsMissingOrUnusableBlob(t *testing.T) {
	for _, test := range []struct {
		name       string
		avatar     interface{}
		banner     interface{}
		wantAvatar string
		wantBanner string
		wantRows   func(string) []profileBlobRow
	}{
		{
			name: "avatar omitted", banner: profileBlob(profileBannerB), wantBanner: profileBannerB,
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
		{
			name: "banner omitted", avatar: profileBlob(profileAvatarA), wantAvatar: profileAvatarA,
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", false},
				}
			},
		},
		{
			name: "avatar blob without ref", avatar: map[string]interface{}{"$type": "blob"}, banner: profileBlob(profileBannerB), wantBanner: profileBannerB,
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
		{
			name: "avatar with malformed link", avatar: profileBlob("bafkreinotacid"), banner: profileBlob(profileBannerB), wantBanner: profileBannerB,
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
		{
			name: "avatar with truncated link", avatar: profileBlob(profileAvatarC[:len(profileAvatarC)-6]), banner: profileBlob(profileBannerB), wantBanner: profileBannerB,
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
		{
			name: "banner with malformed link", avatar: profileBlob(profileAvatarA), banner: profileBlob("bafkreinotacid"), wantAvatar: profileAvatarA,
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", false},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := indexedProfileWithAvatarAndBanner(t, false)
			require.NoError(t, fixture.consumer.HandleEvent(t.Context(), revCommitEvent(
				fixture.did, CovesProfileCollection, "update", "self", "", profileAvatarC,
				profileBlobUpdatedAt(t, fixture).Add(time.Minute).UnixMicro(), profileBlobRecord(test.avatar, test.banner),
			)))
			avatar, banner := profileBlobColumns(t, fixture)
			require.Equal(t, test.wantAvatar, avatar)
			require.Equal(t, test.wantBanner, banner)
			require.Equal(t, test.wantRows(fixture.did), readProfileBlobRows(t, fixture.db, fixture.uri))
		})
	}
}

func TestUserProfileBlobReferences_UpdateStoresCanonicalBlobCID(t *testing.T) {
	parsed, err := cid.Decode(profileAvatarC)
	require.NoError(t, err)
	base58Alias := parsed.Encode(multibase.MustNewEncoder(multibase.Base58BTC))
	require.NotEqual(t, profileAvatarC, base58Alias, "fixture: the alias must be a different encoding")

	fixture := indexedProfileWithAvatarAndBanner(t, false)
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), revCommitEvent(
		fixture.did, CovesProfileCollection, "update", "self", "", profileAvatarC,
		profileBlobUpdatedAt(t, fixture).Add(time.Minute).UnixMicro(),
		profileBlobRecord(profileBlob(base58Alias), profileBlob(profileBannerB)),
	)))
	avatar, banner := profileBlobColumns(t, fixture)
	require.Equal(t, profileAvatarC, avatar, "the stored avatar CID must equal the canonical reference CID")
	require.Equal(t, profileBannerB, banner)
	require.Equal(t, []profileBlobRow{
		{fixture.did, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "avatar", true},
		{fixture.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
		{fixture.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
	}, readProfileBlobRows(t, fixture.db, fixture.uri))
}

func TestUserProfileBlobReferences_DeleteDeactivates(t *testing.T) {
	fixture := indexedProfileWithAvatarAndBanner(t, false)
	require.NoError(t, fixture.consumer.HandleEvent(t.Context(), revCommitEvent(
		fixture.did, CovesProfileCollection, "delete", "self", "", "",
		profileBlobUpdatedAt(t, fixture).Add(time.Minute).UnixMicro(), nil,
	)))
	avatar, banner := profileBlobColumns(t, fixture)
	require.Empty(t, avatar)
	require.Empty(t, banner)
	require.Equal(t, []profileBlobRow{
		{fixture.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
		{fixture.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", false},
	}, readProfileBlobRows(t, fixture.db, fixture.uri))
}

func TestUserProfileBlobReferences_StaleUpdatesKeepReferences(t *testing.T) {
	for _, test := range []struct {
		name   string
		gated  bool
		rev    string
		offset time.Duration
	}{
		{name: "older rev with newer event time", gated: true, rev: revA, offset: time.Minute},
		{name: "revless event older than user update", offset: -time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := indexedProfileWithAvatarAndBanner(t, test.gated)
			require.NoError(t, fixture.consumer.HandleEvent(t.Context(), revCommitEvent(
				fixture.did, CovesProfileCollection, "update", "self", test.rev, profileAvatarC,
				profileBlobUpdatedAt(t, fixture).Add(test.offset).UnixMicro(),
				profileBlobRecord(profileBlob(profileAvatarC), nil),
			)))
			avatar, banner := profileBlobColumns(t, fixture)
			require.Equal(t, profileAvatarA, avatar)
			require.Equal(t, profileBannerB, banner)
			require.Equal(t, []profileBlobRow{
				{fixture.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
				{fixture.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
			}, readProfileBlobRows(t, fixture.db, fixture.uri))
		})
	}
}
