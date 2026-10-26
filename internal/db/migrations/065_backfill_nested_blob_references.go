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
	goose.AddMigrationContext(upBackfillNestedBlobReferences, downBackfillNestedBlobReferences)
}

func upBackfillNestedBlobReferences(ctx context.Context, tx *sql.Tx) error {
	var census contentBlobCensus
	var blocksInserted int64
	var lastID int64
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, uri, author_did, community_did, embed, deleted_at IS NULL
			FROM posts WHERE id > $1 AND embed->>'$type' = $2
			ORDER BY id LIMIT 500`, lastID, embedblobs.TypeImagesWithAttachment)
		if err != nil {
			return fmt.Errorf("query posts for nested blob backfill: %w", err)
		}
		var batch []contentBlobPost
		for rows.Next() {
			var post contentBlobPost
			if err := rows.Scan(&post.id, &post.uri, &post.authorDID, &post.communityDID, &post.embed, &post.active); err != nil {
				rows.Close()
				return fmt.Errorf("scan post for nested blob backfill: %w", err)
			}
			batch = append(batch, post)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read posts for nested blob backfill: %w", err)
		}
		for _, post := range batch {
			lastID = post.id
			census.postsScanned++
			var owner string
			switch contentBlobCollection(post.uri) {
			case "social.coves.community.postv2":
				owner = post.authorDID
			case "social.coves.community.post":
				owner = post.communityDID
			default:
				census.postsUnknownCollection++
				continue
			}
			embed, err := census.decodeEmbed(post.embed)
			if err != nil {
				return fmt.Errorf("decode nested post embed: %w", err)
			}
			if census.overImageLimit(embed) {
				continue
			}
			references, skipped := embedblobs.PostReferencesAndSkipped(embed)
			if err := census.insert(ctx, tx, owner, post.uri, post.active, references, skipped); err != nil {
				return err
			}
			inserted, err := insertNestedBlobBlocks(ctx, tx, owner, post.uri, references)
			if err != nil {
				return err
			}
			blocksInserted += inserted
		}
		if len(batch) < 500 {
			break
		}
	}

	lastID = 0
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, uri, commenter_did, embed, deleted_at IS NULL
			FROM comments WHERE id > $1 AND embed->>'$type' = $2
			ORDER BY id LIMIT 500`, lastID, embedblobs.TypeImagesWithAttachment)
		if err != nil {
			return fmt.Errorf("query comments for nested blob backfill: %w", err)
		}
		var batch []contentBlobComment
		for rows.Next() {
			var comment contentBlobComment
			if err := rows.Scan(&comment.id, &comment.uri, &comment.commenterDID, &comment.embed, &comment.active); err != nil {
				rows.Close()
				return fmt.Errorf("scan comment for nested blob backfill: %w", err)
			}
			batch = append(batch, comment)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read comments for nested blob backfill: %w", err)
		}
		for _, comment := range batch {
			lastID = comment.id
			census.commentsScanned++
			embed, err := census.decodeEmbed(comment.embed)
			if err != nil {
				return fmt.Errorf("decode nested comment embed: %w", err)
			}
			if census.overImageLimit(embed) {
				continue
			}
			references, skipped := embedblobs.CommentReferencesAndSkipped(embed)
			if err := census.insert(ctx, tx, comment.commenterDID, comment.uri, comment.active, references, skipped); err != nil {
				return err
			}
			inserted, err := insertNestedBlobBlocks(ctx, tx, comment.commenterDID, comment.uri, references)
			if err != nil {
				return err
			}
			blocksInserted += inserted
		}
		if len(batch) < 500 {
			break
		}
	}
	log.Printf("nested blob references backfill: posts scanned=%d comments scanned=%d"+
		" posts skipped unknown collection=%d records non-object embed=%d records over image limit=%d"+
		" records with skipped or duplicate blob entries=%d references extracted=%d references inserted=%d blocks inserted=%d",
		census.postsScanned, census.commentsScanned,
		census.postsUnknownCollection, census.recordsNonObjectEmbed, census.recordsOverImageLimit,
		census.recordsWithSkippedEntries, census.referencesExtracted, census.referencesInserted, blocksInserted)
	return nil
}

func downBackfillNestedBlobReferences(ctx context.Context, tx *sql.Tx) error {
	// References and blocks cannot be distinguished from consumer-written rows.
	return nil
}

func insertNestedBlobBlocks(ctx context.Context, tx *sql.Tx, owner, uri string, references []embedblobs.Reference) (int64, error) {
	var inserted int64
	seen := make(map[string]bool)
	for _, reference := range references {
		// Different reference kinds for the same CID still need only one block.
		if seen[reference.CID] {
			continue
		}
		seen[reference.CID] = true
		result, err := tx.ExecContext(ctx, `
			INSERT INTO moderation_media_blocks (owner_did, blob_cid, action_id, active)
			SELECT $1, $2, d.active_action_id, true
			FROM moderation_decisions d
			JOIN moderation_actions a ON a.id = d.active_action_id
			WHERE d.subject_uri = $3 AND d.active AND d.scope_kind = 'instance'
			  AND d.kind = 'removal' AND a.origin = 'local'
			ON CONFLICT ON CONSTRAINT moderation_media_block_key DO NOTHING`, owner, reference.CID, uri)
		if err != nil {
			return 0, fmt.Errorf("insert nested blob block: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count inserted nested blob block: %w", err)
		}
		inserted += count
	}
	return inserted, nil
}
