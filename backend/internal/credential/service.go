package credential

import "context"

// Service ties a Resolver and a SelectionStore together to implement the
// "user-selects + remember + fall back to prompt" flow from
// docs/multi-tenant-redesign.md §4.2.
type Service struct {
	resolver  Resolver
	selection SelectionStore
}

// NewService builds a Service. Both arguments are required.
func NewService(resolver Resolver, selection SelectionStore) *Service {
	return &Service{resolver: resolver, selection: selection}
}

// Resolver exposes the underlying resolver (for stage-1 ListKeys calls from the
// key-picker handler).
func (s *Service) Resolver() Resolver { return s.resolver }

// Selection exposes the underlying selection store (for the picker's
// get/set-current handlers).
func (s *Service) Selection() SelectionStore { return s.selection }

// ResolveForUser returns the plaintext credential for the user's remembered
// key. It is the per-request entry point used by the image pipeline.
//
// Flow:
//  1. No remembered selection → ErrNoSelection (caller prompts the picker).
//  2. Remembered selection → Resolve it.
//  3. Resolve says the key is unusable → clear the selection and return
//     ErrNoSelection so the caller re-prompts.
//  4. Transient upstream failure → propagate ErrUpstreamUnavailable (do NOT
//     clear the selection; the key may still be fine).
func (s *Service) ResolveForUser(ctx context.Context, userID string) (Credential, error) {
	keyID, ok, err := s.selection.Get(ctx, userID)
	if err != nil {
		return Credential{}, err
	}
	if !ok {
		return Credential{}, ErrNoSelection
	}

	cred, err := s.resolver.Resolve(ctx, userID, keyID)
	if err == nil {
		return cred, nil
	}
	if err == ErrKeyUnusable {
		// Remembered key went bad: forget it so the next request re-prompts.
		_ = s.selection.Clear(ctx, userID)
		return Credential{}, ErrNoSelection
	}
	return Credential{}, err
}

// SetSelection records the user's chosen key after verifying it resolves. This
// makes selection atomic with a validity check: the picker never stores a
// key_id that cannot produce a credential.
func (s *Service) SetSelection(ctx context.Context, userID string, keyID int64) error {
	if _, err := s.resolver.Resolve(ctx, userID, keyID); err != nil {
		return err
	}
	return s.selection.Set(ctx, userID, keyID)
}
