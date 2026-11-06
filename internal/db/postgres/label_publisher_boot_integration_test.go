//go:build integration

package postgres_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	labelhandlers "Coves/internal/api/handlers/labels"
	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigoidentity "github.com/bluesky-social/indigo/atproto/identity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two more throwaway K-256 keys beside the issuance key: the rotation target
// and an unrelated key that is never used to sign.
const (
	rotationLabelKeyPrivateMultibase    = "z3vLgymfj9eFb88rSPuESBGLf4iUcUyLDUiM6FkYMSBApctd"
	rotationLabelKeyPublicMultibase     = "zQ3shQV5pMeCq13FG2HXhKrQW28kTCVZjBcN8upTHnDBAE9za"
	unrelatedLabelKeyPublicMultibase    = "zQ3shuTZmKB1v2ESRSe9cw8uyYukRjYMwqYCmnrK6GXZqvJf4"
	bootPublisherLabelerEndpoint        = "https://labeler.coves.test"
	exportBackdatedDecisionTime         = "2020-01-01T00:00:00Z"
	exportIssuanceTimeLowerBoundRFC3339 = "2026-01-01T00:00:00Z"
)

// startLabelPublisher builds a publisher for the instance DID with key and the
// declared retiring public key, and runs its boot sequence against db.
func startLabelPublisher(t *testing.T, db *sql.DB, key atcrypto.PrivateKey, retiringKey string) (*labels.Publisher, error) {
	t.Helper()
	publisher, err := labels.NewPublisher(labels.Config{
		SourceDID: fixtures.InstanceDID(), LabelerEndpoint: bootPublisherLabelerEndpoint, Key: key, RetiringKey: retiringKey,
	})
	require.NoError(t, err)
	return publisher, publisher.Start(t.Context(), postgres.NewLabelRepository(db))
}

// allStoredLabels reads every published label, oldest first.
func allStoredLabels(t *testing.T, db *sql.DB) []labels.Label {
	t.Helper()
	page, err := postgres.NewLabelRepository(db).Query(t.Context(), labels.Query{URIPatterns: []string{"at://*"}, Limit: 250})
	require.NoError(t, err)
	return page.Labels
}

// seedIssuanceKeyLabels boots a publisher under the issuance key on an empty
// store, then publishes four labels under that key through real decisions:
// a removal, an NSFW label, and a removal followed by its restore (negation).
// It returns the posts in decision order and the published rows.
func seedIssuanceKeyLabels(t *testing.T, db *sql.DB) ([]moderation.StrongRef, []labels.Label) {
	t.Helper()
	_, err := startLabelPublisher(t, db, issuanceLabelSigningKey(t), "")
	require.NoError(t, err, "a publisher must start on an empty store")

	communityDID, authorDID := moderationPostActors(t, db)
	post := func() moderation.StrongRef {
		return indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "boot seed", "")
	}
	removed, labelled, restored := post(), post(), post()
	service := newLabelPublishingModerationService(t, db)
	actor := fixtures.DID("labelbootseedadmin")
	remove := func(subject moderation.StrongRef, key string) *moderation.MutationResult {
		result, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: key, Reason: moderationConcurrencyReason,
		})
		require.NoError(t, err)
		return result
	}
	remove(removed, "boot-seed-remove")
	labelPost(t, service, actor, labelled, "v0", "boot-seed-label")
	restoredRemoval := remove(restored, "boot-seed-remove-restored")
	_, err = service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
		ActionID: restoredRemoval.Action.ID, ReviewedSubject: &restored, ExpectedVersion: "v1",
		IdempotencyKey: "boot-seed-restore", Reason: issuanceRestoreReason,
	})
	require.NoError(t, err)

	rows := allStoredLabels(t, db)
	require.Len(t, rows, 4, "seeding must publish one label per decision under the issuance key")
	for _, row := range rows {
		require.Equal(t, issuanceLabelKeyPublicMultibase, row.SignerKey)
		require.NoError(t, labelSignatureError(row, issuanceLabelPublicKey(t)))
	}
	return []moderation.StrongRef{removed, labelled, restored}, rows
}

