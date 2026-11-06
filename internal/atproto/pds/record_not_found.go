package pds

import (
	"encoding/json"
	"net/http"
	"strings"
)

// IsRecordNotFoundResponse reports whether a getRecord response is a PDS
// saying, definitively, that the record is not there.
//
// It is exported because the answer decides how OTHER callers classify a failed
// fetch, and there must be exactly one line drawn. The firehose consumer's §5.4
// direct fetch marks a genuine not-found as a PERMANENT event — dead-lettered
// with its redrive budget spent — while everything else stays transient and
// costs the connector three inline retries plus ten redrives. Two
// implementations of "is this a real not-found" would eventually disagree, and
// the disagreement would show up as either discarded posts or a blocked lane.
//
// THE TWO SHAPES IT ACCEPTS, and the one it deliberately does not:
//
//   - 400 with an XRPC RecordNotFound (or an InvalidRequest whose message says
//     it could not locate the record) — what the reference PDS answers when the
//     repo exists and the record does not.
//   - 404 WITH an XRPC error envelope — what some other implementations answer.
//   - NOT a bare 404. With no envelope the request most likely never reached a
//     PDS at all: a stale pds_url pointing at a reverse proxy or a generic web
//     server, both of which answer 404 for everything. Trusting that would
//     report a record as gone because somebody mistyped a hostname.
func IsRecordNotFoundResponse(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusNotFound:
		return isXRPCErrorBody(body)
	case http.StatusBadRequest:
		return isRecordNotFoundBody(body)
	default:
		return false
	}
}

// isXRPCErrorBody reports whether body is an XRPC error JSON object (has a
// non-empty "error" field). Used to distinguish a real PDS not-found response
// from a bare 404 served by whatever non-PDS host a stale pds_url points at.
func isXRPCErrorBody(body []byte) bool {
	var xrpcErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &xrpcErr); err != nil {
		return false
	}
	return xrpcErr.Error != ""
}

// isRecordNotFoundBody reports whether an XRPC error body is the reference PDS's
// "record not found" rejection (as opposed to some other 400-class failure).
// The message-substring match is only trusted on the error codes the reference
// PDS actually uses for missing records (RecordNotFound, InvalidRequest) — an
// arbitrary error that merely mentions "could not locate record" is not proof
// the record is missing.
func isRecordNotFoundBody(body []byte) bool {
	var xrpcErr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &xrpcErr); err != nil {
		return false
	}
	if xrpcErr.Error == "RecordNotFound" {
		return true
	}
	if xrpcErr.Error != "InvalidRequest" {
		return false
	}
	return strings.Contains(strings.ToLower(xrpcErr.Message), "could not locate record")
}
