package internal

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

type TokenBucketLimiter struct {
	rate     float64 //tokens per second
	capacity int64
	tokens   int64
	store    Store
	mu       sync.Mutex
	cond     *sync.Cond // used to block Waiters when no tokens present (or per-key conds)
	once     sync.Once  // one-time initialization (e.g., starting refill goroutine)
	pool     *sync.Pool // reuse Reservation objects or small buffers
}

func (tbl *TokenBucketLimiter) Allow(ctx context.Context, key string) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		if tbl.capacity > 0 {
			tbl.capacity--
			return true
		}
	}
	return false
}

func (tbl *TokenBucketLimiter) TryAcquire(key string, n int64) bool { //pick n tokens up
	if n <= tbl.capacity {
		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		tbl.capacity -= n
		return true
	}
	return false
}

func (tbl *TokenBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
	select {
	case <-ctx.Done():
		fmt.Println("Request timeout.")
		return
	default:
		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		if tbl.capacity == 0 {
			tbl.cond.Wait()
		}
		tbl.capacity--
		fmt.Println("The request allows!")
	}
	return nil

}

func (tbl *TokenBucketLimiter) Wait(ctx context.Context, key string) error { //block until token available using Cond or channel
	select {
	case <-ctx.Done():
		fmt.Println("Request timeout.")
		return ctx.Err()
	default:
		tbl.mu.Lock()
		tbl.mu.Unlock()
		if tbl.capacity == 0 {
			tbl.cond.Wait()
		}
		tbl.capacity = -1
		fmt.Println("The request allows!")
	}
	return nil
}

func (tbl *TokenBucketLimiter) Refill() { //background goroutine to periodically refill tokens
	ticker := time.NewTicker(time.Duration(math.Round(tbl.rate)))
	defer ticker.Stop()
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	for {
		select {
		case <-ticker.C:
			tbl.capacity += int64(tbl.rate)
			tbl.cond.Signal()
		}
	}
}

func (tbl *TokenBucketLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (tbl *TokenBucketLimiter) Close() { //stop refill goroutine, broadcast cond.

}