func TestLabelPublisherBootRejectsUnknownSigningKey(t *testing.T) {
	for _, test := range []struct {
		name        string
		key         string
		retiringKey string
		wantErr     error
	}{
		{name: "a new key without a retiring key", key: rotationLabelKeyPrivateMultibase, retiringKey: "", wantErr: labels.ErrSigningKeyMismatch},
		{
			name: "a new key retiring a key that signed nothing", key: rotationLabelKeyPrivateMultibase,
			retiringKey: unrelatedLabelKeyPublicMultibase, wantErr: labels.ErrSigningKeyMismatch,
		},
		{name: "the key that signed the stored labels", key: issuanceLabelKeyPrivateMultibase, retiringKey: "", wantErr: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			_, before := seedIssuanceKeyLabels(t, db)

			_, err := startLabelPublisher(t, db, labelPrivateKey(t, test.key), test.retiringKey)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, before, allStoredLabels(t, db), "boot must leave every stored label unchanged")
		})
	}
}

// failingSecondSignatureKey is the rotation key whose second signature fails,
// interrupting a rotation after it has re-signed one label.
type failingSecondSignatureKey struct {
	atcrypto.PrivateKey
	signatures atomic.Int32
}

var errSecondSignatureFailed = errors.New("second signature failed")

func (k *failingSecondSignatureKey) HashAndSign(content []byte) ([]byte, error) {
	if k.signatures.Add(1) == 2 {
		return nil, errSecondSignatureFailed
	}
	return k.PrivateKey.HashAndSign(content)
}

// unsignedLabels drops each label's signature and signer, leaving the fields a
// rotation must preserve.
func unsignedLabels(rows []labels.Label) []labels.Label {
	unsigned := make([]labels.Label, len(rows))
	for index, row := range rows {
		row.Sig, row.SignerKey = nil, ""
		unsigned[index] = row
	}
	return unsigned
}

