package users

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ProfileCollection is the atProto collection holding Coves user profiles.
// The profile record always lives at rkey "self" in the user's own repository.
const ProfileCollection = "social.coves.actor.profile"

// profileRecordRKey is the fixed rkey for profile records (one profile per repo).
const profileRecordRKey = "self"

// maxProfileResponseBytes bounds how much of a PDS getRecord response we read.
// A legitimate profile record is well under this; the cap protects against a
// misbehaving or malicious PDS streaming an unbounded body.
const maxProfileResponseBytes = 1 << 20 // 1 MiB

// Field caps mirror the users table CHECK constraints (migration 027:
// display_name <= 64, bio <= 256 — Postgres length() counts characters, so
// these are rune counts, not bytes). Overlong values from an untrusted PDS are
// truncated rather than rejected: a truncated profile indexes fine, while an
// over-limit UPDATE would fail the constraint forever on every re-index.
const (
	maxDisplayNameChars = 64
	maxBioChars         = 256
)

// maxBlobCIDLength caps the accepted length of a blob ref's $link. Real CIDs
// are ~59 chars (CIDv1 base32); anything past this is not a plausible CID.
const maxBlobCIDLength = 256

// FetchProfileRecord fetches a user's social.coves.actor.profile/self record directly
// from their PDS via com.atproto.repo.getRecord and converts it to an UpdateProfileInput.
//
// This is the reconciliation path for profile events the firehose never delivered
// (a profile record is written once at rkey "self", so its create event fires exactly
// once — if it was missed, nothing ever replays it). Returns (nil, nil) when the user
// has no profile record or the record carries no profile fields — "nothing to apply"
// is a normal outcome, not an error.
func FetchProfileRecord(ctx context.Context, client *http.Client, pdsURL, did string) (*UpdateProfileInput, error) {
	if client == nil {
		return nil, fmt.Errorf("http client is required")
	}
	if strings.TrimSpace(pdsURL) == "" {
		return nil, fmt.Errorf("PDS URL is required")
	}
	if strings.TrimSpace(did) == "" {
		return nil, fmt.Errorf("DID is required")
	}

	endpoint := strings.TrimSuffix(pdsURL, "/") + "/xrpc/com.atproto.repo.getRecord?repo=" +
		url.QueryEscape(did) + "&collection=" + ProfileCollection + "&rkey=" + profileRecordRKey

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create getRecord request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch profile record from PDS: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read getRecord response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		// fall through to parse below
	case IsRecordNotFoundResponse(resp.StatusCode, body):
		// The PDS was reached and said the record is not there. Absence is a
		// normal outcome here, not an error.
		return nil, nil
	default:
		// SECURITY: cap the echoed body so a hostile PDS can't flood our logs,
		// and quote it so control chars / ANSI escapes can't corrupt log output.
		detail := string(body)
		if len(detail) > 256 {
			detail = detail[:256]
		}
		return nil, fmt.Errorf("PDS getRecord returned status %d: %s", resp.StatusCode, strconv.Quote(detail))
	}

	var parsed struct {
		Value map[string]interface{} `json:"value"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse getRecord response: %w", err)
	}
	if parsed.Value == nil {
		return nil, fmt.Errorf("getRecord response missing record value")
	}

	input := parseProfileRecord(parsed.Value)
	if input.DisplayName == nil && input.Bio == nil && input.AvatarCID == nil && input.BannerCID == nil {
		return nil, nil // record exists but carries nothing we index
	}
	return &input, nil
}

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

// parseProfileRecord extracts indexable profile fields from a social.coves.actor.profile
// record value. Field extraction mirrors the Jetstream profile consumer
// (internal/atproto/jetstream/user_consumer.go handleProfileUpdate): only fields present
// in the record are set, and avatar/banner must be well-formed blob refs.
// SECURITY: the record comes from an untrusted PDS — displayName/description are
// truncated to the DB CHECK constraint limits so an overlong value indexes
// (truncated) instead of failing the UPDATE forever on every re-index.
func parseProfileRecord(record map[string]interface{}) UpdateProfileInput {
	input := UpdateProfileInput{}

	if displayName, ok := record["displayName"].(string); ok {
		displayName = truncateRunes(displayName, maxDisplayNameChars)
		input.DisplayName = &displayName
	}
	if description, ok := record["description"].(string); ok {
		description = truncateRunes(description, maxBioChars)
		input.Bio = &description
	}
	if avatar, ok := record["avatar"].(map[string]interface{}); ok {
		if cid, ok := extractProfileBlobCID(avatar); ok {
			input.AvatarCID = &cid
		}
	}
	if banner, ok := record["banner"].(map[string]interface{}); ok {
		if cid, ok := extractProfileBlobCID(banner); ok {
			input.BannerCID = &cid
		}
	}

	return input
}

// extractProfileBlobCID pulls the CID out of an atProto blob ref
// ({"$type":"blob","ref":{"$link":"<cid>"},...}). Returns false for anything
// that is not a well-formed blob ref or whose $link is not a plausible CID.
func extractProfileBlobCID(blob map[string]interface{}) (string, bool) {
	blobType, ok := blob["$type"].(string)
	if !ok || blobType != "blob" {
		return "", false
	}
	ref, ok := blob["ref"].(map[string]interface{})
	if !ok {
		return "", false
	}
	link, ok := ref["$link"].(string)
	if !ok || !isPlausibleCID(link) {
		return "", false
	}
	return link, true
}

// isPlausibleCID reports whether link looks like a CID: non-empty, bounded
// length, and restricted to the alphanumeric charset of base32/base58 CID
// encodings. This is a sanity gate on untrusted PDS output, not full CID
// validation — it keeps arbitrary strings out of the avatar/banner columns.
func isPlausibleCID(link string) bool {
	if link == "" || len(link) > maxBlobCIDLength {
		return false
	}
	for _, c := range link {
		isAlphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlphanumeric {
			return false
		}
	}
	return true
}

// truncateRunes returns s truncated to at most maxRunes characters without
// splitting a multi-byte rune (the DB constraints count characters, so the cut
// must be by runes, not bytes).
func truncateRunes(s string, maxRunes int) string {
	if len(s) <= maxRunes {
		return s // byte length within the cap implies rune count is too
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i]
		}
		count++
	}
	return s
}
