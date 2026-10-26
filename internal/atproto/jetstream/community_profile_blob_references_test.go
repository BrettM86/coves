//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type communityBlobFixture struct {
	db       *sql.DB
	did      string
	handle   string
	name     string
	subject  string
	consumer *CommunityEventConsumer
}

func newCommunityBlobFixture(t *testing.T, gated bool) communityBlobFixture {
	t.Helper()
	db := testkit.DB(t)
	id := testkit.UniqueID(t)
	fixture := communityBlobFixture{
		db: db, did: fixtures.DID("profileblob" + id),
		handle: "c-blob" + id + ".test.local", name: "blob" + id,
	}
	fixture.subject = "at://" + fixture.did + "/social.coves.community.profile/self"
	repo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	if gated {
		fixture.consumer = NewCommunityEventConsumer(repo, "did:web:test.local", true, nil, WithCommunityRevGate(NewRevGate(db)))
	} else {
		fixture.consumer = NewCommunityEventConsumer(repo, "did:web:test.local", true, nil)
	}
	return fixture
}

func (f communityBlobFixture) event(op, rev, avatar, banner string) *JetstreamEvent {
	base := communityProfileEvent(f.did, f.handle, f.name, rev)
	if avatar != "" {
		base.Commit.Record["avatar"] = profileBlob(avatar)
	}
	if banner != "" {
		base.Commit.Record["banner"] = profileBlob(banner)
	}
	return revCommitEvent(f.did, conflictProfileCollection, op, "self", rev, base.Commit.CID, base.TimeUS, base.Commit.Record)
}

func indexedCommunityWithBlobs(t *testing.T, gated bool) communityBlobFixture {
	t.Helper()
	f := newCommunityBlobFixture(t, gated)
	rev := ""
	if gated {
		rev = revA
	}
	require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("create", rev, profileAvatarA, profileBannerB)))
	return f
}

func communityBlobNullableColumns(t *testing.T, f communityBlobFixture) (sql.NullString, sql.NullString) {
	t.Helper()
	var avatar, banner sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT avatar_cid, banner_cid FROM communities WHERE did = $1`, f.did).Scan(&avatar, &banner))
	return avatar, banner
}

func communityBlobColumns(t *testing.T, f communityBlobFixture) (string, string) {
	t.Helper()
	avatar, banner := communityBlobNullableColumns(t, f)
	return avatar.String, banner.String
}

func communityBlobRowSnapshot(t *testing.T, f communityBlobFixture) []byte {
	t.Helper()
	var snapshot []byte
	require.NoError(t, f.db.QueryRow(`SELECT to_jsonb(c) FROM communities c WHERE did = $1`, f.did).Scan(&snapshot))
	return snapshot
}

func TestCommunityProfileBlobReferences_Create(t *testing.T) {
	f := indexedCommunityWithBlobs(t, false)
	avatar, banner := communityBlobColumns(t, f)
	require.Equal(t, profileAvatarA, avatar)
	require.Equal(t, profileBannerB, banner)
	require.Equal(t, []profileBlobRow{
		{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
		{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
	}, readProfileBlobRows(t, f.db, f.subject))
}

func TestCommunityProfileBlobReferences_ExistingRowFirehoseCreate(t *testing.T) {
	for _, test := range []struct {
		name  string
		seed  func(*testing.T, communityBlobFixture, *JetstreamEvent)
		fails bool
	}{
		{
			name: "synchronous repository create",
			seed: func(t *testing.T, f communityBlobFixture, event *JetstreamEvent) {
				t.Helper()
				createdAt, err := time.Parse(time.RFC3339, event.Commit.Record["createdAt"].(string))
				require.NoError(t, err)
				_, err = postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed()).Create(t.Context(), &communities.Community{
					DID: f.did, Handle: f.handle, Name: f.name, DisplayName: "Conflict " + f.name,
					OwnerDID: f.did, CreatedByDID: "did:plc:conflicttestcreator", HostedByDID: "did:web:test.local",
					Visibility: "public", AllowExternalDiscovery: true, CreatedAt: createdAt,
					RecordURI: f.subject, RecordCID: event.Commit.CID,
					AvatarCID: profileAvatarA, BannerCID: profileBannerB,
				})
				require.NoError(t, err)
			},
		},
		{name: "SQL row with stale active avatar", seed: seedCommunityBlobRowWithStaleReference},
		{name: "rebuild failure is retryable and atomic", seed: seedCommunityBlobRowWithStaleReference, fails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCommunityBlobFixture(t, false)
			event := f.event("create", "", profileAvatarA, profileBannerB)
			test.seed(t, f, event)
			var beforeCommunity []byte
			var beforeRows []profileBlobRow
			if test.fails {
				beforeCommunity = communityBlobRowSnapshot(t, f)
				beforeRows = readProfileBlobRows(t, f.db, f.subject)
				// The trigger rejects every new reference for this profile subject.
				_, err := f.db.Exec(`CREATE FUNCTION reject_community_blob_reference_insert() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN
						IF NEW.subject_uri = TG_ARGV[0] THEN RAISE EXCEPTION 'blocked reference insert'; END IF;
						RETURN NEW;
					END $$`)
				require.NoError(t, err)
				_, err = f.db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_community_blob_reference_insert
					BEFORE INSERT ON blob_references FOR EACH ROW
					EXECUTE FUNCTION reject_community_blob_reference_insert('%s')`, f.subject))
				require.NoError(t, err)
			}
			err := f.consumer.HandleEvent(t.Context(), event)
			if test.fails {
				require.Error(t, err, "a failed reference rebuild must be retried")
				require.False(t, errors.Is(err, ErrPermanentEvent), "reference storage failure is retryable")
				require.Equal(t, beforeCommunity, communityBlobRowSnapshot(t, f))
				require.Equal(t, beforeRows, readProfileBlobRows(t, f.db, f.subject))
				return
			}
			require.NoError(t, err)
			avatar, banner := communityBlobColumns(t, f)
			require.Equal(t, profileAvatarA, avatar)
			require.Equal(t, profileBannerB, banner)
			want := []profileBlobRow{
				{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
				{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
			}
			if test.name == "SQL row with stale active avatar" {
				want = append([]profileBlobRow{{f.did, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "avatar", false}}, want...)
			}
			require.Equal(t, want, readProfileBlobRows(t, f.db, f.subject))
		})
	}
}

