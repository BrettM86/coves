//go:build integration

// Package fixtures is the domain-shaped test support the integration tier
// shares: rows in a test database, and the OAuth fakes that let an in-process
// router authenticate a request.
//
// # WHY IT IS NOT IN tests/testkit
//
// docs/TEST_ARCHITECTURE.md §3.3 gives the kit a hard dependency rule — it
// imports NO internal/core package — because T1 tests are in-package: if the
// kit imported internal/core/users, then users' own test file importing the kit
// would be an import cycle. Everything here breaks that rule on purpose (it
// returns *users.User, builds a votes.PDSClientFactory, wraps
// middleware.OAuthAuthMiddleware), which is exactly the case the spec sends to
// "small leaf packages": nothing imports this, so it can import anything.
//
// The corollary is a rule for its callers, and it is not optional: a test file
// that imports this package must be in an EXTERNAL test package (package
// foo_test, not package foo). An in-package test of internal/api/middleware
// importing fixtures — which imports internal/api/middleware — is a cycle the
// compiler will refuse.
//
// # WHY IT EXISTS AT ALL
//
// It is what survived tests/integration/helpers.go when phase 4 dissolved that
// directory (docs/TEST_ARCHITECTURE.md §3.1). Of that file's 38 declarations,
// nine had no callers at all, and most of the rest had been superseded by the
// kit and were kept alive only by the catch-all package they lived in:
//
//	getTestPDSURL, getTestPLCURL  →  testkit.Endpoints()  (§3.7: no test
//	                                 constructs a base URL)
//	uniqueTestID, uniqueID        →  testkit.UniqueID
//	generateTID                   →  testkit.TID (which emits a REAL TID;
//	                                 the old one never did)
//	createPDSAccount              →  testkit.PDS.CreateAccount
//	writePDSRecord                →  testkit.Account.CreateRecord
//	contains                      →  strings.Contains
//
// What is left is the part the kit may not hold: SQL that knows the schema, and
// fakes that know the domain's interfaces.
package fixtures

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
	"Coves/tests/testkit"

	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// DID builds a syntactically valid did:plc string for a row that never has to
// resolve.
//
// Repo and handler tests need a DID-shaped foreign key, not an identity: the
// PLC directory is never consulted, so registering one would cost a network
// round trip to prove nothing. Tests that DO need a resolvable identity create
// a real account through testkit.PDS instead.
//
// Note for anyone tempted to tighten this: a real did:plc suffix is 24 base32
// characters, and these are not. Nothing in the AppView validates that length
// today (a phase-5 note in loop_state records it), so a fixture DID is accepted
// everywhere — but a validator added later would reject these before it
// rejected anything a user could write.
func DID(suffix string) string {
	return fmt.Sprintf("did:plc:test%s", suffix)
}

// User inserts a user row directly, bypassing the service.
//
// Direct SQL rather than users.Service.IndexUser because the tests that call
// this need a user to EXIST — as the author of a post, the subject of a vote —
// and not to exercise indexing. Going through the service would couple every
// one of them to identity resolution and its network calls.
func User(t *testing.T, db *sql.DB, handle, did string) *users.User {
	t.Helper()

	user := &users.User{}
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO users (did, handle, pds_url, created_at, updated_at)
		VALUES ($1, $2, $3, NOW(), NOW())
		RETURNING did, handle, pds_url, created_at, updated_at
	`, did, handle, testkit.Endpoints().PDS.BaseURL).
		Scan(&user.DID, &user.Handle, &user.PDSURL, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		t.Fatalf("inserting test user %s (%s): %v", handle, did, err)
	}
	return user
}

// Community inserts a community row and the owner user it references.
//
// Both rows are upserts (ON CONFLICT DO NOTHING) because a test that builds
// several communities under one owner would otherwise fail on the second.
// Returns the community DID, which is what callers hang posts off.
func Community(ctx context.Context, db *sql.DB, name, ownerHandle string) (string, error) {
	pdsURL := testkit.Endpoints().PDS.BaseURL

	ownerDID := fmt.Sprintf("did:plc:%s", ownerHandle)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (did) DO NOTHING
	`, ownerDID, ownerHandle, pdsURL); err != nil {
		return "", fmt.Errorf("inserting community owner %s: %w", ownerHandle, err)
	}

	communityDID := fmt.Sprintf("did:plc:community-%s", name)
	_, err := db.ExecContext(ctx, `
		INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (did) DO NOTHING
	`, communityDID, name, ownerDID, ownerDID, InstanceDID(), fmt.Sprintf("%s.coves.social", name), pdsURL)
	if err != nil {
		return "", fmt.Errorf("inserting community %s: %w", name, err)
	}
	return communityDID, nil
}

