package kv

import (
	"github.com/redis/go-redis/v9"
)

type RedisConfig struct {
	Address  string
	Password string
	DB       int
}

func DefaultRedisConfig() *RedisConfig {
	return &RedisConfig{
		Address: "localhost:6379",
		DB:      0,
	}
}

func createRedisClient(config *RedisConfig) redis.UniversalClient {
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    []string{config.Address},
		Password: config.Password,
		DB:       config.DB,
	})

	return client
}
