package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/accountstatus"
)

// WithUserAccountStatus installs the #account takendown/suspended handling:
// such events, and activations of an already-restricted account, are confirmed
// with the account's PDS and the stored restriction follows the confirmed
// answer; a failed confirmation is rechecked by the store's recheck row.
// For a DID with no Coves presence and no stored restriction, a takendown or
// suspended event is confirmed and stored the same way but a failed
// confirmation is not rechecked, and an activation asks no PDS. Without it
// (or with a nil store or confirmer), #account events change nothing.
func WithUserAccountStatus(store accountstatus.Store, confirmer accountstatus.Confirmer) ConsumerOption {
	return func(c *UserEventConsumer) {
		if store == nil || confirmer == nil {
			c.accountStatus = nil
			return
		}
		c.accountStatus = accountstatus.NewReconciler(store, confirmer, time.Now)
	}
}

// reconcileAccountStatus confirms an #account event's status with the PDS
// under the store's per-DID lock and applies the confirmed answer
// (accountstatus.Reconciler.HandleEvent). The event only triggers the check: a
// forged or stale event cannot set or lift a restriction the PDS does not
// report. Jetstream carries #account for the whole network: a takendown or
// suspended event for a DID with no Coves presence and no stored restriction
// is still confirmed and a confirmed restriction stored, so content the
// account wrote and Coves indexes later is restricted; an activation for such
// a DID asks no PDS.
//
// A confirmation failure leaves the restriction alone, records a recheck row
// that the recheck worker retries with backoff (none for a DID with no
// presence and nothing stored), and returns nil: the event is
// consumed, and the recheck row, not the redrive lane, is the retry. A store
// failure, including failing to write the recheck row, stays TRANSIENT. A DID
// that is not did:plc or did:web is ErrPermanentEvent, except for statuses
// this handler ignores.
func (c *UserEventConsumer) reconcileAccountStatus(ctx context.Context, did string, account *AccountEvent) error {
	restrictive := !account.Active &&
		(account.Status == string(accountstatus.Takendown) || account.Status == string(accountstatus.Suspended))
	if !account.Active && !restrictive {
		// deactivated, deleted, throttled, desynchronized, unknown or empty:
		// not a moderation status, nothing to confirm.
		return nil
	}
	if err := requireDIDShaped("account did", did); err != nil {
		return err
	}
	return c.accountStatus.HandleEvent(ctx, did, account.Active)
}

// repoStatusTimeout bounds one getRepoStatus call, so a slow PDS cannot hold
// the single-threaded users lane for long.
const repoStatusTimeout = 5 * time.Second

// maxRepoStatusResponseBytes bounds how much of a getRepoStatus answer is read;
// the host is named by an attacker-writable DID document.
const maxRepoStatusResponseBytes = 64 << 10

// PDSRepoStatusConfirmer confirms an account's status with
// com.atproto.sync.getRepoStatus at its current PDS.
type PDSRepoStatusConfirmer struct {
	resolver identity.UncachedResolver

	// allowPrivateHosts stands the SSRF guard down, exactly as
	// DirectPostFetcher.allowPrivateHosts does; reachable only through
	// PrivateRepoStatusConfirmerOptions.
	allowPrivateHosts bool

	clientOnce sync.Once
	client     *http.Client
}

// RepoStatusConfirmerOption configures a PDSRepoStatusConfirmer.
type RepoStatusConfirmerOption func(*PDSRepoStatusConfirmer)

