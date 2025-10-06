package identity

import "time"

// ResolutionMethod indicates how an identity was resolved
type ResolutionMethod string

const (
	MethodCache ResolutionMethod = "cache"
	MethodDNS   ResolutionMethod = "dns"
	MethodHTTPS ResolutionMethod = "https"
)

// Identity represents a fully resolved atProto identity
type Identity struct {
	DID        string           // Decentralized Identifier (e.g., "did:plc:abc123")
	Handle     string           // Human-readable handle (e.g., "alice.bsky.social")
	PDSURL     string           // Personal Data Server URL
	ResolvedAt time.Time        // When this identity was resolved
	Method     ResolutionMethod // How it was resolved (cache, DNS, HTTPS)
}

// DIDDocument represents an AT Protocol DID document
// For now, we only extract the PDS service endpoint
type DIDDocument struct {
	DID     string
	Service []Service
}

// Service represents a service entry in a DID document
type Service struct {
	ID              string
	Type            string
	ServiceEndpoint string
}
