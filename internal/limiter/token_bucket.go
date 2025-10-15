package internal

import (
	"context"
	"sync"
)

type TokenBucketLimiter struct {
	rate     float64
	capacity int64
	store    Store
	mu       sync.Mutex
	cond     *sync.Cond // used to block Waiters when no tokens present (or per-key conds)
	once     sync.Once  // one-time initialization (e.g., starting refill goroutine)
	pool     *sync.Pool // reuse Reservation objects or small buffers
}

func (tbl *TokenBucketLimiter) Allow(ctx context.Context, key string) bool {
	return true
}

func (tbl *TokenBucketLimiter) TryAcquire(key string, n int) bool { //pick n tokens up
	return true
}

func (tbl *TokenBucketLimiter) Wait(ctx context.Context, key string) error { //block until token available using Cond or channel
	return ctx.Err()
}

func (tbl *TokenBucketLimiter) Refill() { //background goroutine to periodically refill tokens
	
}

func (tbl *TokenBucketLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (tbl *TokenBucketLimiter) Close() { //stop refill goroutine, broadcast cond.
	
}










