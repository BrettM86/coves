package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// banTargetURIPattern matches a community ban record's AT-URI in LIKE.
const banTargetURIPattern = "at://%/" + communities.BanCollection + "/%"

// CommunityAccountBan reads the subject's standing ban row and its
// (community, subject DID) version, in the action transaction.
func (t *communityActionTransaction) CommunityAccountBan(ctx context.Context, communityDID, subjectDID string) (moderation.CommunityAccountBanState, error) {
	var state moderation.CommunityAccountBanState
	err := t.tx.QueryRowContext(ctx, `
		SELECT version FROM community_subject_versions WHERE community_did = $1 AND subject = $2
	`, communityDID, subjectDID).Scan(&state.Version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, fmt.Errorf("read community subject version of %s in %s: %w", subjectDID, communityDID, err)
	}
	ban, err := getCommunityBan(ctx, t.tx, communityDID, subjectDID)
	if errors.Is(err, communities.ErrCommunityBanNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.Ban = ban
	return state, nil
}

// RecentCommunityBans counts the local non-admin community bans since since:
// logged ban actions (community-scope removes of an account subject) and ban
// intents still open, whatever their expiry, until they settle.
func (t *communityActionTransaction) RecentCommunityBans(ctx context.Context, communityDID, actorDID string, since time.Time) (int, int, error) {
	var byActor, byCommunity int
	err := t.tx.QueryRowContext(ctx, `
		WITH bans AS (
			SELECT actor_did FROM moderation_actions
			WHERE scope_kind = 'community' AND scope_community_did = $1
			  AND action = 'remove' AND subject_kind = 'account'
			  AND origin = 'local' AND NOT admin_acting
			  AND created_at > $3
			UNION ALL
			SELECT actor_did FROM community_write_intents
			WHERE community_did = $1 AND state = 'open' AND NOT admin_acting
			  AND action = 'remove' AND target_uri LIKE $4
			  AND created_at > $3
		)
		SELECT COUNT(*) FILTER (WHERE actor_did = $2), COUNT(*) FROM bans
	`, communityDID, actorDID, since.UTC(), banTargetURIPattern).Scan(&byActor, &byCommunity)
	if err != nil {
		return 0, 0, fmt.Errorf("count recent community bans in %s: %w", communityDID, err)
	}
	return byActor, byCommunity, nil
}

// CommunityBanLister lists a community's indexed bans.
type CommunityBanLister struct {
	db *sql.DB
}

// NewCommunityBanLister builds the ban lister over db.
func NewCommunityBanLister(db *sql.DB) *CommunityBanLister {
	return &CommunityBanLister{db: db}
}

var _ moderation.CommunityBanLister = (*CommunityBanLister)(nil)

// communityBanCursor is the position after the last ban of a page, in the
// list's order: newest indexed first, then subject DID.
type communityBanCursor struct {
	IndexedAt time.Time `json:"i"`
	Subject   string    `json:"s"`
}

// ListCommunityBans lists a page of communityDID's indexed bans, in force and
// lapsed, newest indexed first. A cursor this lister did not issue is
// ErrInvalidRequest. The ban index stores no record createdAt, so CreatedAt
// is nil.
func (l *CommunityBanLister) ListCommunityBans(ctx context.Context, communityDID string, limit int, cursor string) ([]moderation.CommunityBanEntry, string, error) {
	var after *communityBanCursor
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		var position communityBanCursor
		if err == nil {
			err = json.Unmarshal(decoded, &position)
		}
		if err != nil || position.Subject == "" {
			return nil, "", fmt.Errorf("%w: malformed cursor", moderation.ErrInvalidRequest)
		}
		after = &position
	}
	var afterIndexed any
	afterSubject := ""
	if after != nil {
		afterIndexed, afterSubject = after.IndexedAt, after.Subject
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT subject_did, record_key, record_cid, reason, expires_at, indexed_at
		FROM community_bans
		WHERE community_did = $1
		  AND ($2::timestamptz IS NULL OR (indexed_at, subject_did) < ($2::timestamptz, $3))
		ORDER BY indexed_at DESC, subject_did DESC
		LIMIT $4
	`, communityDID, afterIndexed, afterSubject, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list community bans of %s: %w", communityDID, err)
	}
	defer func() { _ = rows.Close() }()
	var entries []moderation.CommunityBanEntry
	for rows.Next() {
		var entry moderation.CommunityBanEntry
		var recordKey string
		var reason sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&entry.SubjectDID, &recordKey, &entry.CID, &reason, &expiresAt, &entry.IndexedAt); err != nil {
			return nil, "", fmt.Errorf("scan community ban of %s: %w", communityDID, err)
		}
		entry.URI = "at://" + communityDID + "/" + communities.BanCollection + "/" + recordKey
		entry.Reason = reason.String
		if expiresAt.Valid {
			expiry := expiresAt.Time
			entry.ExpiresAt = &expiry
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list community bans of %s: %w", communityDID, err)
	}
	if len(entries) <= limit {
		return entries, "", nil
	}
	entries = entries[:limit]
	last := entries[len(entries)-1]
	encoded, err := json.Marshal(communityBanCursor{IndexedAt: last.IndexedAt, Subject: last.SubjectDID})
	if err != nil {
		return nil, "", fmt.Errorf("encode community ban cursor: %w", err)
	}
	return entries, base64.RawURLEncoding.EncodeToString(encoded), nil
}

// isBanIntent reports whether the intent writes a community ban record.
func isBanIntent(intent moderation.CommunityIntent) bool {
	uri, err := syntax.ParseATURI(intent.TargetURI)
	return err == nil && uri.Collection().String() == communities.BanCollection
}

// banRecordBody is the part of a ban record body the ban index keeps.
type banRecordBody struct {
	Subject   string `json:"subject"`
	Reason    string `json:"reason"`
	ExpiresAt string `json:"expiresAt"`
}

// communityBanAction is one ban (remove) or unban (restore) in a community's
// log, about the subject's account. actorDID is empty for a ban record
// changed outside this instance (origin inherited).
type communityBanAction struct {
	action, actorDID         string
	adminActing              bool
	communityDID, subjectDID string
	origin                   string
	expiresAt                *time.Time
	reversesActionID         string
	createdAt                time.Time
	// recordKey and recordCID name the ban record the action logged (its
	// write, or for a restore the record deleted), in the role_record_*
	// columns, so an unban completion links only to the restore of its own
	// record.
	recordKey, recordCID string
}

// insertCommunityBanAction logs one ban action with the moderator-discretion
// reason (never the ban's free-text reason) and returns its ID.
func insertCommunityBanAction(ctx context.Context, tx *sql.Tx, action communityBanAction) (string, error) {
	var expiresAt sql.NullTime
	if action.expiresAt != nil {
		expiresAt = sql.NullTime{Time: action.expiresAt.UTC(), Valid: true}
	}
	actionID := moderationActionClock.Next().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, admin_acting, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, subject_kind, action, reason, origin,
		     expires_at, reverses_action_id, created_at, role_record_key, role_record_cid)
		VALUES ($1, $2, $3, $4, $5, $4, $6, NULL, $4, 'account', $7, $8, $9, $10, NULLIF($11, ''), $12,
		        NULLIF($13, ''), NULLIF($14, ''))
	`, actionID, action.actorDID, action.adminActing, action.communityDID, moderation.ScopeCommunity,
		moderation.AccountSubjectURI(action.subjectDID), action.action, moderation.CommunityBanActionReason,
		action.origin, expiresAt, action.reversesActionID, action.createdAt.UTC(),
		action.recordKey, action.recordCID); err != nil {
		return "", fmt.Errorf("insert community ban action: %w", err)
	}
	return actionID, nil
}

