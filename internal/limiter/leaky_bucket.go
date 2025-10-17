package internal

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

type LeakyBucketLimiter struct {
	rate     float64
	capacity int64
	lastLeak time.Time
	tokens int64
	store    Store
	mu       sync.Mutex
	cond     *sync.Cond // used to block Waiters when no tokens present (or per-key conds)
	once     sync.Once  // one-time initialization (e.g., starting refill goroutine)
	pool     *sync.Pool // reuse Reservation objects or small buffers
}

func (lbl *LeakyBucketLimiter) Allow(ctx context.Context, key string) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.capacity > 0 {
			lbl.capacity--
			return true
		}
	}
	return false
}

func (lbl *LeakyBucketLimiter) TryAcquire(key string, n int64) bool { //pick n tokens up
	if n <= lbl.capacity {
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		lbl.capacity -= n
		return true
	}
	return false
}

func (lbl *TokenBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
	select {
	case <-ctx.Done():
		fmt.Println("Request timeout.")
		return nil, false
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.capacity == 0 {
			lbl.cond.Wait()
		}
		lbl.capacity --
		fmt.Println("The request allows!")
	}
	return nil

}

func (lbl *LeakyBucketLimiter) Wait(ctx context.Context, key string) error { //block until token available using Cond or channel
	select {
	case <-ctx.Done():
		fmt.Println("Request timeout.")
		return ctx.Err()
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.capacity == 0 {
			lbl.cond.Wait()
		}
		lbl.capacity --
		fmt.Println("The request allows!")
		return nil	
	}
	return nil
}

func (lbl *LeakyBucketLimiter) Refill() { //background goroutine to periodically refill tokens
	lbl.mu.Lock()
	defer lbl.mu.Unlock()
	leakRate := time.Duration(math.Round(lbl.rate))
	now := time.Now()
	elapsedTime := now.Sub(lbl.lastLeak)
	if elapsedTime >= leakRate {
		lbl.capacity += int64(lbl.rate)
		lbl.cond.Signal()
	}
	lbl.lastLeak = lbl.lastLeak.Add(leakRate)	

}

func (lbl *LeakyBucketLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (lbl *LeakyBucketLimiter) Close() { //stop refill goroutine, broadcast cond.

}
