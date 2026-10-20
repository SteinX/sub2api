package devin

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var ErrSessionNotFound = errors.New("devin session not found")

// SessionStore keeps authorization state shared across server instances.
type SessionStore interface {
	Get(context.Context, string) ([]byte, error)
	Set(context.Context, string, []byte, time.Duration) error
	Delete(context.Context, string) error
	Acquire(context.Context, string, time.Duration) (func(), bool, error)
}
type RedisSessionStore struct{ client *redis.Client }

func NewRedisSessionStore(client *redis.Client) SessionStore {
	if client == nil {
		return nil
	}
	return &RedisSessionStore{client}
}
func (s *RedisSessionStore) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := s.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrSessionNotFound
	}
	return b, err
}
func (s *RedisSessionStore) Set(ctx context.Context, key string, b []byte, ttl time.Duration) error {
	return s.client.Set(ctx, key, b, ttl).Err()
}
func (s *RedisSessionStore) Delete(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}
func (s *RedisSessionStore) Acquire(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	ticket := uuid.NewString()
	ok, err := s.client.SetNX(ctx, key, ticket, ttl).Result()
	if err != nil || !ok {
		return nil, ok, err
	}
	return func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.client.Eval(cleanup, `if redis.call('get',KEYS[1]) == ARGV[1] then return redis.call('del',KEYS[1]) end return 0`, []string{key}, ticket).Err()
	}, true, nil
}
