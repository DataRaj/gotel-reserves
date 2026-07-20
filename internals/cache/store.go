package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

var client *redis.Client

func SetClient(c *redis.Client) {
	client = c
}

// Client returns the shared Redis client (nil if SetClient was never called).
func Client() *redis.Client {
	return client
}

// Get returns the cached string for key. ok is false on cache miss or when no
// client is configured. TODO(you): decide how to surface non-miss errors.
func Get(ctx context.Context, key string) (value string, ok bool) {
	if client == nil {
		return "", false
	}
	v, err := client.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

// Set stores value under key with the given TTL. No-op when no client is set.
func Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if client == nil {
		return nil
	}
	return client.Set(ctx, key, value, ttl).Err()
}
