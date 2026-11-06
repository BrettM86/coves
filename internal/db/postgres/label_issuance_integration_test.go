//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/labeling"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A throwaway K-256 label signing key fixed for these tests, and its public
// half as the labeler publishes it.
const (
	issuanceLabelKeyPrivateMultibase = "z3vLhUAjHvPNd6Z3rb5QQ2Dnd51QfVsvSxnTwYfpx7RdhCf8"
	issuanceLabelKeyPublicMultibase  = "zQ3shgJ7Pd7JWkPhsfnk4nNADhHZPr9QjXsWZNssxPApcfsxf"
	issuanceRestoreReason            = "social.coves.moderation.defs#reasonModeratorDiscretion"
)

func issuanceLabelSigningKey(t *testing.T) atcrypto.PrivateKey {
	t.Helper()
	return labelPrivateKey(t, issuanceLabelKeyPrivateMultibase)
}

func issuanceLabelPublicKey(t *testing.T) atcrypto.PublicKey {
	t.Helper()
	return labelPublicKey(t, issuanceLabelKeyPublicMultibase)
}

func labelPrivateKey(t *testing.T, multibase string) atcrypto.PrivateKey {
	t.Helper()
	key, err := atcrypto.ParsePrivateMultibase(multibase)
	require.NoError(t, err)
	return key
}

func labelPublicKey(t *testing.T, multibase string) atcrypto.PublicKey {
	t.Helper()
	key, err := atcrypto.ParsePublicMultibase(multibase)
	require.NoError(t, err)
	return key
}

// newLabelPublishingModerationService is the Postgres moderation service with
// label publication enabled under the fixed issuance key.
func newLabelPublishingModerationService(t *testing.T, db *sql.DB) moderation.Service {
	t.Helper()
	return newPostgresModerationServiceWith(db, postgres.NewModerationRepository(db), issuanceLabelSigningKey(t))
}

// storedLabels reads the published labels for one subject URI, oldest first.
func storedLabels(t *testing.T, db *sql.DB, uri string) []labels.Label {
	t.Helper()
	page, err := postgres.NewLabelRepository(db).Query(t.Context(), labels.Query{URIPatterns: []string{uri}, Limit: 50})
	require.NoError(t, err)
	return page.Labels
}

type expectedLabel struct {
	uri, val, actionID string
	neg                bool
}

// requireSignedLabel checks a stored label row against the decision that
// issued it and verifies its signature with the configured public key.
func requireSignedLabel(t *testing.T, got labels.Label, want expectedLabel) {
	t.Helper()
	assert.Equal(t, fixtures.InstanceDID(), got.Src)
	assert.Equal(t, want.uri, got.URI)
	assert.Equal(t, want.val, got.Val)
	assert.Equal(t, want.neg, got.Neg)
	assert.Nil(t, got.CID, "a URI-scoped label carries no cid")
	assert.EqualValues(t, 1, got.Version)
	assert.Equal(t, want.actionID, got.ActionID)
	assert.Equal(t, issuanceLabelKeyPublicMultibase, got.SignerKey)
	require.NoError(t, labelSignatureError(got, issuanceLabelPublicKey(t)), "label signature must verify with the configured key")
}

// labelSignatureError verifies a stored label's signature against key.
func labelSignatureError(got labels.Label, key atcrypto.PublicKey) error {
	signed := labeling.Label{
		CID: got.CID, CreatedAt: got.CreatedAt, ExpiresAt: got.ExpiresAt, SourceDID: got.Src,
		URI: got.URI, Val: got.Val, Version: got.Version, Sig: got.Sig,
	}
	if got.Neg {
		negated := true
		signed.Negated = &negated
	}
	return signed.VerifySignature(key)
}

