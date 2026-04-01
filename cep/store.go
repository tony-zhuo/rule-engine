package cep

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// PatternProgress tracks an in-flight pattern match for a single member.
type PatternProgress struct {
	ID          string         `json:"id"`
	PatternID   string         `json:"pattern_id"`
	MemberID    string         `json:"member_id"`
	CurrentStep int            `json:"current_step"`
	Variables   map[string]any `json:"variables"`
	StartedAt   time.Time      `json:"started_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

// ProgressStore persists in-flight pattern progress records.
type ProgressStore interface {
	// Get returns the progress record by its unique ID.
	Get(ctx context.Context, id string) (*PatternProgress, error)

	// Save creates or updates a progress record.
	Save(ctx context.Context, p *PatternProgress) error

	// Delete removes a progress record.
	Delete(ctx context.Context, id string) error

	// ListByMember returns all in-progress records for a given member.
	ListByMember(ctx context.Context, memberID string) ([]*PatternProgress, error)
}

// ---- MemoryStore -------------------------------------------------------

// MemoryStore is a thread-safe in-memory implementation for testing.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]*PatternProgress // keyed by progress ID
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]*PatternProgress)}
}

func (s *MemoryStore) Get(_ context.Context, id string) (*PatternProgress, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.records[id]
	if !ok {
		return nil, nil
	}
	copy := *p
	return &copy, nil
}

func (s *MemoryStore) Save(_ context.Context, p *PatternProgress) error {
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

func (s *MemoryStore) ListByMember(_ context.Context, memberID string) ([]*PatternProgress, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []*PatternProgress
	for _, p := range s.records {
		if p.MemberID == memberID && now.Before(p.ExpiresAt) {
			copy := *p
			out = append(out, &copy)
		}
	}
	return out, nil
}

// ---- RedisStore --------------------------------------------------------

const (
	redisProgressPrefix = "rule_engine:progress:"
	redisMemberIndex    = "rule_engine:member:"
)

// RedisStore persists pattern progress in Redis using JSON blobs.
// Each progress record is stored at key rule_engine:progress:<id> with a TTL.
// A member index set at rule_engine:member:<memberID> tracks progress IDs.
type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

func (s *RedisStore) Get(ctx context.Context, id string) (*PatternProgress, error) {
	data, err := s.client.Get(ctx, redisProgressPrefix+id).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis get progress %s: %w", id, err)
	}
	var p PatternProgress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("redis unmarshal progress %s: %w", id, err)
	}
	return &p, nil
}

func (s *RedisStore) Save(ctx context.Context, p *PatternProgress) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("redis marshal progress %s: %w", p.ID, err)
	}
	ttl := time.Until(p.ExpiresAt)
	if ttl <= 0 {
		ttl = time.Minute // safety floor
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, redisProgressPrefix+p.ID, data, ttl)
	pipe.SAdd(ctx, redisMemberIndex+p.MemberID, p.ID)
	pipe.Expire(ctx, redisMemberIndex+p.MemberID, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis save progress %s: %w", p.ID, err)
	}
	return nil
}

func (s *RedisStore) Delete(ctx context.Context, id string) error {
	// Retrieve first so we can clean up the member index.
	p, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	pipe := s.client.Pipeline()
	pipe.Del(ctx, redisProgressPrefix+id)
	if p != nil {
		pipe.SRem(ctx, redisMemberIndex+p.MemberID, id)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis delete progress %s: %w", id, err)
	}
	return nil
}

func (s *RedisStore) ListByMember(ctx context.Context, memberID string) ([]*PatternProgress, error) {
	ids, err := s.client.SMembers(ctx, redisMemberIndex+memberID).Result()
	if err != nil {
		return nil, fmt.Errorf("redis list member %s: %w", memberID, err)
	}

	now := time.Now()
	out := make([]*PatternProgress, 0, len(ids))
	for _, id := range ids {
		p, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if p == nil || now.After(p.ExpiresAt) {
			// Stale index entry — clean up opportunistically.
			_ = s.client.SRem(ctx, redisMemberIndex+memberID, id)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
