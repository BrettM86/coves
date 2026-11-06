package moderation

import (
	"context"
	"time"

	"Coves/internal/core/labels"
)

// Label values decisions publish. A removal publishes !takedown; a content
// label publishes its own value. A reversal publishes the same value negated.
const takedownLabelValue = labels.TakedownValue

// publishLabel writes the signed label for a decision inside its transaction.
// It writes nothing when publication is disabled (no signing key).
func (s *service) publishLabel(ctx context.Context, tx Transaction, subjectURI, value string, negated bool, actionID string, now time.Time) error {
	return s.publishExpiringLabel(ctx, tx, subjectURI, value, negated, actionID, now, nil)
}

// publishExpiringLabel is publishLabel for a label carrying a signed exp;
// expiresAt nil publishes one with no expiry.
func (s *service) publishExpiringLabel(ctx context.Context, tx Transaction, subjectURI, value string, negated bool, actionID string, now time.Time, expiresAt *time.Time) error {
	if s.config.LabelSigningKey == nil {
		return nil
	}
	return tx.InsertLabel(ctx, labels.Issue{
		SourceDID: s.config.InstanceDID,
		URI:       subjectURI,
		Val:       value,
		Neg:       negated,
		ActionID:  actionID,
		Key:       s.config.LabelSigningKey,
		Now:       now,
		ExpiresAt: expiresAt,
	})
}
