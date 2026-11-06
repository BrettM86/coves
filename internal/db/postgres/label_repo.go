package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"Coves/internal/core/labels"
	"Coves/internal/core/moderation"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/lib/pq"
)

// labelAllocationLockKey is the transaction-scoped advisory lock every label
// write takes immediately before it inserts, held to commit. Commits therefore
// happen in seq order, so a reader that has seen seq N never later finds a
// committed row below N, and cts can be made strictly increasing under it.
var labelAllocationLockKey = func() int64 {
	digest := sha256.Sum256([]byte("coves/label-allocation"))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}()

// LabelRepository stores this instance's published labels.
type LabelRepository struct {
	db *sql.DB
}

// NewLabelRepository returns a LabelRepository backed by db.
func NewLabelRepository(db *sql.DB) *LabelRepository {
	return &LabelRepository{db: db}
}

// insertLabel takes the label allocation lock, assigns cts after the latest
// label's, signs issue and stores it, all inside tx.
func insertLabel(ctx context.Context, tx *sql.Tx, issue labels.Issue) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, labelAllocationLockKey); err != nil {
		return fmt.Errorf("taking the label allocation lock: %w", err)
	}
	var previous string
	err := tx.QueryRowContext(ctx, `SELECT cts FROM labels ORDER BY seq DESC LIMIT 1`).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading the latest label cts: %w", err)
	}
	createdAt, err := labels.NextCreatedAt(issue.Now, previous)
	if err != nil {
		return err
	}
	label, err := labels.Sign(issue, createdAt)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO labels (src, uri, cid, val, neg, cts, exp, sig, ver, action_id, signer_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, label.Src, label.URI, label.CID, label.Val, label.Neg, label.CreatedAt, label.ExpiresAt,
		label.Sig, label.Version, label.ActionID, label.SignerKey); err != nil {
		return fmt.Errorf("inserting label: %w", err)
	}
	return nil
}

// Query returns a page of labels matching q in ascending seq order. A URI
// pattern ending in * matches every URI with the preceding prefix; any other
// pattern matches exactly.
func (r *LabelRepository) Query(ctx context.Context, q labels.Query) (labels.Page, error) {
	if len(q.URIPatterns) == 0 {
		return labels.Page{}, fmt.Errorf("%w: at least one URI pattern is required", labels.ErrInvalidQuery)
	}
	if err := q.ValidateValues(); err != nil {
		return labels.Page{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = labels.DefaultQueryLimit
	}
	if limit < 1 || limit > labels.MaxQueryLimit {
		return labels.Page{}, fmt.Errorf("%w: limit must be between 1 and %d", labels.ErrInvalidQuery, labels.MaxQueryLimit)
	}
	var after int64
	if q.Cursor != "" {
		parsed, err := strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || parsed < 0 {
			return labels.Page{}, fmt.Errorf("%w: cursor must be a label sequence number", labels.ErrInvalidQuery)
		}
		after = parsed
	}

	var exact, prefixes []string
	for _, pattern := range q.URIPatterns {
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			prefixes = append(prefixes, escapeLikePattern(prefix)+"%")
		} else {
			exact = append(exact, pattern)
		}
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, src, uri, cid, val, neg, cts, exp, sig, ver, action_id, signer_key
		FROM labels
		WHERE seq > $1
		  AND (uri = ANY($2::text[]) OR uri LIKE ANY($3::text[]))
		  AND (cardinality($4::text[]) = 0 OR src = ANY($4::text[]))
		ORDER BY seq
		LIMIT $5
	`, after, pq.Array(nonNilStrings(exact)), pq.Array(nonNilStrings(prefixes)), pq.Array(nonNilStrings(q.Sources)), limit)
	if err != nil {
		return labels.Page{}, fmt.Errorf("querying labels: %w", err)
	}
	var page labels.Page
	if page.Labels, err = scanLabels(rows); err != nil {
		return labels.Page{}, err
	}
	if len(page.Labels) > 0 {
		page.Cursor = strconv.FormatInt(page.Labels[len(page.Labels)-1].Seq, 10)
	}
	return page, nil
}

// LabelsAfter returns up to limit labels with seq greater than afterSeq, in
// ascending seq order.
func (r *LabelRepository) LabelsAfter(ctx context.Context, afterSeq int64, limit int) ([]labels.Label, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT seq, src, uri, cid, val, neg, cts, exp, sig, ver, action_id, signer_key
		FROM labels
		WHERE seq > $1
		ORDER BY seq
		LIMIT $2
	`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("querying labels after %d: %w", afterSeq, err)
	}
	return scanLabels(rows)
}

