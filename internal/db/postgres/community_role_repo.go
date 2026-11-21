package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// CommunityRoleRepository is the Postgres community role index. It indexes
// role records from every indexed community, but its reads serve roles only in
// communities this instance hosts: those whose PDS refresh token it stores
// (`pds_refresh_token_encrypted IS NOT NULL`, the predicate of
// NewHostedCommunityQuery). A role row whose community is not indexed is never
// served.
type CommunityRoleRepository struct {
	db *sql.DB
}

// NewCommunityRoleRepository builds the community role index repository.
func NewCommunityRoleRepository(db *sql.DB) *CommunityRoleRepository {
	return &CommunityRoleRepository{db: db}
}

const communityRoleColumns = `roles.community_did, roles.subject_did, roles.role, roles.record_key, roles.record_cid, roles.record_created_at, roles.indexed_at`

// hostedCommunityRoles is the role rows of hosted communities, aliased roles.
const hostedCommunityRoles = `community_roles roles
		JOIN communities ON communities.did = roles.community_did
		    AND communities.pds_refresh_token_encrypted IS NOT NULL`

// ListCommunityRoles returns every indexed role record in communityDID, ordered
// by record key, or none when this instance does not host communityDID.
// communities.EffectiveRoster resolves them into a roster.
func (r *CommunityRoleRepository) ListCommunityRoles(ctx context.Context, communityDID string) ([]communities.CommunityRole, error) {
	return listHostedCommunityRoles(ctx, r.db, communityDID)
}

func listHostedCommunityRoles(ctx context.Context, querier roleQuerier, communityDID string) ([]communities.CommunityRole, error) {
	return queryCommunityRoles(ctx, querier, `
		SELECT `+communityRoleColumns+`
		FROM `+hostedCommunityRoles+`
		WHERE roles.community_did = $1
		ORDER BY roles.record_key
	`, communityDID)
}

// ListRolesBySubject returns subjectDID's effective role in each hosted
// community, ordered by community DID: a losing owner record reads as
// moderator and an unknown role is omitted. It reads the subject's own rows
// plus the rival owner rows of the same communities, which is all
// communities.EffectiveRoster needs to place the subject.
func (r *CommunityRoleRepository) ListRolesBySubject(ctx context.Context, subjectDID string) ([]communities.CommunityRole, error) {
	rows, err := queryCommunityRoles(ctx, r.db, `
		SELECT `+communityRoleColumns+`
		FROM `+hostedCommunityRoles+`
		WHERE roles.subject_did = $1
		   OR (roles.role = $2 AND roles.community_did IN (
		       SELECT community_did FROM community_roles WHERE subject_did = $1 AND role = $2))
		ORDER BY roles.community_did, roles.record_key
	`, subjectDID, communities.RoleOwner)
	if err != nil {
		return nil, err
	}

	var effective []communities.CommunityRole
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) && rows[end].CommunityDID == rows[start].CommunityDID {
			end++
		}
		roster := communities.EffectiveRoster(rows[start:end])
		if roster.Owner != nil && roster.Owner.SubjectDID == subjectDID {
			effective = append(effective, *roster.Owner)
		}
		for _, moderator := range roster.Moderators {
			if moderator.SubjectDID == subjectDID {
				effective = append(effective, moderator)
			}
		}
		start = end
	}
	return effective, nil
}

// UpsertCommunityRole indexes a role record, overwriting any earlier record on
// the same subject in the same community. A change advances the community's
// roster version; rewriting an identical record changes nothing.
func (r *CommunityRoleRepository) UpsertCommunityRole(ctx context.Context, role communities.CommunityRole) error {
	return r.inTransaction(ctx, func(tx *sql.Tx) error {
		_, err := upsertCommunityRoleTx(ctx, tx, role)
		return err
	})
}

