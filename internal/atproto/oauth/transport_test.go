package oauth

import (
	"net"
	"net/http"
	"testing"
)

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		// Loopback addresses
		{"IPv4 loopback", "127.0.0.1", true},
		{"IPv6 loopback", "::1", true},

		// Private IPv4 ranges
		{"Private 10.x.x.x", "10.0.0.1", true},
		{"Private 10.x.x.x edge", "10.255.255.255", true},
		{"Private 172.16.x.x", "172.16.0.1", true},
		{"Private 172.31.x.x edge", "172.31.255.255", true},
		{"Private 192.168.x.x", "192.168.1.1", true},
		{"Private 192.168.x.x edge", "192.168.255.255", true},

		// Link-local addresses
		{"Link-local IPv4", "169.254.1.1", true},
		{"Link-local IPv6", "fe80::1", true},

		// IPv6 private ranges
		{"IPv6 unique local fc00", "fc00::1", true},
		{"IPv6 unique local fd00", "fd00::1", true},

		// Public addresses
		{"Public IP 1.1.1.1", "1.1.1.1", false},
		{"Public IP 8.8.8.8", "8.8.8.8", false},
		{"Public IP 172.15.0.1", "172.15.0.1", false},  // Just before 172.16/12
		{"Public IP 172.32.0.1", "172.32.0.1", false},  // Just after 172.31/12
		{"Public IP 11.0.0.1", "11.0.0.1", false},      // Just after 10/8
		{"Public IPv6", "2001:4860:4860::8888", false}, // Google DNS
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("Failed to parse IP: %s", tt.ip)
			}

			result := isPrivateIP(ip)
			if result != tt.expected {
				t.Errorf("isPrivateIP(%s) = %v, expected %v", tt.ip, result, tt.expected)
			}
		})
	}
}

func TestIsPrivateIP_NilIP(t *testing.T) {
	result := isPrivateIP(nil)
	if result != false {
		t.Errorf("isPrivateIP(nil) = %v, expected false", result)
	}
}

func TestNewSSRFSafeHTTPClient(t *testing.T) {
	tests := []struct {
		name         string
		allowPrivate bool
	}{
		{"Production client (no private IPs)", false},
		{"Development client (allow private IPs)", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewSSRFSafeHTTPClient(tt.allowPrivate)

			if client == nil {
				t.Fatal("NewSSRFSafeHTTPClient returned nil")
			}

			if client.Timeout == 0 {
				t.Error("Expected timeout to be set")
			}

			if client.Transport == nil {
				t.Error("Expected transport to be set")
			}

			transport, ok := client.Transport.(*ssrfSafeTransport)
			if !ok {
				t.Error("Expected ssrfSafeTransport")
			}

			if transport.allowPrivate != tt.allowPrivate {
				t.Errorf("Expected allowPrivate=%v, got %v", tt.allowPrivate, transport.allowPrivate)
			}
		})
	}
}

func TestSSRFSafeHTTPClient_RedirectLimit(t *testing.T) {
	client := NewSSRFSafeHTTPClient(false)

	// Simulate checking redirect limit
	if client.CheckRedirect == nil {
		t.Fatal("Expected CheckRedirect to be set")
	}

	// Test redirect limit (5 redirects)
	var via []*http.Request
	for i := 0; i < 5; i++ {
		req := &http.Request{}
		via = append(via, req)
	}

	err := client.CheckRedirect(nil, via)
	if err == nil {
		t.Error("Expected error for too many redirects")
	}
	if err.Error() != "too many redirects" {
		t.Errorf("Expected 'too many redirects' error, got: %v", err)
	}

	// Test within limit (4 redirects)
	via = via[:4]
	err = client.CheckRedirect(nil, via)
	if err != nil {
		t.Errorf("Expected no error for 4 redirects, got: %v", err)
	}
}
