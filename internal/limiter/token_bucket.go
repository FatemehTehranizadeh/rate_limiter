package internal

import (
	"context"
	"sync"
	"time"
)

// About Token Bucket Limiter:
// TokenBucketLimiter implements a token bucket rate limiter.
// Tokens are added to the bucket at a fixed rate up to a maximum capacity.
// Requests consume tokens from the bucket; if no tokens are available, requests can wait or be denied.
// Imagine a bucket that holds tokens. Tokens are added to the bucket at a steady rate (e.g., 5 tokens per second).
// The bucket can hold a maximum number of tokens (capacity). When a request comes in, it tries to take a token from the bucket.
// If a token is available, the request proceeds; if not, the request can either wait for a token to become available or be denied immediately.

// For example, if the rate is 5 tokens/second and capacity is 10 tokens:
// - Initially, the bucket is full with 10 tokens.
// - If 7 requests come in at once, they can all proceed, leaving 3 tokens in the bucket.
// - If another request comes in immediately, it will have to wait until a token is added (which happens at the rate of 5 tokens/second).
// - Over time, tokens are added back to the bucket up to the capacity limit.
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
	stats     sync.Map      // per-key stats storage, map[string]*Stats
	lastRefill atomic.Pointer[time.Time]
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
	stats := tbl.getStats(key)
	select {
	case <-ctx.Done(): // check context first, if it's canceled, return false
		stats.Denied.Add(1)
		return false
	default:
		tbl.mu.Lock() // lock to check and update tokens
		defer tbl.mu.Unlock()
		if tbl.tokens > 0 { // if tokens available, consume one and return true
			tbl.tokens-- // consume one token
			stats.Allowed.Add(1)
			return true
		}
		stats.Denied.Add(1)
		return false // no tokens available, return false
	}
}

// TryAcquire attempts to acquire n tokens without blocking.
func (tbl *TokenBucketLimiter) TryAcquire(key string, n int64) bool {
	stats := tbl.getStats(key)
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	if n <= tbl.tokens { // if enough tokens available, consume n and return true
		tbl.tokens -= n // consume n tokens
		stats.Allowed.Add(uint64(n))
		return true
	}
	stats.Denied.Add(uint64(n))
	return false // not enough tokens, return false
}

