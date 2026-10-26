package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"Coves/internal/atproto/embedblobs"

	"github.com/pressly/goose/v3"
)

func init() {
	goose.AddMigrationContext(upBackfillContentBlobReferences, downBackfillContentBlobReferences)
}

func upBackfillContentBlobReferences(ctx context.Context, tx *sql.Tx) error {
	var census contentBlobCensus
	var lastID int64
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, uri, author_did, community_did, embed, deleted_at IS NULL
			FROM posts WHERE id > $1 ORDER BY id LIMIT 500`, lastID)
		if err != nil {
			return fmt.Errorf("query posts for blob backfill: %w", err)
		}
		var batch []contentBlobPost
		for rows.Next() {
			var post contentBlobPost
			if err := rows.Scan(&post.id, &post.uri, &post.authorDID, &post.communityDID, &post.embed, &post.active); err != nil {
				rows.Close()
				return fmt.Errorf("scan post for blob backfill: %w", err)
			}
			batch = append(batch, post)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read posts for blob backfill: %w", err)
		}
		for _, post := range batch {
			lastID = post.id
			census.postsScanned++
			collection := contentBlobCollection(post.uri)
			var owner string
			switch collection {
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
				return fmt.Errorf("decode post embed: %w", err)
			}
			if census.overImageLimit(embed) {
				continue
			}
			references, skipped := embedblobs.PostReferencesAndSkipped(embed)
			if err := census.insert(ctx, tx, owner, post.uri, post.active, references, skipped); err != nil {
				return err
			}
		}
		if len(batch) < 500 {
			break
		}
	}

	lastID = 0
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, uri, commenter_did, embed, deleted_at IS NULL
			FROM comments WHERE id > $1 ORDER BY id LIMIT 500`, lastID)
		if err != nil {
			return fmt.Errorf("query comments for blob backfill: %w", err)
		}
		var batch []contentBlobComment
		for rows.Next() {
			var comment contentBlobComment
			if err := rows.Scan(&comment.id, &comment.uri, &comment.commenterDID, &comment.embed, &comment.active); err != nil {
				rows.Close()
				return fmt.Errorf("scan comment for blob backfill: %w", err)
			}
			batch = append(batch, comment)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read comments for blob backfill: %w", err)
		}
		for _, comment := range batch {
			lastID = comment.id
			census.commentsScanned++
			embed, err := census.decodeEmbed(comment.embed)
			if err != nil {
				return fmt.Errorf("decode comment embed: %w", err)
			}
			if census.overImageLimit(embed) {
				continue
			}
			references, skipped := embedblobs.CommentReferencesAndSkipped(embed)
			if err := census.insert(ctx, tx, comment.commenterDID, comment.uri, comment.active, references, skipped); err != nil {
				return err
			}
		}
		if len(batch) < 500 {
			break
		}
	}
	log.Printf("content blob references backfill: posts scanned=%d comments scanned=%d"+
		" posts skipped unknown collection=%d records non-object embed=%d records over image limit=%d"+
		" records with skipped or duplicate blob entries=%d references extracted=%d references inserted=%d",
		census.postsScanned, census.commentsScanned,
		census.postsUnknownCollection, census.recordsNonObjectEmbed, census.recordsOverImageLimit,
		census.recordsWithSkippedEntries, census.referencesExtracted, census.referencesInserted)
	return nil
}

func downBackfillContentBlobReferences(ctx context.Context, tx *sql.Tx) error {
	// Keep backfilled references; migration 061's Down drops the table.
	return nil
}

type contentBlobPost struct {
	id           int64
	uri          string
	authorDID    string
	communityDID string
	embed        []byte
	active       bool
}

type contentBlobComment struct {
	id           int64
	uri          string
	commenterDID string
	embed        []byte
	active       bool
}

func contentBlobCollection(uri string) string {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return ""
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return ""
	}
	return parts[1]
}

// contentBlobCensus counts what the backfill scanned, skipped and wrote. It
// holds counts only, never record content.
type contentBlobCensus struct {
	postsScanned              int64
	commentsScanned           int64
	postsUnknownCollection    int64
	recordsNonObjectEmbed     int64
	recordsOverImageLimit     int64
	recordsWithSkippedEntries int64
	referencesExtracted       int64
	referencesInserted        int64
}

// decodeEmbed decodes a stored embed. A JSON value that is present but not an
// object is counted and treated as no embed.
func (census *contentBlobCensus) decodeEmbed(raw []byte) (map[string]interface{}, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	embed, ok := value.(map[string]interface{})
	if !ok && value != nil {
		census.recordsNonObjectEmbed++
	}
	return embed, nil
}

// overImageLimit reports, and counts, an embed the live consumers refuse for
// carrying more than embedblobs.MaxEmbedImages images. They index no such
// record, so the backfill writes no references for it either.
func (census *contentBlobCensus) overImageLimit(embed map[string]interface{}) bool {
	if !embedblobs.ExceedsImageLimit(embed) {
		return false
	}
	census.recordsOverImageLimit++
	return true
}

func (census *contentBlobCensus) insert(ctx context.Context, tx *sql.Tx, owner, uri string, active bool, references []embedblobs.Reference, skipped int) error {
	if skipped > 0 {
		census.recordsWithSkippedEntries++
	}
	census.referencesExtracted += int64(len(references))
	inserted, err := insertContentBlobReferences(ctx, tx, owner, uri, active, references)
	if err != nil {
		return err
	}
	census.referencesInserted += inserted
	return nil
}

func insertContentBlobReferences(ctx context.Context, tx *sql.Tx, owner, uri string, active bool, references []embedblobs.Reference) (int64, error) {
	var inserted int64
	for _, reference := range references {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (owner_did, blob_cid, subject_uri, kind) DO NOTHING`, owner, reference.CID, uri, reference.Kind, active)
		if err != nil {
			return 0, fmt.Errorf("insert content blob reference: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count inserted content blob reference: %w", err)
		}
		inserted += count
	}
	return inserted, nil
}
