package identity

import (
	"context"
	"sync"
	"time"
)

// MemoryJTIStore is an in-process, mutex-guarded JTIStore. It suits single
// instance / redis-less deployments. Entries expire after their TTL; expired
// entries are removed lazily on access and by an occasional sweep so the map
// can't grow unbounded under steady traffic.
//
// For multi-instance deployments a shared store (e.g. redis SETNX with TTL)
// should be used instead so a replayed ticket is caught across instances.
type MemoryJTIStore struct {
	mu        sync.Mutex
	seen      map[string]time.Time // jti -> expiry
	now       func() time.Time     // injectable for tests
	lastSweep time.Time
}

// NewMemoryJTIStore creates an empty in-memory store.
func NewMemoryJTIStore() *MemoryJTIStore {
	return &MemoryJTIStore{
		seen: make(map[string]time.Time),
		now:  time.Now,
	}
}

// Consume records jti and reports whether it was newly added. A jti whose
// previous record has expired is treated as fresh again (its replay window has
// passed). ttl bounds how long the jti is remembered.
func (s *MemoryJTIStore) Consume(_ context.Context, jti string, ttl time.Duration) (bool, error) {
	if jti == "" {
		// An empty jti is never accepted as fresh; the caller already rejects
		// tickets without a jti, this is just defense in depth.
		return false, nil
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(now)

	if exp, ok := s.seen[jti]; ok && now.Before(exp) {
		return false, nil // still within replay window
	}
	s.seen[jti] = now.Add(ttl)
	return true, nil
}

// sweepLocked drops expired entries at most once per minute to keep the map
// bounded without paying a full scan on every Consume.
func (s *MemoryJTIStore) sweepLocked(now time.Time) {
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < time.Minute {
		return
	}
	for jti, exp := range s.seen {
		if !now.Before(exp) {
			delete(s.seen, jti)
		}
	}
	s.lastSweep = now
}
