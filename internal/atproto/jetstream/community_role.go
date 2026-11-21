package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"Coves/internal/core/communities"
)

// maxRoleBytes is the maxLength of role in the social.coves.community.moderator lexicon.
const maxRoleBytes = 64

// WithCommunityRoster installs the community role index. Without it,
// social.coves.community.moderator records are not indexed.
func WithCommunityRoster(store communities.RoleRepository) CommunityConsumerOption {
	return func(c *CommunityEventConsumer) { c.roster = store }
}

// roleWireRecord is the wire shape of social.coves.community.moderator.
type roleWireRecord struct {
	Subject   string `json:"subject"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

// handleCommunityRoleEvent indexes one role commit. event.Did is the community:
// only an indexed community's repo may hold a roster, so any other repo is
// refused. The record key must be the one derived from the subject, which keeps
// one role per community and subject. The role index applies the commit in one
// transaction under the record's rev gate (jetstream_record_revs), so a stale
// create replayed after its delete does not bring the role back, and in that
// transaction completes a matching open community write intent or logs the
// change with no actor (CommunityRoleRepository.ApplyRoleRecordEvent). A
// create or update that fails validation removes the role held at its record
// key. An unknown role value is stored as written; the roster reader grants it
// nothing.
func (c *CommunityEventConsumer) handleCommunityRoleEvent(ctx context.Context, communityDID string, commit *CommitEvent) error {
	if c.roster == nil {
		return nil
	}

	exists, err := c.repo.ExistsByDID(ctx, communityDID)
	if err != nil {
		return fmt.Errorf("%w: failed to verify community %s exists: %v", errValidationInfra, communityDID, err)
	}
	if !exists {
		// A community's first role record can outrun its own profile event, so
		// this is left to the bounded redrive budget rather than dropped.
		log.Printf("🚨 SECURITY: refusing %s from %s - not an indexed community repo",
			commit.Collection, communityDID)
		return fmt.Errorf("%w: community not found: %s - cannot apply a %s from a repo that is not an indexed community",
			ErrUnresolvedReference, communityDID, commit.Collection)
	}

	apply := func(role *communities.CommunityRole) error {
		applied, err := c.roster.ApplyRoleRecordEvent(ctx, communities.RoleRecordEvent{
			CommunityDID: communityDID, RKey: commit.RKey, Rev: commit.Rev, CID: commit.CID,
			Op: commit.Operation, Role: role,
		})
		if err != nil {
			return fmt.Errorf("applying role %s/%s: %w", communityDID, commit.RKey, err)
		}
		if !applied {
			logSkippedStaleRev(ConsumerCommunities, commit.Operation, commitRecordURI(communityDID, commit), commit.Rev)
		}
		return nil
	}
	if commit.Operation == "delete" {
		return apply(nil)
	}

	role, err := parseRoleRecord(communityDID, commit)
	if errors.Is(err, ErrPermanentEvent) {
		// The record at this key no longer grants anything, so the role it held
		// is removed, rev-gated so an invalid event older than the indexed
		// record leaves that record's role alone. The event is refused either
		// way; a failed removal returns its own error so the event is retried
		// rather than dead-lettered with the old role still indexed.
		if removeErr := apply(nil); removeErr != nil {
			return fmt.Errorf("removing the role replaced by an invalid record (%v): %w", err, removeErr)
		}
		return err
	}
	if err != nil {
		return err
	}
	return apply(role)
}

// parseRoleRecord validates a role create or update against the lexicon and
// the record-key rule.
func parseRoleRecord(communityDID string, commit *CommitEvent) (*communities.CommunityRole, error) {
	if commit.Record == nil {
		return nil, fmt.Errorf("%w: %s %s event missing record data", ErrPermanentEvent, commit.Collection, commit.Operation)
	}
	recordJSON, err := json.Marshal(commit.Record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s record: %w", commit.Collection, err)
	}
	var record roleWireRecord
	if err := json.Unmarshal(recordJSON, &record); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal %s record: %v", ErrPermanentEvent, commit.Collection, err)
	}

	if err := requireDIDShaped("role subject", record.Subject); err != nil {
		return nil, err
	}
	if want := communities.RoleRecordKey(record.Subject); commit.RKey != want {
		return nil, fmt.Errorf("%w: role %s/%s is not at the record key of its subject %s",
			ErrPermanentEvent, communityDID, commit.RKey, record.Subject)
	}
	if record.Role == "" || len(record.Role) > maxRoleBytes {
		return nil, fmt.Errorf("%w: role %s/%s has a role outside the lexicon length bounds", ErrPermanentEvent, communityDID, commit.RKey)
	}
	createdAt, err := time.Parse(time.RFC3339, record.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("%w: role %s/%s has invalid createdAt %q", ErrPermanentEvent, communityDID, commit.RKey, record.CreatedAt)
	}

	return &communities.CommunityRole{
		CommunityDID:    communityDID,
		SubjectDID:      record.Subject,
		Role:            record.Role,
		RecordKey:       commit.RKey,
		RecordCID:       commit.CID,
		RecordCreatedAt: createdAt.UTC(),
	}, nil
}
