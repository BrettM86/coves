package user

import (
	"net/http"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/users"
)

// resolveHandleErrorMapper maps resolution failures for
// com.atproto.identity.resolveHandle.
//
// The taxonomy: a handle that resolves to no account is 400, a resolution that
// broke upstream is 502, and anything unrecognised falls through to the 500 the
// mapper answers by default.
//
// 400 rather than 404 for an absent handle mirrors the PDS: atProto clients
// read this route the same way, and atcute's XrpcHandleResolver treats any 400
// here as DidNotFound. 502 rather than 500 for a broken resolution says the
// fault is the PLC directory, a DNS server, or the handle's own web host, not
// this AppView — so our own 5xx rate stays a signal about us rather than about
// every unreachable third-party host.
var resolveHandleErrorMapper = xrpc.NewMapper("user.resolveHandle",
	xrpc.Sentinel(users.ErrUserNotFound, http.StatusBadRequest,
		"InvalidRequest", "Unable to resolve handle"),
	xrpc.As[*users.InvalidHandleError](http.StatusBadRequest, "InvalidRequest",
		func(e *users.InvalidHandleError) string { return e.Error() }),
	// Fixed message: the resolver's Reason carries upstream text — a remote
	// status line, a host name we fetched — and this route is unauthenticated.
	xrpc.As[*identity.ErrResolutionFailed](http.StatusBadGateway, "UpstreamFailure",
		func(*identity.ErrResolutionFailed) string {
			return "Handle resolution is temporarily unavailable"
		}),
)

// resolveHandleResponse is the lexicon's output: the resolved DID and nothing
// else.
type resolveHandleResponse struct {
	DID string `json:"did"`
}

// ResolveHandleHandler serves com.atproto.identity.resolveHandle.
type ResolveHandleHandler struct {
	userService users.UserService
}

// NewResolveHandleHandler creates a new resolveHandle handler.
func NewResolveHandleHandler(service users.UserService) *ResolveHandleHandler {
	return &ResolveHandleHandler{userService: service}
}

// HandleResolveHandle handles com.atproto.identity.resolveHandle.
//
// The handle reaches the service exactly as submitted: normalising it here
// would shadow the service's own normalisation, which is where that behaviour
// is defined and tested.
func (h *ResolveHandleHandler) HandleResolveHandle(w http.ResponseWriter, r *http.Request) {
	handle := r.URL.Query().Get("handle")
	trimmed := strings.TrimSpace(handle)
	if trimmed == "" {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest",
			"handle parameter is required")
		return
	}

	// A DID is answered here rather than forwarded. The service hands anything
	// it cannot find locally to the identity resolver, so a forwarded DID
	// becomes a PLC directory lookup that this unauthenticated route would let
	// any caller trigger at will — and the caller learns nothing, since they
	// already hold the DID. Handle SYNTAX is left to the service, whose own
	// error names the rule that was broken.
	if _, err := syntax.ParseDID(trimmed); err == nil {
		xrpc.WriteError(w, http.StatusBadRequest, "InvalidRequest",
			"handle must be a handle, not a DID")
		return
	}

	did, err := h.userService.ResolveHandleToDID(r.Context(), handle)
	if err != nil {
		resolveHandleErrorMapper.Write(w, err)
		return
	}

	xrpc.WriteJSON(w, http.StatusOK, resolveHandleResponse{DID: did})
}
