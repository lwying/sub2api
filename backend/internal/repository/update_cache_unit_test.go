//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// The update cache lived under one repository-agnostic key while the updater
// read the upstream repository, so switching the source to the fork must not
// leave the old entry readable: a pre-seeded upstream release would otherwise be
// served whenever the fork lookup fails.

func newTestUpdateCache(t *testing.T) (*updateCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewUpdateCache(rdb).(*updateCache), mr
}

func TestUpdateCacheUsesForkScopedKey(t *testing.T) {
	cache, mr := newTestUpdateCache(t)
	ctx := context.Background()

	require.NoError(t, cache.SetUpdateInfo(ctx, `{"repo":"lwying/sub2api"}`, 5*time.Minute))

	value, err := mr.Get("update:latest:lwying/sub2api")
	require.NoError(t, err, "the fork result must be stored under its own key")
	require.Equal(t, `{"repo":"lwying/sub2api"}`, value)

	_, legacyErr := mr.Get("update:latest")
	require.Error(t, legacyErr, "the pre-switch, repository-agnostic key must not be written")

	stored, err := cache.GetUpdateInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, `{"repo":"lwying/sub2api"}`, stored)

	ttl := mr.TTL("update:latest:lwying/sub2api")
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, 5*time.Minute)
}

func TestUpdateCacheNeverReadsLegacyUpstreamEntry(t *testing.T) {
	cache, mr := newTestUpdateCache(t)
	ctx := context.Background()

	// An installation that ran the upstream-era updater still holds a fresh
	// upstream release under the old key; nothing is cached for the fork yet.
	require.NoError(t, mr.Set("update:latest", `{"latest":"9.9.9","timestamp":1}`))

	_, err := cache.GetUpdateInfo(ctx)

	require.ErrorIs(t, err, redis.Nil, "an upstream-era entry must not be served as a fork result")
}
