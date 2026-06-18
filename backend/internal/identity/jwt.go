package identity

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Errors returned by the entry-ticket verifier. Callers map these to HTTP
// status codes (all are 401 to the client, but distinguishing them helps
// logging and tests).
var (
	// ErrTicketInvalid covers signature / claim / expiry failures.
	ErrTicketInvalid = errors.New("entry ticket invalid")
	// ErrTicketReplayed means the jti was already consumed (one-time use).
	ErrTicketReplayed = errors.New("entry ticket already used")
)

// JTIStore records consumed one-time ticket IDs. image-studio owns this
// (it holds the public key and performs verification); the mother system only
// stamps a jti into the claim. Implementations must be safe for concurrent use.
type JTIStore interface {
	// Consume atomically records jti and reports whether it was newly added.
	// It returns (true, nil) on first use, (false, nil) on replay, and a
	// non-nil error only on storage failure. ttl bounds how long the jti must
	// be remembered (>= the ticket's max lifetime).
	Consume(ctx context.Context, jti string, ttl time.Duration) (bool, error)
}

// EntryVerifier verifies short-lived RS256 entry tickets minted by the mother
// system and enforces one-time use via a JTIStore.
type EntryVerifier struct {
	publicKey *rsa.PublicKey
	issuer    string
	audience  string
	jtiStore  JTIStore
	// jtiTTL bounds how long consumed jti values are remembered. It should be
	// >= the maximum ticket lifetime (exp - iat). Defaults to 5 minutes.
	jtiTTL time.Duration
}

// NewEntryVerifier builds a verifier from a PEM-encoded RSA public key.
func NewEntryVerifier(publicKeyPEM []byte, issuer, audience string, jtiStore JTIStore) (*EntryVerifier, error) {
	key, err := jwt.ParseRSAPublicKeyFromPEM(publicKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse RSA public key: %w", err)
	}
	if jtiStore == nil {
		return nil, errors.New("identity: jtiStore is required for one-time ticket enforcement")
	}
	return &EntryVerifier{
		publicKey: key,
		issuer:    strings.TrimSpace(issuer),
		audience:  strings.TrimSpace(audience),
		jtiStore:  jtiStore,
		jtiTTL:    5 * time.Minute,
	}, nil
}

// NewEntryVerifierFromFile loads the public key from a PEM file path.
func NewEntryVerifierFromFile(path, issuer, audience string, jtiStore JTIStore) (*EntryVerifier, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key %q: %w", path, err)
	}
	return NewEntryVerifier(pem, issuer, audience, jtiStore)
}

// SetJTITTL overrides how long consumed jti values are remembered.
func (v *EntryVerifier) SetJTITTL(ttl time.Duration) {
	if ttl > 0 {
		v.jtiTTL = ttl
	}
}

// Verify validates the token's signature, issuer, audience and expiry, then
// enforces one-time use of its jti. On success it returns the subject
// (userID). On any failure it returns ErrTicketInvalid or ErrTicketReplayed.
func (v *EntryVerifier) Verify(ctx context.Context, tokenString string) (userID string, err error) {
	tokenString = strings.TrimSpace(tokenString)
	if tokenString == "" {
		return "", ErrTicketInvalid
	}

	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
	}
	if v.issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.issuer))
	}
	parser := jwt.NewParser(opts...)

	claims := jwt.RegisteredClaims{}
	token, parseErr := parser.ParseWithClaims(tokenString, &claims, func(t *jwt.Token) (any, error) {
		return v.publicKey, nil
	})
	if parseErr != nil || !token.Valid {
		return "", ErrTicketInvalid
	}

	// Audience is validated explicitly: jwt/v5's WithAudience requires the
	// claim, and we want a uniform invalid-ticket error.
	if v.audience != "" && !hasAudience(claims.Audience, v.audience) {
		return "", ErrTicketInvalid
	}

	subject := strings.TrimSpace(claims.Subject)
	jti := strings.TrimSpace(claims.ID)
	if subject == "" || jti == "" {
		return "", ErrTicketInvalid
	}

	fresh, consumeErr := v.jtiStore.Consume(ctx, jti, v.jtiTTL)
	if consumeErr != nil {
		// Storage failure: fail closed (treat as invalid) so a broken
		// blacklist can never silently allow replays.
		return "", fmt.Errorf("%w: jti store: %v", ErrTicketInvalid, consumeErr)
	}
	if !fresh {
		return "", ErrTicketReplayed
	}

	return subject, nil
}

func hasAudience(audience jwt.ClaimStrings, want string) bool {
	for _, a := range audience {
		if strings.TrimSpace(a) == want {
			return true
		}
	}
	return false
}
