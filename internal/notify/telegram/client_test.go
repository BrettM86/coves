package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "123456789:AAExampleBotTokenValueNotReal"

// newTestClient wires a Client to handler and returns both.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewClient(Config{
		Enabled:  true,
		BotToken: testToken,
		ChatID:   "-1001234567890",
		Timeout:  5 * time.Second,
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return client
}

// okResponse writes the Bot API's success envelope.
func okResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
}

// Asserted against raw wire keys rather than sendMessagePayload: decoding into
// the production struct shares its tags with the encoder, so a renamed tag
// would satisfy the test while sending a key Telegram ignores.
func TestSendMessage_PostsToBotAPI(t *testing.T) {
	var gotPath string
	var raw map[string]any
	var gotContentType string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		okResponse(w)
	})

	if err := client.SendMessage(context.Background(), "alert text"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if want := "/bot" + testToken + "/sendMessage"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if got, ok := raw["text"]; !ok || got != "alert text" {
		t.Errorf("wire key text = %v, want %q", got, "alert text")
	}
	if got, ok := raw["chat_id"]; !ok || got != "-1001234567890" {
		t.Errorf("wire key chat_id = %v, want %q", got, "-1001234567890")
	}
}

// Success requires a 2xx AND ok:true. A proxy, CDN, or WAF between us and
// Telegram that synthesizes or replays a JSON envelope on an error response
// would otherwise make a failed send return nil — the one failure shape that
// produces no log line anywhere.
func TestSendMessage_RejectsNon2xxWithOKBody(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"gateway error with an ok envelope", http.StatusBadGateway, `{"ok":true,"result":{"message_id":1}}`},
		{"service unavailable with an ok envelope", http.StatusServiceUnavailable, `{"ok":true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})

			err := client.SendMessage(context.Background(), "alert text")
			if err == nil {
				t.Fatalf("status %d with ok:true must not count as delivered", tt.status)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tt.status)) {
				t.Errorf("error should carry the status code, got: %v", err)
			}
		})
	}
}

// The API description is remote-controlled text headed straight for a log line.
func TestSendMessage_SanitizesAPIDescription(t *testing.T) {
	t.Run("redacts the bot token", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(apiResponse{
				OK:          false,
				ErrorCode:   400,
				Description: "bad request for /bot" + testToken + "/sendMessage",
			})
		})

		err := client.SendMessage(context.Background(), "alert text")
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("description path leaked the bot token: %v", err)
		}
	})

	t.Run("caps the length and flattens newlines", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(apiResponse{
				OK:          false,
				ErrorCode:   400,
				Description: strings.Repeat("A", 5000) + "\nforged log line",
			})
		})

		err := client.SendMessage(context.Background(), "alert text")
		if err == nil {
			t.Fatal("expected an error")
		}
		if runes := len([]rune(err.Error())); runes > maxDescriptionLength+200 {
			t.Errorf("error is %d runes; the description should be capped near %d",
				runes, maxDescriptionLength)
		}
		// A newline in remote text must not be able to forge an extra log line.
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("description newlines must be flattened, got: %q", err.Error())
		}
	})
}

// Link previews must stay off: alert text carries the URI of reported content,
// and Telegram resolving it server-side would render the reported material into
// the chat.
// Asserted against the raw wire keys, deliberately: decoding into
// sendMessagePayload would share the struct tag with the encoder, so the check
// would pass even if the tag were renamed and Telegram silently began fetching
// reported URIs again.
func TestSendMessage_DisablesLinkPreview(t *testing.T) {
	var raw map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		okResponse(w)
	})

	if err := client.SendMessage(context.Background(), "at://did:plc:x/social.coves.post/y"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if got, ok := raw["disable_web_page_preview"]; !ok || got != true {
		t.Errorf("wire key disable_web_page_preview must be true so Telegram never "+
			"fetches reported URIs; got %v (present=%v) in payload %v", got, ok, raw)
	}
}

// No parse_mode means Telegram renders the body literally, so an alert
// containing Markdown or HTML metacharacters cannot be reinterpreted as markup.
func TestSendMessage_SendsPlainTextOnly(t *testing.T) {
	var raw map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		okResponse(w)
	})

	if err := client.SendMessage(context.Background(), "_underscores_ and <tags>"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if _, present := raw["parse_mode"]; present {
		t.Errorf("parse_mode must not be sent; got payload %v", raw)
	}
}

func TestSendMessage_ReturnsAPIError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"chat not found"}`)
	})

	err := client.SendMessage(context.Background(), "alert text")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The description is the actionable part — "chat not found" tells the
	// operator their chat ID is wrong, which a bare 400 does not.
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error should carry the API description, got: %v", err)
	}
}

