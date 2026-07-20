package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

func Connect(url string) (*redis.Client, error) {
	if url == "" {
		return nil, fmt.Errorf("cache.Connect: REDIS_URL must not be empty")
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("cache.Connect: parse url: %w", err)
	}

	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("cache.Connect: ping failed: %w", err)
	}

	return rdb, nil
}
