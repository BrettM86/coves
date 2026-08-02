package telegram

import (
	"strings"
	"testing"
	"time"
)

// telegramEnvVars is every variable ConfigFromEnv reads. Tests clear all of
// them so a value in the developer's own shell cannot change a result.
var telegramEnvVars = []string{
	"TELEGRAM_ALERTS_ENABLED",
	"TELEGRAM_BOT_TOKEN",
	"TELEGRAM_CHAT_ID",
	"TELEGRAM_ALERT_REASONS",
	"TELEGRAM_TIMEOUT_SECONDS",
}

// clearEnv unsets every Telegram variable for the duration of the test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range telegramEnvVars {
		t.Setenv(key, "")
	}
}

// Alerting must be opt-in: a self-hosted Coves instance cannot need a Telegram
// account in order to boot.
func TestConfigFromEnv_DisabledByDefault(t *testing.T) {
	clearEnv(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Enabled {
		t.Error("Telegram alerts must default to disabled")
	}
}

// A disabled config must not be judged against the enabled requirements, or an
// operator who never wanted alerts cannot start the server.
func TestConfigFromEnv_DisabledIgnoresMissingCredentials(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "false")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Enabled {
		t.Error("expected alerts to be disabled")
	}
}

// The most likely way to deploy this feature and have it do nothing: fill in
// the credentials, never flip the flag. The loader is otherwise structurally
// blind to it, since it returns before reading them.
func TestConfigFromEnv_FlagsCredentialsWhileDisabled(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"token only", map[string]string{"TELEGRAM_BOT_TOKEN": testToken}, true},
		{"chat ID only", map[string]string{"TELEGRAM_CHAT_ID": "-1001234567890"}, true},
		{"both", map[string]string{
			"TELEGRAM_BOT_TOKEN": testToken,
			"TELEGRAM_CHAT_ID":   "-1001234567890",
		}, true},
		{"neither", map[string]string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TELEGRAM_ALERTS_ENABLED", "false")
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			cfg, err := ConfigFromEnv()
			if err != nil {
				t.Fatalf("a half-finished setup must not fail startup, got: %v", err)
			}
			if cfg.Enabled {
				t.Fatal("expected alerts to stay disabled")
			}
			if cfg.CredentialsPresentWhileDisabled != tt.want {
				t.Errorf("CredentialsPresentWhileDisabled = %v, want %v",
					cfg.CredentialsPresentWhileDisabled, tt.want)
			}
			// The presence check must not retain the credential.
			if cfg.BotToken != "" || cfg.ChatID != "" {
				t.Errorf("a disabled config must not carry credentials, got token=%q chat=%q",
					cfg.BotToken, cfg.ChatID)
			}
		})
	}
}

func TestConfigFromEnv_LoadsEnabledConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "true")
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	t.Setenv("TELEGRAM_CHAT_ID", "-1001234567890")
	t.Setenv("TELEGRAM_ALERT_REASONS", "csam, doxing ,illegal")
	t.Setenv("TELEGRAM_TIMEOUT_SECONDS", "8")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if !cfg.Enabled {
		t.Error("expected alerts to be enabled")
	}
	if cfg.BotToken != testToken {
		t.Errorf("BotToken = %q, want %q", cfg.BotToken, testToken)
	}
	if cfg.ChatID != "-1001234567890" {
		t.Errorf("ChatID = %q, want %q", cfg.ChatID, "-1001234567890")
	}
	if cfg.Timeout != 8*time.Second {
		t.Errorf("Timeout = %v, want 8s", cfg.Timeout)
	}

	want := []string{"csam", "doxing", "illegal"}
	if len(cfg.AlertReasons) != len(want) {
		t.Fatalf("AlertReasons = %v, want %v", cfg.AlertReasons, want)
	}
	for i, reason := range cfg.AlertReasons {
		if reason != want[i] {
			t.Errorf("AlertReasons[%d] = %q, want %q", i, reason, want[i])
		}
	}
}

// Whitespace around a value is the standard docker-compose and .env hazard.
func TestConfigFromEnv_TrimsWhitespace(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_ALERTS_ENABLED", " true ")
	t.Setenv("TELEGRAM_BOT_TOKEN", "  "+testToken+"  ")
	t.Setenv("TELEGRAM_CHAT_ID", " @covesalerts ")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.BotToken != testToken {
		t.Errorf("BotToken = %q, want it trimmed", cfg.BotToken)
	}
	if cfg.ChatID != "@covesalerts" {
		t.Errorf("ChatID = %q, want %q", cfg.ChatID, "@covesalerts")
	}
}

func TestConfigFromEnv_DefaultsTimeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "true")
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	t.Setenv("TELEGRAM_CHAT_ID", "1")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Timeout != defaultTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, defaultTimeout)
	}
}

// Enabling alerts without credentials must fail startup. Degrading to "alerts
// off" would leave the deployment silent and looking healthy — the original
// bug.
func TestConfigFromEnv_RejectsIncompleteEnabledConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr error
	}{
		{
			name:    "no bot token",
			env:     map[string]string{"TELEGRAM_CHAT_ID": "1"},
			wantErr: ErrMissingBotToken,
		},
		{
			name:    "no chat ID",
			env:     map[string]string{"TELEGRAM_BOT_TOKEN": testToken},
			wantErr: ErrMissingChatID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TELEGRAM_ALERTS_ENABLED", "true")
			for key, value := range tt.env {
				t.Setenv(key, value)
			}

			if _, err := ConfigFromEnv(); err != tt.wantErr {
				t.Fatalf("expected %v, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestConfigFromEnv_RejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		value    string
		wantText string
	}{
		{"non-boolean enabled", "TELEGRAM_ALERTS_ENABLED", "yes", "TELEGRAM_ALERTS_ENABLED"},
		{"non-integer timeout", "TELEGRAM_TIMEOUT_SECONDS", "5s", "TELEGRAM_TIMEOUT_SECONDS"},
		{"zero timeout", "TELEGRAM_TIMEOUT_SECONDS", "0", "TELEGRAM_TIMEOUT_SECONDS"},
		{"negative timeout", "TELEGRAM_TIMEOUT_SECONDS", "-1", "TELEGRAM_TIMEOUT_SECONDS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("TELEGRAM_ALERTS_ENABLED", "true")
			t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
			t.Setenv("TELEGRAM_CHAT_ID", "1")
			t.Setenv(tt.key, tt.value)

			_, err := ConfigFromEnv()
			if err == nil {
				t.Fatalf("expected an error for %s=%q", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error should name %s, got: %v", tt.wantText, err)
			}
		})
	}
}

// A config error must not quote the credential it was validating.
func TestConfigFromEnv_ErrorsNeverLeakBotToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "true")
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	t.Setenv("TELEGRAM_CHAT_ID", "1")
	t.Setenv("TELEGRAM_TIMEOUT_SECONDS", "nonsense")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("config error leaked the bot token: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	t.Run("disabled is always valid", func(t *testing.T) {
		if err := (Config{Enabled: false}).Validate(); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("rejects a non-positive timeout", func(t *testing.T) {
		cfg := Config{Enabled: true, BotToken: testToken, ChatID: "1"}
		if err := cfg.Validate(); err != ErrInvalidTimeout {
			t.Fatalf("expected ErrInvalidTimeout, got: %v", err)
		}
	})

	t.Run("accepts a complete config", func(t *testing.T) {
		cfg := Config{Enabled: true, BotToken: testToken, ChatID: "1", Timeout: time.Second}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})
}
