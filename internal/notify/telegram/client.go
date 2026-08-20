package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// maxResponseBytes caps how much of a Bot API response is read. Responses
	// are small JSON objects; the limit stops a misbehaving or spoofed endpoint
	// from feeding the process an unbounded body.
	maxResponseBytes = 64 * 1024

	// redactedToken replaces the bot token wherever it would otherwise be
	// rendered into a string.
	redactedToken = "[REDACTED]"

	// maxDescriptionLength caps how much of the Bot API's error description is
	// carried into our error. The description is remote-controlled text headed
	// straight for a log line, and the response budget allows up to
	// maxResponseBytes of it.
	maxDescriptionLength = 256
)

// ErrEmptyMessage indicates SendMessage was called with no text. The Bot API
// rejects empty messages, so this is caught before spending a request.
var ErrEmptyMessage = errors.New("cannot send an empty Telegram message")

// Client sends plain-text messages to a fixed Telegram chat.
//
// It satisfies adminreports.MessageSender without importing it — the domain
// defines the interface it needs, and this package just happens to fit.
type Client struct {
	botToken   string
	chatID     string
	baseURL    string
	httpClient *http.Client
}

// NewClient builds a Client from cfg.
//
// A disabled config is an error rather than a no-op client: "constructed but
// inert" is a state that reads as working at every call site, and callers
// should branch on cfg.Enabled where the decision is visible.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrDisabled
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	return &Client{
		botToken:   cfg.BotToken,
		chatID:     cfg.ChatID,
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{Timeout: cfg.Timeout}, // coves:allow-bare-client: api.telegram.org, a fixed host in this package's own const; no caller-supplied URL reaches it
	}, nil
}

// sendMessagePayload is the Bot API sendMessage request body.
type sendMessagePayload struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`

	// DisableWebPagePreview stops Telegram from fetching and rendering any URL
	// in the message. This is a safety requirement, not a cosmetic one: alert
	// text carries the URI of reported content, and Telegram resolving that
	// link server-side would pull the reported material into the chat — the
	// precise outcome to avoid when the report is about CSAM.
	DisableWebPagePreview bool `json:"disable_web_page_preview"`
}

// apiResponse is the envelope every Bot API method returns.
type apiResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// SendMessage delivers text to the configured chat.
//
// No parse_mode is set, so Telegram treats the body as literal text. That is
// deliberate: alert text embeds a caller-supplied URI, and enabling Markdown or
// HTML would turn every escaping mistake into either a rejected alert or an
// injected link.
func (c *Client) SendMessage(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return ErrEmptyMessage
	}

	body, err := json.Marshal(sendMessagePayload{
		ChatID:                c.chatID,
		Text:                  text,
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("telegram: encoding sendMessage body: %w", err)
	}

	// The token sits in the path, which is what the Bot API requires and what
	// makes every error out of this call a redaction hazard.
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", c.baseURL, c.botToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: building sendMessage request: %w", c.scrub(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: sendMessage request failed: %w", c.scrub(err))
	}
	defer resp.Body.Close()

	// Read before inspecting the status: the body carries the Bot API's own
	// error description, which is far more useful than the code alone. Draining
	// it also lets the connection be reused.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("telegram: reading sendMessage response (status %d): %w",
			resp.StatusCode, c.scrub(err))
	}

	var api apiResponse
	if err := json.Unmarshal(payload, &api); err != nil {
		// An unparseable body means this is not the Bot API — a proxy error
		// page, say. The body itself is not echoed: it is untrusted content of
		// unknown size headed for the logs.
		return fmt.Errorf("telegram: sendMessage returned status %d with an unparseable body",
			resp.StatusCode)
	}

	// Both conditions, not just api.OK. The real Bot API pairs 200 with ok:true,
	// but this response crossed an untrusted network path — a proxy, CDN, or WAF
	// that synthesizes or replays a JSON envelope on a 5xx would otherwise make
	// a failed send return nil, which is the one failure shape that produces no
	// log line anywhere.
	if !api.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The description is the actionable part ("chat not found" tells an
		// operator their chat ID is wrong), but it is remote-controlled text, so
		// it gets the same redaction every other error path here gets, plus a
		// length cap.
		return fmt.Errorf("telegram: sendMessage rejected (status %d, error_code %d): %s",
			resp.StatusCode, api.ErrorCode, c.safeDescription(api.Description))
	}

	return nil
}

// safeDescription prepares a Bot API error description for inclusion in an
// error: token redacted, length capped, newlines flattened so a hostile
// description cannot forge extra log lines.
func (c *Client) safeDescription(description string) string {
	safe := c.redact(strings.ReplaceAll(strings.ReplaceAll(description, "\n", " "), "\r", " "))

	runes := []rune(safe)
	if len(runes) > maxDescriptionLength {
		safe = string(runes[:maxDescriptionLength]) + "…"
	}
	return safe
}

// scrub removes the bot token from an error before it can be returned.
//
// net/http embeds the full request URL in *url.Error, and the token is part of
// that URL — so wrapping a transport error verbatim publishes the credential to
// wherever logs go. The error chain is flattened to a plain message rather than
// wrapped, because errors.As on the original would hand a caller back the very
// *url.Error whose URL field still holds the token. Losing errors.Is on a
// network failure costs little; leaking a bot token costs a takeover of the
// alert channel.
func (c *Client) scrub(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(c.redact(err.Error()))
}

// redact replaces every occurrence of the bot token in s.
func (c *Client) redact(s string) string {
	if c.botToken == "" {
		return s
	}
	return strings.ReplaceAll(s, c.botToken, redactedToken)
}
