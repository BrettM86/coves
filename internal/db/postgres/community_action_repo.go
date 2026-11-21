package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/notifications"

	"github.com/lib/pq"
)

// CommunityActionRepository is the Postgres community action store: the
// community write intents (migration 084), and the action transaction that
// takes the community locks and applies a completed write's local effect.
//
// Action transactions run on actionDB, a pool of their own: each holds its
// connection while it waits on or holds the community locks, and the lock
// holder takes further connections from db (authorization, credentials, the
// intent). Were both on one bounded pool, waiters could hold every connection
// and the holder could never finish.
type CommunityActionRepository struct {
	db            *sql.DB
	actionDB      *sql.DB
	notifications notifications.Repository
}

var _ moderation.CommunityActionStore = (*CommunityActionRepository)(nil)

// NewCommunityActionRepository builds the community action store. db is the
// request pool; actionDB is the pool action transactions hold connections
// from, which the server opens apart from db (cmd/server
// openCommunityActionDatabase).
func NewCommunityActionRepository(db, actionDB *sql.DB) *CommunityActionRepository {
	return &CommunityActionRepository{db: db, actionDB: actionDB, notifications: NewNotificationRepository(db)}
}

const communityIntentColumns = `id, action, actor_did, admin_acting, community_did, subject_did, target_uri, op,
	record, COALESCE(replaced_cid, ''), guard_commit_cid, guard_rev, idempotency_key, fingerprint,
	idempotency_retention_microseconds, state, COALESCE(commit_rev, ''), COALESCE(record_cid, ''), COALESCE(action_id, ''), created_at, expires_at,
	COALESCE(invitation_id, ''), COALESCE(audit_actor_did, ''), COALESCE(paired_target_uri, ''), paired_record,
	COALESCE(paired_replaced_cid, ''), COALESCE(subject_uri, '')`

// BeginAction opens one action transaction and takes lock's locks in the one
// community lock order (docs/MODERATION.md): the shared erasure gates of the
// lock's accounts and gate-only accounts in ascending DID order
// (gateRoleSubjects), the roster
// advisory lock, the
// per-(community, account) advisory locks in ascending DID order, the target
// records' rev-gate rows in ascending URI order, each inserted at rev ” when
// absent, then the ban-subject advisory locks in ascending DID order. All are
// held to the end of the transaction.
func (r *CommunityActionRepository) BeginAction(ctx context.Context, lock moderation.CommunityActionLock) (moderation.CommunityActionTx, error) {
	tx, err := r.actionDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin community action transaction: %w", err)
	}
	if err := lockCommunityAction(ctx, tx, lock); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		return nil, err
	}
	return &communityActionTransaction{tx: tx, notifications: r.notifications}, nil
}

func lockCommunityAction(ctx context.Context, tx *sql.Tx, lock moderation.CommunityActionLock) error {
	// The accounts and the gate-only accounts are every party whose role
	// record or invitation the action can write; their erasure gates come
	// before the invitation, role and version rows the action locks.
	gated := append(append([]string(nil), lock.AccountDIDs...), lock.ErasureGateDIDs...)
	if _, err := gateRoleSubjects(ctx, tx, gated...); err != nil {
		return err
	}
	if lock.Roster {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('community-roster:' || $1))`, lock.CommunityDID); err != nil {
			return fmt.Errorf("lock community roster: %w", err)
		}
	}
	for _, account := range ascendingUnique(lock.AccountDIDs) {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('community-actor:' || $1 || ' ' || $2))`,
			lock.CommunityDID, account); err != nil {
			return fmt.Errorf("lock community account: %w", err)
		}
	}
	for _, target := range ascendingUnique(append([]string{lock.TargetURI}, lock.OtherTargetURIs...)) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, '')
			ON CONFLICT (record_uri) DO NOTHING
		`, target); err != nil {
			return fmt.Errorf("insert target record rev gate: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM jetstream_record_revs WHERE record_uri = $1 FOR UPDATE`, target); err != nil {
			return fmt.Errorf("lock target record rev gate: %w", err)
		}
	}
	for _, subject := range ascendingUnique(lock.BanSubjectDIDs) {
		if err := lockBanSubject(ctx, tx, subject); err != nil {
			return err
		}
	}
	return nil
}

