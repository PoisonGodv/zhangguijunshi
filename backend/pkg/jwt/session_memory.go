package jwt

// Memory SessionStore: stdlib-only, for local dev and unit tests. It keeps
// refresh tokens and blocked jtis in maps with absolute expiry times; a
// background goroutine sweeps expired entries once a minute so a long-running
// dev server does not leak. Close stops the goroutine.

import (
	"context"
	"sync"
	"time"
)

// MemorySessionStore is the in-memory SessionStore implementation.
type MemorySessionStore struct {
	mu         sync.RWMutex
	refresh    map[string]refreshEntry
	blacklist  map[string]time.Time
	stopOnce   sync.Once
	stopCh     chan struct{}
	stopped    bool
	cleanupTTL time.Duration
}

type refreshEntry struct {
	data   RefreshData
	expiry time.Time
}

// NewMemorySessionStore constructs a started MemorySessionStore. The cleanup
// sweep runs every minute; Close stops it.
func NewMemorySessionStore() *MemorySessionStore {
	s := &MemorySessionStore{
		refresh:    make(map[string]refreshEntry),
		blacklist:  make(map[string]time.Time),
		stopCh:     make(chan struct{}),
		cleanupTTL: time.Minute,
	}
	go s.sweep()
	return s
}

func (s *MemorySessionStore) sweep() {
	t := time.NewTicker(s.cleanupTTL)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.gc(time.Now())
		}
	}
}

func (s *MemorySessionStore) gc(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.refresh {
		if !e.expiry.After(now) {
			delete(s.refresh, k)
		}
	}
	for k, exp := range s.blacklist {
		if !exp.After(now) {
			delete(s.blacklist, k)
		}
	}
}

// IssueRefresh implements SessionStore.
func (s *MemorySessionStore) IssueRefresh(_ context.Context, data RefreshData, ttl time.Duration) (string, error) {
	tok, err := generateRefreshToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.refresh[tok] = refreshEntry{data: data, expiry: time.Now().Add(ttl)}
	s.mu.Unlock()
	return tok, nil
}

// GetRefresh implements SessionStore.
func (s *MemorySessionStore) GetRefresh(_ context.Context, token string) (RefreshData, error) {
	s.mu.RLock()
	e, ok := s.refresh[token]
	s.mu.RUnlock()
	if !ok || !e.expiry.After(time.Now()) {
		return RefreshData{}, ErrRefreshNotFound
	}
	return e.data, nil
}

// RevokeRefresh implements SessionStore.
func (s *MemorySessionStore) RevokeRefresh(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refresh, token)
	return nil
}

// BlockJTI implements SessionStore.
func (s *MemorySessionStore) BlockJTI(_ context.Context, jti string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blacklist[jti] = time.Now().Add(ttl)
	return nil
}

// IsBlocked implements SessionStore.
func (s *MemorySessionStore) IsBlocked(_ context.Context, jti string) (bool, error) {
	s.mu.RLock()
	exp, ok := s.blacklist[jti]
	s.mu.RUnlock()
	return ok && exp.After(time.Now()), nil
}

// Close stops the background sweep. Repeated calls are no-ops.
func (s *MemorySessionStore) Close() error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.stopCh)
	})
	return nil
}