// DeleteCommunityRole removes the role record at recordKey in communityDID,
// advancing the community's roster version. Deleting a role that is not
// indexed is not an error and advances nothing.
func (r *CommunityRoleRepository) DeleteCommunityRole(ctx context.Context, communityDID, recordKey string) error {
	return r.inTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM community_roles WHERE community_did = $1 AND record_key = $2
		`, communityDID, recordKey)
		if err != nil {
			return fmt.Errorf("delete community role: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check community role delete: %w", err)
		}
		if deleted == 0 {
			return nil
		}
		return advanceCommunitySubjectVersion(ctx, tx, communityDID, communityDID)
	})
}

// UpsertCommunityRoleAtRev indexes a role record this instance wrote at its
// commit rev: the owner record a community's creation writes. In one
// transaction it first claims the record's rev gate (jetstream_record_revs) at
// rev, then upserts the role and, when the row changed, logs one grant-role
// action naming the role's subject, the account that created the community.
// When the firehose has already handled that rev or a newer one, the claim
// loses: it writes no row and returns false. When the firehose copy indexed
// the record first (the claim loses, or the row is already the record), its
// actorless grant of that record is attributed to the subject instead, so
// either delivery order logs one grant naming the creator. The claim comes
// before the role row, the order of the firehose role handler, so the two
// cannot deadlock. A creator erased before this runs gets no row, no version
// and no action: the transaction takes the creator's shared erasure gate
// first, and a winning claim queues the written record's repo delete instead
// (writeRoleUnlessErased); it returns false.
func (r *CommunityRoleRepository) UpsertCommunityRoleAtRev(ctx context.Context, role communities.CommunityRole, rev string) (bool, error) {
	applied := false
	err := r.inTransaction(ctx, func(tx *sql.Tx) error {
		erased, err := gateRoleSubjects(ctx, tx, role.SubjectDID)
		if err != nil {
			return err
		}
		claimed, err := claimRoleRecordRev(ctx, tx, roleRecordURI(role.CommunityDID, role.RecordKey), rev)
		if err != nil {
			return err
		}
		if erased[role.SubjectDID] {
			if !claimed {
				return nil
			}
			_, err := writeRoleUnlessErased(ctx, tx, erased, role)
			return err
		}
		if !claimed {
			return attributeRecordGrant(ctx, tx, role)
		}
		applied = true
		changed, err := upsertCommunityRoleTx(ctx, tx, role)
		if err != nil {
			return err
		}
		if !changed {
			return attributeRecordGrant(ctx, tx, role)
		}
		_, err = insertCommunityRoleAction(ctx, tx, communityRoleAction{
			action: moderation.ActionGrantRole, actorDID: role.SubjectDID, role: role.Role,
			communityDID: role.CommunityDID, subjectDID: role.SubjectDID, origin: moderation.OriginLocal,
			recordKey: role.RecordKey, recordCID: role.RecordCID,
		})
		return err
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// ApplyRoleRecordEvent applies one firehose role commit in one transaction.
// It claims the record's rev gate first (an empty rev bypasses the gate) and
// returns false, writing nothing, when the gate already holds that rev or a
// newer one. A create or update first looks up the open create or update
// intent writing the record (as its target or as a transfer's paired target)
// and, for a transfer, locks both records' gates in ascending URI order before
// the claim, the action transaction's order. Then:
//   - a create or update whose rev is past that intent's guard rev and whose
//     CID is the CID of the intent's body for the record completes the
//     intent from the event (completeWriteIntentFromEvent);
//   - any other create or update with a valid role upserts the row and, when
//     the row changed, logs one grant-role action with no actor, leaving any
//     open intent for its retry or the sweep;
//   - a delete, or a create or update that clears the record key's role,
//     removes the indexed row at the record key. Only a delete of a row whose
//     CID is the replaced CID of an open delete intent on the record, at a
//     rev past the intent's guard rev, completes that intent: its action
//     names the intent's actor and its idempotency record is saved, so a
//     later retry replays it. Any other removal logs one remove-role action
//     with no actor.
//
// A change that leaves the index as it was logs nothing; every change
// advances the community's roster version.
//
// A create or update takes the shared erasure gate of every subject it can
// write (the event's, the intent's records' subjects and an accept intent's
// inviter) before its claim.
// A record naming an erased subject is never indexed: in a community this
// instance hosts its repo delete is queued at the event's CID
// (writeRoleUnlessErased), the intent it completes is settled, and the event
// returns false. Deletes are not gated.
func (r *CommunityRoleRepository) ApplyRoleRecordEvent(ctx context.Context, event communities.RoleRecordEvent) (bool, error) {
	applied := false
	err := r.inTransaction(ctx, func(tx *sql.Tx) error {
		uri := roleRecordURI(event.CommunityDID, event.RKey)
		var intent *moderation.CommunityIntent
		erased := map[string]bool{}
		if event.Role != nil {
			var err error
			if event.Rev != "" {
				if intent, err = lockWriteIntentTargets(ctx, tx, uri); err != nil {
					return err
				}
			}
			subjects := []string{event.Role.SubjectDID}
			if intent != nil {
				// An accept's audit actor is its inviter, whose invitation
				// the completion marks accepted.
				subjects = append(subjects, intentRecordSubject(intent.Record), intentRecordSubject(intent.PairedRecord), intent.AuditActorDID)
			}
			if erased, err = gateRoleSubjects(ctx, tx, subjects...); err != nil {
				return err
			}
		}
		claimed, err := claimRoleRecordRev(ctx, tx, uri, event.Rev)
		if err != nil || !claimed {
			return err
		}
		if event.Role != nil {
			if intent != nil {
				// The completion adds the subjects erased while the intent
				// was open to erased.
				completed, err := completeWriteIntentFromEvent(ctx, tx, erased, intent.ID, uri, event)
				applied = !erased[event.Role.SubjectDID]
				if err != nil || completed {
					return err
				}
			}
			// A later copy of a record an intent wrote for a subject
			// erased while it was open stays unindexed after the intent
			// settled, even once the account signs up again.
			marked, err := erasureMarkedIntentRecord(ctx, tx, uri, event.CID, event.Role.SubjectDID)
			if err != nil {
				return err
			}
			if marked {
				erased[event.Role.SubjectDID] = true
			}
			applied = !erased[event.Role.SubjectDID]
			return applyRoleGrantEvent(ctx, tx, erased, *event.Role)
		}
		applied = true
		return applyRoleRemovalEvent(ctx, tx, event, uri)
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// attributeRecordGrant names role's subject as the actor of the newest
// actorless grant-role action logged for the role record (its record key and
// CID) in the role's community, the grant the firehose copy of a record this
// instance wrote logged without an actor. It changes nothing when there is no
// such action.
func attributeRecordGrant(ctx context.Context, tx *sql.Tx, role communities.CommunityRole) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE moderation_actions SET actor_did = $1, origin = $2
		WHERE id = (
		    SELECT id FROM moderation_actions
		    WHERE authority_did = $3 AND scope_kind = $4 AND action = $5 AND subject_uri = $6
		      AND role_record_key = $7 AND role_record_cid = $8
		    ORDER BY created_at DESC, id DESC
		    LIMIT 1
		) AND actor_did = ''
	`, role.SubjectDID, moderation.OriginLocal, role.CommunityDID, moderation.ScopeCommunity, moderation.ActionGrantRole,
		moderation.AccountSubjectURI(role.SubjectDID), role.RecordKey, role.RecordCID); err != nil {
		return fmt.Errorf("attribute the role record's grant: %w", err)
	}
	return nil
}

