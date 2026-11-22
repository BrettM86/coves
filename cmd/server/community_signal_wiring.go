package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"Coves/internal/core/adminreports"
	"Coves/internal/core/communitysignals"
	postgresRepo "Coves/internal/db/postgres"
	"Coves/internal/notify/telegram"
)

// communitySignalDigestCheckInterval is how often the weekly digest checks
// whether its Monday 09:00 UTC slot is due, so the digest goes out within a
// minute of the slot (or at startup if the process was down then).
const communitySignalDigestCheckInterval = time.Minute

// buildCommunitySignals builds the community nsfw flag alert service. Flag
// events are recorded by the community repository whatever this decides; the
// service sends only when COMMUNITY_SIGNAL_ALERTS_ENABLED is true and Telegram
// is configured. An invalid COMMUNITY_SIGNAL_* or Telegram setting fails
// startup.
func (a *application) buildCommunitySignals() error {
	cfg, err := communitysignals.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("community signals: %w", err)
	}
	telegramConfig, err := telegram.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("community signals: telegram: %w", err)
	}

	// Untyped nil when Telegram is disabled: a nil *telegram.Client inside the
	// interface would not compare equal to nil and would be called.
	var sender adminreports.MessageSender
	if telegramConfig.Enabled {
		client, err := telegram.NewClient(telegramConfig)
		if err != nil {
			return fmt.Errorf("community signals: telegram: %w", err)
		}
		sender = client
	}

	switch {
	case !cfg.AlertsEnabled:
		slog.Info("community nsfw flag alerts disabled; flag events are recorded but not sent",
			"enable_with", "COMMUNITY_SIGNAL_ALERTS_ENABLED=true")
	case sender == nil:
		slog.Warn("COMMUNITY_SIGNAL_ALERTS_ENABLED is true but Telegram is not enabled; "+
			"community nsfw flag events are recorded and NOBODY is notified",
			"fix", "set TELEGRAM_ALERTS_ENABLED=true")
	default:
		slog.Info("community nsfw flag alerts enabled", "channel", "telegram",
			"sweep_interval", cfg.SweepInterval, "attempt_limit", cfg.AttemptLimit)
	}

	a.communitySignalConfig = cfg
	a.communitySignals = communitysignals.NewService(
		postgresRepo.NewCommunitySignalRepository(a.db, a.cfg.Instance.Domain), sender, cfg, time.Now, slog.Default())
	return nil
}

// startCommunitySignalJobs runs the flag alert retry sweep every
// sweepInterval and the weekly digest check every minute.
func startCommunitySignalJobs(ctx context.Context, waitGroup *sync.WaitGroup, service *communitysignals.Service, sweepInterval time.Duration) {
	runTicker(ctx, waitGroup, "community-signal-sweep", sweepInterval, func(ctx context.Context) {
		err := service.Sweep(ctx)
		switch {
		case err == nil || errors.Is(err, context.Canceled):
		case errors.Is(err, context.DeadlineExceeded):
			slog.Warn("community nsfw flag alert sweep outlasted its interval", "error_code", "sweep_timed_out")
		default:
			slog.Error("community nsfw flag alert sweep failed", "error", err)
		}
	})
	runTicker(ctx, waitGroup, "community-signal-digest", communitySignalDigestCheckInterval, func(ctx context.Context) {
		err := service.RunWeeklyDigest(ctx)
		switch {
		case err == nil || errors.Is(err, context.Canceled):
		case errors.Is(err, context.DeadlineExceeded):
			slog.Warn("community nsfw weekly digest outlasted its check interval", "error_code", "digest_timed_out")
		default:
			slog.Error("community nsfw weekly digest failed", "error_code", "community_signal_digest_failed")
		}
	})
}
