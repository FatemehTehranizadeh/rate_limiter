package internal

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type TokenBucketLimiter struct {
	rate      float64 // tokens per second
	capacity  int64
	tokens    int64
	store     Store
	mu        sync.Mutex
	cond      *sync.Cond    // used to block Waiters when no tokens present (or per-key conds)
	once      sync.Once     // one-time initialization (e.g., starting refill goroutine)
	pool      *sync.Pool    // reuse Reservation objects or small buffers
	stopCh    chan struct{} // signal to stop refill goroutine
	refillDur time.Duration
}

type tokenReservation struct {
	ok     bool
	delay  time.Duration
	cancel func()
}

func (r *tokenReservation) OK() bool {
	return r.ok
}

func (r *tokenReservation) Delay() time.Duration {
	return r.delay
}

func (r *tokenReservation) Cancel() {
	if r.cancel != nil {
		r.cancel()
	}
}

func NewTokenBucketLimiter(rate float64, capacity int64) *TokenBucketLimiter {
	tbl := &TokenBucketLimiter{
		rate:      rate,
		capacity:  capacity,
		tokens:    capacity,
		stopCh:    make(chan struct{}),
		refillDur: time.Second,
	}
	tbl.cond = sync.NewCond(&tbl.mu)
	tbl.Refill()
	return tbl
}

func (tbl *TokenBucketLimiter) Allow(ctx context.Context, key string) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		if tbl.capacity > 0 {
			tbl.tokens--
			return true
		}
	}
	return false
}

func (tbl *TokenBucketLimiter) TryAcquire(key string, n int64) bool { //pick n tokens up
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	if n <= tbl.tokens && tbl.tokens <= tbl.capacity {
		tbl.tokens -= n
		return true
	}
	return false
}

func (tbl *TokenBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
	select {
	case <-ctx.Done():
		fmt.Println("Request timeout.")
		res := &tokenReservation{
			ok: false,
			cancel: func() {
				fmt.Println("The request can not be handled right now.")
			},
		}
		return res, false
	default:
		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		if tbl.tokens > 0 {
			tbl.tokens--
			res := &tokenReservation{
				ok:    true,
				delay: 0,
			}
			return res, true
		} else {
			waitingTime := time.Duration(float64(float64(time.Second) / tbl.rate))
			res := &tokenReservation{
				ok:    true,
				delay: waitingTime,
				cancel: func() {
					fmt.Println("Reservation cancelled.")
				},
			}
			return res, true
		}
	}

}

func (tbl *TokenBucketLimiter) Wait(ctx context.Context, key string) error { //block until token available using Cond or channel
	select {
	case <-ctx.Done():
		fmt.Println("Request timeout.")
		return ctx.Err()
	default:
		tbl.mu.Lock()
		defer tbl.mu.Unlock()
		for tbl.tokens == 0 {
			tbl.cond.Wait()
		}
		tbl.tokens--
		fmt.Println("The request allows!")
	}
	return nil
}

func (tbl *TokenBucketLimiter) Refill() { //background goroutine to periodically refill tokens
	tbl.once.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			select {
			case <-tbl.stopCh:
				close(tbl.stopCh)
			case <-ticker.C:
				tbl.mu.Lock()
				tbl.tokens = min(tbl.capacity, tbl.tokens+int64(tbl.rate))
				tbl.mu.Unlock()
				tbl.cond.Broadcast()
			}
		}()
	})
}

func (tbl *TokenBucketLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (tbl *TokenBucketLimiter) Close() { //stop refill goroutine, broadcast cond.
	_, ok := <-tbl.stopCh 
	if ok {
		close(tbl.stopCh)
	}
	tbl.cond.Broadcast()
}