func seedCommunityBlobRowWithStaleReference(t *testing.T, f communityBlobFixture, _ *JetstreamEvent) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO communities
		(did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at, avatar_cid, banner_cid)
		VALUES ($1, $2, $1, $3, $4, $5, $6, NOW(), $7, $8)`,
		f.did, f.name, "did:plc:conflicttestcreator", "did:web:test.local", f.handle, "", profileAvatarA, profileBannerB)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
		VALUES ($1, $2, $3, 'avatar', true)`, f.did, profileAvatarC, f.subject)
	require.NoError(t, err)
}

func TestCommunityProfileBlobReferences_OlderEvent(t *testing.T) {
	for _, test := range []struct {
		name  string
		gated bool
	}{
		{name: "ungated create replay"},
		{name: "older update rev omits avatar", gated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCommunityBlobFixture(t, test.gated)
			rev := ""
			if test.gated {
				rev = revA
			}
			first := f.event("create", rev, profileAvatarA, profileBannerB)
			require.NoError(t, f.consumer.HandleEvent(t.Context(), first))
			nextRev := ""
			if test.gated {
				nextRev = revB
			}
			require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("update", nextRev, profileAvatarC, profileBannerB)))
			if test.gated {
				before := communityBlobRowSnapshot(t, f)
				// The stale record omits the avatar; it must not change any stored field.
				require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("update", revA, "", profileBannerB)))
				var recordCID string
				require.NoError(t, f.db.QueryRow(`SELECT record_cid FROM communities WHERE did = $1`, f.did).Scan(&recordCID))
				require.Equal(t, "bafyconflict3lrevtestaa2b", recordCID, "the older profile must not replace the newer record CID")
				require.Equal(t, before, communityBlobRowSnapshot(t, f))
			} else {
				require.NoError(t, f.consumer.HandleEvent(t.Context(), first))
			}
			avatar, banner := communityBlobColumns(t, f)
			require.Equal(t, profileAvatarC, avatar)
			require.Equal(t, profileBannerB, banner)
			require.Equal(t, []profileBlobRow{
				{f.did, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "avatar", true},
				{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
				{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
			}, readProfileBlobRows(t, f.db, f.subject))
		})
	}
}

