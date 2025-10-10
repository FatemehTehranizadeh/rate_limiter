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
	
}
