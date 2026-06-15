package identity

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func newTestSessionManager(t *testing.T) *SessionManager {
	t.Helper()
	m, err := NewSessionManager("test-secret-please-change", time.Hour)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	return m
}

func TestSessionMintVerifyRoundTrip(t *testing.T) {
	m := newTestSessionManager(t)

	token, err := m.Mint("12345")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	userID, err := m.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if userID != "12345" {
		t.Fatalf("userID = %q, want 12345", userID)
	}
}

func TestSessionMintRejectsEmptyUserID(t *testing.T) {
	m := newTestSessionManager(t)
	if _, err := m.Mint("   "); err == nil {
		t.Fatal("expected error for empty userID")
	}
}

func TestSessionVerifyRejectsTamperedSignature(t *testing.T) {
	m := newTestSessionManager(t)
	token, _ := m.Mint("12345")

	// Flip a character in the signature segment.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape: %q", token)
	}
	sig := []byte(parts[2])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	if _, err := m.Verify(tampered); err != ErrSessionInvalid {
		t.Fatalf("Verify tampered = %v, want ErrSessionInvalid", err)
	}
}

func TestSessionVerifyRejectsTamperedUserID(t *testing.T) {
	m := newTestSessionManager(t)
	token, _ := m.Mint("12345")
	parts := strings.Split(token, ".")

	// Re-encode a different userID but keep the original signature.
	forged := encodeForgedUID("99999") + "." + parts[1] + "." + parts[2]
	if _, err := m.Verify(forged); err != ErrSessionInvalid {
		t.Fatalf("Verify forged uid = %v, want ErrSessionInvalid", err)
	}
}

func TestSessionVerifyExpired(t *testing.T) {
	m := newTestSessionManager(t)

	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }
	token, _ := m.Mint("12345")

	// Advance past TTL.
	m.now = func() time.Time { return base.Add(2 * time.Hour) }
	if _, err := m.Verify(token); err != ErrSessionExpired {
		t.Fatalf("Verify expired = %v, want ErrSessionExpired", err)
	}
}

func TestSessionVerifyMalformed(t *testing.T) {
	m := newTestSessionManager(t)
	for _, bad := range []string{"", "a.b", "a.b.c.d", "only-one-part"} {
		if _, err := m.Verify(bad); err != ErrSessionInvalid {
			t.Fatalf("Verify(%q) = %v, want ErrSessionInvalid", bad, err)
		}
	}
}

func TestSessionSecretMismatchRejects(t *testing.T) {
	a, _ := NewSessionManager("secret-a", time.Hour)
	b, _ := NewSessionManager("secret-b", time.Hour)

	token, _ := a.Mint("12345")
	if _, err := b.Verify(token); err != ErrSessionInvalid {
		t.Fatalf("cross-secret Verify = %v, want ErrSessionInvalid", err)
	}
}

func TestNewSessionManagerValidation(t *testing.T) {
	if _, err := NewSessionManager("", time.Hour); err == nil {
		t.Fatal("expected error for empty secret")
	}
	if _, err := NewSessionManager("secret", 0); err == nil {
		t.Fatal("expected error for non-positive ttl")
	}
}

// encodeForgedUID mirrors mintAt's userID encoding so a test can build a token
// segment with a different identity but a stale signature.
func encodeForgedUID(userID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(userID))
}
