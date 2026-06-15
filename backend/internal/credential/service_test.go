package credential

import (
	"context"
	"errors"
	"testing"
)

// fakeResolver is a programmable Resolver for service-level tests.
type fakeResolver struct {
	listResult KeyListResult
	listErr    error
	// resolveFn lets a test control per-keyID behavior.
	resolveFn func(userID string, keyID int64) (Credential, error)
	// resolveCalls records how many times Resolve was invoked.
	resolveCalls int
}

func (f *fakeResolver) ListKeys(_ context.Context, _ string) (KeyListResult, error) {
	return f.listResult, f.listErr
}

func (f *fakeResolver) Resolve(_ context.Context, userID string, keyID int64) (Credential, error) {
	f.resolveCalls++
	if f.resolveFn != nil {
		return f.resolveFn(userID, keyID)
	}
	return Credential{}, ErrUpstreamUnavailable
}

func TestServiceResolveForUserNoSelection(t *testing.T) {
	svc := NewService(&fakeResolver{}, NewMemorySelectionStore())
	if _, err := svc.ResolveForUser(context.Background(), "u1"); err != ErrNoSelection {
		t.Fatalf("ResolveForUser without selection = %v, want ErrNoSelection", err)
	}
}

func TestServiceSetSelectionThenResolve(t *testing.T) {
	want := Credential{APIKey: "sk-abc", BaseURL: "http://gw/v1", Model: "gpt-image-2"}
	res := &fakeResolver{
		resolveFn: func(_ string, keyID int64) (Credential, error) {
			if keyID == 7 {
				return want, nil
			}
			return Credential{}, ErrKeyUnusable
		},
	}
	svc := NewService(res, NewMemorySelectionStore())

	if err := svc.SetSelection(context.Background(), "u1", 7); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}
	got, err := svc.ResolveForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if got != want {
		t.Fatalf("credential = %+v, want %+v", got, want)
	}
}

func TestServiceSetSelectionRejectsUnusableKey(t *testing.T) {
	res := &fakeResolver{
		resolveFn: func(_ string, _ int64) (Credential, error) {
			return Credential{}, ErrKeyUnusable
		},
	}
	store := NewMemorySelectionStore()
	svc := NewService(res, store)

	if err := svc.SetSelection(context.Background(), "u1", 99); err != ErrKeyUnusable {
		t.Fatalf("SetSelection with bad key = %v, want ErrKeyUnusable", err)
	}
	// The bad key must NOT have been remembered.
	if _, ok, _ := store.Get(context.Background(), "u1"); ok {
		t.Fatal("unusable key should not be stored as a selection")
	}
}

func TestServiceResolveForUserClearsUnusableSelection(t *testing.T) {
	usable := true
	res := &fakeResolver{
		resolveFn: func(_ string, _ int64) (Credential, error) {
			if usable {
				return Credential{APIKey: "sk", BaseURL: "http://gw/v1"}, nil
			}
			return Credential{}, ErrKeyUnusable
		},
	}
	store := NewMemorySelectionStore()
	svc := NewService(res, store)

	if err := svc.SetSelection(context.Background(), "u1", 5); err != nil {
		t.Fatalf("SetSelection: %v", err)
	}

	// The key goes bad. ResolveForUser should clear the selection and report
	// ErrNoSelection so the caller re-prompts.
	usable = false
	if _, err := svc.ResolveForUser(context.Background(), "u1"); err != ErrNoSelection {
		t.Fatalf("ResolveForUser with now-bad key = %v, want ErrNoSelection", err)
	}
	if _, ok, _ := store.Get(context.Background(), "u1"); ok {
		t.Fatal("selection should have been cleared after ErrKeyUnusable")
	}
}

func TestServiceResolveForUserKeepsSelectionOnTransientError(t *testing.T) {
	res := &fakeResolver{
		resolveFn: func(_ string, _ int64) (Credential, error) {
			return Credential{}, ErrUpstreamUnavailable
		},
	}
	store := NewMemorySelectionStore()
	// Seed a selection directly (SetSelection would fail on the transient error).
	_ = store.Set(context.Background(), "u1", 3)
	svc := NewService(res, store)

	if _, err := svc.ResolveForUser(context.Background(), "u1"); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("ResolveForUser transient = %v, want ErrUpstreamUnavailable", err)
	}
	// Selection must survive a transient failure.
	if _, ok, _ := store.Get(context.Background(), "u1"); !ok {
		t.Fatal("selection should survive a transient upstream error")
	}
}

func TestMemorySelectionStoreRoundTrip(t *testing.T) {
	store := NewMemorySelectionStore()
	ctx := context.Background()

	if _, ok, _ := store.Get(ctx, "u1"); ok {
		t.Fatal("expected no selection initially")
	}
	if err := store.Set(ctx, "u1", 42); err != nil {
		t.Fatalf("Set: %v", err)
	}
	keyID, ok, _ := store.Get(ctx, "u1")
	if !ok || keyID != 42 {
		t.Fatalf("Get = (%d, %v), want (42, true)", keyID, ok)
	}
	if err := store.Clear(ctx, "u1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok, _ := store.Get(ctx, "u1"); ok {
		t.Fatal("expected no selection after Clear")
	}
}