// ascendingUnique is values without empty or repeated entries, ascending.
func ascendingUnique(values []string) []string {
	sorted := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			sorted = append(sorted, value)
		}
	}
	sort.Strings(sorted)
	unique := sorted[:0]
	for i, value := range sorted {
		if i == 0 || value != sorted[i-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

// SaveIntent stores a new open intent in its own transaction, so it is durable
// before the PDS write. A second open intent on the same target is refused by
// the one-open-intent-per-target index with moderation.ErrStateConflict.
func (r *CommunityActionRepository) SaveIntent(ctx context.Context, intent moderation.CommunityIntent) error {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO community_write_intents
		    (id, action, actor_did, admin_acting, community_did, subject_did, target_uri, op, record,
		     replaced_cid, guard_commit_cid, guard_rev, idempotency_key, fingerprint,
		     idempotency_retention_microseconds, state, created_at, expires_at,
		     invitation_id, audit_actor_did, paired_target_uri, paired_record, paired_replaced_cid, subject_uri)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12, $13, $14, $15, $16, $17, $18,
		        NULLIF($19, ''), NULLIF($20, ''), NULLIF($21, ''), $22, NULLIF($23, ''), NULLIF($24, ''))
	`, intent.ID, intent.Action, intent.ActorDID, intent.AdminActing, intent.CommunityDID, intent.SubjectDID,
		intent.TargetURI, intent.Op, jsonColumn(intent.Record), intent.ReplacedCID, intent.GuardCommitCID, intent.GuardRev,
		intent.IdempotencyKey, intent.Fingerprint, intent.IdempotencyRetention.Microseconds(), string(intent.State),
		intent.CreatedAt.UTC(), intent.ExpiresAt.UTC(),
		intent.InvitationID, intent.AuditActorDID, intent.PairedTargetURI, jsonColumn(intent.PairedRecord),
		intent.PairedReplacedCID, intent.SubjectURI); err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == pqUniqueViolation &&
			(pqErr.Constraint == "community_write_intents_open_target" || pqErr.Constraint == "community_write_intents_open_paired_target") {
			return fmt.Errorf("%w: another write to %s is in flight", moderation.ErrStateConflict, intent.TargetURI)
		}
		return fmt.Errorf("insert community write intent: %w", err)
	}
	return nil
}

// jsonColumn is a JSONB parameter: the body, or NULL when it is empty.
func jsonColumn(body json.RawMessage) any {
	if len(body) == 0 {
		return nil
	}
	return []byte(body)
}

// ExpiredOpenIntents lists up to limit open intents whose expiry is at or
// before now, oldest expiry first, starting after the intent after (nil
// starts from the first).
func (r *CommunityActionRepository) ExpiredOpenIntents(ctx context.Context, now time.Time, after *moderation.CommunityIntent, limit int) ([]moderation.CommunityIntent, error) {
	var afterExpiresAt any
	afterID := ""
	if after != nil {
		afterExpiresAt, afterID = after.ExpiresAt.UTC(), after.ID
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE state = 'open' AND expires_at <= $1
		  AND ($3::timestamptz IS NULL OR (expires_at, id) > ($3::timestamptz, $4::text))
		ORDER BY expires_at, id
		LIMIT $2
	`, now.UTC(), limit, afterExpiresAt, afterID)
	if err != nil {
		return nil, fmt.Errorf("list expired community write intents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var intents []moderation.CommunityIntent
	for rows.Next() {
		intent, err := scanCommunityIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read expired community write intents: %w", err)
	}
	return intents, nil
}

func scanCommunityIntent(row interface{ Scan(dest ...any) error }) (moderation.CommunityIntent, error) {
	var intent moderation.CommunityIntent
	var record, pairedRecord []byte
	var state string
	var retentionMicroseconds int64
	if err := row.Scan(&intent.ID, &intent.Action, &intent.ActorDID, &intent.AdminActing, &intent.CommunityDID,
		&intent.SubjectDID, &intent.TargetURI, &intent.Op, &record, &intent.ReplacedCID, &intent.GuardCommitCID,
		&intent.GuardRev, &intent.IdempotencyKey, &intent.Fingerprint, &retentionMicroseconds, &state,
		&intent.CommitRev, &intent.RecordCID, &intent.ActionID, &intent.CreatedAt, &intent.ExpiresAt,
		&intent.InvitationID, &intent.AuditActorDID, &intent.PairedTargetURI, &pairedRecord,
		&intent.PairedReplacedCID, &intent.SubjectURI); err != nil {
		return moderation.CommunityIntent{}, fmt.Errorf("scan community write intent: %w", err)
	}
	if record != nil {
		intent.Record = json.RawMessage(record)
	}
	if pairedRecord != nil {
		intent.PairedRecord = json.RawMessage(pairedRecord)
	}
	intent.IdempotencyRetention = time.Duration(retentionMicroseconds) * time.Microsecond
	intent.State = moderation.CommunityIntentState(state)
	return intent, nil
}

type communityActionTransaction struct {
	tx            *sql.Tx
	notifications notifications.Repository
}

func (t *communityActionTransaction) IdempotencyRecord(ctx context.Context, actorDID, communityDID, key string, now time.Time) (*moderation.CommunityIdempotencyRecord, error) {
	var record moderation.CommunityIdempotencyRecord
	var stored []byte
	err := t.tx.QueryRowContext(ctx, `
		SELECT fingerprint, stored_result FROM moderation_idempotency_keys
		WHERE actor_did = $1 AND authority_did = $2 AND key = $3 AND expires_at > $4
	`, actorDID, communityDID, key, now.UTC()).Scan(&record.Fingerprint, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read community idempotency record: %w", err)
	}
	if err := json.Unmarshal(stored, &record.Result); err != nil {
		return nil, fmt.Errorf("decode community idempotency record: %w", err)
	}
	return &record, nil
}

// IntentByKey returns the key's newest intent in any state, locked.
func (t *communityActionTransaction) IntentByKey(ctx context.Context, actorDID, communityDID, key string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(t.tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE actor_did = $1 AND community_did = $2 AND idempotency_key = $3
		ORDER BY created_at DESC, id DESC
		LIMIT 1
		FOR UPDATE
	`, actorDID, communityDID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

// OpenIntentByTarget returns the open intent that writes the target record,
// as its target or as a transfer's paired target, locked, or nil.
func (t *communityActionTransaction) OpenIntentByTarget(ctx context.Context, targetURI string) (*moderation.CommunityIntent, error) {
	return t.openIntentByTarget(ctx, targetURI, "FOR UPDATE")
}

// OpenIntentByTargetUnlocked is OpenIntentByTarget without the row lock.
func (t *communityActionTransaction) OpenIntentByTargetUnlocked(ctx context.Context, targetURI string) (*moderation.CommunityIntent, error) {
	return t.openIntentByTarget(ctx, targetURI, "")
}

// openIntentByTarget is OpenIntentByTarget with the given locking clause.
func (t *communityActionTransaction) openIntentByTarget(ctx context.Context, targetURI, locking string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(t.tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE (target_uri = $1 OR paired_target_uri = $1) AND state = 'open'
		ORDER BY created_at, id
		LIMIT 1
		`+locking, targetURI))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

// OpenIntentByAction returns the community's oldest open intent of action,
// locked, or nil.
func (t *communityActionTransaction) OpenIntentByAction(ctx context.Context, communityDID, action string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(t.tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE community_did = $1 AND action = $2 AND state = 'open'
		ORDER BY created_at, id
		LIMIT 1
		FOR UPDATE
	`, communityDID, action))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

// OpenInvitationIntent returns the oldest open intent that accepts one of the
// invitee's invitations in the community, or nil. It does not lock the row:
// the caller only refuses while one is open.
func (t *communityActionTransaction) OpenInvitationIntent(ctx context.Context, communityDID, inviteeDID string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(t.tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE community_did = $1 AND subject_did = $2 AND invitation_id IS NOT NULL AND state = 'open'
		ORDER BY created_at, id
		LIMIT 1
	`, communityDID, inviteeDID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

// ListCommunityRoles reads the community's indexed role rows in the action
// transaction, as CommunityRoleRepository.ListCommunityRoles does.
func (t *communityActionTransaction) ListCommunityRoles(ctx context.Context, communityDID string) ([]communities.CommunityRole, error) {
	return listHostedCommunityRoles(ctx, t.tx, communityDID)
}

// communityPostResolver is the post-to-community index read the community
// post actions authorize against.
type communityPostResolver struct {
	db *sql.DB
}

// NewCommunityPostResolver builds the resolver over db.
func NewCommunityPostResolver(db *sql.DB) moderation.CommunityPostResolver {
	return &communityPostResolver{db: db}
}

// PostCommunity returns the community the indexed, undeleted post was
// submitted to and has an admission in, or moderation.ErrPostNotFound.
func (r *communityPostResolver) PostCommunity(ctx context.Context, postURI string) (string, error) {
	var communityDID string
	err := r.db.QueryRowContext(ctx, `
		SELECT p.community_did
		FROM posts p
		JOIN community_post_admissions a ON a.community_did = p.community_did AND a.post_uri = p.uri
		WHERE p.uri = $1 AND p.deleted_at IS NULL
	`, postURI).Scan(&communityDID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", moderation.ErrPostNotFound, postURI)
	}
	if err != nil {
		return "", fmt.Errorf("resolving the community of post %s: %w", postURI, err)
	}
	return communityDID, nil
}

// PendingRoleRecordDeleteCID returns the CID a pending role record delete
// queued for (communityDID, recordKey) is guarded on, or "" when none is
// queued.
func (t *communityActionTransaction) PendingRoleRecordDeleteCID(ctx context.Context, communityDID, recordKey string) (string, error) {
	var cid string
	err := t.tx.QueryRowContext(ctx, `
		SELECT record_cid FROM community_role_record_deletes WHERE community_did = $1 AND record_key = $2
	`, communityDID, recordKey).Scan(&cid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the pending role record delete: %w", err)
	}
	return cid, nil
}

// RosterVersion reads the community's roster version; a missing row is 0.
func (t *communityActionTransaction) RosterVersion(ctx context.Context, communityDID string) (int64, error) {
	return communityRosterVersion(ctx, t.tx, communityDID)
}

// RosterVersion reads the community's roster version outside any action
// transaction; a missing row is 0.
func (r *CommunityActionRepository) RosterVersion(ctx context.Context, communityDID string) (int64, error) {
	return communityRosterVersion(ctx, r.db, communityDID)
}

func communityRosterVersion(ctx context.Context, querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, communityDID string,
) (int64, error) {
	var version int64
	err := querier.QueryRowContext(ctx, `
		SELECT version FROM community_subject_versions WHERE community_did = $1 AND subject = $1
	`, communityDID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read community roster version: %w", err)
	}
	return version, nil
}

func (t *communityActionTransaction) CompleteIntent(ctx context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	if commit.Rev == "" {
		return moderation.CommunityActionResult{}, errors.New("complete community write intent: no commit rev")
	}
	switch intent.Op {
	case "delete", "create", "update":
	default:
		return moderation.CommunityActionResult{}, fmt.Errorf("complete community write intent: unsupported op %q", intent.Op)
	}
	var state string
	err := t.tx.QueryRowContext(ctx, `SELECT state FROM community_write_intents WHERE id = $1 FOR UPDATE`, intent.ID).Scan(&state)
	if err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("lock community write intent %s: %w", intent.ID, err)
	}
	if moderation.CommunityIntentState(state) == moderation.CommunityIntentCompleted {
		return t.storedResult(ctx, intent)
	}
	if moderation.CommunityIntentState(state) != moderation.CommunityIntentOpen {
		return moderation.CommunityActionResult{}, fmt.Errorf("complete community write intent %s: intent is %s", intent.ID, state)
	}
	if isCommentIntent(intent) {
		return t.completeCommentIntent(ctx, intent, commit, idempotencyTTL)
	}
	if isPostIntent(intent) {
		return t.completePostIntent(ctx, intent, commit, idempotencyTTL, false)
	}
	if isBanIntent(intent) {
		return t.completeBanIntent(ctx, intent, commit, idempotencyTTL)
	}
	if intent.Op == "delete" {
		return t.completeDelete(ctx, intent, commit, idempotencyTTL)
	}
	return t.completeWrite(ctx, intent, commit, idempotencyTTL)
}

// completeDelete applies a delete intent's local effect: the gate stamped,
// the index row dropped if it still holds the replaced CID, and the version
// advanced and the action logged once; a row already removed links to the
// action that logged its removal.
func (t *communityActionTransaction) completeDelete(ctx context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	if _, err := claimRoleRecordRev(ctx, t.tx, intent.TargetURI, commit.GateRev); err != nil {
		return moderation.CommunityActionResult{}, err
	}
	recordKey := intentRecordKey(intent.TargetURI)
	var deletedRole string
	err := t.tx.QueryRowContext(ctx, `
		DELETE FROM community_roles WHERE community_did = $1 AND record_key = $2 AND record_cid = $3
		RETURNING role
	`, intent.CommunityDID, recordKey, intent.ReplacedCID).Scan(&deletedRole)
	removed := !errors.Is(err, sql.ErrNoRows)
	if err != nil && removed {
		return moderation.CommunityActionResult{}, fmt.Errorf("delete community role: %w", err)
	}
	actionID := ""
	if !removed {
		// The row was already removed, and that removal logged its action and
		// advanced the version: link to it rather than log the change twice.
		actionID, err = recordRoleAction(ctx, t.tx, intent.CommunityDID, moderation.ActionRemoveRole, recordKey, intent.ReplacedCID)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
	} else if err := advanceCommunitySubjectVersion(ctx, t.tx, intent.CommunityDID, intent.CommunityDID); err != nil {
		return moderation.CommunityActionResult{}, err
	}
	return t.finishCompletion(ctx, intent, commit, actionID, deletedRole, idempotencyTTL)
}

// completeWrite applies a create or update intent's local effect in this one
// transaction. Each record the intent wrote (a transfer's two) has its gate
// stamped at commit.GateRev and, unless that gate already holds a later
// change, which is kept, its index row upserted from the intent's body at the
// CID of that body: the CID the commit names, else the one the PDS assigns
// the body (recordBodyCID), so the write's delayed firehose copies find the
// row already indexed even when the record changed again after the write.
// When a row changed the roster version advances once and one action is
// logged; otherwise the change was already indexed and the intent links to
// the action that logged it. The intent's invitation is marked accepted first,
// before any role row or version. A record whose subject is erased (its
// shared erasure gate taken first), or was erased while the intent was open
// (addErasedIntentSubjects), is not indexed and its repo delete is queued at
// the written CID; an intent whose target subject is erased is settled failed
// (settleErasedWrite).
func (t *communityActionTransaction) completeWrite(ctx context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	type intentWrite struct {
		uri  string
		body json.RawMessage
		cid  *string
	}
	writes := []intentWrite{{intent.TargetURI, intent.Record, &commit.RecordCID}}
	if intent.PairedTargetURI != "" {
		writes = append(writes, intentWrite{intent.PairedTargetURI, intent.PairedRecord, &commit.PairedRecordCID})
	}
	erased, err := gateRoleSubjects(ctx, t.tx, intentRecordSubject(intent.Record), intentRecordSubject(intent.PairedRecord))
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	if err := addErasedIntentSubjects(ctx, t.tx, intent, erased); err != nil {
		return moderation.CommunityActionResult{}, err
	}
	if err := acceptIntentInvitation(ctx, t.tx, intent.InvitationID); err != nil {
		return moderation.CommunityActionResult{}, err
	}
	changed := false
	for _, write := range writes {
		if *write.cid == "" {
			bodyCID, err := recordBodyCID(write.body)
			if err != nil {
				return moderation.CommunityActionResult{}, fmt.Errorf("the CID of intent %s's record for %s: %w", intent.ID, write.uri, err)
			}
			*write.cid = bodyCID
		}
		current, err := stampIntentRecordGate(ctx, t.tx, write.uri, commit.GateRev)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
		if !current {
			continue
		}
		rowChanged, err := writeIntentRole(ctx, t.tx, erased, intent.CommunityDID, write.uri, write.body, *write.cid)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
		changed = changed || rowChanged
	}
	if changed {
		if err := advanceCommunitySubjectVersion(ctx, t.tx, intent.CommunityDID, intent.CommunityDID); err != nil {
			return moderation.CommunityActionResult{}, err
		}
	}
	if erased[intentRecordSubject(intent.Record)] {
		return t.settleErasedWrite(ctx, intent)
	}
	actionID := ""
	if !changed {
		actionID, err = recordRoleAction(ctx, t.tx, intent.CommunityDID, intent.Action, intentRecordKey(intent.TargetURI), commit.RecordCID)
		if err != nil {
			return moderation.CommunityActionResult{}, err
		}
	}
	return t.finishCompletion(ctx, intent, commit, actionID, "", idempotencyTTL)
}

// settleErasedWrite settles a create or update intent whose PDS commit landed
// but whose target subject was erased before its completion: the record was
// not indexed and its repo delete is queued, so the intent ends failed,
// logging no action and saving no idempotency record for the erased account.
func (t *communityActionTransaction) settleErasedWrite(ctx context.Context, intent moderation.CommunityIntent) (moderation.CommunityActionResult, error) {
	if err := t.FinishIntent(ctx, intent.ID, moderation.CommunityIntentFailed); err != nil {
		return moderation.CommunityActionResult{}, err
	}
	version, err := t.RosterVersion(ctx, intent.CommunityDID)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	return moderation.CommunityActionResult{
		CommunityDID: intent.CommunityDID,
		SubjectDID:   intent.SubjectDID,
		RecordURI:    intent.TargetURI,
		Version:      version,
		InvitationID: intent.InvitationID,
	}, nil
}

// addErasedIntentSubjects adds to erased the subject of each of the intent's
// records that account erasure marked while the intent was open
// (community_write_intent_erasures): that subject stays erased for the
// intent's completion even after the account signs up again. The caller has
// taken the subjects' erasure gates, so an erasure's marks are committed
// before this read.
func addErasedIntentSubjects(ctx context.Context, tx *sql.Tx, intent moderation.CommunityIntent, erased map[string]bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT paired FROM community_write_intent_erasures WHERE intent_id = $1`, intent.ID)
	if err != nil {
		return fmt.Errorf("read community write intent %s's erased subjects: %w", intent.ID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var paired bool
		if err := rows.Scan(&paired); err != nil {
			return fmt.Errorf("scan community write intent %s's erased subject: %w", intent.ID, err)
		}
		subject := intentRecordSubject(intent.Record)
		if paired {
			subject = intentRecordSubject(intent.PairedRecord)
		}
		if subject != "" {
			erased[subject] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read community write intent %s's erased subjects: %w", intent.ID, err)
	}
	return nil
}

// acceptIntentInvitation marks the open invitation an intent accepts
// accepted; an empty invitationID changes nothing. An invitation no longer
// open (or gone with its community) keeps its state: the committed write
// still completes, and the anomaly is logged.
func acceptIntentInvitation(ctx context.Context, tx *sql.Tx, invitationID string) error {
	if invitationID == "" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE community_role_invitations SET state = 'accepted', updated_at = NOW() WHERE id = $1 AND state = 'open'
	`, invitationID)
	if err != nil {
		return fmt.Errorf("accept community invitation: %w", err)
	}
	accepted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check community invitation acceptance: %w", err)
	}
	if accepted == 0 {
		slog.Warn("community write intent completed for an invitation that is not open; its state is kept",
			"invitation", invitationID)
	}
	return nil
}

// stampIntentRecordGate advances a record's rev gate to gateRev when it
// holds an older rev, and reports whether the gate held no change after
// gateRev: a later change of the record already indexed is kept, not
// overwritten from the intent. An empty gateRev leaves the gate and reports
// true.
func stampIntentRecordGate(ctx context.Context, tx *sql.Tx, uri, gateRev string) (bool, error) {
	if gateRev == "" {
		return true, nil
	}
	current := true
	err := tx.QueryRowContext(ctx, `SELECT rev <= $2 FROM jetstream_record_revs WHERE record_uri = $1`, uri, gateRev).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read the paired record's rev gate: %w", err)
	}
	if _, err := claimRoleRecordRev(ctx, tx, uri, gateRev); err != nil {
		return false, err
	}
	return current, nil
}

