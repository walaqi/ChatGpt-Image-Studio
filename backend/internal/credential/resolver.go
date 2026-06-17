package credential

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
)

// Resolver lists a user's image-capable keys and resolves a chosen key into a
// plaintext credential. Implementations must be safe for concurrent use.
type Resolver interface {
	// ListKeys returns the user's image-capable key candidates (stage 1). It
	// never returns plaintext keys.
	ListKeys(ctx context.Context, userID string) (KeyListResult, error)
	// Resolve exchanges a chosen keyID for the plaintext credential (stage 2).
	// It returns ErrKeyUnusable if the key is no longer valid, or
	// ErrUpstreamUnavailable on transient failures.
	Resolve(ctx context.Context, userID string, keyID int64) (Credential, error)
}

// HTTPResolver talks to the mother system's internal credential endpoints:
//
//	GET <base>/internal/cred/keys?uid=<userID>           (stage 1)
//	GET <base>/internal/cred?uid=<userID>&key_id=<id>    (stage 2)
//
// Both carry the service-to-service secret in the X-Internal-Secret header.
// Resolved plaintext credentials are cached per (userID, keyID) for cacheTTL.
type HTTPResolver struct {
	baseURL    string
	secret     string
	gatewayURL string // overrides any base_url the mother returns; may be empty
	httpClient *http.Client
	cacheTTL   time.Duration

	mu    sync.Mutex
	cache map[string]cachedCredential
	now   func() time.Time // injectable for tests
}

type cachedCredential struct {
	cred    Credential
	expires time.Time
}

// HTTPResolverConfig configures a HTTPResolver.
type HTTPResolverConfig struct {
	// EndpointBase is the mother system's internal base URL (no trailing slash
	// required). Required.
	EndpointBase string
	// InternalSecret is the X-Internal-Secret value. Required.
	InternalSecret string
	// GatewayBaseURL, when set, overrides the base_url returned by the mother
	// system (image-studio decides the gateway address per environment).
	GatewayBaseURL string
	// RequestTimeout bounds each callback. Defaults to 20s.
	RequestTimeout time.Duration
	// CacheTTL bounds how long resolved plaintext credentials are cached.
	// Defaults to 60s.
	CacheTTL time.Duration
}

// NewHTTPResolver builds a resolver. EndpointBase and InternalSecret are
// required.
func NewHTTPResolver(cfg HTTPResolverConfig) (*HTTPResolver, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.EndpointBase), "/")
	if base == "" {
		return nil, fmt.Errorf("credential: endpoint_base is required")
	}
	if strings.TrimSpace(cfg.InternalSecret) == "" {
		return nil, fmt.Errorf("credential: internal_secret is required")
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &HTTPResolver{
		baseURL:    base,
		secret:     strings.TrimSpace(cfg.InternalSecret),
		gatewayURL: strings.TrimSpace(cfg.GatewayBaseURL),
		httpClient: &http.Client{Timeout: timeout},
		cacheTTL:   ttl,
		cache:      map[string]cachedCredential{},
		now:        time.Now,
	}, nil
}

// decodeEnvelope decodes an internal-credential response into out, transparently
// unwrapping the mother system's standard response envelope. The mother wraps
// every payload as {"code":0,"message":"success","data":{...}} (response.Success),
// but the legacy/standalone contract (and our tests) used a BARE object. To work
// with both, we read the whole body, peek for a top-level "data" field, and:
//   - envelope present → require code==0 (non-zero = mother-side error), then
//     decode the "data" object into out;
//   - no "data" field → decode the bare body into out (backward-compatible).
//
// This guards against the silent-zero-value trap: decoding an envelope straight
// into a bare struct matches no fields and yields an all-zero result (empty
// keys + can_create=false, or empty api_key), which previously surfaced as a
// bogus "image feature not enabled" / ErrKeyUnusable.
func decodeEnvelope(body io.Reader, out any) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	var envelope struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Data != nil {
		// Wrapped response. A non-zero code means the mother system reported an
		// application-level error even though the HTTP status was 2xx.
		if envelope.Code != nil && *envelope.Code != 0 {
			msg := strings.TrimSpace(envelope.Message)
			if msg == "" {
				msg = "unknown error"
			}
			return fmt.Errorf("mother system returned code %d: %s", *envelope.Code, msg)
		}
		return json.Unmarshal(envelope.Data, out)
	}
	// Bare object (no envelope): decode directly.
	return json.Unmarshal(raw, out)
}