// LatestSeq returns the highest committed label seq, or 0 when there is none.
func (r *LabelRepository) LatestSeq(ctx context.Context) (int64, error) {
	var latest int64
	if err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM labels`).Scan(&latest); err != nil {
		return 0, fmt.Errorf("reading the latest label seq: %w", err)
	}
	return latest, nil
}

// scanLabels reads every row of a full labels SELECT and closes rows.
func scanLabels(rows *sql.Rows) ([]labels.Label, error) {
	defer rows.Close()
	var scanned []labels.Label
	for rows.Next() {
		var label labels.Label
		var cid, expiresAt sql.NullString
		if err := rows.Scan(&label.Seq, &label.Src, &label.URI, &cid, &label.Val, &label.Neg, &label.CreatedAt,
			&expiresAt, &label.Sig, &label.Version, &label.ActionID, &label.SignerKey); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		if cid.Valid {
			label.CID = &cid.String
		}
		if expiresAt.Valid {
			label.ExpiresAt = &expiresAt.String
		}
		scanned = append(scanned, label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading labels: %w", err)
	}
	return scanned, nil
}

// resignBatchSize bounds how many labels a rotation reads into memory at once.
const resignBatchSize = 1000

// SigningKeyHistory returns every label signing key this labeler has used,
// oldest first.
func (r *LabelRepository) SigningKeyHistory(ctx context.Context) ([]labels.SigningKeyRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT public_multibase, began_at, retired_at FROM label_signing_keys ORDER BY began_at, public_multibase
	`)
	if err != nil {
		return nil, fmt.Errorf("querying label signing keys: %w", err)
	}
	defer rows.Close()
	var history []labels.SigningKeyRecord
	for rows.Next() {
		var record labels.SigningKeyRecord
		var retiredAt sql.NullTime
		if err := rows.Scan(&record.PublicKey, &record.BeganAt, &retiredAt); err != nil {
			return nil, fmt.Errorf("scanning label signing key: %w", err)
		}
		if retiredAt.Valid {
			record.RetiredAt = &retiredAt.Time
		}
		history = append(history, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading label signing keys: %w", err)
	}
	return history, nil
}

