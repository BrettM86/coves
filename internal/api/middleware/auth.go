package middleware

import (
	"Coves/internal/atproto/auth"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Context keys for storing user information
type contextKey string

const (
	UserDIDKey      contextKey = "user_did"
	JWTClaimsKey    contextKey = "jwt_claims"
	UserAccessToken contextKey = "user_access_token"
	DPoPProofKey    contextKey = "dpop_proof"
)

// AtProtoAuthMiddleware enforces atProto OAuth authentication for protected routes
// Validates JWT Bearer tokens from the Authorization header
// Supports DPoP (RFC 9449) for token binding verification
type AtProtoAuthMiddleware struct {
	jwksFetcher  auth.JWKSFetcher
	dpopVerifier *auth.DPoPVerifier
	skipVerify   bool // For Phase 1 testing only
}

// NewAtProtoAuthMiddleware creates a new atProto auth middleware
// skipVerify: if true, only parses JWT without signature verification (Phase 1)
//
//	if false, performs full signature verification (Phase 2)
//
// IMPORTANT: Call Stop() when shutting down to clean up background goroutines.
func NewAtProtoAuthMiddleware(jwksFetcher auth.JWKSFetcher, skipVerify bool) *AtProtoAuthMiddleware {
	return &AtProtoAuthMiddleware{
		jwksFetcher:  jwksFetcher,
		dpopVerifier: auth.NewDPoPVerifier(),
		skipVerify:   skipVerify,
	}
}

// Stop stops background goroutines. Call this when shutting down the server.
// This prevents goroutine leaks from the DPoP verifier's replay protection cache.
func (m *AtProtoAuthMiddleware) Stop() {
	if m.dpopVerifier != nil {
		m.dpopVerifier.Stop()
	}
}

// RequireAuth middleware ensures the user is authenticated with a valid JWT
// If not authenticated, returns 401
// If authenticated, injects user DID and JWT claims into context
func (m *AtProtoAuthMiddleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeAuthError(w, "Missing Authorization header")
			return
		}

		// Must be Bearer token
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeAuthError(w, "Invalid Authorization header format. Expected: Bearer <token>")
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		token = strings.TrimSpace(token)

		var claims *auth.Claims
		var err error

		if m.skipVerify {
			// Phase 1: Parse only (no signature verification)
			claims, err = auth.ParseJWT(token)
			if err != nil {
				log.Printf("[AUTH_FAILURE] type=parse_error ip=%s method=%s path=%s error=%v",
					r.RemoteAddr, r.Method, r.URL.Path, err)
				writeAuthError(w, "Invalid token")
				return
			}
		} else {
			// Phase 2: Full verification with signature check
			//
			// SECURITY: The access token MUST be verified before trusting any claims.
			// DPoP is an ADDITIONAL security layer, not a replacement for signature verification.
			claims, err = auth.VerifyJWT(r.Context(), token, m.jwksFetcher)
			if err != nil {
				// Token verification failed - REJECT
				// DO NOT fall back to DPoP-only verification, as that would trust unverified claims
				issuer := "unknown"
				if parsedClaims, parseErr := auth.ParseJWT(token); parseErr == nil {
					issuer = parsedClaims.Issuer
				}
				log.Printf("[AUTH_FAILURE] type=verification_failed ip=%s method=%s path=%s issuer=%s error=%v",
					r.RemoteAddr, r.Method, r.URL.Path, issuer, err)
				writeAuthError(w, "Invalid or expired token")
				return
			}

			// Token signature verified - now check if DPoP binding is required
			// If the token has a cnf.jkt claim, DPoP proof is REQUIRED
			dpopHeader := r.Header.Get("DPoP")
			hasCnfJkt := claims.Confirmation != nil && claims.Confirmation["jkt"] != nil

			if hasCnfJkt {
				// Token has DPoP binding - REQUIRE valid DPoP proof
				if dpopHeader == "" {
					log.Printf("[AUTH_FAILURE] type=missing_dpop ip=%s method=%s path=%s error=token has cnf.jkt but no DPoP header",
						r.RemoteAddr, r.Method, r.URL.Path)
					writeAuthError(w, "DPoP proof required")
					return
				}

				proof, err := m.verifyDPoPBinding(r, claims, dpopHeader)
				if err != nil {
					log.Printf("[AUTH_FAILURE] type=dpop_verification_failed ip=%s method=%s path=%s error=%v",
						r.RemoteAddr, r.Method, r.URL.Path, err)
					writeAuthError(w, "Invalid DPoP proof")
					return
				}

				// Store verified DPoP proof in context
				ctx := context.WithValue(r.Context(), DPoPProofKey, proof)
				r = r.WithContext(ctx)
			} else if dpopHeader != "" {
				// DPoP header present but token doesn't have cnf.jkt - this is suspicious
				// Log warning but don't reject (could be a misconfigured client)
				log.Printf("[AUTH_WARNING] type=unexpected_dpop ip=%s method=%s path=%s warning=DPoP header present but token has no cnf.jkt",
					r.RemoteAddr, r.Method, r.URL.Path)
			}
		}

		// Extract user DID from 'sub' claim
		userDID := claims.Subject
		if userDID == "" {
			writeAuthError(w, "Missing user DID in token")
			return
		}

		// Inject user info and access token into context
		ctx := context.WithValue(r.Context(), UserDIDKey, userDID)
		ctx = context.WithValue(ctx, JWTClaimsKey, claims)
		ctx = context.WithValue(ctx, UserAccessToken, token)

		// Call next handler
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalAuth middleware loads user info if authenticated, but doesn't require it
// Useful for endpoints that work for both authenticated and anonymous users
func (m *AtProtoAuthMiddleware) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			// Not authenticated - continue without user context
			next.ServeHTTP(w, r)
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		token = strings.TrimSpace(token)

		var claims *auth.Claims
		var err error

		if m.skipVerify {
			// Phase 1: Parse only
			claims, err = auth.ParseJWT(token)
		} else {
			// Phase 2: Full verification
			// SECURITY: Token MUST be verified before trusting claims
			claims, err = auth.VerifyJWT(r.Context(), token, m.jwksFetcher)
		}

		if err != nil {
			// Invalid token - continue without user context
			log.Printf("Optional auth failed: %v", err)
			next.ServeHTTP(w, r)
			return
		}

		// Check DPoP binding if token has cnf.jkt (after successful verification)
		// SECURITY: If token has cnf.jkt but no DPoP header, we cannot trust it
		// (could be a stolen token). Continue as unauthenticated.
		if !m.skipVerify {
			dpopHeader := r.Header.Get("DPoP")
			hasCnfJkt := claims.Confirmation != nil && claims.Confirmation["jkt"] != nil

			if hasCnfJkt {
				if dpopHeader == "" {
					// Token requires DPoP binding but no proof provided
					// Cannot trust this token - continue without auth
					log.Printf("[AUTH_WARNING] Optional auth: token has cnf.jkt but no DPoP header - treating as unauthenticated (potential token theft)")
					next.ServeHTTP(w, r)
					return
				}

				proof, err := m.verifyDPoPBinding(r, claims, dpopHeader)
				if err != nil {
					// DPoP verification failed - cannot trust this token
					log.Printf("[AUTH_WARNING] Optional auth: DPoP verification failed - treating as unauthenticated: %v", err)
					next.ServeHTTP(w, r)
					return
				}

				// DPoP verified - inject proof into context
				ctx := context.WithValue(r.Context(), UserDIDKey, claims.Subject)
				ctx = context.WithValue(ctx, JWTClaimsKey, claims)
				ctx = context.WithValue(ctx, UserAccessToken, token)
				ctx = context.WithValue(ctx, DPoPProofKey, proof)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// No DPoP binding required - inject user info and access token into context
		ctx := context.WithValue(r.Context(), UserDIDKey, claims.Subject)
		ctx = context.WithValue(ctx, JWTClaimsKey, claims)
		ctx = context.WithValue(ctx, UserAccessToken, token)

		// Call next handler
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetUserDID extracts the user's DID from the request context
// Returns empty string if not authenticated
func GetUserDID(r *http.Request) string {
	did, _ := r.Context().Value(UserDIDKey).(string)
	return did
}

// GetAuthenticatedDID extracts the authenticated user's DID from the context
// This is used by service layers for defense-in-depth validation
// Returns empty string if not authenticated
func GetAuthenticatedDID(ctx context.Context) string {
	did, _ := ctx.Value(UserDIDKey).(string)
	return did
}

// GetJWTClaims extracts the JWT claims from the request context
// Returns nil if not authenticated
func GetJWTClaims(r *http.Request) *auth.Claims {
	claims, _ := r.Context().Value(JWTClaimsKey).(*auth.Claims)
	return claims
}

// SetTestUserDID sets the user DID in the context for testing purposes
// This function should ONLY be used in tests to mock authenticated users
func SetTestUserDID(ctx context.Context, userDID string) context.Context {
	return context.WithValue(ctx, UserDIDKey, userDID)
}

// GetUserAccessToken extracts the user's access token from the request context
// Returns empty string if not authenticated
func GetUserAccessToken(r *http.Request) string {
	token, _ := r.Context().Value(UserAccessToken).(string)
	return token
}

// GetDPoPProof extracts the DPoP proof from the request context
// Returns nil if no DPoP proof was verified
func GetDPoPProof(r *http.Request) *auth.DPoPProof {
	proof, _ := r.Context().Value(DPoPProofKey).(*auth.DPoPProof)
	return proof
}

// verifyDPoPBinding verifies DPoP proof binding for an ALREADY VERIFIED token.
//
// SECURITY: This function ONLY verifies the DPoP proof and its binding to the token.
// The access token MUST be signature-verified BEFORE calling this function.
// DPoP is an ADDITIONAL security layer, not a replacement for signature verification.
//
// This prevents token theft attacks by proving the client possesses the private key
// corresponding to the public key thumbprint in the token's cnf.jkt claim.
func (m *AtProtoAuthMiddleware) verifyDPoPBinding(r *http.Request, claims *auth.Claims, dpopProofHeader string) (*auth.DPoPProof, error) {
	// Extract the cnf.jkt claim from the already-verified token
	jkt, err := auth.ExtractCnfJkt(claims)
	if err != nil {
		return nil, fmt.Errorf("token requires DPoP but missing cnf.jkt: %w", err)
	}

	// Build the HTTP URI for DPoP verification
	// Use the full URL including scheme and host
	scheme := strings.TrimSpace(r.URL.Scheme)
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto != "" {
		// Forwarded proto may contain a comma-separated list; use the first entry
		parts := strings.Split(forwardedProto, ",")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			scheme = strings.ToLower(strings.TrimSpace(parts[0]))
		}
	}
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	scheme = strings.ToLower(scheme)
	httpURI := scheme + "://" + r.Host + r.URL.Path

	// Verify the DPoP proof
	proof, err := m.dpopVerifier.VerifyDPoPProof(dpopProofHeader, r.Method, httpURI)
	if err != nil {
		return nil, fmt.Errorf("DPoP proof verification failed: %w", err)
	}

	// Verify the binding between the proof and the token
	if err := m.dpopVerifier.VerifyTokenBinding(proof, jkt); err != nil {
		return nil, fmt.Errorf("DPoP binding verification failed: %w", err)
	}

	return proof, nil
}

// writeAuthError writes a JSON error response for authentication failures
func writeAuthError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Simple error response matching XRPC error format
	response := `{"error":"AuthenticationRequired","message":"` + message + `"}`
	if _, err := w.Write([]byte(response)); err != nil {
		log.Printf("Failed to write auth error response: %v", err)
	}
}
