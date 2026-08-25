package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/core/userblocks"
	"Coves/internal/core/users"
)

// social.coves.actor.signup seen from the wire: what a would-be account holder
// is told when registration fails.
//
// # WHY THE MAPPING IS THE SUBJECT AND NOT THE HAPPY PATH
//
// Signup itself is four lines — decode, call, encode — and creating a real
// account is the pipeline tier's job, against a real PDS. What lives only here
// is respondWithLexiconError, a switch over six typed errors that decides both
// the HTTP status and the error NAME in the body, and the name is the part
// clients branch on: the signup form retries on one of these, asks for a
// different handle on another, and shows a dead end on a third.
//
// Two properties make that switch worth a test of its own:
//
//   - It is `errors.As`, so it matches through wrapping. A service that starts
//     annotating its returns with %w keeps working; a mapper written with a type
//     assertion instead would silently start answering 500 to every one of these.
//     The cases below wrap on purpose so that this stays true.
//   - The default arm is a security boundary. Anything unrecognised must become a
//     generic 500 with no detail, because the alternative — echoing err.Error()
//     — leaks PDS internals, connection strings and, on the invite path, the
//     code itself.
//
// The PDS arm is the odd one out and the reason this is a table rather than six
// asserts: it does not pick a status, it FORWARDS one. A PDS answering 502 must
// not become a 400 that tells the user their handle is bad.

// signupService is a users.UserService that answers RegisterAccount with one
// canned outcome and records what it was asked for.
//
// The embedded interface is nil: any other method call panics rather than
// returning a zero value that would quietly change what these tests prove.
type signupService struct {
	users.UserService

	response *users.RegisterAccountResponse
	err      error

	received users.RegisterAccountRequest
	calls    int
}

func (s *signupService) RegisterAccount(_ context.Context, req users.RegisterAccountRequest) (*users.RegisterAccountResponse, error) {
	s.calls++
	s.received = req
	if s.err != nil {
		return nil, s.err
	}
	return s.response, nil
}

// postSignup drives the handler with a raw body, so that malformed JSON is
// expressible.
func postSignup(t *testing.T, svc users.UserService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.actor.signup",
		bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewUserHandler(svc).Signup(rec, req)
	return rec
}

// signupErrorBody is the XRPC error envelope both signup and the lexicon
// mapper write.
type signupErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func decodeSignupError(t *testing.T, rec *httptest.ResponseRecorder) signupErrorBody {
	t.Helper()
	var body signupErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not the XRPC error envelope: %v (body: %s)", err, rec.Body.String())
	}
	return body
}

func TestSignup_LexiconErrorMapping(t *testing.T) {
	t.Parallel()

	// Every error is wrapped, because the service wraps: a mapper written with
	// a type switch rather than errors.As passes when handed a bare sentinel
	// and answers 500 to every real failure.
	tests := []struct {
		name       string
		serviceErr error
		wantStatus int
		wantError  string
	}{
		{
			name:       "a handle that is not a valid handle",
			serviceErr: fmt.Errorf("registering: %w", &users.InvalidHandleError{Handle: "not a handle", Reason: "contains a space"}),
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidHandle",
		},
		{
			name:       "a handle somebody already has",
			serviceErr: fmt.Errorf("registering: %w", &users.HandleNotAvailableError{Handle: "taken.coves.social"}),
			wantStatus: http.StatusBadRequest,
			wantError:  "HandleNotAvailable",
		},
		{
			name:       "an invite code that is spent or wrong",
			serviceErr: fmt.Errorf("registering: %w", &users.InvalidInviteCodeError{Code: "coves-social-abc12"}),
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidInviteCode",
		},
		{
			name:       "an address that is not an email",
			serviceErr: fmt.Errorf("registering: %w", &users.InvalidEmailError{Email: "nope"}),
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidEmail",
		},
		{
			name:       "a password below the strength floor",
			serviceErr: fmt.Errorf("registering: %w", &users.WeakPasswordError{Reason: "too short"}),
			wantStatus: http.StatusBadRequest,
			wantError:  "WeakPassword",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := postSignup(t, &signupService{err: tc.serviceErr}, `{"handle":"someone.coves.social"}`)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := decodeSignupError(t, rec)
			if body.Error != tc.wantError {
				t.Errorf("error = %q, want %q — this name is what the signup form branches on to "+
					"decide whether to ask for a different handle, a different password, or nothing at all",
					body.Error, tc.wantError)
			}
			if body.Message == "" {
				t.Errorf("the %s response carried no message; the client has nothing to show the user", tc.wantError)
			}
		})
	}
}

