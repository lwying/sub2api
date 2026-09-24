package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// updateCacheKey holds the update-check result of one release source.
//
// The key is namespaced by repository on purpose: the updater used to read
// lwying/sub2api's releases while caching under a repository-agnostic key, so a
// pre-switch upstream entry stayed readable after the source moved. A separate
// key per source means the upstream-era entry ("update:latest") can never be
// served as a fork result, not even when the fork lookup fails.
const updateCacheKey = "update:latest:lwying/sub2api"

type updateCache struct {
	rdb *redis.Client
}

func NewUpdateCache(rdb *redis.Client) service.UpdateCache {
	return &updateCache{rdb: rdb}
}

func (c *updateCache) GetUpdateInfo(ctx context.Context) (string, error) {
	return c.rdb.Get(ctx, updateCacheKey).Result()
}

func (c *updateCache) SetUpdateInfo(ctx context.Context, data string, ttl time.Duration) error {
	return c.rdb.Set(ctx, updateCacheKey, data, ttl).Err()
}
