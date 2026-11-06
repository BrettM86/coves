//go:build integration

package main

import (
	"context"
	"database/sql"
	"testing"

	"Coves/internal/config"
	"Coves/internal/core/labels"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const labelWiringInstanceDID = "did:web:labelwiring.test"

// insertRetainedLabel stores one published takedown label, with the removal
// action it names, as an earlier boot with a signing key would have.
func insertRetainedLabel(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO moderation_actions (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection, action, reason, origin, created_at)
		VALUES ('retained-label-action', 'did:plc:labelwiringadmin', $1, 'instance',
		        'at://did:plc:labelwiringauthor/social.coves.community.comment/3lretained', 'social.coves.community.comment',
		        'remove', 'social.coves.moderation.defs#reasonSpam', 'local', '2026-10-22T12:00:00Z')
	`, labelWiringInstanceDID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO labels (src, uri, val, neg, cts, sig, ver, action_id, signer_key)
		VALUES ($1, 'at://did:plc:labelwiringauthor/social.coves.community.comment/3lretained', '!takedown', false,
		        '2026-10-22T12:00:00.000Z', '\x00'::bytea, 1, 'retained-label-action', 'zQ3shgJ7Pd7JWkPhsfnk4nNADhHZPr9QjXsWZNssxPApcfsxf')
	`, labelWiringInstanceDID)
	require.NoError(t, err)
}

func TestBuildLabelPublisherWithoutSigningKey(t *testing.T) {
	for _, test := range []struct {
		name           string
		retainedLabels bool
		wantErr        bool
	}{
		{name: "published labels require the signing key", retainedLabels: true, wantErr: true},
		{name: "an empty label store leaves publication disabled", retainedLabels: false, wantErr: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			if test.retainedLabels {
				insertRetainedLabel(t, db)
			}
			cfg := &config.Config{Instance: config.InstanceConfig{DID: labelWiringInstanceDID}}

			publisher, err := buildLabelPublisher(t.Context(), cfg, db)

			assert.Nil(t, publisher)
			if !test.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, labels.ErrSigningKeyRequired)
			assert.ErrorContains(t, err, "LABEL_SIGNING_KEY")
		})
	}
}

func TestBuildLabelPublisherWithoutSigningKeyReportsAFailedLabelCheck(t *testing.T) {
	db := testkit.DB(t)
	cfg := &config.Config{Instance: config.InstanceConfig{DID: labelWiringInstanceDID}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	publisher, err := buildLabelPublisher(ctx, cfg, db)

	assert.Nil(t, publisher)
	require.ErrorIs(t, err, context.Canceled, "the failed label query is the cause")
	assert.NotErrorIs(t, err, labels.ErrSigningKeyRequired)
	assert.NotContains(t, err.Error(), "LABEL_SIGNING_KEY", "a failed check does not claim labels are stored")
	assert.ErrorContains(t, err, "checking for published labels at boot")
}

// A keyed boot with no moderation decisions publishes no label, yet it does
// record the signing key and the completed export. A later boot without the
// key would leave that published state unsigned, so it must refuse.
func TestBuildLabelPublisherKeyedBootWithoutDecisionsRequiresTheKeyAfterwards(t *testing.T) {
	db := testkit.DB(t)
	signingKey, err := atcrypto.GeneratePrivateKeyK256()
	require.NoError(t, err)
	keyedConfig := &config.Config{
		Instance: config.InstanceConfig{DID: labelWiringInstanceDID},
		OAuth:    config.OAuthConfig{PublicURL: "https://labelwiring.test"},
		Labels:   config.LabelsConfig{SigningKey: signingKey},
	}

	publisher, err := buildLabelPublisher(t.Context(), keyedConfig, db)
	require.NoError(t, err)
	require.NotNil(t, publisher)

	var labelRows int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM labels`).Scan(&labelRows))
	assert.Equal(t, 0, labelRows, "no decision exists, so no label is published")
	var exportCompleted bool
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM label_publication_state WHERE export_completed_at IS NOT NULL)`).Scan(&exportCompleted))
	assert.True(t, exportCompleted, "the keyed boot records the completed export")

	keylessConfig := &config.Config{Instance: config.InstanceConfig{DID: labelWiringInstanceDID}}
	keylessPublisher, err := buildLabelPublisher(t.Context(), keylessConfig, db)

	assert.Nil(t, keylessPublisher)
	require.ErrorIs(t, err, labels.ErrSigningKeyRequired, "a keyless boot after a keyed boot must refuse")
}