func applyRoleGrantEvent(ctx context.Context, tx *sql.Tx, erased map[string]bool, role communities.CommunityRole) error {
	changed, err := writeRoleUnlessErased(ctx, tx, erased, role)
	if err != nil || !changed {
		return err
	}
	if err := advanceCommunitySubjectVersion(ctx, tx, role.CommunityDID, role.CommunityDID); err != nil {
		return err
	}
	_, err = insertCommunityRoleAction(ctx, tx, communityRoleAction{
		action: moderation.ActionGrantRole, communityDID: role.CommunityDID, subjectDID: role.SubjectDID,
		role: role.Role, origin: moderation.OriginInherited, recordKey: role.RecordKey, recordCID: role.RecordCID,
	})
	return err
}

func applyRoleRemovalEvent(ctx context.Context, tx *sql.Tx, event communities.RoleRecordEvent, uri string) error {
	var subjectDID, recordCID, role string
	err := tx.QueryRowContext(ctx, `
		DELETE FROM community_roles WHERE community_did = $1 AND record_key = $2
		RETURNING subject_did, record_cid, role
	`, event.CommunityDID, event.RKey).Scan(&subjectDID, &recordCID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete community role: %w", err)
	}
	if err := advanceCommunitySubjectVersion(ctx, tx, event.CommunityDID, event.CommunityDID); err != nil {
		return err
	}
	if event.Op == "delete" && event.Rev != "" {
		intent, err := openDeleteIntentFor(ctx, tx, uri, recordCID, event.Rev)
		if err != nil {
			return err
		}
		if intent != nil {
			version, err := communityRosterVersion(ctx, tx, event.CommunityDID)
			if err != nil {
				return err
			}
			_, err = completeCommunityIntentTx(ctx, tx, *intent, moderation.CommunityCommit{Rev: event.Rev}, role, version,
				time.Now(), intent.IdempotencyRetention)
			return err
		}
	}
	_, err = insertCommunityRoleAction(ctx, tx, communityRoleAction{
		action: moderation.ActionRemoveRole, communityDID: event.CommunityDID, subjectDID: subjectDID,
		role: role, origin: moderation.OriginInherited, recordKey: event.RKey, recordCID: recordCID,
	})
	return err
}

