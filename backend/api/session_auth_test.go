package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chatgpt2api/internal/config"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// newSessionTestServer builds a server with the entry-ticket verifier wired to
// a freshly generated RSA keypair. It returns the server and the private key so
// the test can mint tickets the way the mother system would.
func newSessionTestServer(t *testing.T) (*Server, *rsa.PrivateKey) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	rootDir := t.TempDir()
	keyPath := filepath.Join(rootDir, "data", "jwt_public.pem")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(keyPath, pubPEM, 0o644); err != nil {
		t.Fatalf("WriteFile pub key: %v", err)
	}

	cfg := config.New(rootDir)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Identity.JWTPublicKeyPath = "data/jwt_public.pem"
	cfg.Identity.JWTIssuer = "sub2api"
	cfg.Identity.JWTAudience = "image-studio"
	cfg.Identity.SessionSecret = "test-session-secret"
	cfg.Identity.SessionTTLSeconds = 3600

	server := NewServer(cfg)
	if server.entryVerifier == nil {
		t.Fatal("entryVerifier is nil; expected it to be configured from the public key")
	}
	return server, priv
}

// mintTicket signs an entry ticket the way the mother system would.
func mintTicket(t *testing.T, priv *rsa.PrivateKey, claims jwt.RegisteredClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return signed
}

func defaultTicketClaims() jwt.RegisteredClaims {
	now := time.Now()
	return jwt.RegisteredClaims{
		Subject:   "12345",
		Issuer:    "sub2api",
		Audience:  jwt.ClaimStrings{"image-studio"},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(45 * time.Second)),
		ID:        uuid.NewString(),
	}
}

func TestSessionExchangeAndProtectedRoute(t *testing.T) {
	server, priv := newSessionTestServer(t)
	handler := server.Handler()

	ticket := mintTicket(t, priv, defaultTicketClaims())

	// Exchange ticket → session cookie.
	req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+ticket)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/session = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie set")
	}
	if !sessionCookie.HttpOnly {
		t.Error("session cookie should be HttpOnly")
	}

	// The cookie should authorize a protected UI route and resolve the userID.
	req2 := httptest.NewRequest(http.MethodGet, "/api/image/conversations", nil)
	req2.AddCookie(sessionCookie)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	// 200 (server storage on) or 400 (server storage disabled) both prove auth
	// passed; 401 would mean the cookie was rejected.
	if rec2.Code == http.StatusUnauthorized {
		t.Fatalf("protected route rejected valid session cookie: %d", rec2.Code)
	}
}

func TestSessionExchangeRejectsReplayedTicket(t *testing.T) {
	server, priv := newSessionTestServer(t)
	handler := server.Handler()

	ticket := mintTicket(t, priv, defaultTicketClaims())

	exchange := func() int {
		req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
		req.Header.Set("Authorization", "Bearer "+ticket)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := exchange(); code != http.StatusOK {
		t.Fatalf("first exchange = %d, want 200", code)
	}
	if code := exchange(); code != http.StatusUnauthorized {
		t.Fatalf("replayed ticket exchange = %d, want 401", code)
	}
}

func TestSessionExchangeRejectsWrongAudience(t *testing.T) {
	server, priv := newSessionTestServer(t)
	handler := server.Handler()

	claims := defaultTicketClaims()
	claims.Audience = jwt.ClaimStrings{"some-other-app"}
	ticket := mintTicket(t, priv, claims)

	req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+ticket)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-audience ticket = %d, want 401", rec.Code)
	}
}

func TestSessionExchangeRejectsExpiredTicket(t *testing.T) {
	server, priv := newSessionTestServer(t)
	handler := server.Handler()

	claims := defaultTicketClaims()
	now := time.Now()
	claims.IssuedAt = jwt.NewNumericDate(now.Add(-5 * time.Minute))
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(-4 * time.Minute))
	ticket := mintTicket(t, priv, claims)

	req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+ticket)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired ticket = %d, want 401", rec.Code)
	}
}

func TestSessionExchangeMissingTicket(t *testing.T) {
	server, _ := newSessionTestServer(t)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing ticket = %d, want 400", rec.Code)
	}
}

func TestSessionUnavailableWithoutVerifier(t *testing.T) {
	rootDir := t.TempDir()
	cfg := config.New(rootDir)
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// No JWTPublicKeyPath configured → entry verifier nil.
	server := NewServer(cfg)
	if server.entryVerifier != nil {
		t.Fatal("expected nil entryVerifier without configured key")
	}
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no verifier = %d, want 503", rec.Code)
	}
}