func requireStrictlyIncreasingLabels(t *testing.T, rows []labels.Label) {
	t.Helper()
	for index := 1; index < len(rows); index++ {
		assert.Greater(t, rows[index].Seq, rows[index-1].Seq, "seq must strictly increase")
		previous, err := time.Parse(time.RFC3339Nano, rows[index-1].CreatedAt)
		require.NoError(t, err)
		current, err := time.Parse(time.RFC3339Nano, rows[index].CreatedAt)
		require.NoError(t, err)
		assert.True(t, current.After(previous), "cts %s must be after %s", rows[index].CreatedAt, rows[index-1].CreatedAt)
	}
}

func TestLabelIssuancePublishesOneSignedLabelPerDecision(t *testing.T) {
	db := testkit.DB(t)
	subject := indexedLabelPost(t, db)
	service := newLabelPublishingModerationService(t, db)
	actor := fixtures.DID("labelissueadmin")
	removeRequest := moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "issue-remove", Reason: moderationConcurrencyReason,
	}
	var removalID, labelID string
	steps := []struct {
		name   string
		decide func(t *testing.T) *moderation.MutationResult
		val    string
		neg    bool
	}{
		{name: "remove", val: "!takedown", neg: false, decide: func(t *testing.T) *moderation.MutationResult {
			result, err := service.RemoveContent(t.Context(), actor, removeRequest)
			require.NoError(t, err)
			removalID = result.Action.ID
			return result
		}},
		{name: "restore", val: "!takedown", neg: true, decide: func(t *testing.T) *moderation.MutationResult {
			result, err := service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
				ActionID: removalID, ReviewedSubject: &subject, ExpectedVersion: "v1",
				IdempotencyKey: "issue-restore", Reason: issuanceRestoreReason,
			})
			require.NoError(t, err)
			return result
		}},
		{name: "label", val: "nsfw", neg: false, decide: func(t *testing.T) *moderation.MutationResult {
			result := labelPost(t, service, actor, subject, "v2", "issue-label")
			labelID = result.Action.ID
			return result
		}},
		{name: "retract", val: "nsfw", neg: true, decide: func(t *testing.T) *moderation.MutationResult {
			result, err := service.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
				ActionID: labelID, ReviewedSubject: &subject, ExpectedVersion: "v3", IdempotencyKey: "issue-retract",
			})
			require.NoError(t, err)
			return result
		}},
	}
	for index, step := range steps {
		result := step.decide(t)
		require.NotNil(t, result, step.name)
		require.Equal(t, moderation.OutcomeApplied, result.Outcome, step.name)
		require.NotNil(t, result.Action, step.name)
		rows := storedLabels(t, db, subject.URI)
		require.Len(t, rows, index+1, "%s must write exactly one label row", step.name)
		requireSignedLabel(t, rows[index], expectedLabel{uri: subject.URI, val: step.val, neg: step.neg, actionID: result.Action.ID})
	}
	requireStrictlyIncreasingLabels(t, storedLabels(t, db, subject.URI))

	_, err := service.RemoveContent(t.Context(), actor, removeRequest)
	require.NoError(t, err, "a replay returns the stored result")
	assert.Len(t, storedLabels(t, db, subject.URI), len(steps), "a replay must write no label row")
}