// publishTemporaryAccountBanLabel bans a new indexed account for a day under
// the issuance key, publishing one label with an exp, and returns that label.
func publishTemporaryAccountBanLabel(t *testing.T, db *sql.DB) labels.Label {
	t.Helper()
	name := testkit.UniqueIDWithPrefix(t, "rotateban")
	account := fixtures.DID(name)
	fixtures.User(t, db, name+".test", account)
	service := newPostgresAccountModerationService(db, postgres.NewModerationRepository(db), issuanceLabelSigningKey(t), time.Now)
	expiresAt := time.Now().Add(24 * time.Hour)
	_, err := service.BanAccount(t.Context(), fixtures.DID("labelrotationbanadmin"), moderation.BanAccountRequest{
		Subject: account, ExpectedVersion: "v0", IdempotencyKey: "rotation-temporary-ban", Reason: accountBanSpam, ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	rows := storedLabels(t, db, account)
	require.Len(t, rows, 1, "a temporary ban publishes one label")
	require.NotNil(t, rows[0].ExpiresAt, "a temporary ban's label carries its expiry")
	return rows[0]
}

// everyLabelURIPattern matches both record labels and account labels, whose
// URI is a bare DID.
var everyLabelURIPattern = []string{"at://*", "did:*"}

// everyStoredLabel reads every published label, record and account, oldest
// first.
func everyStoredLabel(t *testing.T, db *sql.DB) []labels.Label {
	t.Helper()
	page, err := postgres.NewLabelRepository(db).Query(t.Context(), labels.Query{URIPatterns: everyLabelURIPattern, Limit: 250})
	require.NoError(t, err)
	return page.Labels
}

func TestLabelPublisherRotationResignsStoredLabels(t *testing.T) {
	for _, test := range []struct {
		name                 string
		interruptedFirstBoot bool
	}{
		{name: "rotation completes"},
		{name: "an interrupted rotation leaves every label under the retiring key until a later boot completes it", interruptedFirstBoot: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			seedIssuanceKeyLabels(t, db)
			temporaryBan := publishTemporaryAccountBanLabel(t, db)
			before := everyStoredLabel(t, db)
			require.Len(t, before, 5)
			repository := postgres.NewLabelRepository(db)
			firstPage, err := repository.Query(t.Context(), labels.Query{URIPatterns: everyLabelURIPattern, Limit: 2})
			require.NoError(t, err)
			require.Len(t, firstPage.Labels, 2)
			cursor := firstPage.Cursor

			if test.interruptedFirstBoot {
				interrupted := &failingSecondSignatureKey{PrivateKey: labelPrivateKey(t, rotationLabelKeyPrivateMultibase)}
				_, err := startLabelPublisher(t, db, interrupted, issuanceLabelKeyPublicMultibase)
				require.Error(t, err, "a rotation whose signing fails must fail boot")
				assert.Equal(t, before, everyStoredLabel(t, db), "an interrupted rotation must leave every label as it was")
			}

			publisher, err := startLabelPublisher(t, db, labelPrivateKey(t, rotationLabelKeyPrivateMultibase), issuanceLabelKeyPublicMultibase)
			require.NoError(t, err)

			after := everyStoredLabel(t, db)
			require.Len(t, after, 5, "rotation must allocate no new label rows")
			assert.Equal(t, unsignedLabels(before), unsignedLabels(after), "rotation changes only signatures and signer")
			resignedBan := after[4]
			require.Equal(t, temporaryBan.Seq, resignedBan.Seq)
			assert.Equal(t, temporaryBan.ExpiresAt, resignedBan.ExpiresAt, "the re-signed temporary ban keeps its expiry")
			assert.NoError(t, labelSignatureError(resignedBan, labelPublicKey(t, rotationLabelKeyPublicMultibase)),
				"the re-signed temporary ban verifies under the new key with its expiry")
			for _, row := range after {
				assert.Equal(t, rotationLabelKeyPublicMultibase, row.SignerKey)
				assert.NoError(t, labelSignatureError(row, labelPublicKey(t, rotationLabelKeyPublicMultibase)), "seq %d must verify with the new key", row.Seq)
				assert.Error(t, labelSignatureError(row, issuanceLabelPublicKey(t)), "seq %d must no longer verify with the retired key", row.Seq)
			}

			history, err := repository.SigningKeyHistory(t.Context())
			require.NoError(t, err)
			require.Len(t, history, 2)
			retiredAt := map[string]*time.Time{}
			for _, record := range history {
				retiredAt[record.PublicKey] = record.RetiredAt
			}
			require.Contains(t, retiredAt, issuanceLabelKeyPublicMultibase)
			require.Contains(t, retiredAt, rotationLabelKeyPublicMultibase)
			assert.NotNil(t, retiredAt[issuanceLabelKeyPublicMultibase], "the retiring key is recorded as retired")
			assert.Nil(t, retiredAt[rotationLabelKeyPublicMultibase], "the new key is the current key")

			recorder := httptest.NewRecorder()
			labelhandlers.NewDIDDocumentHandler(publisher).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/did.json", nil))
			require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())
			var document indigoidentity.DIDDocument
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &document))
			servedIdentity := indigoidentity.ParseIdentity(&document)
			servedKey, err := servedIdentity.GetPublicKey("atproto_label")
			require.NoError(t, err)
			assert.Equal(t, rotationLabelKeyPublicMultibase, servedKey.Multibase())

			continued, err := repository.Query(t.Context(), labels.Query{URIPatterns: everyLabelURIPattern, Cursor: cursor, Limit: 250})
			require.NoError(t, err)
			var continuedSeqs []int64
			for _, row := range continued.Labels {
				continuedSeqs = append(continuedSeqs, row.Seq)
				assert.NoError(t, labelSignatureError(row, labelPublicKey(t, rotationLabelKeyPublicMultibase)))
			}
			assert.Equal(t, []int64{before[2].Seq, before[3].Seq, before[4].Seq}, continuedSeqs, "a cursor taken before rotation continues with the remaining labels exactly once")

			live := indexedLabelPost(t, db)
			service := newPostgresModerationServiceWith(db, postgres.NewModerationRepository(db), labelPrivateKey(t, rotationLabelKeyPrivateMultibase))
			_, err = service.RemoveContent(t.Context(), fixtures.DID("labelrotationadmin"), moderation.RemoveContentRequest{
				Subject: live, ExpectedVersion: "v0", IdempotencyKey: "rotation-live-remove", Reason: moderationConcurrencyReason,
			})
			require.NoError(t, err)
			afterLive := everyStoredLabel(t, db)
			require.Len(t, afterLive, 6)
			assert.Equal(t, before[4].Seq+1, afterLive[5].Seq, "rotation allocated no sequence numbers")
			assert.Equal(t, rotationLabelKeyPublicMultibase, afterLive[5].SignerKey)
		})
	}
}

