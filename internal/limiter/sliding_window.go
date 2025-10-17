package internal

import (
	"context"
	"sync"
	"time"
)

type SlidingWindowLimiter struct {
	window   time.Duration
	capacity int64
	store    Store
	mu       sync.Mutex
	cond     *sync.Cond // used to block Waiters when no tokens present (or per-key conds)
	once     sync.Once  // one-time initialization (e.g., starting refill goroutine)
	pool     *sync.Pool // reuse Reservation objects or small buffers
}

func (tbl *SlidingWindowLimiter) Allow(ctx context.Context, key string) bool {
	return true
}

func (tbl *SlidingWindowLimiter) TryAcquire(key string, n int) bool { //pick n tokens up
	return true
}

func (tbl *SlidingWindowLimiter) Wait(ctx context.Context, key string) error { //block until token available using Cond or channel
	return ctx.Err()
}

func (tbl *SlidingWindowLimiter) Refill() { //background goroutine to periodically refill tokens

}

func (tbl *SlidingWindowLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (tbl *SlidingWindowLimiter) Close() { //stop refill goroutine, broadcast cond.

}