// TestLabelIssuanceUnchangedDecisionsPublishNothing repeats a removal and a
// label under new idempotency keys, so each repeat reaches the unchanged
// outcome rather than an idempotent replay, and must write no label row.
func TestLabelIssuanceUnchangedDecisionsPublishNothing(t *testing.T) {
	db := testkit.DB(t)
	service := newLabelPublishingModerationService(t, db)
	actor := fixtures.DID("labelunchangedadmin")

	t.Run("re-remove", func(t *testing.T) {
		subject := indexedLabelPost(t, db)
		removed, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "unchanged-remove-1", Reason: moderationConcurrencyReason,
		})
		require.NoError(t, err)
		require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
		again, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v1", IdempotencyKey: "unchanged-remove-2", Reason: moderationConcurrencyReason,
		})
		require.NoError(t, err)
		require.Equal(t, moderation.OutcomeUnchanged, again.Outcome)
		rows := storedLabels(t, db, subject.URI)
		require.Len(t, rows, 1, "an unchanged removal must write no label row")
		requireSignedLabel(t, rows[0], expectedLabel{uri: subject.URI, val: "!takedown", actionID: removed.Action.ID})
	})

	t.Run("re-label", func(t *testing.T) {
		subject := indexedLabelPost(t, db)
		labeled := labelPost(t, service, actor, subject, "v0", "unchanged-label-1")
		again, err := service.LabelContent(t.Context(), actor, moderation.LabelContentRequest{
			Subject: subject, LabelValue: moderation.LabelNSFW, ExpectedVersion: "v1", IdempotencyKey: "unchanged-label-2",
		})
		require.NoError(t, err)
		require.Equal(t, moderation.OutcomeUnchanged, again.Outcome)
		rows := storedLabels(t, db, subject.URI)
		require.Len(t, rows, 1, "an unchanged label must write no label row")
		requireSignedLabel(t, rows[0], expectedLabel{uri: subject.URI, val: "nsfw", actionID: labeled.Action.ID})
	})
}

const failingIdempotencyKey = "publish-subject"

var errIdempotencySaveFailed = errors.New("idempotency save failed")

// failingIdempotencySaveStore is the Postgres moderation store whose decision
// transactions fail at SaveIdempotencyRecord for failingIdempotencyKey, after
// the label row has been written.
type failingIdempotencySaveStore struct {
	*postgres.ModerationRepository
}

func (s failingIdempotencySaveStore) InTransaction(ctx context.Context, fn func(ctx context.Context, tx moderation.Transaction) error) error {
	return s.ModerationRepository.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
		return fn(ctx, failingIdempotencySaveTransaction{Transaction: tx})
	})
}

type failingIdempotencySaveTransaction struct {
	moderation.Transaction
}

func (tx failingIdempotencySaveTransaction) SaveIdempotencyRecord(ctx context.Context, record moderation.IdempotencyRecord) error {
	if record.Key == failingIdempotencyKey {
		return errIdempotencySaveFailed
	}
	return tx.Transaction.SaveIdempotencyRecord(ctx, record)
}

func TestLabelIssuanceWritesOnlyForCommittedPublishedDecisions(t *testing.T) {
	for _, test := range []struct {
		name                 string
		publish              bool
		failSubjectSave      bool
		wantControlLabels    int
		wantSubjectLabels    int
		wantSubjectRemoveErr bool
	}{
		{name: "publication enabled labels both removals", publish: true, wantControlLabels: 1, wantSubjectLabels: 1},
		{
			name: "a decision transaction failing after the label write leaves no label", publish: true, failSubjectSave: true,
			wantControlLabels: 1, wantSubjectLabels: 0, wantSubjectRemoveErr: true,
		},
		{name: "publication disabled labels nothing", publish: false, wantControlLabels: 0, wantSubjectLabels: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			control, subject := indexedLabelPost(t, db), indexedLabelPost(t, db)
			var store moderation.Store = postgres.NewModerationRepository(db)
			if test.failSubjectSave {
				store = failingIdempotencySaveStore{ModerationRepository: postgres.NewModerationRepository(db)}
			}
			var signingKey atcrypto.PrivateKey
			if test.publish {
				signingKey = issuanceLabelSigningKey(t)
			}
			service := newPostgresModerationServiceWith(db, store, signingKey)
			actor := fixtures.DID("labelcommitadmin")

			controlResult, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
				Subject: control, ExpectedVersion: "v0", IdempotencyKey: "publish-control", Reason: moderationConcurrencyReason,
			})
			require.NoError(t, err)
			_, err = service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v0", IdempotencyKey: failingIdempotencyKey, Reason: moderationConcurrencyReason,
			})
			if test.wantSubjectRemoveErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			controlRows := storedLabels(t, db, control.URI)
			require.Len(t, controlRows, test.wantControlLabels)
			if test.wantControlLabels == 1 {
				requireSignedLabel(t, controlRows[0], expectedLabel{uri: control.URI, val: "!takedown", actionID: controlResult.Action.ID})
			}
			assert.Len(t, storedLabels(t, db, subject.URI), test.wantSubjectLabels)
		})
	}
}

