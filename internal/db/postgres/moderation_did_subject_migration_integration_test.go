//go:build integration

package postgres_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	moderationDIDSubjectAccount = "at://did:plc:x"
	moderationDIDSubjectRecord  = "at://did:plc:x/social.coves.community.comment/abc"
	moderationDIDSubjectShape   = "moderation_action_subject_shape"
	postgresCheckViolation      = "23514"
)

// insertModerationSubjectAction inserts one instance action row on subject,
// with collection and expiresAt stored as NULL when nil. An account subject
// (no collection) is recorded as account-kind, as BanAccount writes it.
func insertModerationSubjectAction(t *testing.T, db *sql.DB, id, subject string, collection *string, action string, expiresAt *time.Time) error {
	t.Helper()
	row := moderationKindAction{id: id, subject: subject, action: action, expiresAt: expiresAt}
	if collection != nil {
		row.collection = *collection
	} else {
		row.subjectKind = string(moderation.SubjectKindAccount)
	}
	return insertSubjectKindAction(t, db, row)
}

func TestModerationMigration072_AccountSubjectShape(t *testing.T) {
	db := testkit.DB(t)
	comment := moderation.CommentCollection
	expiresAt := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		subject    string
		collection *string
		action     string
		expiresAt  *time.Time
		// wantConstraint is the check constraint that rejects the row; empty
		// means the row is accepted. "*" accepts any check violation.
		wantConstraint string
	}{
		{name: "account subject without a collection", subject: moderationDIDSubjectAccount, action: moderation.ActionRemove},
		{name: "account ban with an expiry", subject: moderationDIDSubjectAccount, action: moderation.ActionRemove, expiresAt: &expiresAt},
		{
			name: "record subject without a collection", subject: moderationDIDSubjectRecord, action: moderation.ActionRemove,
			wantConstraint: moderationDIDSubjectShape,
		},
		{
			name: "account subject with a collection", subject: moderationDIDSubjectAccount, collection: &comment, action: moderation.ActionRemove,
			wantConstraint: moderationDIDSubjectShape,
		},
		{
			name: "account subject ending in a colon", subject: "at://did:plc:x:", action: moderation.ActionRemove,
			wantConstraint: moderationDIDSubjectShape,
		},
		{
			name: "record subject with an expiry", subject: moderationDIDSubjectRecord, collection: &comment, action: moderation.ActionRemove,
			expiresAt: &expiresAt, wantConstraint: "*",
		},
		{
			name: "account restore with an expiry", subject: moderationDIDSubjectAccount, action: moderation.ActionRestore,
			expiresAt: &expiresAt, wantConstraint: "*",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := insertModerationSubjectAction(t, db, testkit.UniqueIDWithPrefix(t, "shape"), test.subject, test.collection, test.action, test.expiresAt)
			if test.wantConstraint == "" {
				require.NoError(t, err)
				return
			}
			var pqErr *pq.Error
			require.True(t, errors.As(err, &pqErr), "want a check violation, got %v", err)
			assert.Equal(t, postgresCheckViolation, string(pqErr.Code), "want a check violation, got %v", err)
			if test.wantConstraint != "*" {
				assert.Equal(t, test.wantConstraint, pqErr.Constraint)
			}
		})
	}
}

// TestModerationMigration072_KeepsRecordSubjectActions stores a record-subject
// action under 071 and reads it back unchanged after 072 applies.
func TestModerationMigration072_KeepsRecordSubjectActions(t *testing.T) {
	db := testkit.DB(t)
	require.EqualValues(t, 80, testkit.MigrateDownOne(t, db, 80),
		"080 (moderation community record writes) must be rolled back first")
	require.EqualValues(t, 79, testkit.MigrateDownOne(t, db, 79),
		"079 (community provisioning accounts) must be rolled back first")
	require.EqualValues(t, 78, testkit.MigrateDownOne(t, db, 78),
		"078 (untyped gallery $type stamp) must be rolled back first")
	require.EqualValues(t, 77, testkit.MigrateDownOne(t, db, 77),
		"077 (Bluesky profile import dismissal) must be rolled back first")
	require.EqualValues(t, 76, testkit.MigrateDownOne(t, db, 76),
		"076 (account status rechecks) must be rolled back first")
	require.EqualValues(t, 75, testkit.MigrateDownOne(t, db, 75),
		"075 (moderation subject kind) must be rolled back first")
	require.EqualValues(t, 74, testkit.MigrateDownOne(t, db, 74),
		"074 (account status restrictions) must be rolled back first")
	require.EqualValues(t, 73, testkit.MigrateDownOne(t, db, 73),
		"073 (account ban hide reason) must be rolled back first")
	require.EqualValues(t, 72, testkit.MigrateDownOne(t, db, 72),
		"072 (account moderation subjects) must be rolled back first")
	row := actionLogRow("migration-072-record", time.Date(2026, time.October, 22, 12, 0, 0, 123456000, time.UTC))
	insertActionLogRow(t, db, row)

	testkit.MigrateUp(t, db)

	stored := listActionLog(t, db, moderation.ActionListQuery{Limit: 20})
	require.Len(t, stored, 1)
	want := row
	want.SubjectAccess = moderation.SubjectAccessRestricted
	assert.Equal(t, want, stored[0])
	assert.Nil(t, stored[0].ExpiresAt)
}
