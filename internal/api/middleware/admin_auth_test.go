package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Coves/internal/atproto/oauth"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	indigoauth "github.com/bluesky-social/indigo/atproto/auth"
	oauthlib "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	instanceAdminDID = "did:plc:instanceadmin"
	otherActorDID    = "did:plc:ordinaryuser"
	adminMethodPath  = "/xrpc/social.coves.moderation.getSubjectState"
)

type instanceAdminAuthorityFake map[string]bool

func (a instanceAdminAuthorityFake) IsInstanceAdmin(did string) bool { return a[did] }

type instanceAdminFixture struct {
	client    *mockOAuthClient
	unsealer  SessionUnsealer
	sessions  *mockOAuthStore
	store     oauthlib.ClientAuthStore
	validator ServiceAuthValidator
	authority instanceAdminAuthorityFake
}

func newInstanceAdminFixture() *instanceAdminFixture {
	sessions := newMockOAuthStore()
	client := newMockOAuthClient()
	return &instanceAdminFixture{
		client:    client,
		unsealer:  client,
		sessions:  sessions,
		store:     sessions,
		authority: instanceAdminAuthorityFake{instanceAdminDID: true},
	}
}

func (f *instanceAdminFixture) sealedToken(t *testing.T, did string) string {
	t.Helper()
	const sessionID = "browser"
	require.NoError(t, f.sessions.SaveSession(t.Context(), oauthlib.ClientSessionData{
		AccountDID: syntax.DID(did), SessionID: sessionID,
	}))
	return f.client.createTestToken(did, sessionID, time.Hour)
}

func instanceAdminBearerRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, adminMethodPath, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

type instanceAdminResult struct {
	response *httptest.ResponseRecorder
	called   bool
	did      string
}

func (f *instanceAdminFixture) serve(request *http.Request) instanceAdminResult {
	result := instanceAdminResult{response: httptest.NewRecorder()}
	gate := NewInstanceAdminMiddleware(f.unsealer, f.store, f.validator, f.authority)
	handler := gate.RequireInstanceAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result.called = true
		result.did = GetUserDID(r)
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(result.response, request)
	return result
}

func assertInstanceAdminResult(t *testing.T, got instanceAdminResult, wantStatus int, wantCode string, wantCalled bool, wantDID string) {
	t.Helper()
	assert.Equal(t, wantStatus, got.response.Code)
	assert.Equal(t, wantCalled, got.called, "next handler must run only for an authorized admin")
	if wantCalled {
		assert.Equal(t, wantDID, got.did, "authenticated DID in next handler")
	}
	if wantCode != "" {
		if assert.NotEmpty(t, got.response.Body.Bytes(), "refusal must include a JSON error") {
			var body struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(got.response.Body.Bytes(), &body))
			assert.Equal(t, wantCode, body.Error)
			assert.NotEmpty(t, body.Message)
		}
	}
}

func TestRequireInstanceAdminOAuthRefusals(t *testing.T) {
	for _, test := range []struct {
		name       string
		request    func(t *testing.T, fixture *instanceAdminFixture) *http.Request
		wantStatus int
		wantCode   string
	}{
		{
			name: "no credential", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
			request: func(_ *testing.T, _ *instanceAdminFixture) *http.Request {
				return httptest.NewRequest(http.MethodGet, adminMethodPath, nil)
			},
		},
		{
			name: "basic authentication", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
			request: func(_ *testing.T, _ *instanceAdminFixture) *http.Request {
				request := httptest.NewRequest(http.MethodGet, adminMethodPath, nil)
				request.Header.Set("Authorization", "Basic abc")
				return request
			},
		},
		{
			name: "authenticated non-admin", wantStatus: http.StatusForbidden, wantCode: "Forbidden",
			request: func(t *testing.T, f *instanceAdminFixture) *http.Request {
				return instanceAdminBearerRequest(f.sealedToken(t, otherActorDID))
			},
		},
		{
			name: "unsealable token", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
			request: func(_ *testing.T, _ *instanceAdminFixture) *http.Request {
				return instanceAdminBearerRequest("garbage-token")
			},
		},
		{
			name: "session missing", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
			request: func(_ *testing.T, f *instanceAdminFixture) *http.Request {
				return instanceAdminBearerRequest(f.client.createTestToken(instanceAdminDID, "missing", time.Hour))
			},
		},
		{
			name: "session DID mismatch", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
			request: func(_ *testing.T, f *instanceAdminFixture) *http.Request {
				f.sessions.sessions[instanceAdminDID+":browser"] = &oauthlib.ClientSessionData{
					AccountDID: syntax.DID(otherActorDID), SessionID: "browser",
				}
				return instanceAdminBearerRequest(f.client.createTestToken(instanceAdminDID, "browser", time.Hour))
			},
		},
		{
			name: "session store unavailable", wantStatus: http.StatusServiceUnavailable, wantCode: "ModerationUnavailable",
			request: func(_ *testing.T, f *instanceAdminFixture) *http.Request {
				f.store = &authenticationFailureStore{failure: errors.New("database unavailable")}
				return instanceAdminBearerRequest(f.client.createTestToken(instanceAdminDID, "browser", time.Hour))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstanceAdminFixture()
			request := test.request(t, fixture)
			assertInstanceAdminResult(t, fixture.serve(request), test.wantStatus, test.wantCode, false, "")
		})
	}
}

