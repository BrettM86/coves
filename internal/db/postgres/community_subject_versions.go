package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// advanceCommunitySubjectVersion adds one to the community moderation version
// of subject in communityDID. Call it inside the transaction that changed the
// subject's community moderation index, and only when that transaction changed
// an index row, so a failed version write rolls the change back with it.
func advanceCommunitySubjectVersion(ctx context.Context, tx *sql.Tx, communityDID, subject string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO community_subject_versions (community_did, subject, version, updated_at)
		VALUES ($1, $2, 1, NOW())
		ON CONFLICT (community_did, subject) DO UPDATE SET
		    version = community_subject_versions.version + 1, updated_at = NOW()
	`, communityDID, subject); err != nil {
		return fmt.Errorf("advance community subject version of %s in %s: %w", subject, communityDID, err)
	}
	return nil
}
