package imageproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"Coves/internal/core/blobs"
)

// CloudflarePurgerConfig identifies the zone, image origins and request deadline.
type CloudflarePurgerConfig struct {
	APIBase  string
	ZoneID   string
	APIToken string
	BaseURLs []string
	Timeout  time.Duration
}

// CloudflarePurger invalidates all registered image presets for each blocked pair.
type CloudflarePurger struct {
	endpoint string
	token    string
	bases    []string
	presets  []string
	client   *http.Client
}

// CDNPurgeFailure identifies a pair Cloudflare did not acknowledge.
type CDNPurgeFailure struct {
	Blob BlockedBlob
	Code string
}

// CDNPurgeResult separates acknowledged and failed owner-scoped blocks.
type CDNPurgeResult struct {
	Acknowledged []BlockedBlob
	Failed       []CDNPurgeFailure
}

// NewCloudflarePurger constructs a bounded client for a fixed Cloudflare endpoint.
func NewCloudflarePurger(config CloudflarePurgerConfig) (*CloudflarePurger, error) {
	if config.APIBase == "" {
		return nil, fmt.Errorf("APIBase is required")
	}
	parsed, err := url.Parse(config.APIBase)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return nil, fmt.Errorf("APIBase must be an absolute http(s) URL")
	}
	if config.ZoneID == "" {
		return nil, fmt.Errorf("ZoneID is required")
	}
	if config.APIToken == "" {
		return nil, fmt.Errorf("APIToken is required")
	}
	if len(config.BaseURLs) == 0 {
		return nil, fmt.Errorf("BaseURLs is required")
	}
	for _, base := range config.BaseURLs {
		parsed, err := url.Parse(base)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return nil, fmt.Errorf("BaseURLs must contain only absolute http(s) URLs")
		}
	}
	if config.Timeout <= 0 {
		return nil, fmt.Errorf("Timeout must be greater than zero")
	}
	presets := ListPresets()
	if len(presets)*len(config.BaseURLs) > 100 || len(presets) == 0 {
		return nil, fmt.Errorf("one pair must fit within 100 purge URLs")
	}
	names := make([]string, 0, len(presets))
	for _, preset := range presets {
		names = append(names, preset.Name)
	}
	sort.Strings(names)
	// coves:allow-bare-client: fixed Cloudflare API, never an untrusted media URL
	client := &http.Client{Timeout: config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &CloudflarePurger{
		endpoint: strings.TrimRight(config.APIBase, "/") + "/zones/" + url.PathEscape(config.ZoneID) + "/purge_cache",
		token:    config.APIToken,
		bases:    append([]string(nil), config.BaseURLs...),
		presets:  names,
		client:   client,
	}, nil
}

// PurgeBlobs sends whole owner/CID pairs in batches of no more than 100 URLs.
func (p *CloudflarePurger) PurgeBlobs(ctx context.Context, blocked []BlockedBlob) CDNPurgeResult {
	var result CDNPurgeResult
	if len(blocked) == 0 {
		return result
	}
	pairSize := len(p.bases) * len(p.presets)
	pairsPerRequest := 100 / pairSize
	for start := 0; start < len(blocked); start += pairsPerRequest {
		end := min(start+pairsPerRequest, len(blocked))
		batch := blocked[start:end]
		files := make([]string, 0, len(batch)*pairSize)
		for _, blob := range batch {
			for _, base := range p.bases {
				for _, preset := range p.presets {
					files = append(files, blobs.HydrateImageProxyURL(base, preset, blob.OwnerDID, blob.CID))
				}
			}
		}
		code := p.purgeBatch(ctx, files)
		if code == "" {
			result.Acknowledged = append(result.Acknowledged, batch...)
		} else {
			for _, blob := range batch {
				result.Failed = append(result.Failed, CDNPurgeFailure{Blob: blob, Code: code})
			}
		}
	}
	return result
}

// purgeBatch returns only a stable failure code; neither credentials nor remote
// response content may be reflected into moderation logs.
func (p *CloudflarePurger) purgeBatch(ctx context.Context, files []string) string {
	data, err := json.Marshal(struct {
		Files []string `json:"files"`
	}{Files: files})
	if err != nil {
		return "transport"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(data))
	if err != nil {
		return "transport"
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return "transport"
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Sprintf("http_%d", response.StatusCode)
	}
	var acknowledgement struct {
		Success bool `json:"success"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&acknowledgement); err != nil || !acknowledgement.Success {
		return "api_unsuccessful"
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "api_unsuccessful"
	}
	return ""
}