// openDeleteIntentFor is the open delete intent the removal of the indexed
// row with recordCID at uri, at rev, completes, locked; nil when there is
// none.
func openDeleteIntentFor(ctx context.Context, tx *sql.Tx, uri, recordCID, rev string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE target_uri = $1 AND state = 'open' AND op = 'delete' AND replaced_cid = $2 AND guard_rev < $3
		FOR UPDATE
	`, uri, recordCID, rev))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

// lockWriteIntentTargets reads the open create or update intent writing the
// record at uri, as its target or as a transfer's paired target, and, when it
// is a transfer, locks both records' rev-gate rows in ascending URI order
// (inserted with an empty rev when absent), so the event holds the intent's whole
// target set before its own claim. It returns the intent as read, unlocked,
// or nil when there is none.
func lockWriteIntentTargets(ctx context.Context, tx *sql.Tx, uri string) (*moderation.CommunityIntent, error) {
	intent, err := scanCommunityIntent(tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE (target_uri = $1 OR paired_target_uri = $1) AND state = 'open' AND op IN ('create', 'update')
		ORDER BY created_at, id
		LIMIT 1
	`, uri))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if intent.PairedTargetURI != "" {
		if err := lockCommunityAction(ctx, tx, moderation.CommunityActionLock{
			TargetURI: intent.TargetURI, OtherTargetURIs: []string{intent.PairedTargetURI},
		}); err != nil {
			return nil, err
		}
	}
	return &intent, nil
}

