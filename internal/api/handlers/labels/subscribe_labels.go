package labels

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"Coves/internal/api/xrpc"
	"Coves/internal/core/labels"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/gorilla/websocket"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// subscriberReadLimit bounds a message from a subscriber, which has nothing
// to say on this stream.
const subscriberReadLimit = 1024

// closeFrameTimeout bounds writing the final close frame to a subscriber.
const closeFrameTimeout = time.Second

// Production defaults for a subscription's connection timeouts.
const (
	defaultWriteTimeout = 10 * time.Second
	defaultPingInterval = 30 * time.Second
	// defaultPongWait exceeds defaultPingInterval, so a peer that answers
	// every ping always extends its read deadline before it passes.
	defaultPongWait = 60 * time.Second
)

// labelStreamUpgrader accepts any origin: the stream is public and read-only.
var labelStreamUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// SubscribeLabelsOption configures NewSubscribeLabelsHandler.
type SubscribeLabelsOption func(*subscribeLabelsConfig)

type subscribeLabelsConfig struct {
	writeTimeout time.Duration
	pingInterval time.Duration
	pongWait     time.Duration
}

// WithStreamTimeouts sets the per-frame write timeout, the ping interval and
// the pong wait after which a silent peer is dropped. Zero values select
// production defaults. pongWait should exceed pingInterval, or a peer that
// answers every ping is still dropped.
func WithStreamTimeouts(writeTimeout, pingInterval, pongWait time.Duration) SubscribeLabelsOption {
	return func(config *subscribeLabelsConfig) {
		if writeTimeout > 0 {
			config.writeTimeout = writeTimeout
		}
		if pingInterval > 0 {
			config.pingInterval = pingInterval
		}
		if pongWait > 0 {
			config.pongWait = pongWait
		}
	}
}

// NewSubscribeLabelsHandler serves com.atproto.label.subscribeLabels from
// stream. Every frame write has a deadline, the peer is pinged every ping
// interval, and a peer silent for the pong wait is dropped, so a stalled or
// departed peer frees its subscription slot.
func NewSubscribeLabelsHandler(stream *labels.Stream, options ...SubscribeLabelsOption) http.Handler {
	config := subscribeLabelsConfig{
		writeTimeout: defaultWriteTimeout,
		pingInterval: defaultPingInterval,
		pongWait:     defaultPongWait,
	}
	for _, option := range options {
		option(&config)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var cursor *int64
		if rawCursor := r.URL.Query().Get("cursor"); rawCursor != "" {
			parsed, err := strconv.ParseInt(rawCursor, 10, 64)
			if err != nil || parsed < 0 {
				xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest", "cursor must be a label sequence number")
				return
			}
			cursor = &parsed
		}

		subscription, err := stream.Subscribe(r.Context(), cursor)
		futureCursor := errors.Is(err, labels.ErrFutureCursor)
		switch {
		case err == nil, futureCursor:
		case errors.Is(err, labels.ErrStreamClosed):
			xrpc.WriteError(w, http.StatusServiceUnavailable, "Unavailable", "The label stream is shutting down")
			return
		case errors.Is(err, labels.ErrStreamFull):
			xrpc.WriteError(w, http.StatusServiceUnavailable, "Unavailable", "The label stream is at its subscriber limit")
			return
		default:
			slog.Error("subscribeLabels failed", slog.String("error", err.Error()))
			xrpc.WriteError(w, http.StatusInternalServerError, "InternalServerError", "An internal error occurred")
			return
		}

		conn, err := labelStreamUpgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade has already answered the request.
			if subscription != nil {
				subscription.Close()
			}
			return
		}
		defer func() { _ = conn.Close() }()

		if futureCursor {
			if err := conn.SetWriteDeadline(time.Now().Add(config.writeTimeout)); err != nil {
				return
			}
			if err := writeErrorFrame(conn, "FutureCursor", "cursor is ahead of the latest label sequence"); err == nil {
				writeCloseFrame(conn, websocket.CloseNormalClosure)
			}
			return
		}

		handlerDone := make(chan struct{})
		defer close(handlerDone)
		go discardUntilClosed(conn, subscription, config.pongWait)
		go pingUntilDone(conn, config, handlerDone)
		writeFailed := false
		err = subscription.Run(func(label labels.Label) error {
			if err := conn.SetWriteDeadline(time.Now().Add(config.writeTimeout)); err != nil {
				writeFailed = true
				return err
			}
			if err := writeLabelsFrame(conn, label); err != nil {
				writeFailed = true
				return err
			}
			return nil
		}, conn)
		if err != nil && !writeFailed {
			slog.Error("subscribeLabels stream failed", slog.String("error", err.Error()))
			writeCloseFrame(conn, websocket.CloseInternalServerErr)
		}
	})
}