func TestLabelPublisherBootExportsActiveDecisionsOnce(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	post := func() moderation.StrongRef {
		return indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "export", "")
	}
	backdatedRemoval, removal, nsfw, restored, erased := post(), post(), post(), post(), post()
	unpublished := newPostgresModerationService(db)
	actor := fixtures.DID("labelexportadmin")
	remove := func(subject moderation.StrongRef, key string) *moderation.MutationResult {
		result, err := unpublished.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: key, Reason: moderationConcurrencyReason,
		})
		require.NoError(t, err)
		return result
	}
	backdatedRemovalID := remove(backdatedRemoval, "export-remove-backdated").Action.ID
	removalID := remove(removal, "export-remove").Action.ID
	nsfwID := labelPost(t, unpublished, actor, nsfw, "v0", "export-label").Action.ID
	restoredRemoval := remove(restored, "export-remove-restored")
	_, err := unpublished.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
		ActionID: restoredRemoval.Action.ID, ReviewedSubject: &restored, ExpectedVersion: "v1",
		IdempotencyKey: "export-restore", Reason: issuanceRestoreReason,
	})
	require.NoError(t, err)
	remove(erased, "export-remove-erased")
	comment, _, _ := indexedModerationComment(t, db, true, "")
	commentRemovalID := remove(comment, "export-remove-comment").Action.ID
	// Account erasure hard-deletes the indexed row, making the subject restricted.
	_, err = db.ExecContext(t.Context(), `DELETE FROM posts WHERE uri = $1`, erased.URI)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE moderation_actions SET created_at = $2 WHERE id = $1`, backdatedRemovalID, exportBackdatedDecisionTime)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE moderation_subjects SET updated_at = $2 WHERE subject_uri = $1`, backdatedRemoval.URI, exportBackdatedDecisionTime)
	require.NoError(t, err)
	require.Empty(t, allStoredLabels(t, db), "decisions made with publication disabled publish nothing")

	_, err = startLabelPublisher(t, db, issuanceLabelSigningKey(t), "")
	require.NoError(t, err)

	exported := allStoredLabels(t, db)
	require.Len(t, exported, 4, "export publishes one label per active decision on an indexed subject")
	byURI := map[string]labels.Label{}
	for _, row := range exported {
		byURI[row.URI] = row
	}
	for _, want := range []expectedLabel{
		{uri: backdatedRemoval.URI, val: "!takedown", actionID: backdatedRemovalID},
		{uri: removal.URI, val: "!takedown", actionID: removalID},
		{uri: nsfw.URI, val: "nsfw", actionID: nsfwID},
		{uri: comment.URI, val: "!takedown", actionID: commentRemovalID},
	} {
		require.Contains(t, byURI, want.uri)
		requireSignedLabel(t, byURI[want.uri], want)
	}
	assert.NotContains(t, byURI, restored.URI, "a restored removal is not exported")
	assert.NotContains(t, byURI, erased.URI, "a removal on an erased subject is not exported")
	exportedAt, err := time.Parse(time.RFC3339Nano, byURI[backdatedRemoval.URI].CreatedAt)
	require.NoError(t, err)
	lowerBound, err := time.Parse(time.RFC3339, exportIssuanceTimeLowerBoundRFC3339)
	require.NoError(t, err)
	assert.True(t, exportedAt.After(lowerBound), "an exported label carries its issuance time, not the decision time; got %s", byURI[backdatedRemoval.URI].CreatedAt)

	_, err = startLabelPublisher(t, db, issuanceLabelSigningKey(t), "")
	require.NoError(t, err)
	require.Len(t, allStoredLabels(t, db), 4, "a later boot exports nothing again")

	live := post()
	liveRemoval, err := newLabelPublishingModerationService(t, db).RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
		Subject: live, ExpectedVersion: "v0", IdempotencyKey: "export-live-remove", Reason: moderationConcurrencyReason,
	})
	require.NoError(t, err)
	afterLive := allStoredLabels(t, db)
	require.Len(t, afterLive, 5)
	requireSignedLabel(t, afterLive[4], expectedLabel{uri: live.URI, val: "!takedown", actionID: liveRemoval.Action.ID})
	assert.Greater(t, afterLive[4].Seq, exported[3].Seq, "live issuance continues after the export")
}