// finishCompletion completes the intent with actionID, or with a new action
// when actionID is empty, at the roster version the transaction now reads.
func (t *communityActionTransaction) finishCompletion(ctx context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, actionID, deletedRole string, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	version, err := t.RosterVersion(ctx, intent.CommunityDID)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	if actionID != "" {
		return completeCommunityIntentWithActionTx(ctx, t.tx, intent, commit, actionID, version, commit.CompletedAt, idempotencyTTL)
	}
	return completeCommunityIntentTx(ctx, t.tx, intent, commit, deletedRole, version, commit.CompletedAt, idempotencyTTL)
}

// intentRecordKey is the record key of a role record URI.
func intentRecordKey(uri string) string {
	return uri[strings.LastIndex(uri, "/")+1:]
}

// intentRoleRecord is the part of a role record body an intent writes that
// the index keeps.
type intentRoleRecord struct {
	Subject   string `json:"subject"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

// intentRecordSubject is the subject of a role record body an intent writes,
// or "" when there is no body or it does not decode; writeIntentRole reports
// a body that does not decode.
func intentRecordSubject(body json.RawMessage) string {
	var record intentRoleRecord
	if len(body) == 0 || json.Unmarshal(body, &record) != nil {
		return ""
	}
	return record.Subject
}

// writeIntentRole upserts the index row of the role record at uri from the
// body the intent wrote, at recordCID, without advancing the version, unless
// its subject is in erased (writeRoleUnlessErased). It reports whether the
// row changed.
func writeIntentRole(ctx context.Context, tx *sql.Tx, erased map[string]bool, communityDID, uri string, body json.RawMessage, recordCID string) (bool, error) {
	var record intentRoleRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return false, fmt.Errorf("decode the intent's role record for %s: %w", uri, err)
	}
	createdAt, err := time.Parse(time.RFC3339, record.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("the intent's role record for %s has invalid createdAt %q: %w", uri, record.CreatedAt, err)
	}
	return writeRoleUnlessErased(ctx, tx, erased, communities.CommunityRole{
		CommunityDID:    communityDID,
		SubjectDID:      record.Subject,
		Role:            record.Role,
		RecordKey:       intentRecordKey(uri),
		RecordCID:       recordCID,
		RecordCreatedAt: createdAt.UTC(),
	})
}

// recordRoleAction is the ID of the newest action of kind action that logged
// the role record at recordKey with recordCID in the community, else "". It
// names the action that already logged a change an intent completes after
// that change was indexed: a delete's removal, or a write's grant.
func recordRoleAction(ctx context.Context, tx *sql.Tx, communityDID, action, recordKey, recordCID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM moderation_actions
		WHERE authority_did = $1 AND scope_kind = $2 AND action = $3
		  AND role_record_key = $4 AND role_record_cid = $5
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, communityDID, moderation.ScopeCommunity, action, recordKey, recordCID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the role record's %s action: %w", action, err)
	}
	return id, nil
}

// completeCommunityIntentTx writes an open intent's completion once the
// caller has applied its index change and roster version in tx: one
// moderation action naming the intent's audit actor (else its requester), the intent completed at
// commit.Rev with that action, and the idempotency record of the result,
// kept for retention from idempotencyCreatedAt. version is the roster version
// after the change. The action's role is the role the intent's record grants
// (for a transfer, the new owner's), or for a delete deletedRole, the role of
// the index row the delete removed.
func completeCommunityIntentTx(ctx context.Context, tx *sql.Tx, intent moderation.CommunityIntent, commit moderation.CommunityCommit, deletedRole string, version int64, idempotencyCreatedAt time.Time, retention time.Duration) (moderation.CommunityActionResult, error) {
	actorDID := intent.ActorDID
	if intent.AuditActorDID != "" {
		actorDID = intent.AuditActorDID
	}
	recordCID, role := intent.ReplacedCID, deletedRole
	if intent.Op != "delete" {
		recordCID = commit.RecordCID
		var record intentRoleRecord
		if err := json.Unmarshal(intent.Record, &record); err != nil {
			return moderation.CommunityActionResult{}, fmt.Errorf("decode intent %s's role record: %w", intent.ID, err)
		}
		role = record.Role
	}
	actionID, err := insertCommunityRoleAction(ctx, tx, communityRoleAction{
		action: intent.Action, actorDID: actorDID, adminActing: intent.AdminActing, role: role,
		communityDID: intent.CommunityDID, subjectDID: intent.SubjectDID, origin: moderation.OriginLocal,
		recordKey: intentRecordKey(intent.TargetURI), recordCID: recordCID,
	})
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	return completeCommunityIntentWithActionTx(ctx, tx, intent, commit, actionID, version, idempotencyCreatedAt, retention)
}

// completeCommunityIntentWithActionTx marks the intent completed at commit.Rev
// with the existing action actionID and saves the idempotency record.
func completeCommunityIntentWithActionTx(ctx context.Context, tx *sql.Tx, intent moderation.CommunityIntent, commit moderation.CommunityCommit, actionID string, version int64, idempotencyCreatedAt time.Time, retention time.Duration) (moderation.CommunityActionResult, error) {
	result := moderation.CommunityActionResult{
		CommunityDID: intent.CommunityDID,
		SubjectDID:   intent.SubjectDID,
		RecordURI:    intent.TargetURI,
		ActionID:     actionID,
		Version:      version,
		InvitationID: intent.InvitationID,
	}
	return saveCompletedIntentTx(ctx, tx, intent, commit, result, idempotencyCreatedAt, retention)
}

// saveCompletedIntentTx marks the intent completed at commit.Rev with
// result's action and saves result as the key's idempotency record, kept for
// retention from idempotencyCreatedAt.
func saveCompletedIntentTx(ctx context.Context, tx *sql.Tx, intent moderation.CommunityIntent, commit moderation.CommunityCommit, result moderation.CommunityActionResult, idempotencyCreatedAt time.Time, retention time.Duration) (moderation.CommunityActionResult, error) {
	actionID := result.ActionID
	if _, err := tx.ExecContext(ctx, `
		UPDATE community_write_intents
		SET state = 'completed', commit_rev = $2, record_cid = NULLIF($3, ''), action_id = $4, finished_at = NOW()
		WHERE id = $1
	`, intent.ID, commit.Rev, commit.RecordCID, actionID); err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("complete community write intent: %w", err)
	}
	stored, err := json.Marshal(result)
	if err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("encode community action result: %w", err)
	}
	createdAt := idempotencyCreatedAt.UTC().Truncate(time.Microsecond)
	// Another request's unexpired result under the key is never replaced.
	saved, err := tx.ExecContext(ctx, `
		INSERT INTO moderation_idempotency_keys (actor_did, authority_did, key, fingerprint, stored_result, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (actor_did, authority_did, key) DO UPDATE SET
		    fingerprint = EXCLUDED.fingerprint, stored_result = EXCLUDED.stored_result,
		    created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at
		WHERE moderation_idempotency_keys.fingerprint = EXCLUDED.fingerprint
		   OR moderation_idempotency_keys.expires_at <= EXCLUDED.created_at
	`, intent.ActorDID, intent.CommunityDID, intent.IdempotencyKey, intent.Fingerprint, stored,
		createdAt, createdAt.Add(retention))
	if err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("save community idempotency record: %w", err)
	}
	rows, err := saved.RowsAffected()
	if err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("check community idempotency record save: %w", err)
	}
	if rows == 0 {
		slog.Warn("completed community write intent's key holds another request's result; that result is kept",
			"intent", intent.ID, "community", intent.CommunityDID)
	}
	return result, nil
}

// communityRoleAction is one role action in a community's log: a role record
// indexed (grant-role) or removed (remove-role). actorDID is empty for a
// change with no matching intent of this instance. role is the role granted,
// removed or transferred. recordKey and recordCID name the role record
// indexed or removed; an empty recordCID or role is stored as NULL.
type communityRoleAction struct {
	action, actorDID         string
	adminActing              bool
	role                     string
	communityDID, subjectDID string
	origin                   string
	recordKey, recordCID     string
}

// insertCommunityRoleAction logs one role action, scoped to and authored for
// its community, about the subject's account, and returns its ID. Its time is
// clock_timestamp(), the database clock when the row is inserted, not the
// transaction start: every caller holds the record's rev-gate row by then
// (an empty-rev firehose event, which skips the gate, excepted), so one
// record's changes log in commit order even after a lock wait.
func insertCommunityRoleAction(ctx context.Context, tx *sql.Tx, action communityRoleAction) (string, error) {
	actionID := moderationActionClock.Next().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, admin_acting, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, subject_kind, action, origin,
		     role_record_key, role_record_cid, role, created_at)
		VALUES ($1, $2, $3, $4, $5, $4, $6, NULL, $4, 'account', $7, $8, $9, NULLIF($10, ''), NULLIF($11, ''), clock_timestamp())
	`, actionID, action.actorDID, action.adminActing, action.communityDID, moderation.ScopeCommunity,
		moderation.AccountSubjectURI(action.subjectDID), action.action, action.origin,
		action.recordKey, action.recordCID, action.role); err != nil {
		return "", fmt.Errorf("insert community role action: %w", err)
	}
	return actionID, nil
}