// discardUntilClosed reads the subscriber's side of the connection, so control
// frames are answered and a departed subscriber ends its subscription. A peer
// that sends no pong for pongWait fails the read; other frames do not extend
// the deadline. It closes conn too, so a write blocked on a subscriber that
// stopped reading is released.
func discardUntilClosed(conn *websocket.Conn, subscription *labels.Subscription, pongWait time.Duration) {
	conn.SetReadLimit(subscriberReadLimit)
	if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		_ = conn.Close()
		subscription.Close()
		return
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		if _, _, err := conn.NextReader(); err != nil {
			_ = conn.Close()
			subscription.Close()
			return
		}
	}
}

// pingUntilDone pings the subscriber every ping interval until done is closed.
// A ping that cannot be written closes conn, which ends the read loop and the
// subscription.
func pingUntilDone(conn *websocket.Conn, config subscribeLabelsConfig, done <-chan struct{}) {
	ticker := time.NewTicker(config.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(config.writeTimeout)); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func writeCloseFrame(conn *websocket.Conn, code int) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""),
		time.Now().Add(closeFrameTimeout))
}

// writeLabelsFrame writes one event-stream message: the header {t: "#labels",
// op: 1} followed by the #labels body carrying label.
func writeLabelsFrame(conn *websocket.Conn, label labels.Label) error {
	var message bytes.Buffer
	writer := cbg.NewCborWriter(&message)
	if err := writer.WriteMajorTypeHeader(cbg.MajMap, 2); err != nil {
		return err
	}
	if err := writeCBORText(writer, "t"); err != nil {
		return err
	}
	if err := writeCBORText(writer, "#labels"); err != nil {
		return err
	}
	if err := writeCBORText(writer, "op"); err != nil {
		return err
	}
	if err := writer.WriteMajorTypeHeader(cbg.MajUnsignedInt, 1); err != nil {
		return err
	}
	lexicon := label.Lexicon()
	body := comatproto.LabelSubscribeLabels_Labels{Seq: label.Seq, Labels: []*comatproto.LabelDefs_Label{&lexicon}}
	if err := body.MarshalCBOR(&message); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.BinaryMessage, message.Bytes())
}

// writeErrorFrame writes an event-stream error message: the header {op: -1}
// followed by {error, message}.
func writeErrorFrame(conn *websocket.Conn, errorName, errorMessage string) error {
	var message bytes.Buffer
	writer := cbg.NewCborWriter(&message)
	if err := writer.WriteMajorTypeHeader(cbg.MajMap, 1); err != nil {
		return err
	}
	if err := writeCBORText(writer, "op"); err != nil {
		return err
	}
	// -1 is CBOR negative integer 0.
	if err := writer.WriteMajorTypeHeader(cbg.MajNegativeInt, 0); err != nil {
		return err
	}
	if err := writer.WriteMajorTypeHeader(cbg.MajMap, 2); err != nil {
		return err
	}
	for _, text := range []string{"error", errorName, "message", errorMessage} {
		if err := writeCBORText(writer, text); err != nil {
			return err
		}
	}
	return conn.WriteMessage(websocket.BinaryMessage, message.Bytes())
}

func writeCBORText(writer *cbg.CborWriter, text string) error {
	if err := writer.WriteMajorTypeHeader(cbg.MajTextString, uint64(len(text))); err != nil {
		return err
	}
	_, err := writer.WriteString(text)
	return err
}