func TestCommunityProfileBlobReferences_Update(t *testing.T) {
	for _, test := range []struct {
		name                   string
		avatar, banner         string
		malformedAvatar        bool
		wantAvatar, wantBanner sql.NullString
		wantRows               func(string) []profileBlobRow
	}{
		{
			name: "replace avatar", avatar: profileAvatarC, banner: profileBannerB,
			wantAvatar: sql.NullString{String: profileAvatarC, Valid: true},
			wantBanner: sql.NullString{String: profileBannerB, Valid: true},
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafkreicy44vctf2bgqnn5wwzdern7bc2khwi7ku2r66bozl4x6bsrvuj2q", "avatar", true},
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
		{
			name: "omitted avatar clears stored column", banner: profileBannerB,
			wantBanner: sql.NullString{String: profileBannerB, Valid: true},
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
		{
			name: "omitted banner clears stored column", avatar: profileAvatarA,
			wantAvatar: sql.NullString{String: profileAvatarA, Valid: true},
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", false},
				}
			},
		},
		{
			name: "malformed avatar ref clears stored column", malformedAvatar: true, banner: profileBannerB,
			wantBanner: sql.NullString{String: profileBannerB, Valid: true},
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := indexedCommunityWithBlobs(t, false)
			event := f.event("update", "", test.avatar, test.banner)
			if test.malformedAvatar {
				event.Commit.Record["avatar"] = map[string]interface{}{"$type": "blob", "ref": map[string]interface{}{}, "mimeType": "image/png", "size": 128}
			}
			require.NoError(t, f.consumer.HandleEvent(t.Context(), event))
			avatar, banner := communityBlobNullableColumns(t, f)
			require.Equal(t, test.wantAvatar, avatar, "avatar CID must match, including SQL NULL")
			require.Equal(t, test.wantBanner, banner, "banner CID must match, including SQL NULL")
			require.Equal(t, test.wantRows(f.did), readProfileBlobRows(t, f.db, f.subject))
		})
	}
}