// SignerKeys returns the distinct public multibases that signed stored labels.
func (r *LabelRepository) SignerKeys(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT signer_key FROM labels ORDER BY signer_key`)
	if err != nil {
		return nil, fmt.Errorf("querying label signers: %w", err)
	}
	defer rows.Close()
	var signers []string
	for rows.Next() {
		var signer string
		if err := rows.Scan(&signer); err != nil {
			return nil, fmt.Errorf("scanning label signer: %w", err)
		}
		signers = append(signers, signer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading label signers: %w", err)
	}
	return signers, nil
}

// RecordSigningKey records publicKey as the current label signing key and
// every other key as retired, in one transaction.
func (r *LabelRepository) RecordSigningKey(ctx context.Context, publicKey string) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning label signing key record: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = recordCurrentSigningKey(ctx, tx, publicKey); err != nil {
		return fmt.Errorf("recording label signing key: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing label signing key record: %w", err)
	}
	return nil
}

// recordCurrentSigningKey retires every other current key, then adds
// publicKey to the key history, or clears its retirement when an earlier
// rotation retired it, so the history holds one current key.
func recordCurrentSigningKey(ctx context.Context, tx *sql.Tx, publicKey string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE label_signing_keys SET retired_at = NOW()
		WHERE retired_at IS NULL AND public_multibase <> $1
	`, publicKey); err != nil {
		return fmt.Errorf("retiring other label signing keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO label_signing_keys (public_multibase) VALUES ($1)
		ON CONFLICT (public_multibase) DO UPDATE SET retired_at = NULL
		WHERE label_signing_keys.retired_at IS NOT NULL
	`, publicKey); err != nil {
		return fmt.Errorf("adding label signing key: %w", err)
	}
	return nil
}

// RotateSigningKey re-signs every label signed by retiringKey with key in one
// transaction holding the label allocation lock, so no decision writes a label
// under the old key meanwhile. seq, cts and every other signed field stay as
// they were and no seq is allocated. The key history records retiringKey as
// retired and key as current in the same transaction. Any failure, including
// a label signed by some third key, rolls the whole rotation back.
func (r *LabelRepository) RotateSigningKey(ctx context.Context, key atcrypto.PrivateKey, retiringKey string) (err error) {
	publicKey, err := key.PublicKey()
	if err != nil {
		return fmt.Errorf("deriving label signing public key: %w", err)
	}
	newKey := publicKey.Multibase()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning label key rotation: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, labelAllocationLockKey); err != nil {
		return fmt.Errorf("taking the label allocation lock: %w", err)
	}
	var after int64
	for {
		var batch []labels.Label
		batch, err = labelsNotSignedBy(ctx, tx, newKey, after)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, label := range batch {
			if label.SignerKey != retiringKey {
				err = fmt.Errorf("%w: label %d is signed by %s", labels.ErrSigningKeyMismatch, label.Seq, label.SignerKey)
				return err
			}
			var resigned labels.Label
			if resigned, err = labels.Resign(label, key); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE labels SET sig = $2, signer_key = $3 WHERE seq = $1`,
				resigned.Seq, resigned.Sig, resigned.SignerKey); err != nil {
				return fmt.Errorf("updating re-signed label %d: %w", label.Seq, err)
			}
			after = label.Seq
		}
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO label_signing_keys (public_multibase, retired_at) VALUES ($1, NOW())
		ON CONFLICT (public_multibase) DO UPDATE SET retired_at = NOW()
		WHERE label_signing_keys.retired_at IS NULL
	`, retiringKey); err != nil {
		return fmt.Errorf("retiring label signing key: %w", err)
	}
	if err = recordCurrentSigningKey(ctx, tx, newKey); err != nil {
		return fmt.Errorf("recording label signing key: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing label key rotation: %w", err)
	}
	return nil
}

// labelsNotSignedBy reads the next batch of labels after seq whose signer is
// not signerKey, locking them for update.
func labelsNotSignedBy(ctx context.Context, tx *sql.Tx, signerKey string, after int64) ([]labels.Label, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT seq, src, uri, cid, val, neg, cts, exp, ver, action_id, signer_key
		FROM labels WHERE signer_key <> $1 AND seq > $2
		ORDER BY seq LIMIT $3 FOR UPDATE
	`, signerKey, after, resignBatchSize)
	if err != nil {
		return nil, fmt.Errorf("reading labels to re-sign: %w", err)
	}
	defer rows.Close()
	var batch []labels.Label
	for rows.Next() {
		var label labels.Label
		var cid, expiresAt sql.NullString
		if err := rows.Scan(&label.Seq, &label.Src, &label.URI, &cid, &label.Val, &label.Neg, &label.CreatedAt,
			&expiresAt, &label.Version, &label.ActionID, &label.SignerKey); err != nil {
			return nil, fmt.Errorf("scanning label to re-sign: %w", err)
		}
		if cid.Valid {
			label.CID = &cid.String
		}
		if expiresAt.Valid {
			label.ExpiresAt = &expiresAt.String
		}
		batch = append(batch, label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading labels to re-sign: %w", err)
	}
	return batch, nil
}

