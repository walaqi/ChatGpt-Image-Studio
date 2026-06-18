package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKeypair generates an RSA keypair and returns the private key (for signing
// test tickets) plus the PEM-encoded public key (fed to NewEntryVerifier).
func testKeypair(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return priv, pubPEM
}

type ticketClaims struct {
	sub string
	iss string
	aud string
	jti string
	exp time.Time
	iat time.Time
}

func mintTicket(t *testing.T, priv *rsa.PrivateKey, c ticketClaims) string {
	t.Helper()
	claims := jwt.RegisteredClaims{
		Subject:   c.sub,
		Issuer:    c.iss,
		ID:        c.jti,
		ExpiresAt: jwt.NewNumericDate(c.exp),
		IssuedAt:  jwt.NewNumericDate(c.iat),
	}
	if c.aud != "" {
		claims.Audience = jwt.ClaimStrings{c.aud}
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("sign ticket: %v", err)
	}
	return signed
}

func goodClaims() ticketClaims {
	now := time.Now()
	return ticketClaims{
		sub: "12345",
		iss: "sub2api",
		aud: "image-studio",
		jti: "jti-" + time.Now().Format("150405.000000000"),
		iat: now,
		exp: now.Add(60 * time.Second),
	}
}

func newTestVerifier(t *testing.T, pubPEM []byte) *EntryVerifier {
	t.Helper()
	v, err := NewEntryVerifier(pubPEM, "sub2api", "image-studio", NewMemoryJTIStore())
	if err != nil {
		t.Fatalf("NewEntryVerifier: %v", err)
	}
	return v
}

func TestEntryVerifyHappyPath(t *testing.T) {
	priv, pubPEM := testKeypair(t)
	v := newTestVerifier(t, pubPEM)

	token := mintTicket(t, priv, goodClaims())
	userID, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if userID != "12345" {
		t.Fatalf("userID = %q, want 12345", userID)
	}
}

func TestEntryVerifyReplayRejected(t *testing.T) {
	priv, pubPEM := testKeypair(t)
	v := newTestVerifier(t, pubPEM)

	token := mintTicket(t, priv, goodClaims())
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	// Same token again must be rejected as a replay.
	if _, err := v.Verify(context.Background(), token); err != ErrTicketReplayed {
		t.Fatalf("replay Verify = %v, want ErrTicketReplayed", err)
	}
}

func TestEntryVerifyExpired(t *testing.T) {
	priv, pubPEM := testKeypair(t)
	v := newTestVerifier(t, pubPEM)

	c := goodClaims()
	c.iat = time.Now().Add(-5 * time.Minute)
	c.exp = time.Now().Add(-1 * time.Minute)
	token := mintTicket(t, priv, c)

	if _, err := v.Verify(context.Background(), token); err != ErrTicketInvalid {
		t.Fatalf("expired Verify = %v, want ErrTicketInvalid", err)
	}
}

func TestEntryVerifyWrongIssuer(t *testing.T) {
	priv, pubPEM := testKeypair(t)
	v := newTestVerifier(t, pubPEM)

	c := goodClaims()
	c.iss = "someone-else"
	token := mintTicket(t, priv, c)

	if _, err := v.Verify(context.Background(), token); err != ErrTicketInvalid {
		t.Fatalf("wrong-issuer Verify = %v, want ErrTicketInvalid", err)
	}
}

func TestEntryVerifyWrongAudience(t *testing.T) {
	priv, pubPEM := testKeypair(t)
	v := newTestVerifier(t, pubPEM)

	c := goodClaims()
	c.aud = "other-app"
	token := mintTicket(t, priv, c)

	if _, err := v.Verify(context.Background(), token); err != ErrTicketInvalid {
		t.Fatalf("wrong-audience Verify = %v, want ErrTicketInvalid", err)
	}
}

func TestEntryVerifyWrongKeyRejected(t *testing.T) {
	priv1, _ := testKeypair(t)
	_, pubPEM2 := testKeypair(t)
	// Verifier trusts key2's public key, but the ticket is signed with key1.
	v := newTestVerifier(t, pubPEM2)

	token := mintTicket(t, priv1, goodClaims())
	if _, err := v.Verify(context.Background(), token); err != ErrTicketInvalid {
		t.Fatalf("wrong-key Verify = %v, want ErrTicketInvalid", err)
	}
}

func TestEntryVerifyMissingJTIRejected(t *testing.T) {
	priv, pubPEM := testKeypair(t)
	v := newTestVerifier(t, pubPEM)

	c := goodClaims()
	c.jti = ""
	token := mintTicket(t, priv, c)

	if _, err := v.Verify(context.Background(), token); err != ErrTicketInvalid {
		t.Fatalf("missing-jti Verify = %v, want ErrTicketInvalid", err)
	}
}

func TestEntryVerifyRejectsHS256(t *testing.T) {
	_, pubPEM := testKeypair(t)
	v := newTestVerifier(t, pubPEM)

	// Forge an HS256 token — the alg-confusion attack the RS256-only allowlist
	// is meant to block.
	claims := jwt.RegisteredClaims{
		Subject:   "12345",
		Issuer:    "sub2api",
		Audience:  jwt.ClaimStrings{"image-studio"},
		ID:        "jti-hs",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte("secret"))
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	if _, err := v.Verify(context.Background(), signed); err != ErrTicketInvalid {
		t.Fatalf("HS256 Verify = %v, want ErrTicketInvalid", err)
	}
}

func TestMemoryJTIStoreConsume(t *testing.T) {
	s := NewMemoryJTIStore()
	ctx := context.Background()

	fresh, err := s.Consume(ctx, "abc", time.Minute)
	if err != nil || !fresh {
		t.Fatalf("first Consume = (%v, %v), want (true, nil)", fresh, err)
	}
	fresh, err = s.Consume(ctx, "abc", time.Minute)
	if err != nil || fresh {
		t.Fatalf("second Consume = (%v, %v), want (false, nil)", fresh, err)
	}
}

func TestMemoryJTIStoreExpiry(t *testing.T) {
	s := NewMemoryJTIStore()
	ctx := context.Background()
	base := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	if fresh, _ := s.Consume(ctx, "abc", time.Minute); !fresh {
		t.Fatal("first Consume should be fresh")
	}
	// After the TTL elapses, the same jti is treated as new again (it would
	// have failed the ticket exp check first in real use; this just verifies
	// the store does not leak entries forever).
	s.now = func() time.Time { return base.Add(2 * time.Minute) }
	if fresh, _ := s.Consume(ctx, "abc", time.Minute); !fresh {
		t.Fatal("Consume after expiry should be fresh again")
	}
}