// storedResult is the result the completer of an already completed intent
// stored with its idempotency record.
func (t *communityActionTransaction) storedResult(ctx context.Context, intent moderation.CommunityIntent) (moderation.CommunityActionResult, error) {
	var stored []byte
	if err := t.tx.QueryRowContext(ctx, `
		SELECT stored_result FROM moderation_idempotency_keys
		WHERE actor_did = $1 AND authority_did = $2 AND key = $3
	`, intent.ActorDID, intent.CommunityDID, intent.IdempotencyKey).Scan(&stored); err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("read the completed intent's stored result: %w", err)
	}
	var result moderation.CommunityActionResult
	if err := json.Unmarshal(stored, &result); err != nil {
		return moderation.CommunityActionResult{}, fmt.Errorf("decode the completed intent's stored result: %w", err)
	}
	return result, nil
}

func (t *communityActionTransaction) FinishIntent(ctx context.Context, intentID string, state moderation.CommunityIntentState) error {
	if state != moderation.CommunityIntentFailed && state != moderation.CommunityIntentExpired {
		return fmt.Errorf("finish community write intent: %q is not a terminal failure state", state)
	}
	result, err := t.tx.ExecContext(ctx, `
		UPDATE community_write_intents SET state = $2, finished_at = NOW()
		WHERE id = $1 AND state = 'open'
	`, intentID, string(state))
	if err != nil {
		return fmt.Errorf("finish community write intent: %w", err)
	}
	finished, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check community write intent finish: %w", err)
	}
	if finished == 0 {
		return fmt.Errorf("finish community write intent %s: no open intent", intentID)
	}
	return nil
}

func (t *communityActionTransaction) Commit() error {
	if err := t.tx.Commit(); err != nil {
		return fmt.Errorf("commit community action transaction: %w", err)
	}
	return nil
}

// Rollback ends the transaction; after Commit it does nothing.
func (t *communityActionTransaction) Rollback() error {
	if err := t.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("roll back community action transaction: %w", err)
	}
	return nil
}