// Reserve attempts to reserve a token, returning a Reservation indicating success and any delay.
// It first checks the context for cancellation.
// Then, it checks if a token is immediately available, if yes, it consumes it and returns an immediate reservation.
// If no token is available, it estimates the delay until the next token will be available based on the refill rate and fractional accumulator.
// If the rate is zero or negative, it treats it as unavailable and returns false.
func (tbl *TokenBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
	stats := tbl.getStats(key)
	select {
	case <-ctx.Done(): // check context first, if it's canceled, return false
		stats.Denied.Add(1)
		return nil, false
	default:
		tbl.mu.Lock()       // lock to check and update tokens
		if tbl.tokens > 0 { // if tokens available, consume one and return immediate reservation
			tbl.tokens-- // consume one token
			stats.Allowed.Add(1)
			tbl.mu.Unlock()
			res := &tokenReservation{
				ok:    true,
				delay: 0,
			}
			return res, true
		}

		// If rate is zero, treat as unavailable
		if tbl.rate <= 0 {
			stats.Denied.Add(1)
			tbl.mu.Unlock()
			return nil, false
		}
		// If no tokens available, estimate delay until next token
		remaining := 1.0 - tbl.acc // tokens needed is 1, so remaining is 1 - acc, where acc is fractional tokens accumulated. Imagine acc=0.3, need 0.7 more tokens to get next full token.
		if remaining < 0 {         // should not happen, but just in case
			remaining = 0
		}
		waitDur := time.Duration(remaining * float64(tbl.refillDur)) // estimate wait duration based on remaining fractional tokens and refill duration, e.g., if remaining=0.7 and refillDur=1s, waitDur=700ms
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

// Wait blocks until a token is available or the context is canceled.
// Imagine multiple goroutines calling Wait concurrently when no tokens are available.
// They will all block on the condition variable. When tokens are refilled, the refill goroutine broadcasts to wake them up.
// Each waiter checks if tokens are available, if yes, it consumes one and proceeds; otherwise, it continues to wait.
func (tbl *TokenBucketLimiter) Wait(ctx context.Context, key string) error {
	stats := tbl.getStats(key)
	stats.Pending.Add(1)
	defer stats.Pending.Add(^uint64(0)) // decrement pending count when done

	doneCh := make(chan struct{}) // channel to signal when Wait is done, so we can stop the goroutine and avoid leaks.
	go func() {
		select {
		case <-ctx.Done():
			stats.Denied.Add(1)
			tbl.cond.Broadcast() // wake up waiters to stop waiting.
		case <-doneCh:
			return
		}
	}()

	tbl.mu.Lock()
	defer func() {
		tbl.mu.Unlock()
		close(doneCh)
	}()

	// wait until tokens are available or context is canceled
	for tbl.tokens == 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tbl.cond.Wait() // wait for tokens to be refilled
	}

	tbl.tokens-- // if any tokens available, consume one
	stats.Allowed.Add(1)
	return nil
}

// Refill starts a background goroutine to periodically refill tokens based on the rate.
// It uses a ticker to trigger refills at fixed intervals (refillDur).
// On each tick, it calculates how many tokens to add based on the rate and the elapsed time.
// It accumulates fractional tokens in acc, and only adds whole tokens to the bucket.
// If adding tokens would exceed capacity, it caps the tokens at capacity.
// After refilling, it broadcasts to wake up any waiters.
func (tbl *TokenBucketLimiter) Refill() {
	tbl.once.Do(func() { // ensure refill goroutine is started only once
		go func() {
			ticker := time.NewTicker(tbl.refillDur)
			defer ticker.Stop()
			for {
				select {
				case <-tbl.stopCh: // stop signal received, exit goroutine
					return
				case <-ticker.C:
					tbl.mu.Lock()
					now := time.Now()
					tbl.lastRefill.Store(&now)
					tbl.acc += tbl.rate * (float64(tbl.refillDur) / float64(time.Second)) // accumulate tokens based on rate and refill duration. Imagine rate=5 tokens/sec, refillDur=1s, then acc+=5 each tick. if rate=2.5 tokens/sec, acc+=2.5 each tick.
					add := int64(tbl.acc)
					if add > 0 { // if we have whole tokens to add, add them to the bucket. Imagine acc=3.7, we can add 3 tokens, and keep 0.7 in acc for next time.
						if tbl.tokens+add > tbl.capacity {
							tbl.tokens = tbl.capacity
						} else {
							tbl.tokens += add
						}
						tbl.acc -= float64(add) // remove whole tokens added from acc, keep fractional part
						tbl.cond.Broadcast()    // wake up any waiters
					}
					tbl.mu.Unlock()
				}
			}
		}()
	})
}

func (tbl *TokenBucketLimiter) getStats(key string) *Stats {
    if val, ok := tbl.stats.Load(key); ok {
        if s, ok := val.(*Stats); ok {
            return s
        }
    }
    s := &Stats{}
    tbl.stats.Store(key, s)
    return s
}


func (tbl *TokenBucketLimiter) Stats(key string) *Stats {
	stats := tbl.getStats(key)

	var s Stats
	s.Allowed.Store(stats.Allowed.Load())
	s.Denied.Store(stats.Denied.Load())
	s.Pending.Store(stats.Pending.Load())
	if ts := tbl.lastRefill.Load(); ts != nil {
		s.LastRefill.Store(ts)
	} 
	return &s
}

// Close stops the refill goroutine and marks the limiter as closed.
func (tbl *TokenBucketLimiter) Close() error {
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	if !tbl.closed {
		close(tbl.stopCh)
		tbl.closed = true
	}
	tbl.cond.Broadcast() // wake up any waiters to exit
	return nil
}
