package pds

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	boundedTestMaxBytes = 4096
	// boundedReadSlack is how far past maxBytes the client may read from the
	// response body: the single probe byte that tells "exactly the cap" from
	// "over the cap".
	boundedReadSlack = 1
	boundedTestToken = "bounded-test-token"
	boundedTestDID   = "did:plc:boundedreader1"
	boundedTestURI   = "at://did:plc:boundedreader1/app.bsky.actor.profile/self"
	boundedTestCID   = "bafyreiboundedrecord00000000000000000000000000000000000000"
)

// countingTransport counts the response-body bytes the client actually reads.
type countingTransport struct {
	base http.RoundTripper
	read *atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &countingBody{ReadCloser: resp.Body, read: c.read}
	return resp, nil
}

type countingBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// paddedRecordBody returns a getRecord response body of exactly size bytes:
// a valid record whose "pad" property absorbs the remainder.
func paddedRecordBody(t *testing.T, size int) (string, string) {
	t.Helper()
	prefix := `{"uri":"` + boundedTestURI + `","cid":"` + boundedTestCID + `","value":{"$type":"app.bsky.actor.profile","pad":"`
	suffix := `"}}`
	padding := size - len(prefix) - len(suffix)
	require.Positive(t, padding, "the fixture must fit inside the size it is testing")
	pad := strings.Repeat("a", padding)
	body := prefix + pad + suffix
	require.Len(t, body, size)
	return body, pad
}

