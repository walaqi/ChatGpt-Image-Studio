// Package identity carries the multi-tenant user identity through the request
// lifecycle. It provides entry-ticket (JWT) verification, image-studio's own
// session cookie minting/verification, and helpers to stash the resolved
// userID in a request context so downstream handlers can isolate per-user data.
package identity

import (
	"context"
	"strings"
)

type ctxKey struct{}

// WithUserID returns a copy of ctx carrying userID. The value is trimmed;
// callers should only attach a non-empty value after successful auth.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, strings.TrimSpace(userID))
}

// UserIDFromContext extracts the userID previously attached with WithUserID.
// The boolean is false when no (or a blank) userID is present, so a blank
// identity can never be mistaken for a valid one downstream.
func UserIDFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	value, ok := ctx.Value(ctxKey{}).(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}
