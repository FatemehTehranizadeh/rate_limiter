package internal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// About Leaky Bucket Limiter:
// LeakyBucketLimiter implements a leaky bucket where `tokens` represents current fill (queued requests).
// Incoming request is allowed (added to bucket) only when fill < capacity.
// The bucket leaks at fixed `rate` tokens/second which reduces fill over time.

// Imagine a bucket with a hole in the bottom. Water (requests) can be added to the bucket, but it leaks out at a steady rate.
// If the bucket is full (at capacity), new water cannot be added until some leaks out.
// This helps to smooth out bursts of incoming requests by controlling the rate at which they are processed.

// For example, if the leak rate is 5 tokens/second and capacity is 10 tokens:
// - Initially, the bucket is empty (0 tokens).
// - If 7 requests come in at once, they can all be added to the bucket, filling it to 7 tokens.
// - If another 5 requests come in immediately, only 3 can be added (filling to capacity of 10), and the other 2 must wait.
// - Over time, tokens leak out of the bucket at the rate of 5 tokens/second, allowing new requests to be added.
type LeakyBucketLimiter struct {
	rate       float64 // tokens per second
	capacity   int64   // max bucket size
	tokens     int64   // current fill (number of requests in bucket)
	acc        float64 // fractional accumulator for leak
	store      Store
	mu         sync.Mutex
	cond       *sync.Cond    // used to block Waiters when bucket is full
	once       sync.Once     // one-time initialization (e.g., starting leak goroutine)
	pool       *sync.Pool    // reuse Reservation objects or small buffers
	stopCh     chan struct{} // signal to stop leak goroutine
	refillDur  time.Duration // duration between leaks
	closed     bool          // whether limiter is closed
	stats      sync.Map      // per-key stats storage, map[string]*Stats
	lastRefill atomic.Pointer[time.Time]
}

// NewLeakyBucketLimiter creates a new LeakyBucketLimiter with the specified leak rate (tokens per second) and capacity.
func NewLeakyBucketLimiter(rate float64, capacity int64) *LeakyBucketLimiter {
	lbl := &LeakyBucketLimiter{
		rate:      rate,
		capacity:  capacity,
		tokens:    0,
		stopCh:    make(chan struct{}),
		refillDur: time.Second,
	}
	lbl.cond = sync.NewCond(&lbl.mu) // initialize condition variable to inform waiters when space is available
	lbl.Refill()
	return lbl
}

// Allow tries to enqueue a request; succeeds only if bucket not full.
// If successful, increments tokens (the number of requests) by 1.
func (lbl *LeakyBucketLimiter) Allow(ctx context.Context, key string) bool {
	stats := lbl.getStats(key)
	select {
	case <-ctx.Done(): // check context first, if it's canceled, return false
		stats.Denied.Add(1)
		return false
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.tokens < lbl.capacity { // space available in bucket
			lbl.tokens++ // occupy one slot
			stats.Allowed.Add(1)
			return true
		}
		stats.Denied.Add(1)
		return false
	}
}

// TryAcquire attempts to add n requests to the bucket.
// Succeeds only if adding n does not exceed capacity.
// If successful, increments tokens by n.
func (lbl *LeakyBucketLimiter) TryAcquire(key string, n int64) bool {
	stats := lbl.getStats(key)
	if n <= 0 {
		stats.Denied.Add(uint64(n))
		return false
	}
	lbl.mu.Lock()
	defer lbl.mu.Unlock()
	if lbl.tokens+n <= lbl.capacity { // space available for n requests
		lbl.tokens += n // occupy n slots
		stats.Allowed.Add(uint64(n))
		return true
	}
	stats.Denied.Add(uint64(n))
	return false
}