func TestClient_GetRecordBounded(t *testing.T) {
	exactBody, exactPad := paddedRecordBody(t, boundedTestMaxBytes)
	overBody, _ := paddedRecordBody(t, boundedTestMaxBytes+1)

	writeBody := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}

	// endless streams a record that never closes, until the client hangs up.
	endless := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"uri":"`+boundedTestURI+`","cid":"`+boundedTestCID+`","value":{"pad":"`)
		chunk := []byte(strings.Repeat("a", 1024))
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}

	tests := []struct {
		name string
		// serverToken is the bearer token the server accepts.
		serverToken string
		handler     http.HandlerFunc
		wantErrIs   error
		wantErr     bool
		wantRecord  bool
		// wantMalformed requires the error to wrap ErrMalformedResponse, the
		// upstream-fault class the handlers answer with 502.
		wantMalformed bool
		// check makes further assertions on the returned error.
		check func(t *testing.T, err error)
	}{
		{
			name:        "body of exactly maxBytes parses",
			serverToken: boundedTestToken,
			handler:     writeBody(http.StatusOK, exactBody),
			wantRecord:  true,
		},
		{
			name:          "body one byte over maxBytes is an error",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusOK, overBody),
			wantErr:       true,
			wantMalformed: true,
		},
		{
			name:          "endless body is an error and is not consumed past the cap",
			serverToken:   boundedTestToken,
			handler:       endless,
			wantErr:       true,
			wantMalformed: true,
		},
		{
			name:        "server requiring another token answers 401",
			serverToken: "some-other-token",
			handler:     writeBody(http.StatusOK, exactBody),
			wantErrIs:   ErrUnauthorized,
		},
		{
			name:        "RecordNotFound maps to ErrNotFound",
			serverToken: boundedTestToken,
			handler:     writeBody(http.StatusBadRequest, `{"error":"RecordNotFound","message":"Could not locate record"}`),
			wantErrIs:   ErrNotFound,
		},
		{
			name:        "InvalidRequest saying it could not locate the record maps to ErrNotFound",
			serverToken: boundedTestToken,
			handler:     writeBody(http.StatusBadRequest, `{"error":"InvalidRequest","message":"Could not locate record: at://did:plc:x/app.bsky.actor.profile/self"}`),
			wantErrIs:   ErrNotFound,
		},
		{
			name:        "other InvalidRequest maps to ErrBadRequest, not ErrNotFound",
			serverToken: boundedTestToken,
			handler:     writeBody(http.StatusBadRequest, `{"error":"InvalidRequest","message":"bad collection"}`),
			wantErrIs:   ErrBadRequest,
		},
		{
			name:        "403 maps to ErrForbidden",
			serverToken: boundedTestToken,
			handler:     writeBody(http.StatusForbidden, `{"error":"Forbidden","message":"no access"}`),
			wantErrIs:   ErrForbidden,
		},
		{
			name:          "2xx without value is an error, not an empty record",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusOK, `{"uri":"`+boundedTestURI+`","cid":"`+boundedTestCID+`"}`),
			wantErr:       true,
			wantMalformed: true,
		},
		{
			name:          "2xx with null value is an error, not an empty record",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusOK, `{"uri":"`+boundedTestURI+`","cid":"`+boundedTestCID+`","value":null}`),
			wantErr:       true,
			wantMalformed: true,
		},
		{
			name:          "malformed 2xx body under the cap is a decode error",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusOK, `{"uri":`),
			wantErr:       true,
			wantMalformed: true,
			check: func(t *testing.T, err error) {
				assert.ErrorContains(t, err, "decoding response body")
			},
		},
		{
			name:          "decode error text from an overflowing number stays capped",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusOK, `{"uri":"`+boundedTestURI+`","cid":"`+boundedTestCID+`","value":{"n":1`+strings.Repeat("9", 1024)+`}}`),
			wantErr:       true,
			wantMalformed: true,
			check: func(t *testing.T, err error) {
				assert.ErrorContains(t, err, "decoding response body")
				assert.LessOrEqual(t, len(err.Error()), 2*maxPDSErrorDetailBytes)
			},
		},
		{
			name:          "bare 404 with a non-JSON body is not ErrNotFound",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusNotFound, `<html>404 page not found</html>`),
			wantErr:       true,
			wantMalformed: true,
		},
		{
			name:          "bare 404 with an empty body is not ErrNotFound",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusNotFound, ``),
			wantErr:       true,
			wantMalformed: true,
		},
		{
			name:          "bare 404 with JSON but no error name is not ErrNotFound",
			serverToken:   boundedTestToken,
			handler:       writeBody(http.StatusNotFound, `{"message":"no route"}`),
			wantErr:       true,
			wantMalformed: true,
		},
		{
			name:        "404 RecordNotFound maps to ErrNotFound",
			serverToken: boundedTestToken,
			handler:     writeBody(http.StatusNotFound, `{"error":"RecordNotFound","message":"Could not locate record"}`),
			wantErrIs:   ErrNotFound,
		},
		{
			name:        "PDS message on a mapped status is capped",
			serverToken: boundedTestToken,
			handler: writeBody(http.StatusForbidden,
				`{"error":"Forbidden","message":"`+strings.Repeat("m", boundedTestMaxBytes/2)+`"}`),
			wantErrIs: ErrForbidden,
			check: func(t *testing.T, err error) {
				assertCappedRun(t, err, "m")
			},
		},
		{
			name:        "PDS name and message on an unmapped status are capped",
			serverToken: boundedTestToken,
			handler: writeBody(http.StatusTeapot,
				`{"error":"`+strings.Repeat("n", boundedTestMaxBytes/4)+`","message":"`+strings.Repeat("m", boundedTestMaxBytes/4)+`"}`),
			wantErr: true,
			check: func(t *testing.T, err error) {
				assertCappedRun(t, err, "n")
				assertCappedRun(t, err, "m")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sawAuth atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/xrpc/com.atproto.repo.getRecord" ||
					r.URL.Query().Get("repo") != boundedTestDID ||
					r.URL.Query().Get("collection") != "app.bsky.actor.profile" ||
					r.URL.Query().Get("rkey") != "self" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":"InvalidRequest","message":"unexpected request"}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer "+tt.serverToken {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":"AuthenticationRequired","message":"bad token"}`)
					return
				}
				sawAuth.Store(true)
				tt.handler(w, r)
			}))
			t.Cleanup(server.Close)

			created, err := NewFromAccessToken(server.URL, boundedTestDID, boundedTestToken, WithPrivateHostsAllowed())
			require.NoError(t, err)
			c, ok := created.(*client)
			require.True(t, ok)
			var read atomic.Int64
			c.apiClient.Client.Transport = &countingTransport{base: c.apiClient.Client.Transport, read: &read}

			// The deadline is a watchdog only: reaching it fails the test.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := c.GetRecordBounded(ctx, "app.bsky.actor.profile", "self", boundedTestMaxBytes)

			require.NotErrorIs(t, err, context.DeadlineExceeded, "the bounded read did not return before the watchdog")

			switch {
			case tt.wantRecord:
				require.NoError(t, err)
				require.NotNil(t, resp, "a body of exactly maxBytes must parse into a record")
				assert.Equal(t, boundedTestURI, resp.URI)
				assert.Equal(t, boundedTestCID, resp.CID)
				assert.Equal(t, "app.bsky.actor.profile", resp.Value["$type"])
				assert.Equal(t, exactPad, resp.Value["pad"])
				assert.True(t, sawAuth.Load())
			case tt.wantErrIs != nil:
				require.Error(t, err)
				assert.Nil(t, resp)
				assert.ErrorIs(t, err, tt.wantErrIs)
			case tt.wantErr:
				require.Error(t, err)
				assert.Nil(t, resp, "a failed read must not return a partial record")
				assert.False(t, errors.Is(err, ErrNotFound), "this failure must not read as a missing record")
				assert.Equal(t, tt.wantMalformed, errors.Is(err, ErrMalformedResponse), "ErrMalformedResponse classification")
				assert.LessOrEqual(t, read.Load(), int64(boundedTestMaxBytes+boundedReadSlack),
					"the client read past maxBytes plus the probe byte")
			}
			if tt.check != nil {
				tt.check(t, err)
			}
		})
	}
}

// assertCappedRun asserts err carries exactly maxPDSErrorDetailBytes of a text
// repeated from char: the run is present up to the cap and stops there.
func assertCappedRun(t *testing.T, err error, char string) {
	t.Helper()
	require.Error(t, err)
	assert.Contains(t, err.Error(), strings.Repeat(char, maxPDSErrorDetailBytes))
	assert.NotContains(t, err.Error(), strings.Repeat(char, maxPDSErrorDetailBytes+1))
}