// TestSignup_PDSErrorForwardsItsStatus covers the one arm that does not choose a
// status of its own.
//
// A PDS that is refusing registrations, rate limiting us, or simply down must
// reach the caller as what it is. Collapsing these into 400 would tell a user
// their details are wrong during an outage and would hide the outage from
// anyone watching 5xx — the same distinction GetProfile draws between an absent
// handle and a broken resolver.
func TestSignup_PDSErrorForwardsItsStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			service := &signupService{err: fmt.Errorf("creating the account: %w",
				&users.PDSError{StatusCode: status, Message: "the PDS said no"})}

			rec := postSignup(t, service, `{"handle":"someone.coves.social"}`)

			if rec.Code != status {
				t.Fatalf("status = %d, want the PDS's own %d (body: %s)", rec.Code, status, rec.Body.String())
			}
			body := decodeSignupError(t, rec)
			if body.Error != "PDSError" {
				t.Errorf("error = %q, want %q", body.Error, "PDSError")
			}
			if body.Message != "the PDS said no" {
				t.Errorf("message = %q, want the PDS's own message", body.Message)
			}
		})
	}
}

// TestSignup_UnrecognisedErrorLeaksNothing is the default arm, and it is a
// security assertion rather than a formatting one.
//
// An unmapped failure carries whatever the layer below put in it. Here that is
// a connection string with a password in it, which is realistic: a driver error
// on the users table would produce exactly this shape. The response must
// contain none of it.
func TestSignup_UnrecognisedErrorLeaksNothing(t *testing.T) {
	t.Parallel()

	const secret = "postgres://coves:hunter2@db.internal:5432/coves"
	service := &signupService{err: fmt.Errorf("dial %s: connection refused", secret)}

	rec := postSignup(t, service, `{"handle":"someone.coves.social"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	body := decodeSignupError(t, rec)
	if body.Error != "InternalServerError" {
		t.Errorf("error = %q, want %q", body.Error, "InternalServerError")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("hunter2")) ||
		bytes.Contains(rec.Body.Bytes(), []byte("db.internal")) {
		t.Errorf("the response echoed the underlying error to an UNAUTHENTICATED caller: %s\n"+
			"signup is a public endpoint, so anything the default arm forwards is disclosed to "+
			"anyone who can provoke it", rec.Body.String())
	}
}

// TestSignup_MalformedBodyNeverReachesTheService checks the decode guard, and
// specifically that it refuses before calling anything.
//
// Signup is the one unauthenticated write in the product. A body that does not
// parse must cost a JSON decode and nothing more — not a PDS round trip, not a
// handle-availability lookup.
func TestSignup_MalformedBodyNeverReachesTheService(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"truncated object", `{"handle":`},
		{"a JSON array", `["handle"]`},
		{"not JSON at all", `handle=someone.coves.social`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service := &signupService{}

			rec := postSignup(t, service, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if service.calls != 0 {
				t.Errorf("the service was called %d time(s) for a body that does not parse", service.calls)
			}
		})
	}
}

// TestSignup_OversizedBodyIsRefusedBeforeParsing pins the DoS guard on the
// one endpoint anyone on the internet may POST to without credentials: a body
// over the tiny tier must die with a 413 before the service — and the PDS
// behind it — hears about the request.
func TestSignup_OversizedBodyIsRefusedBeforeParsing(t *testing.T) {
	t.Parallel()
	service := &signupService{}

	body := `{"handle":"` + strings.Repeat("a", int(reqbody.LimitTiny)) + `"}`
	rec := postSignup(t, service, body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeSignupError(t, rec).Error; got != "PayloadTooLarge" {
		t.Errorf("error code = %q, want PayloadTooLarge", got)
	}
	if service.calls != 0 {
		t.Errorf("the service was called %d time(s) for a body over the size limit", service.calls)
	}
}

// TestSignup_SuccessReturnsTheLexiconOutput pins the response shape.
//
// The four keys below are the social.coves.actor.signup output schema, and a
// client cannot log the new account in without accessJwt and refreshJwt. The
// pdsUrl the service also returns is deliberately NOT forwarded — the handler
// builds its own map rather than marshalling the service's struct — so this
// also asserts what is absent.
func TestSignup_SuccessReturnsTheLexiconOutput(t *testing.T) {
	t.Parallel()

	service := &signupService{response: &users.RegisterAccountResponse{
		DID:        "did:plc:brandnewaccount000000000",
		Handle:     "newcomer.coves.social",
		AccessJwt:  "access.jwt.value",
		RefreshJwt: "refresh.jwt.value",
		PDSURL:     "https://pds.internal.example",
	}}

	rec := postSignup(t, service, `{"handle":"newcomer.coves.social","email":"a@b.test","password":"correct horse"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	for key, want := range map[string]string{
		"did":        service.response.DID,
		"handle":     service.response.Handle,
		"accessJwt":  service.response.AccessJwt,
		"refreshJwt": service.response.RefreshJwt,
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %q", key, body[key], want)
		}
	}
	if _, present := body["pdsUrl"]; present {
		t.Errorf("the response carried pdsUrl. IF THIS FAILED the handler stopped building its own "+
			"output map and now marshals the service struct, which widens the endpoint's contract "+
			"by whatever fields that struct grows next. Body: %s", rec.Body.String())
	}

	// The request really is the one the caller sent, rather than a zero value
	// the handler happened to construct.
	if service.received.Handle != "newcomer.coves.social" || service.received.Email != "a@b.test" {
		t.Errorf("the service received %+v, want the decoded request body", service.received)
	}
}

// blockLookup is a userblocks.Repository that answers GetBlock and records the
// pair it was asked about. Everything else panics via the nil embedded
// interface, so a hydration path that grew a second repository call would crash
// here rather than quietly reading a zero value.
type blockLookup struct {
	userblocks.Repository

	block *userblocks.UserBlock
	err   error

	askedViewer  string
	askedSubject string
	calls        int
}

func (b *blockLookup) GetBlock(_ context.Context, blockerDID, blockedDID string) (*userblocks.UserBlock, error) {
	b.calls++
	b.askedViewer = blockerDID
	b.askedSubject = blockedDID
	if b.err != nil {
		return nil, b.err
	}
	return b.block, nil
}

// TestGetProfile_ViewerBlockingHydration covers the optional-auth half of
// getProfile: the viewer.blocking field that a signed-in caller gets and an
// anonymous one does not.
//
// Three things make it worth its own test. The field is what a client uses to
// draw "you have blocked this account", so a missing one silently un-blocks
// somebody in the UI. The lookup is best-effort by design — a database failure
// must not take the whole profile down — and "best effort" is exactly the
// wording under which a real error gets swallowed, so the failure path is
// asserted rather than assumed. And the self-view skip is not an optimisation:
// without it, every profile page a user loads of themselves costs a block
// lookup that cannot ever match.
func TestGetProfile_ViewerBlockingHydration(t *testing.T) {
	t.Parallel()

	const subject = "did:plc:subjectofthisprofile"
	const viewer = "did:plc:theviewerlookingatit"
	const blockURI = "at://did:plc:theviewerlookingatit/social.coves.actor.block/3kabc"

	profileFor := func() *users.ProfileViewDetailed {
		return &users.ProfileViewDetailed{DID: subject, Handle: "subject.coves.social"}
	}

	// request builds a getProfile request as OptionalAuth would leave it: the
	// viewer's DID in the context when authenticated, nothing when not.
	request := func(viewerDID string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.actor.getProfile?actor="+subject, nil)
		if viewerDID != "" {
			req = req.WithContext(middleware.SetTestUserDID(req.Context(), viewerDID))
		}
		return req
	}

	serve := func(t *testing.T, repo *blockLookup, req *http.Request) *users.ProfileViewDetailed {
		t.Helper()
		handler := NewUserHandler(&stubUserService{profile: profileFor()})
		if repo != nil {
			handler.SetUserBlockRepo(repo)
		}
		rec := httptest.NewRecorder()
		handler.GetProfile(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		var profile users.ProfileViewDetailed
		if err := json.Unmarshal(rec.Body.Bytes(), &profile); err != nil {
			t.Fatalf("body is not a profile: %v (body: %s)", err, rec.Body.String())
		}
		return &profile
	}

	t.Run("an authenticated viewer who has blocked this account sees the block URI", func(t *testing.T) {
		t.Parallel()
		repo := &blockLookup{block: &userblocks.UserBlock{RecordURI: blockURI}}

		profile := serve(t, repo, request(viewer))

		if profile.Viewer == nil || profile.Viewer.Blocking == nil {
			t.Fatalf("viewer state = %+v, want the block URI: the client has no way to show that this "+
				"account is blocked", profile.Viewer)
		}
		if *profile.Viewer.Blocking != blockURI {
			t.Errorf("viewer.blocking = %q, want %q", *profile.Viewer.Blocking, blockURI)
		}
		// The lookup asked about the right pair, in the right direction. Blocks
		// are one-directional, so a repository asked (subject, viewer) would
		// answer about a block the SUBJECT placed on the viewer and report it
		// as the viewer's own.
		if repo.askedViewer != viewer || repo.askedSubject != subject {
			t.Errorf("the block lookup asked about (%q, %q), want (viewer=%q, subject=%q)",
				repo.askedViewer, repo.askedSubject, viewer, subject)
		}
	})

	t.Run("no block means no viewer state at all", func(t *testing.T) {
		t.Parallel()
		repo := &blockLookup{err: userblocks.ErrBlockNotFound}

		profile := serve(t, repo, request(viewer))

		if profile.Viewer != nil {
			t.Errorf("viewer state = %+v, want nil when there is no block", profile.Viewer)
		}
		if repo.calls != 1 {
			t.Errorf("the repository was consulted %d times, want exactly 1", repo.calls)
		}
	})

	t.Run("an anonymous caller costs no block lookup", func(t *testing.T) {
		t.Parallel()
		repo := &blockLookup{block: &userblocks.UserBlock{RecordURI: blockURI}}

		profile := serve(t, repo, request(""))

		if profile.Viewer != nil {
			t.Errorf("viewer state = %+v, want nil for an anonymous caller", profile.Viewer)
		}
		if repo.calls != 0 {
			t.Errorf("the repository was consulted %d time(s) for a caller with no identity", repo.calls)
		}
	})

	t.Run("viewing your own profile costs no block lookup", func(t *testing.T) {
		t.Parallel()
		repo := &blockLookup{block: &userblocks.UserBlock{RecordURI: blockURI}}

		profile := serve(t, repo, request(subject))

		if profile.Viewer != nil {
			t.Errorf("viewer state = %+v, want nil when the viewer is the subject", profile.Viewer)
		}
		if repo.calls != 0 {
			t.Errorf("the repository was consulted %d time(s) for a self-view, which can never match", repo.calls)
		}
	})

	t.Run("a repository failure still serves the profile", func(t *testing.T) {
		t.Parallel()
		repo := &blockLookup{err: errors.New("connection pool exhausted")}

		profile := serve(t, repo, request(viewer))

		// The whole point of the best-effort branch: the profile is still
		// served, with the field simply absent, rather than the page 500ing
		// because one optional decoration could not be computed.
		if profile.DID != subject {
			t.Errorf("profile.did = %q, want %q", profile.DID, subject)
		}
		if profile.Viewer != nil {
			t.Errorf("viewer state = %+v, want nil when the lookup failed", profile.Viewer)
		}
	})

	t.Run("no repository wired means no viewer state and no panic", func(t *testing.T) {
		t.Parallel()
		// The production wiring passes the repository through
		// UserRouteOptions, which is optional; RegisterUserRoutes (no options)
		// leaves it nil. That configuration must serve profiles, not crash.
		profile := serve(t, nil, request(viewer))

		if profile.Viewer != nil {
			t.Errorf("viewer state = %+v, want nil when no block repository is configured", profile.Viewer)
		}
	})
}