func TestLabelIssuanceSkipsReversalsOnErasedSubjects(t *testing.T) {
	for _, test := range []struct {
		name     string
		subject  func(t *testing.T, db *sql.DB) moderation.StrongRef
		labelled bool
		erase    string
	}{
		{name: "restore of an erased post's removal", subject: indexedLabelPost, erase: `DELETE FROM posts WHERE uri = $1`},
		{
			name: "restore of an erased comment's removal", erase: `DELETE FROM comments WHERE uri = $1`,
			subject: func(t *testing.T, db *sql.DB) moderation.StrongRef {
				subject, _, _ := indexedModerationComment(t, db, true, "")
				return subject
			},
		},
		{name: "retraction of an erased post's label", subject: indexedLabelPost, labelled: true, erase: `DELETE FROM posts WHERE uri = $1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			subject := test.subject(t, db)
			service := newLabelPublishingModerationService(t, db)
			actor := fixtures.DID("labelerasedadmin")
			want := expectedLabel{uri: subject.URI, val: "!takedown"}
			if test.labelled {
				want.val = "nsfw"
				want.actionID = labelPost(t, service, actor, subject, "v0", "erased-label").Action.ID
			} else {
				removed, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
					Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "erased-remove", Reason: moderationConcurrencyReason,
				})
				require.NoError(t, err)
				want.actionID = removed.Action.ID
			}
			rows := storedLabels(t, db, subject.URI)
			require.Len(t, rows, 1, "the decision on the indexed subject is published")
			requireSignedLabel(t, rows[0], want)

			// Account erasure hard-deletes the indexed row, making the subject restricted.
			_, err := db.ExecContext(t.Context(), test.erase, subject.URI)
			require.NoError(t, err)

			var reversed *moderation.MutationResult
			if test.labelled {
				reversed, err = service.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
					ActionID: want.actionID, ExpectedVersion: "v1", IdempotencyKey: "erased-retract",
				})
			} else {
				reversed, err = service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
					ActionID: want.actionID, ExpectedVersion: "v1", IdempotencyKey: "erased-restore", Reason: issuanceRestoreReason,
				})
			}
			require.NoError(t, err)
			require.NotNil(t, reversed)
			assert.Equal(t, moderation.OutcomeApplied, reversed.Outcome)

			rows = storedLabels(t, db, subject.URI)
			require.Len(t, rows, 1, "a reversal on a restricted subject publishes nothing")
			requireSignedLabel(t, rows[0], want)
		})
	}
}

func TestLabelIssuancePublishesHiddenReasonRemovals(t *testing.T) {
	for _, reason := range []string{
		"social.coves.moderation.defs#reasonDoxing",
		"social.coves.moderation.defs#reasonIllegalContent",
	} {
		t.Run(reason, func(t *testing.T) {
			db := testkit.DB(t)
			subject := indexedLabelPost(t, db)
			service := newLabelPublishingModerationService(t, db)
			removed, err := service.RemoveContent(t.Context(), fixtures.DID("labelhiddenadmin"), moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "hidden-remove", Reason: reason,
			})
			require.NoError(t, err)
			require.NotNil(t, removed.Action)
			rows := storedLabels(t, db, subject.URI)
			require.Len(t, rows, 1)
			requireSignedLabel(t, rows[0], expectedLabel{uri: subject.URI, val: "!takedown", actionID: removed.Action.ID})
		})
	}
}