// InstanceDID is the hosting instance's DID as the tests see it.
//
// Read from the environment the AppView reads it from, with the same default
// the config package uses, so a fixture community is hosted by whoever the
// process under test believes it is.
func InstanceDID() string {
	if did := os.Getenv("INSTANCE_DID"); did != "" {
		return did
	}
	return "did:web:test.coves.social"
}

// Post inserts a post row with vote counts derived from score.
//
// The derivation is the point: score = upvotes - downvotes is an invariant the
// serving queries rely on, and a fixture that set score alone would produce
// rows no consumer could ever have written — feeds sorted by one field and
// rendered from another.
func Post(t *testing.T, db *sql.DB, communityDID, authorDID, title string, score int, createdAt time.Time) string {
	t.Helper()

	ctx := context.Background()
	// The author may already exist; a post fixture should not care.
	_, _ = db.ExecContext(ctx, `
		INSERT INTO users (did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (did) DO NOTHING
	`, authorDID, fmt.Sprintf("%s.bsky.social", authorDID), testkit.Endpoints().PDS.BaseURL)

	rkey := testkit.TID()
	uri := fmt.Sprintf("at://%s/social.coves.community.post/%s", communityDID, rkey)

	upvotes, downvotes := score, 0
	if score < 0 {
		upvotes, downvotes = 0, -score
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, uri, "bafytest", rkey, authorDID, communityDID, title, createdAt, score, upvotes, downvotes); err != nil {
		t.Fatalf("inserting test post %q in %s: %v", title, communityDID, err)
	}
	return uri
}

// ---------------------------------------------------------------------------
// OAuth fakes
// ---------------------------------------------------------------------------

// The AppView authenticates a request by unsealing a session token and looking
// the session up in the OAuth store (internal/api/middleware). Both halves are
// interfaces, and these are the in-memory implementations that let a T1 test
// drive an authenticated route without a browser authorization-code flow
// against the PDS' login pages.
//
// This is the same limitation tests/e2e documents from the other side: the
// pipeline tier CANNOT authenticate a write (§3.4b's standing note), because
// substituting these fakes means running the router in-process, which T2 is not
// allowed to do. Authenticated write behaviour is therefore proven here, at T1,
// and the pipeline tier proves the auth boundary instead.

// SessionUnsealer resolves bearer tokens to sealed sessions from a map.
type SessionUnsealer struct {
	sessions map[string]*oauth.SealedSession
}

// NewSessionUnsealer creates an empty unsealer.
func NewSessionUnsealer() *SessionUnsealer {
	return &SessionUnsealer{sessions: make(map[string]*oauth.SealedSession)}
}

