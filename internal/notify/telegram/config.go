// Package telegram delivers operator alerts to a Telegram chat via the Bot API.
//
// It exists because Coves is run by a very small team with no on-call rotation:
// an alert that only reaches a database table reaches nobody. Telegram was
// chosen over email for the alerting path because it has no deliverability
// surface — no SPF, no DKIM, no spam folder, no domain reputation to maintain —
// and it reaches a phone in seconds. Email remains the better channel for a
// durable, searchable record; this is the channel for "look at this now".
//
// The client is deliberately generic: SendMessage takes plain text and knows
// nothing about reports, so any domain needing to reach an operator can use it.
//
// Alerting is off unless TELEGRAM_ALERTS_ENABLED is set. Most operators running
// their own Coves instance will not want it, and a self-hosted deployment must
// not need a Telegram account to boot.
package telegram

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultTimeout bounds a single Bot API call. Report submission blocks on
	// this, so it is short: an alert that has not landed in five seconds is
	// better logged as failed than left holding the reporter's request.
	defaultTimeout = 5 * time.Second

	// defaultBaseURL is the public Bot API origin. Tests override it.
	defaultBaseURL = "https://api.telegram.org"
)

// Configuration errors. These fail startup rather than degrading to "alerts
// off", because a silently disabled alerter is the exact fault this package
// was written to remove.
var (
	// ErrMissingBotToken indicates alerts are enabled with no bot token.
	ErrMissingBotToken = errors.New("TELEGRAM_BOT_TOKEN is required when TELEGRAM_ALERTS_ENABLED is true")

	// ErrMissingChatID indicates alerts are enabled with no destination chat.
	ErrMissingChatID = errors.New("TELEGRAM_CHAT_ID is required when TELEGRAM_ALERTS_ENABLED is true")

	// ErrInvalidTimeout indicates a non-positive request timeout.
	ErrInvalidTimeout = errors.New("Telegram request timeout must be positive")

	// ErrDisabled indicates a client was requested from a disabled config.
	ErrDisabled = errors.New("Telegram alerts are disabled")
)

// lookup reads an environment variable, trimming surrounding whitespace.
// Docker Compose and .env files routinely leave trailing spaces, and a token
// that fails only because of one is a miserable thing to debug.
func lookup(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// Config holds the settings for the Telegram alert channel.
type Config struct {
	// Enabled turns the alert channel on. When false every other field is
	// ignored and no client is built.
	Enabled bool

	// BotToken authenticates against the Bot API. It is a credential: it
	// appears in the request path, so it must never reach a log or an error
	// message. See Client.scrub.
	BotToken string

	// ChatID is the destination chat. Accepts a numeric ID (including the
	// negative IDs that groups use) or an "@channelname" handle, so it is
	// carried as a string.
	ChatID string

	// AlertReasons restricts which report categories this channel carries, by
	// reason name. Empty carries every reason.
	//
	// This is channel configuration rather than domain policy: the question it
	// answers is "what does *this* channel deliver", and a second channel added
	// later (email, say) would reasonably answer it differently — everything by
	// mail, urgent-only by push. The names are validated against the domain
	// vocabulary by the caller.
	AlertReasons []string

	// Timeout bounds a single Bot API call. Values above the caller's own
	// backstop (adminreports.alertTimeout, 10s) are capped by it.
	Timeout time.Duration

	// CredentialsPresentWhileDisabled records that a bot token or chat ID was
	// configured while Enabled is false — a half-finished setup that boots
	// clean and alerts nobody. Reported rather than fatal: turning a noisy
	// channel off in a hurry is a legitimate operation and must not require
	// also clearing the credentials.
	CredentialsPresentWhileDisabled bool

	// BaseURL overrides the Bot API origin. Tests point this at an httptest
	// server; production leaves it empty and gets defaultBaseURL.
	BaseURL string
}

// Validate reports whether the configuration can produce a working client.
// A disabled config is always valid — there is nothing to get wrong.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.BotToken) == "" {
		return ErrMissingBotToken
	}
	if strings.TrimSpace(c.ChatID) == "" {
		return ErrMissingChatID
	}
	if c.Timeout <= 0 {
		return ErrInvalidTimeout
	}
	return nil
}

// ConfigFromEnv loads the Telegram alert configuration from the environment.
//
// Unlike the image proxy's loader, a malformed value here is an error rather
// than a warning-plus-default. Every other subsystem degrades visibly when
// misconfigured; this one degrades into silence, which is indistinguishable
// from "no reports have been filed".
//
// Environment variables:
//   - TELEGRAM_ALERTS_ENABLED: "true"/"false" (default: false)
//   - TELEGRAM_BOT_TOKEN: bot token from @BotFather (required when enabled)
//   - TELEGRAM_CHAT_ID: destination chat ID or @channelname (required when enabled)
//   - TELEGRAM_ALERT_REASONS: comma-separated report reasons (default: all)
//   - TELEGRAM_TIMEOUT_SECONDS: per-request timeout (default: 5)
func ConfigFromEnv() (Config, error) {
	cfg := Config{Timeout: defaultTimeout}

	if raw := lookup("TELEGRAM_ALERTS_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TELEGRAM_ALERTS_ENABLED: %q is not a boolean (use true/false)", raw)
		}
		cfg.Enabled = enabled
	}

	if !cfg.Enabled {
		// Presence only — the values are neither stored nor logged. The check
		// exists because "filled in the credentials, never flipped the flag" is
		// the single most likely way to deploy this feature and have it do
		// nothing, and the loader is otherwise structurally blind to it.
		cfg.CredentialsPresentWhileDisabled =
			lookup("TELEGRAM_BOT_TOKEN") != "" || lookup("TELEGRAM_CHAT_ID") != ""
		return cfg, nil
	}

	cfg.BotToken = lookup("TELEGRAM_BOT_TOKEN")
	cfg.ChatID = lookup("TELEGRAM_CHAT_ID")

	if raw := lookup("TELEGRAM_TIMEOUT_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			return Config{}, fmt.Errorf("TELEGRAM_TIMEOUT_SECONDS: %q must be a positive integer", raw)
		}
		cfg.Timeout = time.Duration(seconds) * time.Second
	}

	for _, part := range strings.Split(os.Getenv("TELEGRAM_ALERT_REASONS"), ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			cfg.AlertReasons = append(cfg.AlertReasons, trimmed)
		}
	}

	return cfg, cfg.Validate()
}