// erasureMarkedIntentRecord reports whether a create or update intent wrote
// the record at uri, at recordCID, for subject, and account erasure marked that
// record while the intent was open (community_write_intent_erasures), whatever
// the intent's state now. A record written at another CID, such as a role the
// returning account accepted later, is not marked. It is a plain read taking
// no lock; the caller holds subject's shared erasure gate, so an erasure's
// marks are committed before it.
func erasureMarkedIntentRecord(ctx context.Context, tx *sql.Tx, uri, recordCID, subject string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT intents.id, CASE WHEN erasures.paired THEN intents.paired_record ELSE intents.record END
		FROM community_write_intent_erasures erasures
		JOIN community_write_intents intents ON intents.id = erasures.intent_id
		WHERE intents.op IN ('create', 'update')
		  AND ((NOT erasures.paired AND intents.target_uri = $1)
		    OR (erasures.paired AND intents.paired_target_uri = $1))
	`, uri)
	if err != nil {
		return false, fmt.Errorf("read the erasure-marked intent records for %s: %w", uri, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var intentID string
		var body json.RawMessage
		if err := rows.Scan(&intentID, &body); err != nil {
			return false, fmt.Errorf("scan an erasure-marked intent record for %s: %w", uri, err)
		}
		if intentRecordSubject(body) != subject {
			continue
		}
		cid, err := recordBodyCID(body)
		if err != nil {
			return false, fmt.Errorf("the CID of intent %s's record for %s: %w", intentID, uri, err)
		}
		if cid == recordCID {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the erasure-marked intent records for %s: %w", uri, err)
	}
	return false, nil
}

// completeWriteIntentFromEvent completes the open create or update intent
// intentID from the firehose event on the record at uri, whose gate the event
// has claimed, when the intent is still open, the event's rev is past the
// intent's guard rev, and the event's CID is the CID of the intent's body for
// uri; otherwise it changes nothing and returns false. Every record the
// intent writes was in the event's one commit, so each is indexed from the
// intent's body: the event's record at the event's CID, a transfer's other
// record at the CID of its body, its gate stamped at the event's rev, unless
// that gate already holds a later change, which is kept. When a row changed
// the roster version advances once and one action naming the intent's audit
// actor is logged; the invitation is accepted and the idempotency record of
// the intent's requester saved, so a retry replays the completion. The
// invitation is accepted before any role row or version is written. A record
// whose subject is in erased (the gates the caller took), or was erased while
// the intent was open (addErasedIntentSubjects, which adds it to erased), is
// not indexed but queued for deletion, and an intent whose target subject is
// erased is settled failed (settleErasedWrite).
func completeWriteIntentFromEvent(ctx context.Context, tx *sql.Tx, erased map[string]bool, intentID, uri string, event communities.RoleRecordEvent) (bool, error) {
	intent, err := scanCommunityIntent(tx.QueryRowContext(ctx, `
		SELECT `+communityIntentColumns+`
		FROM community_write_intents
		WHERE id = $1 AND state = 'open' AND guard_rev < $2
		FOR UPDATE
	`, intentID, event.Rev))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	type intentWrite struct {
		uri  string
		body json.RawMessage
	}
	writes := []intentWrite{{intent.TargetURI, intent.Record}}
	if intent.PairedTargetURI != "" {
		writes = append(writes, intentWrite{intent.PairedTargetURI, intent.PairedRecord})
	}
	cids := make([]string, len(writes))
	for i, write := range writes {
		if cids[i], err = recordBodyCID(write.body); err != nil {
			return false, fmt.Errorf("the CID of intent %s's record for %s: %w", intent.ID, write.uri, err)
		}
		if write.uri == uri && cids[i] != event.CID {
			return false, nil
		}
	}
	if err := addErasedIntentSubjects(ctx, tx, intent, erased); err != nil {
		return false, err
	}
	if err := acceptIntentInvitation(ctx, tx, intent.InvitationID); err != nil {
		return false, err
	}
	changed := false
	for i, write := range writes {
		if write.uri != uri {
			current, err := stampIntentRecordGate(ctx, tx, write.uri, event.Rev)
			if err != nil {
				return false, err
			}
			if !current {
				continue
			}
		}
		rowChanged, err := writeIntentRole(ctx, tx, erased, intent.CommunityDID, write.uri, write.body, cids[i])
		if err != nil {
			return false, err
		}
		changed = changed || rowChanged
	}
	if changed {
		if err := advanceCommunitySubjectVersion(ctx, tx, intent.CommunityDID, intent.CommunityDID); err != nil {
			return false, err
		}
	}
	completion := communityActionTransaction{tx: tx}
	if erased[intentRecordSubject(intent.Record)] {
		if _, err := completion.settleErasedWrite(ctx, intent); err != nil {
			return false, err
		}
		return true, nil
	}
	actionID := ""
	if !changed {
		if actionID, err = recordRoleAction(ctx, tx, intent.CommunityDID, intent.Action, intentRecordKey(intent.TargetURI), cids[0]); err != nil {
			return false, err
		}
	}
	commit := moderation.CommunityCommit{Rev: event.Rev, RecordCID: cids[0], GateRev: event.Rev, CompletedAt: time.Now()}
	if _, err := completion.finishCompletion(ctx, intent, commit, actionID, "", intent.IdempotencyRetention); err != nil {
		return false, err
	}
	return true, nil
}

// recordBodyCID is the CID a PDS assigns a record body: DAG-CBOR, CIDv1,
// sha256.
func recordBodyCID(body json.RawMessage) (string, error) {
	data, err := atdata.UnmarshalJSON(body)
	if err != nil {
		return "", fmt.Errorf("decode record body: %w", err)
	}
	encoded, err := atdata.MarshalCBOR(data)
	if err != nil {
		return "", fmt.Errorf("encode record body: %w", err)
	}
	sum, err := cid.NewPrefixV1(cid.DagCBOR, multihash.SHA2_256).Sum(encoded)
	if err != nil {
		return "", fmt.Errorf("hash record body: %w", err)
	}
	return sum.String(), nil
}

// roleRecordURI is the AT-URI of the role record at recordKey in communityDID.
func roleRecordURI(communityDID, recordKey string) string {
	return "at://" + communityDID + "/" + communities.RoleCollection + "/" + recordKey
}

// claimRoleRecordRev claims the role record's rev gate at rev in tx, the
// firehose's per-record gate: it wins when the gate holds no rev or an older
// one. An empty rev bypasses the gate and wins without writing.
func claimRoleRecordRev(ctx context.Context, tx *sql.Tx, uri, rev string) (bool, error) {
	if rev == "" {
		return true, nil
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO jetstream_record_revs (record_uri, rev) VALUES ($1, $2)
		ON CONFLICT (record_uri) DO UPDATE SET rev = EXCLUDED.rev, updated_at = NOW()
		WHERE jetstream_record_revs.rev < EXCLUDED.rev
	`, uri, rev)
	if err != nil {
		return false, fmt.Errorf("claim community role record rev: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check community role record rev claim: %w", err)
	}
	return claimed > 0, nil
}