// standingBanActionID is the ID of the ban action the subject's latest ban
// action in the community is, when that latest action is a ban (remove), else
// "": the action an unban reverses.
func standingBanActionID(ctx context.Context, tx *sql.Tx, communityDID, subjectDID string) (string, error) {
	return latestBanActionIDOf(ctx, tx, communityDID, subjectDID, moderation.ActionRemove)
}

// banRecordRestoreID is the ID of the latest restore logged in the community
// for the deletion of the ban record at recordKey with recordCID, else "".
func banRecordRestoreID(ctx context.Context, tx *sql.Tx, communityDID, subjectDID, recordKey, recordCID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM moderation_actions
		WHERE scope_kind = 'community' AND scope_community_did = $1 AND subject_uri = $2
		  AND subject_kind = 'account' AND action = 'restore'
		  AND role_record_key = $3 AND role_record_cid = $4
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, communityDID, moderation.AccountSubjectURI(subjectDID), recordKey, recordCID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the restore of ban record %s in %s: %w", recordKey, communityDID, err)
	}
	return id, nil
}

// latestBanActionIDOf is the ID of the subject's latest ban action (remove or
// restore) in the community when that action is action, else "".
func latestBanActionIDOf(ctx context.Context, tx *sql.Tx, communityDID, subjectDID, action string) (string, error) {
	var id, latest string
	err := tx.QueryRowContext(ctx, `
		SELECT id, action FROM moderation_actions
		WHERE scope_kind = 'community' AND scope_community_did = $1 AND subject_uri = $2
		  AND subject_kind = 'account' AND action IN ('remove', 'restore')
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, communityDID, moderation.AccountSubjectURI(subjectDID)).Scan(&id, &latest)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && latest != action) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the latest community ban action of %s in %s: %w", subjectDID, communityDID, err)
	}
	return id, nil
}

// communityAccountVersion is the (community, account) version.
func communityAccountVersion(ctx context.Context, tx *sql.Tx, communityDID, subjectDID string) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx, `
		SELECT version FROM community_subject_versions WHERE community_did = $1 AND subject = $2
	`, communityDID, subjectDID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read community subject version of %s in %s: %w", subjectDID, communityDID, err)
	}
	return version, nil
}

// decodeBanRecord is the ban index row a ban record body at recordKey with
// recordCID describes in communityDID.
func decodeBanRecord(communityDID, recordKey, recordCID string, body json.RawMessage) (communities.CommunityBan, error) {
	var record banRecordBody
	if err := json.Unmarshal(body, &record); err != nil {
		return communities.CommunityBan{}, fmt.Errorf("decode the ban record: %w", err)
	}
	ban := communities.CommunityBan{
		CommunityDID: communityDID, SubjectDID: record.Subject, RecordKey: recordKey,
		RecordCID: recordCID, Reason: record.Reason,
	}
	if record.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339, record.ExpiresAt)
		if err != nil {
			return communities.CommunityBan{}, fmt.Errorf("the ban record has invalid expiresAt %q: %w", record.ExpiresAt, err)
		}
		expiry = expiry.UTC()
		ban.ExpiresAt = &expiry
	}
	return ban, nil
}

// completeBanIntent applies a ban or unban intent's local effect in the action
// transaction. The ban record's rev gate is stamped at commit.GateRev, so the
// commit's firehose copy is no new change; when the gate already held a later
// change of the record, that change stands and only the action is logged.
// Otherwise a ban (create or update of the record) indexes the ban with its
// reason and expiry, and an unban (its delete) drops the indexed row; either,
// when the index changed, writes the ban history and advances the (community,
// account) version once. Exactly one action names the intent's actor: remove
// with the ban's expiry, or restore reversing the standing ban action; but an
// unban that finds the ban no longer indexed, the deletion of the very record
// it deleted (its ReplacedCID) already logged as a restore (an unmatched
// firehose delete), links to that restore rather than log the lift twice. A
// restore of any other record (a later ban cycle at the same key) is not the
// unban's, which then logs its own restore. Then the intent completes and its idempotency record is
// saved.
func (t *communityActionTransaction) completeBanIntent(ctx context.Context, intent moderation.CommunityIntent, commit moderation.CommunityCommit, idempotencyTTL time.Duration) (moderation.CommunityActionResult, error) {
	if commit.GateRev == "" {
		return moderation.CommunityActionResult{}, fmt.Errorf("complete community ban intent %s: no gate rev", intent.ID)
	}
	current, err := stampIntentRecordGate(ctx, t.tx, intent.TargetURI, commit.GateRev)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	if err := lockBanSubject(ctx, t.tx, intent.SubjectDID); err != nil {
		return moderation.CommunityActionResult{}, err
	}
	action := communityBanAction{
		action: intent.Action, actorDID: intent.ActorDID, adminActing: intent.AdminActing,
		communityDID: intent.CommunityDID, subjectDID: intent.SubjectDID,
		origin: moderation.OriginLocal, createdAt: commit.CompletedAt,
	}
	recordKey := intentRecordKey(intent.TargetURI)
	action.recordKey = recordKey
	actionID := ""
	if intent.Op == "delete" {
		if action.reversesActionID, err = standingBanActionID(ctx, t.tx, intent.CommunityDID, intent.SubjectDID); err != nil {
			return moderation.CommunityActionResult{}, err
		}
		action.recordCID = intent.ReplacedCID
		removedSubjectDID := ""
		if current {
			if removedSubjectDID, _, err = deleteCommunityBanTx(ctx, t.tx, intent.CommunityDID, recordKey); err != nil {
				return moderation.CommunityActionResult{}, err
			}
		}
		if removedSubjectDID == "" {
			// When the record this unban deleted was already lifted, that lift
			// logged its restore and advanced the version: link to it rather
			// than log the change twice.
			if actionID, err = banRecordRestoreID(ctx, t.tx, intent.CommunityDID, intent.SubjectDID, recordKey, intent.ReplacedCID); err != nil {
				return moderation.CommunityActionResult{}, err
			}
		}
	} else {
		if commit.RecordCID == "" {
			if commit.RecordCID, err = recordBodyCID(intent.Record); err != nil {
				return moderation.CommunityActionResult{}, fmt.Errorf("the CID of community ban intent %s's record: %w", intent.ID, err)
			}
		}
		ban, err := decodeBanRecord(intent.CommunityDID, recordKey, commit.RecordCID, intent.Record)
		if err != nil {
			return moderation.CommunityActionResult{}, fmt.Errorf("community ban intent %s: %w", intent.ID, err)
		}
		if ban.SubjectDID != intent.SubjectDID {
			return moderation.CommunityActionResult{}, fmt.Errorf("community ban intent %s's record names another subject", intent.ID)
		}
		action.expiresAt, action.recordCID = ban.ExpiresAt, ban.RecordCID
		if current {
			if _, err := upsertCommunityBanTx(ctx, t.tx, ban); err != nil {
				return moderation.CommunityActionResult{}, err
			}
		}
	}
	if actionID == "" {
		if actionID, err = insertCommunityBanAction(ctx, t.tx, action); err != nil {
			return moderation.CommunityActionResult{}, err
		}
	}
	version, err := communityAccountVersion(ctx, t.tx, intent.CommunityDID, intent.SubjectDID)
	if err != nil {
		return moderation.CommunityActionResult{}, err
	}
	return saveCompletedIntentTx(ctx, t.tx, intent, commit, moderation.CommunityActionResult{
		CommunityDID: intent.CommunityDID,
		SubjectDID:   intent.SubjectDID,
		RecordURI:    intent.TargetURI,
		ActionID:     actionID,
		Version:      version,
	}, commit.CompletedAt, idempotencyTTL)
}

// ApplyBanRecordEvent applies one firehose commit of a community's ban record
// in one transaction. The record's rev gate is claimed first; a stale or
// repeated rev changes nothing. A create or update whose CID is the body of
// the open ban intent on the record, guarded before the event's rev, and a
// delete of the record the open unban intent on it would delete, complete that
// intent with its actor (completeBanIntent). Any other change of the index,
// in any indexed community, writes the ban history, advances the (community,
// account) version once and logs one action with no actor, origin inherited:
// remove for a ban, restore for its delete. A ban already indexed as the event
// has it, or a delete of a ban no longer indexed, logs nothing.
func (r *CommunityBanRepository) ApplyBanRecordEvent(ctx context.Context, event communities.BanRecordEvent) error {
	return r.inTransaction(ctx, func(tx *sql.Tx) error {
		uri := "at://" + event.CommunityDID + "/" + communities.BanCollection + "/" + event.RKey
		claimed, err := claimRoleRecordRev(ctx, tx, uri, event.Rev)
		if err != nil || !claimed {
			return err
		}
		completed, err := completeBanIntentFromEvent(ctx, tx, uri, event)
		if err != nil || completed {
			return err
		}
		action := communityBanAction{
			communityDID: event.CommunityDID, origin: moderation.OriginInherited, createdAt: time.Now(),
			recordKey: event.RKey,
		}
		if event.Ban != nil {
			changed, err := upsertCommunityBanTx(ctx, tx, *event.Ban)
			if err != nil || !changed {
				return err
			}
			action.action, action.subjectDID, action.expiresAt = moderation.ActionRemove, event.Ban.SubjectDID, event.Ban.ExpiresAt
			action.recordCID = event.Ban.RecordCID
		} else {
			subjectDID, deletedCID, err := deleteCommunityBanTx(ctx, tx, event.CommunityDID, event.RKey)
			if err != nil || subjectDID == "" {
				return err
			}
			action.action, action.subjectDID, action.recordCID = moderation.ActionRestore, subjectDID, deletedCID
			if action.reversesActionID, err = standingBanActionID(ctx, tx, event.CommunityDID, subjectDID); err != nil {
				return err
			}
		}
		_, err = insertCommunityBanAction(ctx, tx, action)
		return err
	})
}

// completeBanIntentFromEvent completes the open ban or unban intent on the
// ban record at uri whose commit the event is, at the event's rev, and reports
// whether one did. An intent is the event's only when its guard rev is before
// the event's rev (an older delayed commit is not), and a ban's body has the
// event's CID, or an unban's pre-read CID is the ban the index holds. The
// caller has claimed the record's rev gate at the event's rev.
func completeBanIntentFromEvent(ctx context.Context, tx *sql.Tx, uri string, event communities.BanRecordEvent) (bool, error) {
	if event.Rev == "" {
		return false, nil
	}
	ops := "('delete')"
	if event.Ban != nil {
		ops = "('create', 'update')"
	}
	intent, err := scanCommunityIntent(tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE target_uri = $1 AND state = 'open' AND op IN `+ops+` AND guard_rev < $2
		FOR UPDATE
	`, uri, event.Rev))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the open ban intent on %s: %w", uri, err)
	}
	commit := moderation.CommunityCommit{Rev: event.Rev, GateRev: event.Rev, CompletedAt: time.Now()}
	if event.Ban != nil {
		bodyCID, err := recordBodyCID(intent.Record)
		if err != nil {
			return false, fmt.Errorf("the CID of community ban intent %s's record: %w", intent.ID, err)
		}
		if bodyCID != event.Ban.RecordCID || intent.SubjectDID != event.Ban.SubjectDID {
			return false, nil
		}
		commit.RecordCID = event.Ban.RecordCID
	} else {
		var indexedCID string
		err := tx.QueryRowContext(ctx, `
			SELECT record_cid FROM community_bans WHERE community_did = $1 AND record_key = $2
		`, event.CommunityDID, event.RKey).Scan(&indexedCID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read the indexed ban %s: %w", uri, err)
		}
		if indexedCID != intent.ReplacedCID {
			return false, nil
		}
	}
	if _, err := (&communityActionTransaction{tx: tx}).completeBanIntent(ctx, intent, commit, intent.IdempotencyRetention); err != nil {
		return false, err
	}
	return true, nil
}
