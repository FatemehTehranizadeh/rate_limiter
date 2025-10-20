package internal

import (
	"context"
	"sync"
	"time"
)

type TokenBucketLimiter struct {
	rate      float64 // tokens per second
	capacity  int64   // How much the bucket can hold
	tokens    int64   // current number of tokens
	acc       float64 // fractional token accumulator
	store     Store
	mu        sync.Mutex
	cond      *sync.Cond    // used to block Waiters when no tokens present (or per-key conds)
	once      sync.Once     // one-time initialization (e.g., starting refill goroutine)
	pool      *sync.Pool    // reuse Reservation objects or small buffers
	stopCh    chan struct{} // signal to stop refill goroutine
	refillDur time.Duration // duration between refills
	closed    bool          // whether limiter is closed
}

// The status of reservations and acquisitions.
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

// NewTokenBucketLimiter creates a new TokenBucketLimiter with the specified rate (tokens per second) and capacity.
func NewTokenBucketLimiter(rate float64, capacity int64) *TokenBucketLimiter {
	tbl := &TokenBucketLimiter{
		rate:      rate,
		capacity:  capacity,
		tokens:    capacity,
		stopCh:    make(chan struct{}),
		refillDur: time.Second,
	}
	tbl.cond = sync.NewCond(&tbl.mu) // initialize condition variable to inform waiters when tokens are added
	tbl.Refill()                     // start the refill goroutine
	return tbl
}

func (tbl *TokenBucketLimiter) Allow(ctx context.Context, key string) bool {
	select {
	case <-ctx.Done(): // check context first, if it's canceled, return false
		return false
	default:
		tbl.mu.Lock() // lock to check and update tokens
		defer tbl.mu.Unlock()
		if tbl.tokens > 0 { // if tokens available, consume one and return true
			tbl.tokens-- // consume one token
			return true
		}
		return false // no tokens available, return false
	}
}

// TryAcquire attempts to acquire n tokens without blocking.
func (tbl *TokenBucketLimiter) TryAcquire(key string, n int64) bool { 
	defer tbl.mu.Unlock()
	if n <= tbl.tokens { // if enough tokens available, consume n and return true
		tbl.tokens -= n // consume n tokens
		return true
	}
	return false // not enough tokens, return false
}

// Reserve attempts to reserve a token, returning a Reservation indicating success and any delay.
// It first checks the context for cancellation.
// Then, it checks if a token is immediately available, if yes, it consumes it and returns an immediate reservation.
// If no token is available, it estimates the delay until the next token will be available based on the refill rate and fractional accumulator.
// If the rate is zero or negative, it treats it as unavailable and returns false.
func (tbl *TokenBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
	select {
	case <-ctx.Done():
		return nil, false
	default:
		tbl.mu.Lock()
		// If token available, consume and return immediate reservation
		if tbl.tokens > 0 {
			tbl.tokens--
			tbl.mu.Unlock()
			res := &tokenReservation{
				ok:    true,
				delay: 0,
			}
			return res, true
		}

		// estimate delay until next token available based on fractional accumulator and rate
		// If rate is zero, treat as unavailable
		if tbl.rate <= 0 {
			tbl.mu.Unlock()
			return nil, false
		}
		// calculate wait: remaining fractional to next whole token
		remaining := 1.0 - tbl.acc
		if remaining < 0 {
			remaining = 0
		}
		waitDur := time.Duration(remaining * float64(tbl.refillDur))
		tbl.mu.Unlock()

		res := &tokenReservation{
			ok:    true,
			delay: waitDur,
			cancel: func() {
				// no-op for now
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
