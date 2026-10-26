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
	goose.AddMigrationContext(upBackfillCommunityBlobReferences, downBackfillCommunityBlobReferences)
}

func upBackfillCommunityBlobReferences(ctx context.Context, tx *sql.Tx) error {
	var lastDID string
	var communitiesScanned, referencesInserted, malformedCIDsSkipped int64
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT did, avatar_cid, banner_cid
			FROM communities WHERE did > $1 ORDER BY did LIMIT 500`, lastDID)
		if err != nil {
			return fmt.Errorf("query communities for blob backfill: %w", err)
		}
		var batch []communityBlobProfile
		for rows.Next() {
			var community communityBlobProfile
			if err := rows.Scan(&community.did, &community.avatarCID, &community.bannerCID); err != nil {
				rows.Close()
				return fmt.Errorf("scan community for blob backfill: %w", err)
			}
			batch = append(batch, community)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read communities for blob backfill: %w", err)
		}
		for _, community := range batch {
			lastDID = community.did
			communitiesScanned++
			references, skipped := embedblobs.ProfileReferencesAndSkipped(community.avatarCID.String, community.bannerCID.String)
			malformedCIDsSkipped += int64(skipped)
			uri := "at://" + community.did + "/social.coves.community.profile/self"
			for _, reference := range references {
				result, err := tx.ExecContext(ctx, `
					INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
					VALUES ($1, $2, $3, $4, true)
					ON CONFLICT (owner_did, blob_cid, subject_uri, kind) DO NOTHING`,
					community.did, reference.CID, uri, reference.Kind)
				if err != nil {
					return fmt.Errorf("insert community blob reference: %w", err)
				}
				count, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("count inserted community blob reference: %w", err)
				}
				referencesInserted += count
			}
		}
		if len(batch) < 500 {
			break
		}
	}
	log.Printf("community blob references backfill: communities scanned=%d references inserted=%d malformed CIDs skipped=%d",
		communitiesScanned, referencesInserted, malformedCIDsSkipped)
	return nil
}

func downBackfillCommunityBlobReferences(ctx context.Context, tx *sql.Tx) error {
	// Inserted references are indistinguishable from consumer-written ones.
	return nil
}

type communityBlobProfile struct {
	did       string
	avatarCID sql.NullString
	bannerCID sql.NullString
}