func TestRequireInstanceAdminOAuthSuccess(t *testing.T) {
	for _, useCookie := range []bool{false, true} {
		name := "bearer header"
		if useCookie {
			name = "session cookie"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newInstanceAdminFixture()
			token := fixture.sealedToken(t, instanceAdminDID)
			request := httptest.NewRequest(http.MethodGet, adminMethodPath, nil)
			if useCookie {
				request.AddCookie(&http.Cookie{Name: "coves_session", Value: token})
			} else {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			assertInstanceAdminResult(t, fixture.serve(request), http.StatusNoContent, "", true, instanceAdminDID)
		})
	}
}

func TestRequireInstanceAdminIgnoresCallerActorAndAuthority(t *testing.T) {
	t.Run("non-admin cannot claim admin in query or body", func(t *testing.T) {
		fixture := newInstanceAdminFixture()
		request := httptest.NewRequest(http.MethodPost,
			adminMethodPath+"?actor="+instanceAdminDID+"&authority="+instanceAdminDID,
			strings.NewReader(`{"actor":"`+instanceAdminDID+`","authority":"`+instanceAdminDID+`"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+fixture.sealedToken(t, otherActorDID))
		assertInstanceAdminResult(t, fixture.serve(request), http.StatusForbidden, "Forbidden", false, "")
	})
	t.Run("admin remains authenticated despite other actor", func(t *testing.T) {
		fixture := newInstanceAdminFixture()
		request := httptest.NewRequest(http.MethodGet, adminMethodPath+"?actor="+otherActorDID, nil)
		request.Header.Set("Authorization", "Bearer "+fixture.sealedToken(t, instanceAdminDID))
		assertInstanceAdminResult(t, fixture.serve(request), http.StatusNoContent, "", true, instanceAdminDID)
	})
}

type recordingInstanceAdminValidator struct {
	mockServiceAuthValidator
	called    bool
	lexMethod *syntax.NSID
}

func (v *recordingInstanceAdminValidator) Validate(ctx context.Context, token string, lexMethod *syntax.NSID) (syntax.DID, error) {
	v.called = true
	if lexMethod != nil {
		method := *lexMethod
		v.lexMethod = &method
	}
	return v.mockServiceAuthValidator.Validate(ctx, token, lexMethod)
}

func TestRequireInstanceAdminServiceJWTWithMockValidator(t *testing.T) {
	for _, test := range []struct {
		name       string
		validator  *recordingInstanceAdminValidator
		wantStatus int
		wantCode   string
		wantCalled bool
	}{
		{
			name: "listed DID and endpoint-scoped lexMethod", validator: &recordingInstanceAdminValidator{
				mockServiceAuthValidator: mockServiceAuthValidator{returnDID: syntax.DID(instanceAdminDID)},
			}, wantStatus: http.StatusNoContent, wantCalled: true,
		},
		{
			name: "validator rejects token", validator: &recordingInstanceAdminValidator{
				mockServiceAuthValidator: mockServiceAuthValidator{shouldFail: true},
			}, wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
		},
		{
			name: "unlisted issuer", validator: &recordingInstanceAdminValidator{
				mockServiceAuthValidator: mockServiceAuthValidator{returnDID: syntax.DID(otherActorDID)},
			}, wantStatus: http.StatusForbidden, wantCode: "Forbidden",
		},
		{
			name: "nil validator", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstanceAdminFixture()
			if test.validator != nil {
				fixture.validator = test.validator
			}
			assertInstanceAdminResult(t, fixture.serve(instanceAdminBearerRequest("header.payload.signature")),
				test.wantStatus, test.wantCode, test.wantCalled, instanceAdminDID)
			if test.validator != nil {
				assert.True(t, test.validator.called, "JWT must be validated, not treated as a sealed session")
				if assert.NotNil(t, test.validator.lexMethod, "JWT validation must bind the called endpoint") {
					assert.Equal(t, "social.coves.moderation.getSubjectState", test.validator.lexMethod.String())
				}
			}
		})
	}
}

// mapSessionUnsealer unseals exactly the tokens it holds, so a JWT-shaped
// string can also be a valid sealed session.
type mapSessionUnsealer map[string]*oauth.SealedSession

func (u mapSessionUnsealer) UnsealSession(token string) (*oauth.SealedSession, error) {
	if sealed, ok := u[token]; ok {
		return sealed, nil
	}
	return nil, errors.New("unknown sealed token")
}

func TestRequireInstanceAdminServiceJWTRefusalDoesNotFallBackToSealedSession(t *testing.T) {
	const jwtShapedToken = "a.b.c"
	require.True(t, isJWTFormat(jwtShapedToken), "fixture token must route as a service JWT")

	for _, test := range []struct {
		name      string
		validator ServiceAuthValidator
	}{
		{name: "validator rejects token", validator: &mockServiceAuthValidator{shouldFail: true}},
		{name: "nil validator"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstanceAdminFixture()
			fixture.unsealer = mapSessionUnsealer{jwtShapedToken: {
				DID: instanceAdminDID, SessionID: "browser", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			}}
			require.NoError(t, fixture.sessions.SaveSession(t.Context(), oauthlib.ClientSessionData{
				AccountDID: syntax.DID(instanceAdminDID), SessionID: "browser",
			}))
			sealed := authenticateSealedSession(t.Context(), jwtShapedToken, fixture.unsealer, fixture.store)
			require.Equal(t, sealedSessionNoFailure, sealed.failure, "the JWT-shaped token must also be a valid admin sealed session")
			fixture.validator = test.validator

			assertInstanceAdminResult(t, fixture.serve(instanceAdminBearerRequest(jwtShapedToken)),
				http.StatusUnauthorized, "AuthRequired", false, "")
		})
	}
}

func TestRequireInstanceAdminRefusalLogs(t *testing.T) {
	const remoteAddress = "203.0.113.77:4242"
	for _, test := range []struct {
		name       string
		path       string
		credential func(t *testing.T, fixture *instanceAdminFixture) string
		wantStatus int
		wantCode   string
		wantLevel  string
		wantError  string
	}{
		{
			name: "unsealable token", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired", wantLevel: "WARN",
			credential: func(_ *testing.T, _ *instanceAdminFixture) string { return "secret-token-value" },
		},
		{
			name: "non-admin", wantStatus: http.StatusForbidden, wantCode: "Forbidden", wantLevel: "WARN",
			credential: func(t *testing.T, f *instanceAdminFixture) string { return f.sealedToken(t, otherActorDID) },
		},
		{
			name: "service JWT rejected", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired", wantLevel: "WARN",
			wantError: "mock validation failure",
			credential: func(_ *testing.T, f *instanceAdminFixture) string {
				f.validator = &mockServiceAuthValidator{shouldFail: true}
				return "distinctive-jwt-header.distinctive-jwt-payload.distinctive-jwt-signature"
			},
		},
		{
			name: "service JWT for invalid method", path: "/xrpc/not-an-nsid", wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired",
			wantLevel: "WARN", wantError: "NSID syntax didn't validate via regex",
			credential: func(_ *testing.T, f *instanceAdminFixture) string {
				f.validator = &mockServiceAuthValidator{returnDID: syntax.DID(instanceAdminDID)}
				return "method-jwt-header.method-jwt-payload.method-jwt-signature"
			},
		},
		{
			name: "session store unavailable", wantStatus: http.StatusServiceUnavailable, wantCode: "ModerationUnavailable", wantLevel: "ERROR",
			wantError: "database unavailable",
			credential: func(_ *testing.T, f *instanceAdminFixture) string {
				f.store = &authenticationFailureStore{failure: errors.New("database unavailable")}
				return f.client.createTestToken(instanceAdminDID, "browser", time.Hour)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logged bytes.Buffer
			previousSlog := slog.Default()
			previousLog := log.Writer()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			log.SetOutput(&logged)
			t.Cleanup(func() {
				slog.SetDefault(previousSlog)
				log.SetOutput(previousLog)
			})

			fixture := newInstanceAdminFixture()
			token := test.credential(t, fixture)
			request := instanceAdminBearerRequest(token)
			if test.path != "" {
				request.URL.Path = test.path
			}
			request.RemoteAddr = remoteAddress
			assertInstanceAdminResult(t, fixture.serve(request), test.wantStatus, test.wantCode, false, "")
			output := logged.String()
			assert.Contains(t, output, "level="+test.wantLevel, "refusal log level")
			if test.wantError != "" {
				assert.Contains(t, output, test.wantError, "refusal log must carry the underlying error")
			}
			assert.NotContains(t, output, token, "credentials must not appear in refusal logs")
			assert.NotContains(t, output, "203.0.113.77", "client IP must not appear in refusal logs")
		})
	}
}

func TestRequireInstanceAdminRealServiceJWT(t *testing.T) {
	method := syntax.NSID("social.coves.moderation.getSubjectState")
	otherMethod := syntax.NSID("social.coves.moderation.removeContent")
	const audience = "did:web:appview.test"

	for _, test := range []struct {
		name       string
		issuer     syntax.DID
		audience   string
		lexMethod  *syntax.NSID
		wrongKey   bool
		expired    bool
		wantStatus int
		wantCode   string
	}{
		{name: "valid admin", issuer: syntax.DID(instanceAdminDID), audience: audience, lexMethod: &method, wantStatus: http.StatusNoContent},
		{name: "wrong audience", issuer: syntax.DID(instanceAdminDID), audience: "did:web:other.test", lexMethod: &method, wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired"},
		{name: "wrong lexMethod", issuer: syntax.DID(instanceAdminDID), audience: audience, lexMethod: &otherMethod, wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired"},
		{name: "missing lexMethod", issuer: syntax.DID(instanceAdminDID), audience: audience, wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired"},
		{name: "wrong signing key", issuer: syntax.DID(instanceAdminDID), audience: audience, lexMethod: &method, wrongKey: true, wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired"},
		{name: "expired token", issuer: syntax.DID(instanceAdminDID), audience: audience, lexMethod: &method, expired: true, wantStatus: http.StatusUnauthorized, wantCode: "AuthRequired"},
		{name: "valid unlisted issuer", issuer: syntax.DID(otherActorDID), audience: audience, lexMethod: &method, wantStatus: http.StatusForbidden, wantCode: "Forbidden"},
	} {
		t.Run(test.name, func(t *testing.T) {
			privateKey, err := atcrypto.GeneratePrivateKeyP256()
			require.NoError(t, err)
			publicKey, err := privateKey.PublicKey()
			require.NoError(t, err)
			directory := identity.NewMockDirectory()
			directory.Insert(identity.Identity{
				DID: test.issuer,
				Keys: map[string]identity.VerificationMethod{
					"atproto": {Type: "Multikey", PublicKeyMultibase: publicKey.Multibase()},
				},
			})
			validator := &indigoauth.ServiceAuthValidator{
				Audience: audience, Dir: directory, TimestampLeeway: 30 * time.Second,
			}
			signingKey := atcrypto.PrivateKey(privateKey)
			if test.wrongKey {
				signingKey, err = atcrypto.GeneratePrivateKeyP256()
				require.NoError(t, err)
			}
			lifetime := time.Minute
			if test.expired {
				lifetime = -2 * time.Minute
			}
			token, err := indigoauth.SignServiceAuth(test.issuer, test.audience, lifetime, test.lexMethod, signingKey)
			require.NoError(t, err)
			verifiedDID, validationErr := validator.Validate(t.Context(), token, &method)
			if test.wantStatus == http.StatusUnauthorized {
				require.Error(t, validationErr, "the signed JWT fixture must be rejected by indigo")
			} else {
				require.NoError(t, validationErr, "the signed JWT fixture must be verifiable by indigo")
				require.Equal(t, test.issuer, verifiedDID)
			}

			fixture := newInstanceAdminFixture()
			fixture.validator = validator
			assertInstanceAdminResult(t, fixture.serve(instanceAdminBearerRequest(token)),
				test.wantStatus, test.wantCode, test.wantStatus == http.StatusNoContent, test.issuer.String())
		})
	}
}

func TestSealedSessionAuthenticationZeroValueIsNotAuthenticated(t *testing.T) {
	var zero sealedSessionAuthentication
	assert.NotEqual(t, sealedSessionNoFailure, zero.failure, "an unset result must not read as authenticated")
	assert.NotEmpty(t, zero.failure.reason(), "an unset result must have a refusal reason")
}
