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
	goose.AddMigrationContext(upStampUntypedGalleryTypes, downStampUntypedGalleryTypes)
}

// imagesWithAttachment.images is a plain ref, so atproto leaves its $type
// optional, and rows ingested before canonicalization may lack it. Readers
// that keyed off that $type saw no gallery, so 062 and 065 wrote no blob
// references or removal blocks for its images. This migration stamps the
// $type and writes what those backfills would have. It selects exactly the
// rows scripts/sql/count_untyped_embed_galleries.sql counts: a gallery with
// "$type": null has the key and is left alone.
func upStampUntypedGalleryTypes(ctx context.Context, tx *sql.Tx) error {
	var census contentBlobCensus
	var postsStamped, commentsStamped, blocksInserted int64
	var lastID int64
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, uri, author_did, community_did, embed, deleted_at IS NULL
			FROM posts WHERE id > $1 AND embed->>'$type' = $2
			  AND jsonb_typeof(embed->'images') = 'object' AND NOT (embed->'images') ? '$type'
			ORDER BY id LIMIT 500`, lastID, embedblobs.TypeImagesWithAttachment)
		if err != nil {
			return fmt.Errorf("query posts for untyped gallery stamp: %w", err)
		}
		var batch []contentBlobPost
		for rows.Next() {
			var post contentBlobPost
			if err := rows.Scan(&post.id, &post.uri, &post.authorDID, &post.communityDID, &post.embed, &post.active); err != nil {
				rows.Close()
				return fmt.Errorf("scan post for untyped gallery stamp: %w", err)
			}
			batch = append(batch, post)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read posts for untyped gallery stamp: %w", err)
		}
		for _, post := range batch {
			lastID = post.id
			census.postsScanned++
			embed, err := stampUntypedGallery(ctx, tx, "posts", post.id, post.embed, &census)
			if err != nil {
				return err
			}
			postsStamped++
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
			// Removal blocks never serve anything, so only the reference
			// insert honours the image limit.
			references, skipped := embedblobs.PostReferencesAndSkipped(embed)
			if !census.overImageLimit(embed) {
				if err := census.insert(ctx, tx, owner, post.uri, post.active, references, skipped); err != nil {
					return err
				}
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
			  AND jsonb_typeof(embed->'images') = 'object' AND NOT (embed->'images') ? '$type'
			ORDER BY id LIMIT 500`, lastID, embedblobs.TypeImagesWithAttachment)
		if err != nil {
			return fmt.Errorf("query comments for untyped gallery stamp: %w", err)
		}
		var batch []contentBlobComment
		for rows.Next() {
			var comment contentBlobComment
			if err := rows.Scan(&comment.id, &comment.uri, &comment.commenterDID, &comment.embed, &comment.active); err != nil {
				rows.Close()
				return fmt.Errorf("scan comment for untyped gallery stamp: %w", err)
			}
			batch = append(batch, comment)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read comments for untyped gallery stamp: %w", err)
		}
		for _, comment := range batch {
			lastID = comment.id
			census.commentsScanned++
			embed, err := stampUntypedGallery(ctx, tx, "comments", comment.id, comment.embed, &census)
			if err != nil {
				return err
			}
			commentsStamped++
			references, skipped := embedblobs.CommentReferencesAndSkipped(embed)
			if !census.overImageLimit(embed) {
				if err := census.insert(ctx, tx, comment.commenterDID, comment.uri, comment.active, references, skipped); err != nil {
					return err
				}
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
	log.Printf("untyped gallery stamp: posts stamped=%d comments stamped=%d"+
		" posts skipped unknown collection=%d records over image limit=%d"+
		" records with skipped or duplicate blob entries=%d references extracted=%d references inserted=%d blocks inserted=%d",
		postsStamped, commentsStamped,
		census.postsUnknownCollection, census.recordsOverImageLimit,
		census.recordsWithSkippedEntries, census.referencesExtracted, census.referencesInserted, blocksInserted)
	return nil
}

func downStampUntypedGalleryTypes(ctx context.Context, tx *sql.Tx) error {
	// The stamped $type is the canonical shape, and references and blocks
	// cannot be distinguished from consumer-written rows.
	return nil
}

// stampUntypedGallery writes the gallery $type into the stored row and returns
// the decoded embed carrying it, for the reference extractors.
func stampUntypedGallery(ctx context.Context, tx *sql.Tx, table string, id int64, raw []byte, census *contentBlobCensus) (map[string]interface{}, error) {
	embed, err := census.decodeEmbed(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s embed for untyped gallery stamp: %w", table, err)
	}
	gallery, isObject := embed["images"].(map[string]interface{})
	if !isObject {
		return nil, fmt.Errorf("%s row %d: embed images is not an object", table, id)
	}
	gallery["$type"] = embedblobs.TypeImages
	// The table name is one of two constants from the caller.
	if _, err := tx.ExecContext(ctx, `
		UPDATE `+table+` SET embed = jsonb_set(embed, '{images,$type}', to_jsonb($2::text))
		WHERE id = $1`, id, embedblobs.TypeImages); err != nil {
		return nil, fmt.Errorf("stamp %s gallery $type: %w", table, err)
	}
	return embed, nil
}
