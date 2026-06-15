package credential

import (
	"context"
	"strings"
	"sync"
)

// SelectionStore remembers which key_id a user picked for the image workbench,
// so image-studio does not re-prompt on every request. Implementations must be
// safe for concurrent use.
//
// A selection is advisory: Resolve may still reject a remembered key (expired /
// out of quota), in which case the caller clears it via Clear and re-prompts.
type SelectionStore interface {
	// Get returns the remembered key_id for userID and whether one exists.
	Get(ctx context.Context, userID string) (int64, bool, error)
	// Set records userID's chosen key_id.
	Set(ctx context.Context, userID string, keyID int64) error
	// Clear forgets userID's selection (e.g. after the key became unusable).
	Clear(ctx context.Context, userID string) error
}

// MemorySelectionStore is an in-memory SelectionStore. Selections are lost on
// restart, in which case the user is simply re-prompted once. Sufficient for
// stateless / single-instance deployments; a persistent backend can replace it
// without touching callers.
type MemorySelectionStore struct {
	mu sync.RWMutex
	m  map[string]int64
}

// NewMemorySelectionStore builds an empty in-memory selection store.
func NewMemorySelectionStore() *MemorySelectionStore {
	return &MemorySelectionStore{m: map[string]int64{}}
}

func (s *MemorySelectionStore) Get(_ context.Context, userID string) (int64, bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keyID, ok := s.m[userID]
	return keyID, ok, nil
}

func (s *MemorySelectionStore) Set(_ context.Context, userID string, keyID int64) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	s.mu.Lock()
	s.m[userID] = keyID
	s.mu.Unlock()
	return nil
}

func (s *MemorySelectionStore) Clear(_ context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	s.mu.Lock()
	delete(s.m, userID)
	s.mu.Unlock()
	return nil
}
