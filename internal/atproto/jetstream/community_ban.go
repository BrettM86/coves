package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"Coves/internal/core/communities"

	"github.com/rivo/uniseg"
)

// Bounds from the social.coves.moderation.ban lexicon.
const (
	maxBanReasonGraphemes = 2000
	maxBanReasonBytes     = 20000
)

// WithPostCommunityBans installs the community ban index. Without it,
// social.coves.moderation.ban records are not indexed.
func WithPostCommunityBans(store communities.BanRepository) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.communityBans = store }
}

// banRecord is the wire shape of social.coves.moderation.ban.
type banRecord struct {
	Subject   string  `json:"subject"`
	Reason    string  `json:"reason"`
	ExpiresAt *string `json:"expiresAt"`
	CreatedAt string  `json:"createdAt"`
}

// handleCommunityBanEvent indexes one ban commit. event.Did is the community:
// only an indexed community's repo may ban, so any other repo is refused. The
// record key must be the one derived from the subject, which keeps one ban per
// community and subject. Writes are rev-gated by record URI, so a stale create
// replayed after its delete does not bring the ban back.
func (c *PostEventConsumer) handleCommunityBanEvent(ctx context.Context, event *JetstreamEvent, commit *CommitEvent) error {
	communityDID := event.Did

	exists, err := c.communityRepo.ExistsByDID(ctx, communityDID)
	if err != nil {
		return fmt.Errorf("%w: failed to verify community %s exists: %v", errValidationInfra, communityDID, err)
	}
	if !exists {
		// A community's first ban can outrun its own profile event, so this is
		// left to the bounded redrive budget rather than dropped.
		log.Printf("🚨 SECURITY: refusing %s from %s - not an indexed community repo",
			commit.Collection, communityDID)
		return fmt.Errorf("%w: community not found: %s - cannot apply a %s from a repo that is not an indexed community",
			ErrUnresolvedReference, communityDID, commit.Collection)
	}

	var gate *RevGate
	if c.db != nil {
		gate = NewRevGate(c.db)
	}

	if commit.Operation == "delete" {
		return applyGated(ctx, gate, "posts", communityDID, commit, func() error {
			if err := c.communityBans.DeleteCommunityBan(ctx, communityDID, commit.RKey); err != nil {
				return fmt.Errorf("lifting ban %s/%s: %w", communityDID, commit.RKey, err)
			}
			return nil
		})
	}

	ban, err := parseBanRecord(communityDID, commit)
	if err != nil {
		return err
	}
	return applyGated(ctx, gate, "posts", communityDID, commit, func() error {
		if err := c.communityBans.UpsertCommunityBan(ctx, *ban); err != nil {
			return fmt.Errorf("indexing ban %s/%s: %w", communityDID, commit.RKey, err)
		}
		return nil
	})
}

// parseBanRecord validates a ban create or update against the lexicon and the
// record-key rule.
func parseBanRecord(communityDID string, commit *CommitEvent) (*communities.CommunityBan, error) {
	if commit.Record == nil {
		return nil, fmt.Errorf("%w: %s %s event missing record data", ErrPermanentEvent, commit.Collection, commit.Operation)
	}
	recordJSON, err := json.Marshal(commit.Record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s record: %w", commit.Collection, err)
	}
	var record banRecord
	if err := json.Unmarshal(recordJSON, &record); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal %s record: %v", ErrPermanentEvent, commit.Collection, err)
	}

	if err := requireDIDShaped("ban subject", record.Subject); err != nil {
		return nil, err
	}
	if want := communities.BanRecordKey(record.Subject); commit.RKey != want {
		return nil, fmt.Errorf("%w: ban %s/%s is not at the record key of its subject %s",
			ErrPermanentEvent, communityDID, commit.RKey, record.Subject)
	}
	if _, err := time.Parse(time.RFC3339, record.CreatedAt); err != nil {
		return nil, fmt.Errorf("%w: ban %s/%s has invalid createdAt %q", ErrPermanentEvent, communityDID, commit.RKey, record.CreatedAt)
	}
	if len(record.Reason) > maxBanReasonBytes || uniseg.GraphemeClusterCount(record.Reason) > maxBanReasonGraphemes {
		return nil, fmt.Errorf("%w: ban %s/%s reason exceeds the lexicon limit", ErrPermanentEvent, communityDID, commit.RKey)
	}

	ban := &communities.CommunityBan{
		CommunityDID: communityDID,
		SubjectDID:   record.Subject,
		RecordKey:    commit.RKey,
		RecordCID:    commit.CID,
		Reason:       record.Reason,
	}
	if record.ExpiresAt != nil {
		expiresAt, err := time.Parse(time.RFC3339, *record.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("%w: ban %s/%s has invalid expiresAt %q", ErrPermanentEvent, communityDID, commit.RKey, *record.ExpiresAt)
		}
		utc := expiresAt.UTC()
		ban.ExpiresAt = &utc
	}
	return ban, nil
}
