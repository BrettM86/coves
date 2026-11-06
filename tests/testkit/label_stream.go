package testkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/gorilla/websocket"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// A com.atproto.label.subscribeLabels client.
//
// It speaks the atProto event-stream framing (https://atproto.com/specs/event-stream)
// and decodes it independently of the AppView's own framing code: every binary
// message is a CBOR header map {op, t} followed by a CBOR body. op 1 with t
// "#labels" carries {seq, labels}; op -1 (no t) is an error frame {error,
// message} after which the server closes the connection.
//
// One reader goroutine per connection feeds frames to Next in arrival order.
// The client never reconnects and never deduplicates: a test that wants to
// prove "exactly once" across reconnects dials twice and compares what each
// connection delivered.

// ErrLabelStreamClosed is wrapped by Next once the server has ended the
// connection (close frame, EOF, reset). A local timeout or cancellation is
// reported as the context's error instead, never as this.
var ErrLabelStreamClosed = errors.New("label stream closed by the server")

// LabelStreamHeader is a decoded event-stream frame header.
type LabelStreamHeader struct {
	Op int64
	// T is the message type, such as "#labels". HasT reports whether the
	// header carried a t key at all (an error frame must not).
	T    string
	HasT bool
}

// LabelStreamFrame is one message received on a label stream.
type LabelStreamFrame struct {
	// MessageType is the WebSocket message type (websocket.BinaryMessage for
	// every well-formed event-stream frame).
	MessageType int
	Header      LabelStreamHeader

	// Seq and Labels are set for an op 1 "#labels" frame.
	Seq    int64
	Labels []comatproto.LabelDefs_Label

	// Error and Message are set for an op -1 error frame.
	Error   string
	Message string

	// Raw is the whole message as received.
	Raw []byte
}

// LabelStream is one open subscribeLabels connection.
type LabelStream struct {
	conn      *websocket.Conn
	results   chan labelStreamResult
	closed    chan struct{}
	closeOnce sync.Once

	mutex    sync.Mutex
	terminal error
}

type labelStreamResult struct {
	frame LabelStreamFrame
	err   error
}

// LabelStreamHandshakeError is wrapped by DialLabelStream and
// DialLabelStreamConn when the server answered the handshake without
// upgrading, so a test can inspect the refusal with errors.As.
type LabelStreamHandshakeError struct {
	StatusCode int
	// Body is the start of the response body (the XRPC error JSON).
	Body []byte
}

func (e *LabelStreamHandshakeError) Error() string {
	return fmt.Sprintf("handshake refused with HTTP %d: %s", e.StatusCode, e.Body)
}

// DialLabelStream opens <baseURL>/xrpc/com.atproto.label.subscribeLabels, with
// ?cursor=N when cursor is non-nil. It returns once the WebSocket handshake has
// completed. The connection is closed at test cleanup.
func DialLabelStream(t TestingT, ctx context.Context, baseURL string, cursor *int64) (*LabelStream, error) {
	t.Helper()
	conn, err := DialLabelStreamConn(t, ctx, baseURL, cursor)
	if err != nil {
		return nil, err
	}
	stream := &LabelStream{
		conn:    conn,
		results: make(chan labelStreamResult),
		closed:  make(chan struct{}),
	}
	go stream.read()
	t.Cleanup(stream.Close)
	return stream, nil
}