func TestLabelPublisherBootWithNewKeyRetiresAnUnusedCurrentKey(t *testing.T) {
	db := testkit.DB(t)
	_, err := startLabelPublisher(t, db, issuanceLabelSigningKey(t), "")
	require.NoError(t, err, "a publisher must start on an empty store")
	require.Empty(t, allStoredLabels(t, db), "the first key signs nothing, so the next boot takes no rotation path")

	_, err = startLabelPublisher(t, db, labelPrivateKey(t, rotationLabelKeyPrivateMultibase), "")
	require.NoError(t, err, "a new key must start when no stored label carries the old key")

	history, err := postgres.NewLabelRepository(db).SigningKeyHistory(t.Context())
	require.NoError(t, err)
	require.Len(t, history, 2)
	var current []string
	retiredAt := map[string]*time.Time{}
	for _, record := range history {
		retiredAt[record.PublicKey] = record.RetiredAt
		if record.RetiredAt == nil {
			current = append(current, record.PublicKey)
		}
	}
	require.Contains(t, retiredAt, issuanceLabelKeyPublicMultibase)
	assert.NotNil(t, retiredAt[issuanceLabelKeyPublicMultibase], "the replaced key is recorded as retired")
	assert.Equal(t, []string{rotationLabelKeyPublicMultibase}, current, "the key history holds exactly one current key")
}

// TestLabelPublisherBootExportsInForceAccountBans bans three accounts while
// publication is off: one permanently, one until a future instant, and one
// whose expiry has since passed. The first boot with a key exports a takedown
// on each in-force ban's DID, carrying the ban's expiry, and nothing for the
// lapsed one.
func TestLabelPublisherBootExportsInForceAccountBans(t *testing.T) {
	db := testkit.DB(t)
	unpublished := newPostgresAccountModerationService(db, postgres.NewModerationRepository(db), nil, nil)
	actor := fixtures.DID("labelexportbanadmin")
	account := func(prefix string) string {
		name := testkit.UniqueIDWithPrefix(t, prefix)
		did := fixtures.DID(name)
		fixtures.User(t, db, name+".test", did)
		return did
	}
	ban := func(did, key string, expiresAt *time.Time) string {
		result, err := unpublished.BanAccount(t.Context(), actor, moderation.BanAccountRequest{
			Subject: did, ExpectedVersion: "v0", IdempotencyKey: key, Reason: moderationConcurrencyReason, ExpiresAt: expiresAt,
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Action)
		return result.Action.ID
	}
	permanent, temporary, lapsed := account("exportpermanent"), account("exporttemporary"), account("exportlapsed")
	permanentID := ban(permanent, "export-ban-permanent", nil)
	temporaryID := ban(temporary, "export-ban-temporary", accountBanInstantPointer(t, "2099-01-01T00:00:00Z"))
	lapsedID := ban(lapsed, "export-ban-lapsed", accountBanInstantPointer(t, "2099-01-01T00:00:00Z"))
	_, err := db.ExecContext(t.Context(), `UPDATE moderation_actions SET expires_at = '2020-01-01T00:00:00Z' WHERE id = $1`, lapsedID)
	require.NoError(t, err)
	require.Empty(t, allStoredAccountLabels(t, db), "bans made with publication disabled publish nothing")

	_, err = startLabelPublisher(t, db, issuanceLabelSigningKey(t), "")
	require.NoError(t, err)

	require.Len(t, allStoredAccountLabels(t, db), 2, "export publishes one label per in-force account ban")
	permanentRows := storedLabels(t, db, permanent)
	require.Len(t, permanentRows, 1)
	requireAccountLabel(t, permanentRows[0], permanent, permanentID, false, nil)
	temporaryRows := storedLabels(t, db, temporary)
	require.Len(t, temporaryRows, 1)
	requireAccountLabel(t, temporaryRows[0], temporary, temporaryID, false, accountBanString("2099-01-01T00:00:00.000Z"))
	assert.Empty(t, storedLabels(t, db, lapsed), "a lapsed ban is not exported")
}

// allStoredAccountLabels reads every published account label (bare DID URIs),
// oldest first.
func allStoredAccountLabels(t *testing.T, db *sql.DB) []labels.Label {
	t.Helper()
	page, err := postgres.NewLabelRepository(db).Query(t.Context(), labels.Query{URIPatterns: []string{"did:*"}, Limit: 250})
	require.NoError(t, err)
	return page.Labels
}
