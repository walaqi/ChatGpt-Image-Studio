package credential

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestResolver builds an HTTPResolver pointed at srv with a fixed secret.
func newTestResolver(t *testing.T, srv *httptest.Server, gatewayURL string) *HTTPResolver {
	t.Helper()
	r, err := NewHTTPResolver(HTTPResolverConfig{
		EndpointBase:   srv.URL,
		InternalSecret: "test-secret",
		GatewayBaseURL: gatewayURL,
		CacheTTL:       time.Minute,
	})
	if err != nil {
		t.Fatalf("NewHTTPResolver: %v", err)
	}
	return r
}

func TestNewHTTPResolverValidation(t *testing.T) {
	if _, err := NewHTTPResolver(HTTPResolverConfig{InternalSecret: "x"}); err == nil {
		t.Fatal("expected error for empty endpoint_base")
	}
	if _, err := NewHTTPResolver(HTTPResolverConfig{EndpointBase: "http://x"}); err == nil {
		t.Fatal("expected error for empty internal_secret")
	}
}

func TestListKeysHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/cred/keys" {
			t.Errorf("path = %q, want /internal/cred/keys", r.URL.Path)
		}
		if r.URL.Query().Get("uid") != "12345" {
			t.Errorf("uid = %q, want 12345", r.URL.Query().Get("uid"))
		}
		if r.Header.Get("X-Internal-Secret") != "test-secret" {
			t.Errorf("missing/invalid X-Internal-Secret header")
		}
		w.Header().Set("Content-Type", "application/json")
		// Mirror the mother system's real response shape (comments-from-mother.md
		// §D): expires_at is a Unix-second integer (or null), and candidates carry
		// group_id. The first key has a numeric expiry, the second is null.
		_, _ = w.Write([]byte(`{"keys":[{"key_id":7,"name":"img","quota":10,"quota_used":2,"expires_at":1788192000,"group_id":4,"group_name":"g"},{"key_id":8,"name":"img2","quota":0,"quota_used":0,"expires_at":null,"group_id":4,"group_name":"g"}],"can_create":true,"image_group_id":9}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	res, err := r.ListKeys(context.Background(), "12345")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(res.Keys) != 2 || res.Keys[0].KeyID != 7 {
		t.Fatalf("unexpected keys: %+v", res.Keys)
	}
	if res.Keys[0].ExpiresAt == nil || *res.Keys[0].ExpiresAt != 1788192000 {
		t.Fatalf("expires_at not decoded: %+v", res.Keys[0])
	}
	if res.Keys[0].GroupID != 4 {
		t.Fatalf("group_id not decoded: %+v", res.Keys[0])
	}
	if res.Keys[1].ExpiresAt != nil {
		t.Fatalf("null expires_at should decode to nil, got %+v", res.Keys[1].ExpiresAt)
	}
	if !res.CanCreate || res.ImageGroupID == nil || *res.ImageGroupID != 9 {
		t.Fatalf("unexpected can_create/image_group_id: %+v", res)
	}
}

func TestListKeysEmptyUserID(t *testing.T) {
	r := newTestResolver(t, httptest.NewServer(http.NotFoundHandler()), "")
	if _, err := r.ListKeys(context.Background(), "  "); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("ListKeys empty uid = %v, want ErrNoCredential", err)
	}
}

func TestListKeysUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	if _, err := r.ListKeys(context.Background(), "12345"); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("ListKeys 500 = %v, want ErrUpstreamUnavailable", err)
	}
}

func TestResolveHappyPathAndGatewayOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/cred" {
			t.Errorf("path = %q, want /internal/cred", r.URL.Path)
		}
		if r.URL.Query().Get("key_id") != "7" {
			t.Errorf("key_id = %q, want 7", r.URL.Query().Get("key_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_key":"sk-abc","base_url":"http://mother/v1","model":"gpt-image-2"}`))
	}))
	defer srv.Close()

	// Gateway override replaces the mother-returned base_url.
	r := newTestResolver(t, srv, "http://studio-configured/v1")
	cred, err := r.Resolve(context.Background(), "12345", 7)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.APIKey != "sk-abc" {
		t.Fatalf("api_key = %q, want sk-abc", cred.APIKey)
	}
	if cred.BaseURL != "http://studio-configured/v1" {
		t.Fatalf("base_url = %q, want gateway override", cred.BaseURL)
	}
	if cred.Model != "gpt-image-2" {
		t.Fatalf("model = %q, want gpt-image-2", cred.Model)
	}
}