// NewPDSRepoStatusConfirmer wires the getRepoStatus confirmation. SSRF
// protection is on unless PrivateRepoStatusConfirmerOptions(true) stands it
// down. The resolver is the uncached one: the PDS asked must be the account's
// current one, not a cached former host.
func NewPDSRepoStatusConfirmer(resolver identity.UncachedResolver, opts ...RepoStatusConfirmerOption) *PDSRepoStatusConfirmer {
	c := &PDSRepoStatusConfirmer{resolver: resolver}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// PrivateRepoStatusConfirmerOptions returns the private-host hatch when
// allowPrivate is set, and nothing otherwise. It mirrors
// PrivatePostFetcherOptions; do not inline it into cmd/server.
func PrivateRepoStatusConfirmerOptions(allowPrivate bool) []RepoStatusConfirmerOption {
	if !allowPrivate {
		return nil
	}
	return []RepoStatusConfirmerOption{func(c *PDSRepoStatusConfirmer) { c.allowPrivateHosts = true }} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

func (c *PDSRepoStatusConfirmer) httpClient() *http.Client {
	c.clientOnce.Do(func() {
		c.client = oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(c.allowPrivateHosts)...)
	})
	return c.client
}

// repoStatusResponse is com.atproto.sync.getRepoStatus's output. Active is a
// pointer so a missing or null field is refused rather than read as false.
type repoStatusResponse struct {
	DID    string `json:"did"`
	Active *bool  `json:"active"`
	Status string `json:"status"`
}

// confirmationFailure wraps err as an *accountstatus.ConfirmationError
// carrying failure.
func confirmationFailure(failure accountstatus.ConfirmationFailure, err error) error {
	return &accountstatus.ConfirmationError{Failure: failure, Err: err}
}

// inactiveRepoErrors maps the XRPC error names a PDS answers with status 400
// for an inactive repo onto that inactive answer. getRepoStatus itself
// declares only RepoNotFound; these names come from the sibling
// com.atproto.sync lexicons (getRepo, getLatestCommit), and a PDS that
// answers getRepoStatus with one of them is reporting that status.
var inactiveRepoErrors = map[string]accountstatus.RepoStatus{
	"RepoTakendown":   {Active: false, Status: "takendown"},
	"RepoSuspended":   {Active: false, Status: "suspended"},
	"RepoDeactivated": {Active: false, Status: "deactivated"},
}

// RepoStatus implements accountstatus.Confirmer. A 400 RepoTakendown,
// RepoSuspended or RepoDeactivated is that inactive answer. Every failure
// other than accountstatus.ErrRepoNotFound is an
// *accountstatus.ConfirmationError carrying its failure code.
func (c *PDSRepoStatusConfirmer) RepoStatus(ctx context.Context, did string) (accountstatus.RepoStatus, error) {
	if c.resolver == nil {
		return accountstatus.RepoStatus{}, fmt.Errorf("confirming the repo status of %s: no identity resolver is wired", did)
	}
	requestCtx, cancel := context.WithTimeout(ctx, repoStatusTimeout)
	defer cancel()

	declared, err := c.resolver.ResolveDeclaredIdentity(requestCtx, did)
	if err != nil {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailureIdentityUnresolved, fmt.Errorf("resolving the PDS of %s: %w", did, err))
	}
	if declared == nil || declared.PDSURL == "" {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailureIdentityUnresolved, fmt.Errorf("resolving the PDS of %s: no PDS endpoint in the DID document", did))
	}

	endpoint := strings.TrimSuffix(declared.PDSURL, "/") +
		"/xrpc/com.atproto.sync.getRepoStatus?did=" + url.QueryEscape(did)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSUnreachable, fmt.Errorf("building the getRepoStatus request for %s: %w", did, err))
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSUnreachable, fmt.Errorf("asking the PDS for the repo status of %s: %w", did, err))
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxRepoStatusResponseBytes+1))
	if err != nil {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSUnreachable, fmt.Errorf("reading the getRepoStatus response for %s: %w", did, err))
	}
	if len(body) > maxRepoStatusResponseBytes {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSInvalidResponse, fmt.Errorf("the getRepoStatus response for %s exceeds %d bytes", did, maxRepoStatusResponseBytes))
	}

	if response.StatusCode != http.StatusOK {
		var xrpcError struct {
			Error string `json:"error"`
		}
		if response.StatusCode == http.StatusBadRequest && json.Unmarshal(body, &xrpcError) == nil {
			if xrpcError.Error == "RepoNotFound" {
				return accountstatus.RepoStatus{}, fmt.Errorf("getRepoStatus for %s: %w", did, accountstatus.ErrRepoNotFound)
			}
			if inactive, ok := inactiveRepoErrors[xrpcError.Error]; ok {
				return inactive, nil
			}
		}
		detail := string(body)
		if len(detail) > maxFetchErrorDetailBytes {
			detail = detail[:maxFetchErrorDetailBytes]
		}
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSErrorStatus,
			fmt.Errorf("getRepoStatus for %s answered status %d: %s", did, response.StatusCode, strconv.Quote(detail)))
	}

	var decoded repoStatusResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSInvalidResponse, fmt.Errorf("decoding the getRepoStatus response for %s: %w", did, err))
	}
	if decoded.DID != did {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSInvalidResponse, fmt.Errorf("getRepoStatus for %s answered for a different DID %q", did, decoded.DID))
	}
	if decoded.Active == nil {
		return accountstatus.RepoStatus{}, confirmationFailure(accountstatus.FailurePDSInvalidResponse, fmt.Errorf("getRepoStatus for %s answered without an active flag", did))
	}
	if *decoded.Active {
		return accountstatus.RepoStatus{Active: true}, nil
	}
	return accountstatus.RepoStatus{Active: false, Status: decoded.Status}, nil
}
