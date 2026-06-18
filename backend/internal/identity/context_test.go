package identity

import (
	"context"
	"testing"
)

func TestUserIDContextRoundTrip(t *testing.T) {
	ctx := WithUserID(context.Background(), "12345")
	got, ok := UserIDFromContext(ctx)
	if !ok {
		t.Fatal("UserIDFromContext: ok = false, want true")
	}
	if got != "12345" {
		t.Fatalf("userID = %q, want 12345", got)
	}
}

func TestUserIDFromContextMissing(t *testing.T) {
	if _, ok := UserIDFromContext(context.Background()); ok {
		t.Fatal("expected ok = false for empty context")
	}
}

func TestWithUserIDEmptyIsNotStored(t *testing.T) {
	// An empty userID must never look like a valid identity downstream.
	ctx := WithUserID(context.Background(), "   ")
	if _, ok := UserIDFromContext(ctx); ok {
		t.Fatal("expected ok = false when userID is blank")
	}
}
