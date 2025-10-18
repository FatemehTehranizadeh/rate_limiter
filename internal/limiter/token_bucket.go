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
	acc       float64
	store     Store
	mu        sync.Mutex
	cond      *sync.Cond    // used to block Waiters when no tokens present (or per-key conds)
	once      sync.Once     // one-time initialization (e.g., starting refill goroutine)
	pool      *sync.Pool    // reuse Reservation objects or small buffers
	stopCh    chan struct{} // signal to stop refill goroutine
	refillDur time.Duration
	closed    bool
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
		if tbl.tokens > 0 {
			tbl.tokens--
			tbl.mu.Unlock()
			res := &tokenReservation{
				ok:    true,
				delay: 0,
			}
			return res, true
		}
		if tbl.rate <= 0 {
			tbl.mu.Unlock()
			return nil, false
		}
		remainingTokens := 1.0 - tbl.acc // We need 1 token to satisfy the request, So we check how many tokens are remaining to get that 1 token
		if remainingTokens < 0 {
			remainingTokens = 0
		}
		waitDur := time.Duration(remainingTokens * float64(tbl.refillDur))
		tbl.mu.Unlock()

		res := &tokenReservation{
			ok:    true,
			delay: waitDur,
			cancel: func() {
				fmt.Println("The request has been cancelled.")
			},
		}
		return res, true
	}

}

func (tbl *TokenBucketLimiter) Wait(ctx context.Context, key string) error { //block until token available using Cond or channel
	// ensure Wait is interruptible by context: broadcast on ctx.Done()
	doneCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			tbl.cond.Broadcast()
		case <-doneCh:
			// normal path
		}
	}()

	tbl.mu.Lock()
	defer func() {
		tbl.mu.Unlock()
		close(doneCh)
	}()

	for tbl.tokens == 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tbl.cond.Wait()
	}

	tbl.tokens--
	return nil
}

func (tbl *TokenBucketLimiter) Refill() { //background goroutine to periodically refill tokens
	tbl.once.Do(func() {
		go func() {
			ticker := time.NewTicker(tbl.refillDur)
			defer ticker.Stop()
			for {
				select {
				case <-tbl.stopCh:
					return
				case <-ticker.C:
					tbl.mu.Lock()
					// accumulate fractional tokens
					tbl.acc += tbl.rate * (float64(tbl.refillDur) / float64(time.Second))
					add := int64(tbl.acc)
					if add > 0 {
						if tbl.tokens+add > tbl.capacity {
							tbl.tokens = tbl.capacity
						} else {
							tbl.tokens += add
						}
						tbl.acc -= float64(add)
						tbl.cond.Broadcast()
					}
					tbl.mu.Unlock()
				}
			}
		}()
	})
}

func (tbl *TokenBucketLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (tbl *TokenBucketLimiter) Close() error { //stop refill goroutine, broadcast cond.
	tbl.mu.Lock()
	if tbl.closed {
		tbl.mu.Unlock()
		return nil
	}
	tbl.closed = true
	close(tbl.stopCh)
	tbl.cond.Broadcast()
	tbl.mu.Unlock()
	return nil
}