func TestSendMessage_HandlesNonAPIResponse(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
	})

	err := client.SendMessage(context.Background(), "alert text")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should carry the status code, got: %v", err)
	}
	// The body is untrusted content of unknown size; it must not be echoed into
	// the logs.
	if strings.Contains(err.Error(), "<html>") {
		t.Errorf("error must not echo the response body, got: %v", err)
	}
}

// The bot token sits in the request path, so net/http embeds it in *url.Error.
// Every error this client returns is destined for a log line.
func TestSendMessage_NeverLeaksBotToken(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		// A server closed before the request guarantees a transport-level
		// *url.Error carrying the full URL.
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		serverURL := server.URL
		server.Close()

		client, err := NewClient(Config{
			Enabled:  true,
			BotToken: testToken,
			ChatID:   "-1001234567890",
			Timeout:  time.Second,
			BaseURL:  serverURL,
		})
		if err != nil {
			t.Fatalf("building client: %v", err)
		}

		sendErr := client.SendMessage(context.Background(), "alert text")
		if sendErr == nil {
			t.Fatal("expected a transport error")
		}
		if strings.Contains(sendErr.Error(), testToken) {
			t.Errorf("error leaked the bot token: %v", sendErr)
		}
		if !strings.Contains(sendErr.Error(), redactedToken) {
			t.Errorf("expected the token to be redacted, got: %v", sendErr)
		}
	})

	t.Run("request timeout", func(t *testing.T) {
		// The handler parks until the test releases it, so the client's own
		// timeout is what ends the request. Cleanups run last-registered-first,
		// so registering the release *after* server.Close makes it fire first —
		// which is what lets Close, whose job is to wait on in-flight handlers,
		// return at all.
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-release
		}))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(release) })

		client, err := NewClient(Config{
			Enabled:  true,
			BotToken: testToken,
			ChatID:   "-1001234567890",
			Timeout:  50 * time.Millisecond,
			BaseURL:  server.URL,
		})
		if err != nil {
			t.Fatalf("building client: %v", err)
		}

		sendErr := client.SendMessage(context.Background(), "alert text")
		if sendErr == nil {
			t.Fatal("expected a timeout error")
		}
		if strings.Contains(sendErr.Error(), testToken) {
			t.Errorf("error leaked the bot token: %v", sendErr)
		}
	})
}

// A context cancelled before the call must fail without ever reaching the wire.
func TestSendMessage_HonoursContextCancellation(t *testing.T) {
	// Atomic because the counter is written on the server goroutine and read on
	// the test goroutine. It is zero today only because the request never goes
	// out — which is exactly the assertion, so the regression case is the one
	// that would race.
	var requests atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		okResponse(w)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := client.SendMessage(ctx, "alert text"); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("expected no request to be sent, got %d", got)
	}
}

func TestSendMessage_RejectsEmptyText(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent for empty text")
		okResponse(w)
	})

	for _, text := range []string{"", "   ", "\n\t"} {
		if err := client.SendMessage(context.Background(), text); err != ErrEmptyMessage {
			t.Errorf("text %q: expected ErrEmptyMessage, got: %v", text, err)
		}
	}
}

func TestNewClient(t *testing.T) {
	t.Run("rejects a disabled config", func(t *testing.T) {
		_, err := NewClient(Config{Enabled: false})
		if err != ErrDisabled {
			t.Fatalf("expected ErrDisabled, got: %v", err)
		}
	})

	t.Run("rejects an invalid config", func(t *testing.T) {
		_, err := NewClient(Config{Enabled: true, ChatID: "1", Timeout: time.Second})
		if err != ErrMissingBotToken {
			t.Fatalf("expected ErrMissingBotToken, got: %v", err)
		}
	})

	t.Run("defaults the base URL to the public API", func(t *testing.T) {
		client, err := NewClient(Config{
			Enabled:  true,
			BotToken: testToken,
			ChatID:   "1",
			Timeout:  time.Second,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if client.baseURL != defaultBaseURL {
			t.Errorf("baseURL = %q, want %q", client.baseURL, defaultBaseURL)
		}
	})

	t.Run("trims a trailing slash from the base URL", func(t *testing.T) {
		client, err := NewClient(Config{
			Enabled:  true,
			BotToken: testToken,
			ChatID:   "1",
			Timeout:  time.Second,
			BaseURL:  "https://example.test/",
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if client.baseURL != "https://example.test" {
			t.Errorf("baseURL = %q, want %q", client.baseURL, "https://example.test")
		}
	})
}
