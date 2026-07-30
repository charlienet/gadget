package redis

import (
	"context"
	"time"

	"github.com/go-redis/redis_rate/v10"
)

// RateLimiter wraps the go-redis/redis_rate library and provides a simplified
// API for rate limiting based on the current Redis client connection.
type RateLimiter struct {
	limiter *redis_rate.Limiter
}

// RateResult contains the result of a rate limit check.
type RateResult struct {
	// Allowed indicates whether the request is allowed.
	Allowed bool

	// Remaining is the remaining quota in the current window.
	Remaining int

	// RetryAfter is the duration to wait before retrying (when not allowed).
	RetryAfter time.Duration
}

// NewRateLimiter creates a RateLimiter using the current Redis client.
func (rdb *redisClient) NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		limiter: redis_rate.NewLimiter(rdb),
	}
}

// Allow checks if a request identified by key is allowed at the given rate
// (operations per second). Returns the result including remaining quota.
func (rl *RateLimiter) Allow(ctx context.Context, key string, ratePerSec int) (*RateResult, error) {
	res, err := rl.limiter.Allow(ctx, key, redis_rate.PerSecond(ratePerSec))
	if err != nil {
		return nil, err
	}
	return &RateResult{
		Allowed:    res.Allowed == 1,
		Remaining:  res.Remaining,
		RetryAfter: res.RetryAfter,
	}, nil
}

// AllowN checks if a request identified by key is allowed at the given rate
// with a custom period. For example, AllowN(ctx, "api:1", 100, time.Minute)
// allows 100 operations per minute.
func (rl *RateLimiter) AllowN(ctx context.Context, key string, n int, per time.Duration) (*RateResult, error) {
	limit := redis_rate.Limit{
		Rate:   n,
		Burst:  n,
		Period: per,
	}
	res, err := rl.limiter.Allow(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	return &RateResult{
		Allowed:    res.Allowed == 1,
		Remaining:  res.Remaining,
		RetryAfter: res.RetryAfter,
	}, nil
}