// DialLabelStreamConn opens the same endpoint as DialLabelStream but returns
// the bare WebSocket connection with no reader, for a test that controls the
// connection itself (for example, a subscriber that deliberately stops
// reading). The connection is closed at test cleanup.
func DialLabelStreamConn(t TestingT, ctx context.Context, baseURL string, cursor *int64) (*websocket.Conn, error) {
	t.Helper()
	endpoint, err := url.Parse(strings.TrimRight(baseURL, "/") + "/xrpc/com.atproto.label.subscribeLabels")
	if err != nil {
		return nil, fmt.Errorf("parsing label stream base URL: %w", err)
	}
	switch endpoint.Scheme {
	case "http":
		endpoint.Scheme = "ws"
	case "https":
		endpoint.Scheme = "wss"
	}
	if cursor != nil {
		endpoint.RawQuery = url.Values{"cursor": {strconv.FormatInt(*cursor, 10)}}.Encode()
	}
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint.String(), nil)
	if err != nil {
		if response != nil {
			// gorilla buffers at most the first 1 KiB of a refused handshake's
			// body and needs no Close; an XRPC error body fits.
			body, _ := io.ReadAll(response.Body)
			return nil, fmt.Errorf("dialing %s: %w: %w", endpoint, err,
				&LabelStreamHandshakeError{StatusCode: response.StatusCode, Body: body})
		}
		return nil, fmt.Errorf("dialing %s: %w", endpoint, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, nil
}

// read is the connection's one reader. It hands each message to Next and ends
// when the server ends the connection or Close is called.
func (s *LabelStream) read() {
	defer close(s.results)
	for {
		messageType, data, err := s.conn.ReadMessage()
		if err != nil {
			s.mutex.Lock()
			s.terminal = fmt.Errorf("%w: %v", ErrLabelStreamClosed, err)
			s.mutex.Unlock()
			return
		}
		result := labelStreamResult{frame: LabelStreamFrame{MessageType: messageType, Raw: data}}
		if messageType == websocket.BinaryMessage {
			result.err = decodeLabelStreamFrame(data, &result.frame)
		}
		select {
		case s.results <- result:
		case <-s.closed:
			return
		}
	}
}

// Next returns the next frame. It returns ctx.Err() if ctx ends first, an
// error wrapping ErrLabelStreamClosed once the server has ended the
// connection, or a decoding error for a binary message that is not a valid
// event-stream frame (the stream stays usable after one).
//
// A frame the reader is already holding is returned even when ctx has ended,
// so a probe with an expired context reports whether anything has arrived
// without consuming a timeout as if it were a frame.
func (s *LabelStream) Next(ctx context.Context) (LabelStreamFrame, error) {
	select {
	case result, open := <-s.results:
		return s.deliver(result, open)
	default:
	}
	select {
	case result, open := <-s.results:
		return s.deliver(result, open)
	case <-ctx.Done():
		return LabelStreamFrame{}, ctx.Err()
	}
}

func (s *LabelStream) deliver(result labelStreamResult, open bool) (LabelStreamFrame, error) {
	if !open {
		s.mutex.Lock()
		defer s.mutex.Unlock()
		if s.terminal == nil {
			return LabelStreamFrame{}, fmt.Errorf("%w: closed locally", ErrLabelStreamClosed)
		}
		return LabelStreamFrame{}, s.terminal
	}
	return result.frame, result.err
}

// Close ends the connection from the client side. It is safe to call more
// than once.
func (s *LabelStream) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.conn.Close()
	})
}

// decodeLabelStreamFrame decodes the header and, for the frame kinds the label
// stream defines, the body.
func decodeLabelStreamFrame(data []byte, frame *LabelStreamFrame) error {
	reader := bytes.NewReader(data)
	header, err := decodeLabelStreamHeader(cbg.NewCborReader(reader))
	if err != nil {
		return fmt.Errorf("decoding label stream header: %w", err)
	}
	frame.Header = header
	switch {
	case header.Op == 1 && header.T == "#labels":
		var body comatproto.LabelSubscribeLabels_Labels
		if err := body.UnmarshalCBOR(reader); err != nil {
			return fmt.Errorf("decoding #labels body: %w", err)
		}
		frame.Seq = body.Seq
		for _, label := range body.Labels {
			if label == nil {
				return errors.New("decoding #labels body: null label")
			}
			frame.Labels = append(frame.Labels, *label)
		}
	case header.Op == -1:
		fields, err := decodeStringMap(cbg.NewCborReader(reader))
		if err != nil {
			return fmt.Errorf("decoding error frame body: %w", err)
		}
		frame.Error = fields["error"]
		frame.Message = fields["message"]
	}
	if reader.Len() != 0 && (header.Op == -1 || header.T == "#labels") {
		return fmt.Errorf("%d trailing bytes after the frame body", reader.Len())
	}
	return nil
}

func decodeLabelStreamHeader(reader *cbg.CborReader) (LabelStreamHeader, error) {
	var header LabelStreamHeader
	major, entries, err := reader.ReadHeader()
	if err != nil {
		return header, err
	}
	if major != cbg.MajMap {
		return header, fmt.Errorf("header is CBOR major type %d, want a map", major)
	}
	sawOp := false
	for range entries {
		key, err := cbg.ReadString(reader)
		if err != nil {
			return header, err
		}
		switch key {
		case "op":
			major, value, err := reader.ReadHeader()
			if err != nil {
				return header, err
			}
			switch major {
			case cbg.MajUnsignedInt:
				header.Op = int64(value)
			case cbg.MajNegativeInt:
				header.Op = -1 - int64(value)
			default:
				return header, fmt.Errorf("header op is CBOR major type %d, want an integer", major)
			}
			sawOp = true
		case "t":
			header.T, err = cbg.ReadString(reader)
			if err != nil {
				return header, err
			}
			header.HasT = true
		default:
			return header, fmt.Errorf("unexpected header key %q", key)
		}
	}
	if !sawOp {
		return header, errors.New("header has no op")
	}
	return header, nil
}

// decodeStringMap decodes a CBOR map whose keys and values are all text.
func decodeStringMap(reader *cbg.CborReader) (map[string]string, error) {
	major, entries, err := reader.ReadHeader()
	if err != nil {
		return nil, err
	}
	if major != cbg.MajMap {
		return nil, fmt.Errorf("body is CBOR major type %d, want a map", major)
	}
	fields := make(map[string]string, entries)
	for range entries {
		key, err := cbg.ReadString(reader)
		if err != nil {
			return nil, err
		}
		value, err := cbg.ReadString(reader)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		fields[key] = value
	}
	return fields, nil
}
