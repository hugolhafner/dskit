package kv

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound  = errors.New("key not found")
	ErrKeyLocked = errors.New("key is locked")
)

type Store interface {
	Get(ctx context.Context, key []byte) ([]byte, error)
	Set(ctx context.Context, key []byte, value []byte) error
	Delete(ctx context.Context, key []byte) error
}

type ManyStore interface {
	Store
	GetMany(ctx context.Context, keys [][]byte) ([][]byte, error)
	SetMany(ctx context.Context, keys [][]byte, values [][]byte) error
}

type LockStore interface {
	Store
	Lock(ctx context.Context, key []byte) error
	Unlock(ctx context.Context, key []byte) error
}

type ExpiringStore interface {
	Store
	GetEx(ctx context.Context, key []byte, ttl time.Duration) ([]byte, error)
	SetEx(ctx context.Context, key []byte, value []byte, ttl time.Duration) error
	Expire(ctx context.Context, key []byte, ttl time.Duration) error
	TTL(ctx context.Context, key []byte) (time.Duration, error)
}

type FullStore interface {
	ManyStore
	LockStore
	ExpiringStore
}
