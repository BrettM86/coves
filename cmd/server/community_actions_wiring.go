package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	moderationhandlers "Coves/internal/api/handlers/moderation"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	postgresRepo "Coves/internal/db/postgres"
)

// communityIntentTTL is how long a community write intent waits for its PDS
// write to be confirmed before the sweep reconciles it. It is longer than any
// PDS write timeout, so the sweep never settles a write still in flight.
const communityIntentTTL = 10 * time.Minute

// communityRemovalLimitWindow is the window the per-moderator and
// per-community post removal limits count over; the settings are per hour.
const communityRemovalLimitWindow = time.Hour

// communityBanLimitWindow is the window the per-moderator and per-community
// ban limits count over; the settings are per hour.
const communityBanLimitWindow = time.Hour

// communityActionConfig is the community action service's production
// configuration: its store, PDS repos, resolvers, ban lister, and the removal
// and ban limits from the moderation settings.
func (a *application) communityActionConfig(store *postgresRepo.CommunityActionRepository) moderation.CommunityActionConfig {
	return moderation.CommunityActionConfig{
		Authorizer: a.communityRoleAuthorizer,
		Store:      store,
		Repos: posts.NewCommunityRepoFactory(a.communityService,
			pds.PrivateHostOptions(a.allowPrivateHosts())...),
		Now:            time.Now,
		IntentTTL:      communityIntentTTL,
		IdempotencyTTL: a.cfg.Moderation.IdempotencyRetention,
		Posts:          postgresRepo.NewCommunityPostResolver(a.db),
		Comments:       postgresRepo.NewCommunityCommentResolver(a.db),
		RemovalLimits: moderation.CommunityRemovalLimits{
			PerModerator: a.cfg.Moderation.CommunityRemovalsPerModerator,
			PerCommunity: a.cfg.Moderation.CommunityRemovalsPerCommunity,
			Window:       communityRemovalLimitWindow,
		},
		BanLimits: moderation.CommunityRemovalLimits{
			PerModerator: a.cfg.Moderation.CommunityBansPerModerator,
			PerCommunity: a.cfg.Moderation.CommunityBansPerCommunity,
			Window:       communityBanLimitWindow,
		},
		Bans: postgresRepo.NewCommunityBanLister(a.db),
	}
}

// communityIntentSweepInterval is how often the expired community write
// intent sweep runs after boot. runTicker also bounds each cycle by it.
const communityIntentSweepInterval = 2 * time.Minute

// startCommunityIntentSweepJob reconciles expired community write intents at
// boot and each interval.
func startCommunityIntentSweepJob(ctx context.Context, waitGroup *sync.WaitGroup, service *moderation.CommunityActionService, interval time.Duration) {
	if service == nil || interval <= 0 {
		return
	}
	runTicker(ctx, waitGroup, "community-intent-sweep", interval, func(ctx context.Context) {
		settled, err := service.SweepExpiredIntents(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("community write intent sweep failed", "settled", settled, "error", err)
			return
		}
		if settled > 0 {
			slog.Info("community write intent sweep settled intents", "settled", settled)
		}
	})
}

// communityInvitationActions serves the invitation endpoints: invite, offer,
// decline and the lists from the invitation service, accept from the
// community action service (it runs the roster write pipeline), and the
// invitation and roster version reads from the action store.
type communityInvitationActions struct {
	*moderation.CommunityInvitationService
	actions *moderation.CommunityActionService
	store   *postgresRepo.CommunityActionRepository
}

var _ moderationhandlers.CommunityInvitationActions = (*communityInvitationActions)(nil)

// newCommunityInvitationActions builds the invitation service over the
// community action store and wraps it with the action service's accept.
// resolver resolves invitee handles through the instance's configured
// identity resolution.
func newCommunityInvitationActions(authorizer communities.RoleAuthorizer, store *postgresRepo.CommunityActionRepository,
	invitations communities.InvitationReader, resolver moderation.InviteeResolver,
	actions *moderation.CommunityActionService, idempotencyTTL time.Duration,
) *communityInvitationActions {
	return &communityInvitationActions{
		CommunityInvitationService: moderation.NewCommunityInvitationService(moderation.CommunityInvitationConfig{
			Authorizer:     authorizer,
			Store:          store,
			Invitations:    invitations,
			Resolver:       resolver,
			Now:            time.Now,
			IdempotencyTTL: idempotencyTTL,
		}),
		actions: actions,
		store:   store,
	}
}

// AcceptInvitation runs the accept through the community action pipeline.
func (c *communityInvitationActions) AcceptInvitation(ctx context.Context, req moderation.AcceptInvitationRequest) (*moderation.CommunityActionResult, error) {
	return c.actions.AcceptInvitation(ctx, req)
}

// Invitation reads one invitation in any state, or nil when there is none.
func (c *communityInvitationActions) Invitation(ctx context.Context, invitationID string) (*communities.Invitation, error) {
	return c.store.InvitationByID(ctx, invitationID)
}

// RosterVersion reads the community's current roster version.
func (c *communityInvitationActions) RosterVersion(ctx context.Context, communityDID string) (int64, error) {
	return c.store.RosterVersion(ctx, communityDID)
}