// gateRoleSubjects takes the shared erasure gate (erasureGateTx) of each
// subject, in ascending DID order, and returns the erased ones. A transaction
// that writes a role row takes its subject's gate, and one that writes an
// invitation the inviter's and the invitee's, before any community role
// invitation, community_roles or community_subject_versions row lock, so
// account erasure, which locks those rows only after its exclusive gate,
// waits at the gate instead of on a row an action holds while it waits
// outside Postgres. Action transactions take the gates first
// (lockCommunityAction); the role consumer takes them before its rev claim,
// but a create or update on a record of an open transfer intent locks both
// records' rev-gate rows before them (lockWriteIntentTargets). Two invitation
// writes do not hold the inviter's gate: a decline holds only the invitee's,
// and an invite that supersedes the invitee's earlier open invitation does
// not hold that invitation's inviter's. Neither waits outside Postgres or
// holds a lock erasure takes, and both update only a row still 'open': the
// inviter's erasure waits on the row until the writer commits and then
// deletes it, or the writer waits on the erasure and finds the row gone.
func gateRoleSubjects(ctx context.Context, tx *sql.Tx, subjects ...string) (map[string]bool, error) {
	erased := map[string]bool{}
	for _, subject := range ascendingUnique(subjects) {
		isErased, err := erasureGateTx(ctx, tx, subject)
		if err != nil {
			return nil, fmt.Errorf("check role subject erasure: %w", err)
		}
		if isErased {
			erased[subject] = true
		}
	}
	return erased, nil
}

