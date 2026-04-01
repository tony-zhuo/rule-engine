package memory

import (
	"context"
	"sync"
	"time"

	"github.com/tony-zhuo/rule-engine/service/base/cep/model"
)

var _ model.ProgressStore = (*MemoryStore)(nil)

// MemoryStore is a thread-safe in-memory implementation for testing.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]*model.PatternProgress // keyed by progress ID
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]*model.PatternProgress)}
}

func (s *MemoryStore) Get(_ context.Context, id string) (*model.PatternProgress, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.records[id]
	if !ok {
		return nil, nil
	}
	copy := *p
	return &copy, nil
}

func (s *MemoryStore) Save(_ context.Context, p *model.PatternProgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *p
	s.records[p.ID] = &copy
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	return nil
}

func (s *MemoryStore) ListByMember(_ context.Context, memberID string) ([]*model.PatternProgress, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []*model.PatternProgress
	for _, p := range s.records {
		if p.MemberID == memberID && now.Before(p.ExpiresAt) {
			copy := *p
			out = append(out, &copy)
		}
	}
	return out, nil
}