// AddSession registers a token for a DID, valid for an hour.
func (u *SessionUnsealer) AddSession(token, did, sessionID string) {
	u.sessions[token] = &oauth.SealedSession{
		DID:       did,
		SessionID: sessionID,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
}

// UnsealSession implements the middleware's unsealer interface.
func (u *SessionUnsealer) UnsealSession(token string) (*oauth.SealedSession, error) {
	if session, ok := u.sessions[token]; ok {
		return session, nil
	}
	return nil, fmt.Errorf("unknown token")
}

// OAuthStore is an in-memory oauthlib.ClientAuthStore.
type OAuthStore struct {
	sessions map[string]*oauthlib.ClientSessionData
}

// NewOAuthStore creates an empty store.
func NewOAuthStore() *OAuthStore {
	return &OAuthStore{sessions: make(map[string]*oauthlib.ClientSessionData)}
}

// AddSession registers a session whose PDS is the test stack's.
func (s *OAuthStore) AddSession(did, sessionID, accessToken string) {
	s.AddSessionWithPDS(did, sessionID, accessToken, testkit.Endpoints().PDS.BaseURL)
}

// AddSessionWithPDS registers a session against a named PDS, for the tests that
// hold a real access token and need writes to reach the real repo.
func (s *OAuthStore) AddSessionWithPDS(did, sessionID, accessToken, pdsURL string) {
	parsedDID, _ := syntax.ParseDID(did)
	s.sessions[did+":"+sessionID] = &oauthlib.ClientSessionData{
		AccountDID:  parsedDID,
		SessionID:   sessionID,
		AccessToken: accessToken,
		HostURL:     pdsURL,
	}
}

// GetSession implements oauthlib.ClientAuthStore.
func (s *OAuthStore) GetSession(_ context.Context, did syntax.DID, sessionID string) (*oauthlib.ClientSessionData, error) {
	if session, ok := s.sessions[did.String()+":"+sessionID]; ok {
		return session, nil
	}
	return nil, fmt.Errorf("session not found")
}

// SaveSession implements oauthlib.ClientAuthStore.
func (s *OAuthStore) SaveSession(_ context.Context, session oauthlib.ClientSessionData) error {
	s.sessions[session.AccountDID.String()+":"+session.SessionID] = &session
	return nil
}

// DeleteSession implements oauthlib.ClientAuthStore.
func (s *OAuthStore) DeleteSession(_ context.Context, did syntax.DID, sessionID string) error {
	delete(s.sessions, did.String()+":"+sessionID)
	return nil
}

// GetAuthRequestInfo implements oauthlib.ClientAuthStore. The authorization-code
// flow is not modelled here: a test that needs it uses the real Postgres store.
func (s *OAuthStore) GetAuthRequestInfo(context.Context, string) (*oauthlib.AuthRequestData, error) {
	return nil, fmt.Errorf("fixtures.OAuthStore does not model auth requests")
}

// SaveAuthRequestInfo implements oauthlib.ClientAuthStore.
func (s *OAuthStore) SaveAuthRequestInfo(context.Context, oauthlib.AuthRequestData) error { return nil }

// DeleteAuthRequestInfo implements oauthlib.ClientAuthStore.
func (s *OAuthStore) DeleteAuthRequestInfo(context.Context, string) error { return nil }

// OAuthMiddleware is a real middleware.OAuthAuthMiddleware over the fakes
// above, with a way to mint a token per identity.
type OAuthMiddleware struct {
	*middleware.OAuthAuthMiddleware
	unsealer *SessionUnsealer
	store    *OAuthStore
}

// NewOAuthMiddleware builds middleware that will accept any identity AddUser
// registers.
func NewOAuthMiddleware() *OAuthMiddleware {
	unsealer := NewSessionUnsealer()
	store := NewOAuthStore()
	return &OAuthMiddleware{
		OAuthAuthMiddleware: middleware.NewOAuthAuthMiddleware(unsealer, store),
		unsealer:            unsealer,
		store:               store,
	}
}

// AddUser registers a DID and returns the bearer token that authenticates as it.
func (m *OAuthMiddleware) AddUser(did string) string {
	token := "test-token-" + did
	sessionID := "session-" + did
	m.unsealer.AddSession(token, did, sessionID)
	m.store.AddSession(did, sessionID, "access-token-"+did)
	return token
}

// SingleUserOAuthMiddleware is the one-identity form: middleware plus the token
// that authenticates as userDID.
func SingleUserOAuthMiddleware(userDID string) (*middleware.OAuthAuthMiddleware, string) {
	m := NewOAuthMiddleware()
	return m.OAuthAuthMiddleware, m.AddUser(userDID)
}

// PasswordAuthPDSClientFactory builds PDS clients from a session's access
// token, which is how a test acts as a real account against the real PDS.
//
// Production mints per-request DPoP-bound clients through the OAuth flow; these
// tests hold a password-session token instead, and this is the seam that lets
// the same service code use it.
func PasswordAuthPDSClientFactory() votes.PDSClientFactory {
	return func(_ context.Context, session *oauthlib.ClientSessionData) (pds.Client, error) {
		if session.AccessToken == "" {
			return nil, fmt.Errorf("session has no access token")
		}
		if session.HostURL == "" {
			return nil, fmt.Errorf("session has no host URL")
		}
		return pds.NewFromAccessToken(session.HostURL, session.AccountDID.String(), session.AccessToken)
	}
}