// writeRoleUnlessErased writes role's row (writeCommunityRoleRowTx) unless
// its subject is in erased, the subjects gateRoleSubjects found erased. An
// erased subject's record is never indexed: in a community this instance
// hosts, a guarded repo delete of the record at its CID is queued instead. It
// reports whether the row changed.
func writeRoleUnlessErased(ctx context.Context, tx *sql.Tx, erased map[string]bool, role communities.CommunityRole) (bool, error) {
	if !erased[role.SubjectDID] {
		return writeCommunityRoleRowTx(ctx, tx, role)
	}
	var hosted bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM communities WHERE did = $1 AND pds_refresh_token_encrypted IS NOT NULL)
	`, role.CommunityDID).Scan(&hosted); err != nil {
		return false, fmt.Errorf("check the erased role record's community is hosted: %w", err)
	}
	if !hosted {
		return false, nil
	}
	return false, enqueueRoleRecordDelete(ctx, tx, role.CommunityDID, role.RecordKey, role.RecordCID)
}

// upsertCommunityRoleTx writes the role row and, when the row changed,
// advances the community's roster version. It reports whether the row
// changed.
func upsertCommunityRoleTx(ctx context.Context, tx *sql.Tx, role communities.CommunityRole) (bool, error) {
	changed, err := writeCommunityRoleRowTx(ctx, tx, role)
	if err != nil || !changed {
		return false, err
	}
	return true, advanceCommunitySubjectVersion(ctx, tx, role.CommunityDID, role.CommunityDID)
}

// writeCommunityRoleRowTx writes the role row, leaving the roster version to
// the caller, and reports whether the row changed.
func writeCommunityRoleRowTx(ctx context.Context, tx *sql.Tx, role communities.CommunityRole) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO community_roles
		    (community_did, subject_did, role, record_key, record_cid, record_created_at, indexed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (community_did, subject_did) DO UPDATE SET
		    role = EXCLUDED.role,
		    record_key = EXCLUDED.record_key,
		    record_cid = EXCLUDED.record_cid,
		    record_created_at = EXCLUDED.record_created_at,
		    indexed_at = EXCLUDED.indexed_at
		WHERE (community_roles.role, community_roles.record_key, community_roles.record_cid)
		    IS DISTINCT FROM (EXCLUDED.role, EXCLUDED.record_key, EXCLUDED.record_cid)
	`, role.CommunityDID, role.SubjectDID, role.Role, role.RecordKey, role.RecordCID,
		role.RecordCreatedAt.UTC(), time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("upsert community role: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check community role upsert: %w", err)
	}
	return changed > 0, nil
}

func (r *CommunityRoleRepository) inTransaction(ctx context.Context, apply func(tx *sql.Tx) error) (err error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin community role transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// roleQuerier is a pool or a transaction.
type roleQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryCommunityRoles(ctx context.Context, querier roleQuerier, query string, args ...any) ([]communities.CommunityRole, error) {
	rows, err := querier.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list community roles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var roles []communities.CommunityRole
	for rows.Next() {
		var role communities.CommunityRole
		if err := rows.Scan(&role.CommunityDID, &role.SubjectDID, &role.Role, &role.RecordKey,
			&role.RecordCID, &role.RecordCreatedAt, &role.IndexedAt); err != nil {
			return nil, fmt.Errorf("scan community role: %w", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list community roles: %w", err)
	}
	return roles, nil
}

var _ communities.RoleRepository = (*CommunityRoleRepository)(nil)