// TestListKeysUnwrapsEnvelope guards the real-world bug: the mother system wraps
// every payload as {"code":0,"message":"success","data":{...}}. Decoding that
// straight into KeyListResult matched no fields and yielded an all-zero result
// (empty keys + can_create=false), which surfaced as a bogus "image feature not
// enabled". The resolver must unwrap the envelope and read data.
func TestListKeysUnwrapsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"keys":[{"key_id":7,"name":"img","quota":10,"quota_used":2,"expires_at":null,"group_id":4,"group_name":"g"}],"can_create":true,"image_group_id":4}}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	res, err := r.ListKeys(context.Background(), "1")
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(res.Keys) != 1 || res.Keys[0].KeyID != 7 {
		t.Fatalf("envelope keys not unwrapped: %+v", res.Keys)
	}
	if !res.CanCreate || res.ImageGroupID == nil || *res.ImageGroupID != 4 {
		t.Fatalf("envelope can_create/image_group_id not unwrapped: %+v", res)
	}
}

// TestListKeysEnvelopeNonZeroCode: a non-zero code inside a 2xx envelope is an
// application-level error and must surface as an error, not a zero-value result.
func TestListKeysEnvelopeNonZeroCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":40001,"message":"bad uid","data":{}}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	if _, err := r.ListKeys(context.Background(), "1"); err == nil {
		t.Fatal("expected error for non-zero envelope code, got nil")
	}
}

// TestResolveUnwrapsEnvelope: stage 2 has the same envelope, and decoding it
// bare yielded an empty api_key → configured()==false → bogus ErrKeyUnusable.
func TestResolveUnwrapsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"api_key":"sk-xyz","base_url":"http://mother/v1","model":"gpt-image-2"}}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	cred, err := r.Resolve(context.Background(), "1", 7)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.APIKey != "sk-xyz" || cred.Model != "gpt-image-2" {
		t.Fatalf("envelope credential not unwrapped: %+v", cred)
	}
}

func TestResolveCachesByUserAndKey(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"api_key":"sk-abc","base_url":"http://mother/v1","model":"m"}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	for i := 0; i < 3; i++ {
		if _, err := r.Resolve(context.Background(), "12345", 7); err != nil {
			t.Fatalf("Resolve #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (cached)", got)
	}

	// Different key bypasses the cache.
	if _, err := r.Resolve(context.Background(), "12345", 8); err != nil {
		t.Fatalf("Resolve key 8: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 after new key", got)
	}
}

func TestResolveCacheExpiry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"api_key":"sk-abc","base_url":"http://mother/v1","model":"m"}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	if _, err := r.Resolve(context.Background(), "12345", 7); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Advance past TTL (1m) → cache miss → second upstream hit.
	r.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := r.Resolve(context.Background(), "12345", 7); err != nil {
		t.Fatalf("Resolve after expiry: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 after expiry", got)
	}
}

func TestResolveKeyUnusable(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict, http.StatusForbidden, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		r := newTestResolver(t, srv, "")
		if _, err := r.Resolve(context.Background(), "12345", 7); !errors.Is(err, ErrKeyUnusable) {
			t.Fatalf("Resolve status %d = %v, want ErrKeyUnusable", status, err)
		}
		srv.Close()
	}
}

func TestResolveTransientUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	if _, err := r.Resolve(context.Background(), "12345", 7); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("Resolve 502 = %v, want ErrUpstreamUnavailable", err)
	}
}

func TestResolveRejectsUnconfiguredCredential(t *testing.T) {
	// Mother returns a row missing the api_key → treated as unusable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"api_key":"","base_url":"http://mother/v1"}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	if _, err := r.Resolve(context.Background(), "12345", 7); !errors.Is(err, ErrKeyUnusable) {
		t.Fatalf("Resolve missing api_key = %v, want ErrKeyUnusable", err)
	}
}

func TestInvalidateDropsCache(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"api_key":"sk-abc","base_url":"http://mother/v1","model":"m"}`))
	}))
	defer srv.Close()

	r := newTestResolver(t, srv, "")
	_, _ = r.Resolve(context.Background(), "12345", 7)
	r.Invalidate("12345", 7)
	_, _ = r.Resolve(context.Background(), "12345", 7)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 after invalidate", got)
	}
}
