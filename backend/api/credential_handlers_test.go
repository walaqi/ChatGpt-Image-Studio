package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chatgpt2api/internal/config"
	"chatgpt2api/internal/credential"
	"chatgpt2api/internal/identity"
)

// fakeResolver is an in-memory credential.Resolver for handler tests.
type fakeResolver struct {
	keys       map[string]credential.KeyListResult
	creds      map[string]credential.Credential // key: userID:keyID
	listErr    error
	resolveErr error
}

func (f *fakeResolver) ListKeys(_ context.Context, userID string) (credential.KeyListResult, error) {
	if f.listErr != nil {
		return credential.KeyListResult{}, f.listErr
	}
	return f.keys[userID], nil
}

func (f *fakeResolver) Resolve(_ context.Context, userID string, keyID int64) (credential.Credential, error) {
	if f.resolveErr != nil {
		return credential.Credential{}, f.resolveErr
	}
	cred, ok := f.creds[credKey(userID, keyID)]
	if !ok {
		return credential.Credential{}, credential.ErrKeyUnusable
	}
	return cred, nil
}

func credKey(userID string, keyID int64) string {
	return userID + ":" + itoa(keyID)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	s := string(buf[i:])
	if neg {
		return "-" + s
	}
	return s
}

// newCredentialTestServer builds a server with a fake credential service and a
// session manager, returning a helper to mint a session cookie for a userID.
func newCredentialTestServer(t *testing.T, resolver credential.Resolver) (*Server, func(userID string) *http.Cookie) {
	t.Helper()
	cfg := config.New(t.TempDir())
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Identity.SessionSecret = "test-secret"
	cfg.Identity.SessionTTLSeconds = 3600

	server := NewServer(cfg, nil, nil)
	server.credService = credential.NewService(resolver, credential.NewMemorySelectionStore())

	mint := func(userID string) *http.Cookie {
		token, err := server.sessionManager.Mint(userID)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		return &http.Cookie{Name: sessionCookieName, Value: token}
	}
	return server, mint
}

func TestListCredentialKeys(t *testing.T) {
	resolver := &fakeResolver{
		keys: map[string]credential.KeyListResult{
			"12345": {
				Keys: []credential.KeyCandidate{
					{KeyID: 7, Name: "image-key", GroupName: "图片专用", Quota: 10, QuotaUsed: 2},
				},
				CanCreate: true,
			},
		},
	}
	server, mint := newCredentialTestServer(t, resolver)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/image/credential/keys", nil)
	req.AddCookie(mint("12345"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got credential.KeyListResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0].KeyID != 7 {
		t.Fatalf("keys = %+v, want one key with id 7", got.Keys)
	}
}

func TestSetAndGetCredentialSelection(t *testing.T) {
	resolver := &fakeResolver{
		creds: map[string]credential.Credential{
			"12345:7": {APIKey: "sk-abc", BaseURL: "http://gw/v1", Model: "gpt-image-2"},
		},
	}
	server, mint := newCredentialTestServer(t, resolver)
	handler := server.Handler()
	cookie := mint("12345")

	// Set selection to key 7.
	setReq := httptest.NewRequest(http.MethodPut, "/api/image/credential/current",
		strings.NewReader(`{"key_id":7}`))
	setReq.AddCookie(cookie)
	setRec := httptest.NewRecorder()
	handler.ServeHTTP(setRec, setReq)
	if setRec.Code != http.StatusOK {
		t.Fatalf("set status = %d, want 200 (body %s)", setRec.Code, setRec.Body.String())
	}

	// Get current selection back.
	getReq := httptest.NewRequest(http.MethodGet, "/api/image/credential/current", nil)
	getReq.AddCookie(cookie)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", getRec.Code)
	}
	var got struct {
		KeyID int64 `json:"key_id"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KeyID != 7 {
		t.Fatalf("remembered key_id = %d, want 7", got.KeyID)
	}
}

func TestSetCredentialSelectionRejectsUnusableKey(t *testing.T) {
	// resolver has no cred for key 99 → Resolve returns ErrKeyUnusable.
	resolver := &fakeResolver{creds: map[string]credential.Credential{}}
	server, mint := newCredentialTestServer(t, resolver)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPut, "/api/image/credential/current",
		strings.NewReader(`{"key_id":99}`))
	req.AddCookie(mint("12345"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for unusable key", rec.Code)
	}
}

func TestResolveImageCredentialUsesRememberedSelection(t *testing.T) {
	resolver := &fakeResolver{
		creds: map[string]credential.Credential{
			"12345:7": {APIKey: "sk-abc", BaseURL: "http://gw/v1", Model: "my-model"},
		},
	}
	server, _ := newCredentialTestServer(t, resolver)

	ctx := identity.WithUserID(context.Background(), "12345")

	// No selection yet → user-facing "select a key" error.
	if _, err := server.resolveImageCredential(ctx); err == nil {
		t.Fatal("expected error before a key is selected")
	}

	// Remember key 7, then resolution should return its credential.
	if err := server.credService.SetSelection(ctx, "12345", 7); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}
	cred, err := server.resolveImageCredential(ctx)
	if err != nil {
		t.Fatalf("resolveImageCredential: %v", err)
	}
	if cred.APIKey != "sk-abc" || cred.Model != "my-model" {
		t.Fatalf("cred = %+v, want sk-abc/my-model", cred)
	}
}

func TestResolveImageCredentialFallbackToConfig(t *testing.T) {
	// No credService → single-tenant fallback to global [cpa] config.
	cfg := config.New(t.TempDir())
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.CPA.BaseURL = "http://cpa/v1"
	cfg.CPA.APIKey = "sk-global"
	server := NewServer(cfg, nil, nil)
	server.credService = nil // force fallback

	cred, err := server.resolveImageCredential(context.Background())
	if err != nil {
		t.Fatalf("resolveImageCredential fallback: %v", err)
	}
	if cred.APIKey != "sk-global" || cred.BaseURL != "http://cpa/v1" {
		t.Fatalf("cred = %+v, want global cpa config", cred)
	}
}
