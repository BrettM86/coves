package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"Coves/internal/atproto/embedblobs"

	"github.com/pressly/goose/v3"
)

func init() {
	goose.AddMigrationContext(upBackfillUserBlobReferences, downBackfillUserBlobReferences)
}

func upBackfillUserBlobReferences(ctx context.Context, tx *sql.Tx) error {
	var lastDID string
	var usersScanned, referencesInserted, malformedCIDsSkipped int64
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT did, avatar_cid, banner_cid
			FROM users WHERE did > $1 ORDER BY did LIMIT 500`, lastDID)
		if err != nil {
			return fmt.Errorf("query users for blob backfill: %w", err)
		}
		var batch []userBlobProfile
		for rows.Next() {
			var user userBlobProfile
			if err := rows.Scan(&user.did, &user.avatarCID, &user.bannerCID); err != nil {
				rows.Close()
				return fmt.Errorf("scan user for blob backfill: %w", err)
			}
			batch = append(batch, user)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read users for blob backfill: %w", err)
		}
		for _, user := range batch {
			lastDID = user.did
			usersScanned++
			references, skipped := embedblobs.ProfileReferencesAndSkipped(user.avatarCID.String, user.bannerCID.String)
			malformedCIDsSkipped += int64(skipped)
			uri := "at://" + user.did + "/social.coves.actor.profile/self"
			for _, reference := range references {
				result, err := tx.ExecContext(ctx, `
					INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
					VALUES ($1, $2, $3, $4, true)
					ON CONFLICT (owner_did, blob_cid, subject_uri, kind) DO NOTHING`,
					user.did, reference.CID, uri, reference.Kind)
				if err != nil {
					return fmt.Errorf("insert user blob reference: %w", err)
				}
				count, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("count inserted user blob reference: %w", err)
				}
				referencesInserted += count
			}
		}
		if len(batch) < 500 {
			break
		}
	}
	log.Printf("user blob references backfill: users scanned=%d references inserted=%d malformed CIDs skipped=%d",
		usersScanned, referencesInserted, malformedCIDsSkipped)
	return nil
}

func downBackfillUserBlobReferences(ctx context.Context, tx *sql.Tx) error {
	// Keep backfilled references; rerunning Up inserts only missing rows.
	return nil
}

type userBlobProfile struct {
	did       string
	avatarCID sql.NullString
	bannerCID sql.NullString
}