// Reserve estimates wait time until a request can be added to the bucket.
// If space is available, returns a Reservation with zero delay.
// If not, calculates the wait time based on current fill and leak rate.
func (lbl *LeakyBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
	stats := lbl.getStats(key)
	stats.Pending.Add(1)
	defer stats.Pending.Add(^uint64(0)) // decrement pending count on return
	select {
	case <-ctx.Done(): // check context first
		stats.Denied.Add(1)
		return nil, false
	default:
		lbl.mu.Lock()
		// if space available, occupy one immediately
		if lbl.tokens < lbl.capacity {
			lbl.tokens++ // occupy one slot
			stats.Allowed.Add(1)
			lbl.mu.Unlock()
			res := &tokenReservation{
				ok:    true,
				delay: 0,
			}
			return res, true
		}
		// If rate is zero or negative, no tokens will ever leak out, so reservation fails.
		if lbl.rate <= 0 {
			lbl.mu.Unlock()
			stats.Denied.Add(1)
			return nil, false
		}
		// Imagine you have 9 tokens in a 10-capacity bucket, and rate is 1 token/sec.
		// To add one more token, you need to wait until at least one token leaks out.
		// The time to wait is determined by how many tokens need to leak to make space.
		// Why 1 is added? Because we want to account for the new token being added.
		// So, in this case, you need to wait for (9 + 1 - 10) / 1 = 0 seconds (immediate).
		// If the bucket was full (10 tokens), you'd wait (10 + 1 - 10) / 1 = 1 second.
		remaining := (float64(lbl.tokens + 1 - lbl.capacity))
		waitSec := remaining / lbl.rate // seconds to wait for enough tokens to leak
		waitDur := time.Duration(waitSec * float64(lbl.refillDur))
		lbl.mu.Unlock()

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

// Wait blocks until a request can be added to the bucket or context is done.
// If successful, increments tokens by 1.
// If context is canceled before space is available, returns context error.
func (lbl *LeakyBucketLimiter) Wait(ctx context.Context, key string) error {
	stats := lbl.getStats(key)
	stats.Pending.Add(1)
	defer stats.Pending.Add(^uint64(0))
	doneCh := make(chan struct{}) // channel to signal when Wait is done, so we can stop the goroutine and avoid leaks.
	go func() {
		select {
		case <-ctx.Done():
			stats.Denied.Add(1)
			lbl.cond.Broadcast() // wake up waiters to stop waiting.
		case <-doneCh:
			return
		}
	}()
	lbl.mu.Lock()
	defer lbl.mu.Unlock()
	for lbl.tokens >= lbl.capacity { // bucket full, wait for space
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lbl.cond.Wait()
	}
	lbl.tokens++ // occupy one slot
	stats.Allowed.Add(1)
	close(doneCh)
	return nil
}

// Refill starts a background goroutine that leaks tokens at the specified rate.
// This reduces the fill of the bucket over time, allowing new requests to be added.
func (lbl *LeakyBucketLimiter) Refill() {
	lbl.once.Do(func() { // start leak goroutine only once
		go func() {
			ticker := time.NewTicker(lbl.refillDur) // leak interval
			defer ticker.Stop()
			for {
				select {
				case <-lbl.stopCh: // stop signal received
					return
				case <-ticker.C: // time to leak tokens
					lbl.mu.Lock()
					now := time.Now()
					lbl.lastRefill.Store(&now)
					// calculate how many tokens to leak based on rate and elapsed time
					// Imagine rate is 5 tokens/sec, and refillDur is 1 second.
					// Each tick, we add 5 tokens to the accumulator.
					// If the accumulator reaches or exceeds 1, we leak that many tokens from the bucket.
					// The fractional part remains in the accumulator for future ticks.
					lbl.acc += lbl.rate * (float64(lbl.refillDur) / float64(time.Second))
					leaked := int64(lbl.acc)
					// if we have leaked tokens, reduce the fill accordingly
					if leaked > 0 {
						if leaked >= lbl.tokens {
							lbl.tokens = 0 // all tokens leaked
						} else {
							lbl.tokens -= leaked // leak tokens
						}
						lbl.acc -= float64(leaked) // retain fractional part
						lbl.cond.Broadcast()       // notify waiters that space is available
					}
					lbl.mu.Unlock()
				}
			}
		}()
	})
}

func (lbl *LeakyBucketLimiter) getStats(key string) *Stats {
	if val, ok := lbl.stats.Load(key); ok {
		if s, ok := val.(*Stats); ok {
			return s
		}
	}
	s := &Stats{}
	lbl.stats.Store(key, s)
	return s
}

func (lbl *LeakyBucketLimiter) Stats(key string) *Stats {
	stats := lbl.getStats(key)

	var s Stats
	s.Allowed.Store(stats.Allowed.Load())
	s.Denied.Store(stats.Denied.Load())
	s.Pending.Store(stats.Pending.Load())
	if ts := lbl.lastRefill.Load(); ts != nil {
		s.LastRefill.Store(ts)
	}
	return &s
}

func (lbl *LeakyBucketLimiter) Close() error {
	lbl.mu.Lock()
	defer lbl.mu.Unlock()
	if !lbl.closed {
		close(lbl.stopCh)
		lbl.closed = true
	}
	lbl.cond.Broadcast() // wake up any waiters to exit
	return nil
}