// ListKeys implements Resolver (stage 1).
func (r *HTTPResolver) ListKeys(ctx context.Context, userID string) (KeyListResult, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return KeyListResult{}, ErrNoCredential
	}
	endpoint := r.baseURL + "/internal/cred/keys?uid=" + url.QueryEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return KeyListResult{}, fmt.Errorf("%w: build request: %v", ErrUpstreamUnavailable, err)
	}
	req.Header.Set("X-Internal-Secret", r.secret)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return KeyListResult{}, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return KeyListResult{}, fmt.Errorf("%w: list keys returned %d", ErrUpstreamUnavailable, resp.StatusCode)
	}

	var result KeyListResult
	if err := decodeEnvelope(resp.Body, &result); err != nil {
		return KeyListResult{}, fmt.Errorf("%w: decode list response: %v", ErrUpstreamUnavailable, err)
	}
	return result, nil
}

// Resolve implements Resolver (stage 2), with per-(userID,keyID) TTL caching.
func (r *HTTPResolver) Resolve(ctx context.Context, userID string, keyID int64) (Credential, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Credential{}, ErrNoCredential
	}
	cacheKey := userID + ":" + strconv.FormatInt(keyID, 10)
	if cred, ok := r.cachedGet(cacheKey); ok {
		return cred, nil
	}

	endpoint := r.baseURL + "/internal/cred?uid=" + url.QueryEscape(userID) +
		"&key_id=" + strconv.FormatInt(keyID, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("%w: build request: %v", ErrUpstreamUnavailable, err)
	}
	req.Header.Set("X-Internal-Secret", r.secret)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict ||
		resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone:
		// Key no longer valid / not owned / out of quota: not retryable.
		return Credential{}, ErrKeyUnusable
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Credential{}, fmt.Errorf("%w: resolve returned %d", ErrUpstreamUnavailable, resp.StatusCode)
	}

	var cred Credential
	if err := decodeEnvelope(resp.Body, &cred); err != nil {
		return Credential{}, fmt.Errorf("%w: decode resolve response: %v", ErrUpstreamUnavailable, err)
	}
	// image-studio's configured gateway URL wins over the mother's base_url so
	// the gateway address is environment-controlled.
	if r.gatewayURL != "" {
		cred.BaseURL = r.gatewayURL
	}
	if !cred.configured() {
		return Credential{}, ErrKeyUnusable
	}

	r.cachePut(cacheKey, cred)
	return cred, nil
}

// Invalidate drops any cached credential for (userID, keyID). Called when a
// resolved key turns out to be unusable mid-flight.
func (r *HTTPResolver) Invalidate(userID string, keyID int64) {
	cacheKey := strings.TrimSpace(userID) + ":" + strconv.FormatInt(keyID, 10)
	r.mu.Lock()
	delete(r.cache, cacheKey)
	r.mu.Unlock()
}

func (r *HTTPResolver) cachedGet(key string) (Credential, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[key]
	if !ok {
		return Credential{}, false
	}
	if !r.now().Before(entry.expires) {
		delete(r.cache, key)
		return Credential{}, false
	}
	return entry.cred, true
}

func (r *HTTPResolver) cachePut(key string, cred Credential) {
	r.mu.Lock()
	r.cache[key] = cachedCredential{cred: cred, expires: r.now().Add(r.cacheTTL)}
	r.mu.Unlock()
}