func TestCommunityProfileBlobReferences_OmitBothClearsView(t *testing.T) {
	f := indexedCommunityWithBlobs(t, false)
	// The nil-resolver consumer fixture has no PDS URL; seed the configured
	// endpoint so the pre-update view demonstrably advertises both images.
	_, err := f.db.ExecContext(t.Context(), `UPDATE communities SET pds_url = $2 WHERE did = $1`, f.did, testkit.Endpoints().PDS.BaseURL)
	require.NoError(t, err)
	repo := postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed())
	service := communities.NewCommunityService(repo, "", "did:web:test.local", "test.local", nil, nil, nil, nil)
	before, err := service.GetCommunity(t.Context(), f.did)
	require.NoError(t, err)
	require.NotEmpty(t, before.ToCommunityViewDetailed().Avatar)
	require.NotEmpty(t, before.ToCommunityViewDetailed().Banner)

	require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("update", "", "", "")))
	avatar, banner := communityBlobNullableColumns(t, f)
	require.False(t, avatar.Valid, "omitted avatar must be SQL NULL")
	require.False(t, banner.Valid, "omitted banner must be SQL NULL")
	require.Equal(t, []profileBlobRow{
		{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
		{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", false},
	}, readProfileBlobRows(t, f.db, f.subject))
	community, err := service.GetCommunity(t.Context(), f.did)
	require.NoError(t, err)
	view := community.ToCommunityViewDetailed()
	require.Empty(t, view.Avatar)
	require.Empty(t, view.Banner)
}

func TestCommunityProfileBlobReferences_ClearThenReaddSameAvatar(t *testing.T) {
	f := indexedCommunityWithBlobs(t, true)
	require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("update", revB, "", profileBannerB)))
	avatar, banner := communityBlobNullableColumns(t, f)
	require.False(t, avatar.Valid, "the intermediate omitted avatar must be SQL NULL")
	require.Equal(t, sql.NullString{String: profileBannerB, Valid: true}, banner)
	require.Equal(t, []profileBlobRow{
		{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
		{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
	}, readProfileBlobRows(t, f.db, f.subject))

	require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("update", revC, profileAvatarA, profileBannerB)))
	avatar, banner = communityBlobNullableColumns(t, f)
	require.Equal(t, sql.NullString{String: profileAvatarA, Valid: true}, avatar)
	require.Equal(t, sql.NullString{String: profileBannerB, Valid: true}, banner)
	require.Equal(t, []profileBlobRow{
		{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
		{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
	}, readProfileBlobRows(t, f.db, f.subject))
}

func TestCommunityProfileBlobReferences_Delete(t *testing.T) {
	f := indexedCommunityWithBlobs(t, false)
	require.NoError(t, f.consumer.HandleEvent(t.Context(), revCommitEvent(
		f.did, conflictProfileCollection, "delete", "self", "", "", 0, nil)))
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM communities WHERE did = $1`, f.did).Scan(&count))
	require.Zero(t, count)
	require.Equal(t, []profileBlobRow{
		{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
		{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", false},
	}, readProfileBlobRows(t, f.db, f.subject))
}

// capturedLogs records every slog record while installed as the default handler.
type capturedLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capturedLogs) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturedLogs) Handle(_ context.Context, record slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, record.Clone())
	return nil
}

func (c *capturedLogs) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *capturedLogs) WithGroup(string) slog.Handler { return c }

func (c *capturedLogs) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slog.Record(nil), c.records...)
}

// captureDefaultLogs installs a capturing default slog handler until the test
// ends. slog.SetDefault also redirects the log package, so its writer and flags
// are restored too.
func captureDefaultLogs(t *testing.T) *capturedLogs {
	t.Helper()
	capture := &capturedLogs{}
	previous, previousWriter, previousFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return capture
}

func recordAttrs(record slog.Record) map[string]string {
	attrs := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	return attrs
}

// TestCommunityProfileBlobReferences_UnusableCID covers a record whose blob
// ref.$link is present but is not a usable CID: the slot is cleared, its
// reference deactivated, and a warning names the DID and slot but not the CID.
func TestCommunityProfileBlobReferences_UnusableCID(t *testing.T) {
	const unusableCID = "bafynotacid"
	for _, test := range []struct {
		name                   string
		run                    func(*testing.T) communityBlobFixture
		slot                   string
		wantAvatar, wantBanner string
		wantRows               func(string) []profileBlobRow
	}{
		{
			name: "update avatar", slot: "avatar", wantBanner: profileBannerB,
			run: func(t *testing.T) communityBlobFixture {
				f := indexedCommunityWithBlobs(t, false)
				require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("update", "", unusableCID, profileBannerB)))
				return f
			},
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
					{did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", true},
				}
			},
		},
		{
			name: "create banner", slot: "banner", wantAvatar: profileAvatarA,
			run: func(t *testing.T) communityBlobFixture {
				f := newCommunityBlobFixture(t, false)
				require.NoError(t, f.consumer.HandleEvent(t.Context(), f.event("create", "", profileAvatarA, unusableCID)))
				return f
			},
			wantRows: func(did string) []profileBlobRow {
				return []profileBlobRow{
					{did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", true},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := captureDefaultLogs(t)
			f := test.run(t)
			avatar, banner := communityBlobColumns(t, f)
			require.Equal(t, test.wantAvatar, avatar)
			require.Equal(t, test.wantBanner, banner)
			require.Equal(t, test.wantRows(f.did), readProfileBlobRows(t, f.db, f.subject))

			var warnings []map[string]string
			for _, record := range capture.snapshot() {
				attrs := recordAttrs(record)
				require.NotContains(t, record.Message, unusableCID, "the CID must not be logged")
				for key, value := range attrs {
					require.NotContains(t, value, unusableCID, "the CID must not be logged in attribute %q", key)
				}
				if record.Level == slog.LevelWarn && attrs["did"] == f.did {
					warnings = append(warnings, attrs)
				}
			}
			require.Equal(t, []map[string]string{{"did": f.did, "slot": test.slot}}, warnings)
		})
	}
}

// rebuildDeletesCommunityRepository deletes the community just before the
// reference rebuild, as a delete racing a create replay would.
type rebuildDeletesCommunityRepository struct {
	communities.Repository
}

func (r rebuildDeletesCommunityRepository) RebuildProfileBlobReferences(ctx context.Context, did string) error {
	if err := r.Delete(ctx, did); err != nil {
		return err
	}
	return r.Repository.RebuildProfileBlobReferences(ctx, did)
}

func TestCommunityProfileBlobReferences_CreateReplayRacingDelete(t *testing.T) {
	f := newCommunityBlobFixture(t, false)
	event := f.event("create", "", profileAvatarA, profileBannerB)
	createdAt, err := time.Parse(time.RFC3339, event.Commit.Record["createdAt"].(string))
	require.NoError(t, err)
	repo := postgres.NewCommunityRepository(f.db, credentialciphertest.Fixed())
	_, err = repo.Create(t.Context(), &communities.Community{
		DID: f.did, Handle: f.handle, Name: f.name, DisplayName: "Conflict " + f.name,
		OwnerDID: f.did, CreatedByDID: "did:plc:conflicttestcreator", HostedByDID: "did:web:test.local",
		Visibility: "public", AllowExternalDiscovery: true, CreatedAt: createdAt,
		RecordURI: f.subject, RecordCID: event.Commit.CID,
		AvatarCID: profileAvatarA, BannerCID: profileBannerB,
	})
	require.NoError(t, err)
	consumer := NewCommunityEventConsumer(rebuildDeletesCommunityRepository{repo}, "did:web:test.local", true, nil)

	require.NoError(t, consumer.HandleEvent(t.Context(), event), "a create replay that races a delete is not retryable")

	var count int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM communities WHERE did = $1`, f.did).Scan(&count))
	require.Zero(t, count, "the replay must not resurrect the deleted community")
	require.Equal(t, []profileBlobRow{
		{f.did, "bafyreib6tbnql2ux3whnfysbzabthaj2vvck53nimhbi5g5a7jgvgr5eqm", "avatar", false},
		{f.did, "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si", "banner", false},
	}, readProfileBlobRows(t, f.db, f.subject))
}
