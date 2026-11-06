package testkit

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// writeCborText writes one CBOR text string.
func writeCborText(t *testing.T, writer *cbg.CborWriter, value string) {
	t.Helper()
	require.NoError(t, writer.WriteMajorTypeHeader(cbg.MajTextString, uint64(len(value))))
	_, err := writer.WriteString(value)
	require.NoError(t, err)
}

// TestLabelStreamClientDecodesFramesAndPeerClose drives the client against an
// in-process server that sends a #labels frame, an error frame, then closes:
// the client must decode both frames and then report peer termination, while a
// local deadline with nothing pending reports the context's error.
func TestLabelStreamClientDecodesFramesAndPeerClose(t *testing.T) {
	var labelsFrame bytes.Buffer
	writer := cbg.NewCborWriter(&labelsFrame)
	require.NoError(t, writer.WriteMajorTypeHeader(cbg.MajMap, 2))
	writeCborText(t, writer, "op")
	require.NoError(t, writer.WriteMajorTypeHeader(cbg.MajUnsignedInt, 1))
	writeCborText(t, writer, "t")
	writeCborText(t, writer, "#labels")
	body := comatproto.LabelSubscribeLabels_Labels{Seq: 8, Labels: []*comatproto.LabelDefs_Label{{
		Src: "did:plc:labeler", Uri: "at://did:plc:author/app.bsky.feed.post/one", Val: "!takedown", Cts: "2026-10-22T00:00:00.000Z",
	}}}
	require.NoError(t, body.MarshalCBOR(&labelsFrame))

	var errorFrame bytes.Buffer
	writer = cbg.NewCborWriter(&errorFrame)
	require.NoError(t, writer.WriteMajorTypeHeader(cbg.MajMap, 1))
	writeCborText(t, writer, "op")
	require.NoError(t, writer.WriteMajorTypeHeader(cbg.MajNegativeInt, 0))
	require.NoError(t, writer.WriteMajorTypeHeader(cbg.MajMap, 2))
	writeCborText(t, writer, "error")
	writeCborText(t, writer, "FutureCursor")
	writeCborText(t, writer, "message")
	writeCborText(t, writer, "cursor is ahead")

	finish := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "7", r.URL.Query().Get("cursor"))
		conn, err := upgrader.Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer func() { _ = conn.Close() }()
		assert.NoError(t, conn.WriteMessage(websocket.BinaryMessage, labelsFrame.Bytes()))
		<-finish
		assert.NoError(t, conn.WriteMessage(websocket.BinaryMessage, errorFrame.Bytes()))
	}))
	t.Cleanup(server.Close)
	var finishOnce sync.Once
	release := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(release) // runs before server.Close, which waits for the handler

	cursor := int64(7)
	stream, err := DialLabelStream(t, t.Context(), server.URL, &cursor)
	require.NoError(t, err)

	frame, err := stream.Next(t.Context())
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, frame.MessageType)
	assert.Equal(t, LabelStreamHeader{Op: 1, T: "#labels", HasT: true}, frame.Header)
	assert.EqualValues(t, 8, frame.Seq)
	require.Len(t, frame.Labels, 1)
	assert.Equal(t, "at://did:plc:author/app.bsky.feed.post/one", frame.Labels[0].Uri)

	waiting, cancelWaiting := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelWaiting()
	_, err = stream.Next(waiting)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a local timeout must not read as peer termination")
	assert.False(t, errors.Is(err, ErrLabelStreamClosed))
	release()

	frame, err = stream.Next(t.Context())
	require.NoError(t, err)
	assert.Equal(t, LabelStreamHeader{Op: -1}, frame.Header)
	assert.Equal(t, "FutureCursor", frame.Error)
	assert.Equal(t, "cursor is ahead", frame.Message)

	_, err = stream.Next(t.Context())
	assert.ErrorIs(t, err, ErrLabelStreamClosed)
}
