package kv

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	client redis.UniversalClient
}

func NewRedisStore(config *RedisConfig) *RedisStore {
	return &RedisStore{
		client: createRedisClient(config),
	}
}

var _ Store = (*RedisStore)(nil)

func (s *RedisStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	res, err := s.client.Get(ctx, string(key)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return []byte(res), nil
}

func (s *RedisStore) Set(ctx context.Context, key []byte, value []byte) error {
	return s.client.Set(ctx, string(key), value, 0).Err()
}

func (s *RedisStore) Delete(ctx context.Context, key []byte) error {
	return s.client.Del(ctx, string(key)).Err()
}

var _ ManyStore = (*RedisStore)(nil)

func (s *RedisStore) GetMany(ctx context.Context, keys [][]byte) ([][]byte, error) {
	stringKeys := make([]string, len(keys))
	for i, key := range keys {
		stringKeys[i] = string(key)
	}

	res, err := s.client.MGet(ctx, stringKeys...).Result()
	if err != nil {
		return nil, err
	}

	values := make([][]byte, len(res))
	for i, v := range res {
		if v == nil {
			values[i] = nil
		} else {
			values[i] = []byte(v.(string))
		}
	}

	return values, nil
}

func (s *RedisStore) SetMany(ctx context.Context, keys [][]byte, values [][]byte) error {
	if len(keys) != len(values) {
		return errors.New("keys and values length mismatch")
	}

	variables := make([]interface{}, 0, len(keys)*2)
	for i := range keys {
		variables = append(variables, string(keys[i]), values[i])
	}

	return s.client.MSet(ctx, variables...).Err()
}

var _ LockStore = (*RedisStore)(nil)

func (s *RedisStore) Lock(ctx context.Context, key []byte) error {
	ok, res := s.client.SetNX(ctx, string(key), "locked", 0).Result()
	if res != nil {
		return res
	}

	if !ok {
		return ErrKeyLocked
	}

	return nil
}

func (s *RedisStore) Unlock(ctx context.Context, key []byte) error {
	return s.client.Del(ctx, string(key)).Err()
}

var _ ExpiringStore = (*RedisStore)(nil)

func (s *RedisStore) GetEx(ctx context.Context, key []byte, ttl time.Duration) ([]byte, error) {
	res, err := s.client.GetEx(ctx, string(key), ttl).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	return []byte(res), nil
}
func (s *RedisStore) SetEx(ctx context.Context, key []byte, value []byte, ttl time.Duration) error {
	return s.client.SetEx(ctx, string(key), value, ttl).Err()
}

func (s *RedisStore) Expire(ctx context.Context, key []byte, ttl time.Duration) error {
	err := s.client.Expire(ctx, string(key), ttl).Err()
	if errors.Is(err, redis.Nil) {
		return ErrNotFound
	}

	return err
}

func (s *RedisStore) TTL(ctx context.Context, key []byte) (time.Duration, error) {
	ttl, err := s.client.TTL(ctx, string(key)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, ErrNotFound
	}

	return ttl, err
}

var _ FullStore = (*RedisStore)(nil)
