package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Session errors.
var (
	ErrSessionInvalid = errors.New("session invalid")
	ErrSessionExpired = errors.New("session expired")
)

// SessionManager mints and verifies image-studio's own stateless session
// tokens. The token is the value placed in the HttpOnly session cookie. It is
// signed with image-studio's own HMAC secret and is independent of the mother
// system's RS256 key — the entry ticket is only used once to bootstrap a
// session, after which this self-issued session carries the identity.
//
// Token format (all parts base64url, no padding):
//
//	<userID>.<expUnix>.<hmac(userID + "." + expUnix)>
//
// Stateless by design: no server-side session store, so it works in
// redis-less / multi-instance deployments.
type SessionManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // injectable for tests
}

// NewSessionManager builds a manager. secret must be non-empty; ttl must be
// positive.
func NewSessionManager(secret string, ttl time.Duration) (*SessionManager, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("identity: session secret must not be empty")
	}
	if ttl <= 0 {
		return nil, errors.New("identity: session ttl must be positive")
	}
	return &SessionManager{
		secret: []byte(secret),
		ttl:    ttl,
		now:    time.Now,
	}, nil
}

// Mint creates a signed session token for userID valid for the configured TTL.
func (m *SessionManager) Mint(userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", errors.New("identity: userID must not be empty")
	}
	exp := m.now().Add(m.ttl).Unix()
	return m.mintAt(userID, exp), nil
}

func (m *SessionManager) mintAt(userID string, exp int64) string {
	uidPart := base64.RawURLEncoding.EncodeToString([]byte(userID))
	expPart := strconv.FormatInt(exp, 10)
	payload := uidPart + "." + expPart
	sig := m.sign(payload)
	return payload + "." + sig
}

// Verify validates a session token's signature and expiry, returning the
// userID. Returns ErrSessionExpired or ErrSessionInvalid on failure.
func (m *SessionManager) Verify(token string) (string, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrSessionInvalid
	}
	uidPart, expPart, sigPart := parts[0], parts[1], parts[2]

	payload := uidPart + "." + expPart
	expectedSig := m.sign(payload)
	if subtle.ConstantTimeCompare([]byte(sigPart), []byte(expectedSig)) != 1 {
		return "", ErrSessionInvalid
	}

	exp, err := strconv.ParseInt(expPart, 10, 64)
	if err != nil {
		return "", ErrSessionInvalid
	}
	if m.now().Unix() >= exp {
		return "", ErrSessionExpired
	}

	uidBytes, err := base64.RawURLEncoding.DecodeString(uidPart)
	if err != nil {
		return "", ErrSessionInvalid
	}
	userID := strings.TrimSpace(string(uidBytes))
	if userID == "" {
		return "", ErrSessionInvalid
	}
	return userID, nil
}

// TTL reports the configured session lifetime (used for cookie Max-Age).
func (m *SessionManager) TTL() time.Duration {
	return m.ttl
}

func (m *SessionManager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
