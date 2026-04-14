package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tony-zhuo/rule-engine/service/base/cep/model"
)

var _ model.ProgressStore = (*RedisStore)(nil)

const (
	redisProgressPrefix = "rule_engine:progress:"
	redisMemberIndex    = "rule_engine:member:"
)

// RedisStore persists pattern progress in Redis using JSON blobs.
// Each progress record is stored at key rule_engine:progress:<id> with a TTL.
// A member index set at rule_engine:member:<memberID> tracks progress IDs.
type RedisStore struct {
	client *goredis.Client
}

func NewRedisStore(client *goredis.Client) *RedisStore {
	return &RedisStore{client: client}
}

func (s *RedisStore) Get(ctx context.Context, id string) (*model.PatternProgress, error) {
	data, err := s.client.Get(ctx, redisProgressPrefix+id).Bytes()
	if err == goredis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis get progress %s: %w", id, err)
	}
	var p model.PatternProgress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("redis unmarshal progress %s: %w", id, err)
	}
	return &p, nil
}

func (s *RedisStore) Save(ctx context.Context, p *model.PatternProgress) error {
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

func (s *RedisStore) ListByMember(ctx context.Context, memberID string) ([]*model.PatternProgress, error) {
	ids, err := s.client.SMembers(ctx, redisMemberIndex+memberID).Result()
	if err != nil {
		return nil, fmt.Errorf("redis list member %s: %w", memberID, err)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Batch fetch all progress records in a single MGET round-trip.
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = redisProgressPrefix + id
	}
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis mget member %s: %w", memberID, err)
	}

	now := time.Now()
	out := make([]*model.PatternProgress, 0, len(ids))
	for i, val := range values {
		if val == nil {
			// Key expired or deleted — clean up index.
			_ = s.client.SRem(ctx, redisMemberIndex+memberID, ids[i])
			continue
		}
		str, ok := val.(string)
		if !ok {
			continue
		}
		var p model.PatternProgress
		if err := json.Unmarshal([]byte(str), &p); err != nil {
			continue
		}
		if now.After(p.ExpiresAt) {
			_ = s.client.SRem(ctx, redisMemberIndex+memberID, ids[i])
			continue
		}
		out = append(out, &p)
	}
	return out, nil
}