// ExportActiveDecisions publishes, once ever, one current label for each
// active instance-scope decision by sourceDID whose subject is still indexed:
// !takedown for a removal, the label value for an NSFW label. Each gets a new
// cts under the label allocation lock. Taking that lock before reading the
// decisions orders the export against live issuance: a restore that commits
// first is seen as inactive, and one that commits later writes its negation
// after the exported label. The completion mark commits with the labels. An
// account ban in force at now is exported as !takedown on the account DID with
// its exp; a lapsed one is not.
func (r *LabelRepository) ExportActiveDecisions(ctx context.Context, sourceDID string, key atcrypto.PrivateKey, now time.Time) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning label export: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, labelAllocationLockKey); err != nil {
		return fmt.Errorf("taking the label allocation lock: %w", err)
	}
	var completed bool
	if err = tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM label_publication_state WHERE export_completed_at IS NOT NULL)
	`).Scan(&completed); err != nil {
		return fmt.Errorf("reading label export state: %w", err)
	}
	if completed {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return fmt.Errorf("rolling back the already-completed label export: %w", rollbackErr)
		}
		return nil
	}

	type decision struct {
		subjectURI, value, actionID string
		expiresAt                   *time.Time
	}
	var decisions []decision
	// An account ban (an instance removal on at://<did>, no collection) is
	// exported while in force, as !takedown on the bare DID carrying its exp;
	// a lapsed one is not. A community removal (the same shape on an indexed
	// community's DID, never expiring) is exported as !takedown on the bare
	// community DID.
	rows, err := tx.QueryContext(ctx, `
		SELECT CASE WHEN a.subject_collection IS NULL THEN substr(d.subject_uri, 6) ELSE d.subject_uri END,
		       CASE d.kind WHEN 'removal' THEN $2 ELSE d.value END, d.active_action_id, a.expires_at
		FROM moderation_decisions d
		JOIN moderation_actions a ON a.id = d.active_action_id
		WHERE d.active AND d.authority_did = $1 AND d.scope_kind = 'instance'
		  AND (d.kind = 'removal' OR (d.kind = 'label' AND d.value = $3))
		  AND (EXISTS (SELECT 1 FROM posts p WHERE p.uri = d.subject_uri)
		       OR EXISTS (SELECT 1 FROM comments c WHERE c.uri = d.subject_uri)
		       OR (a.subject_collection IS NULL AND d.kind = 'removal'
		           AND (a.expires_at IS NULL OR a.expires_at > $4)))
		ORDER BY a.created_at, a.id
	`, sourceDID, labels.TakedownValue, moderation.LabelNSFW, now)
	if err != nil {
		return fmt.Errorf("reading active decisions to export: %w", err)
	}
	for rows.Next() {
		var d decision
		var expiresAt sql.NullTime
		if err = rows.Scan(&d.subjectURI, &d.value, &d.actionID, &expiresAt); err != nil {
			rows.Close()
			return fmt.Errorf("scanning active decision: %w", err)
		}
		if expiresAt.Valid {
			expiry := expiresAt.Time.UTC()
			d.expiresAt = &expiry
		}
		decisions = append(decisions, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("reading active decisions to export: %w", err)
	}

	for _, d := range decisions {
		if err = insertLabel(ctx, tx, labels.Issue{
			SourceDID: sourceDID, URI: d.subjectURI, Val: d.value, ActionID: d.actionID, Key: key, Now: now,
			ExpiresAt: d.expiresAt,
		}); err != nil {
			return fmt.Errorf("exporting label for %s: %w", d.subjectURI, err)
		}
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO label_publication_state (singleton, export_completed_at) VALUES (true, NOW())
		ON CONFLICT (singleton) DO UPDATE SET export_completed_at = EXCLUDED.export_completed_at
	`); err != nil {
		return fmt.Errorf("marking label export complete: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing label export: %w", err)
	}
	return nil
}

// escapeLikePattern escapes LIKE's wildcards and its escape character, so a
// prefix matches only itself. The default LIKE escape character is backslash.
func escapeLikePattern(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// nonNilStrings returns values, or an empty slice for nil, so the array
// parameter is an empty array rather than NULL.
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// HasPublicationState reports whether any published label is stored or the
// label export has completed, so label publication was on at some boot.
func (r *LabelRepository) HasPublicationState(ctx context.Context) (bool, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM labels)
		    OR EXISTS (SELECT 1 FROM label_publication_state WHERE export_completed_at IS NOT NULL)
	`).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking for label publication state: %w", err)
	}
	return exists, nil
}
