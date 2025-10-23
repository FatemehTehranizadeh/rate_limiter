package internal

import (
	"context"
	"sync"
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
	rate      float64 // tokens per second
	capacity  int64   // max bucket size
	tokens    int64   // current fill (number of requests in bucket)
	acc       float64 // fractional accumulator for leak
	store     Store
	mu        sync.Mutex
	cond      *sync.Cond    // used to block Waiters when bucket is full
	once      sync.Once     // one-time initialization (e.g., starting leak goroutine)
	pool      *sync.Pool    // reuse Reservation objects or small buffers
	stopCh    chan struct{} // signal to stop leak goroutine
	refillDur time.Duration // duration between leaks
	closed    bool          // whether limiter is closed
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
	select {
	case <-ctx.Done(): // check context first, if it's canceled, return false
		return false
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.tokens < lbl.capacity { // space available in bucket
			lbl.tokens++ // occupy one slot
			return true
		}
		return false
	}
}

// TryAcquire attempts to add n requests to the bucket.
// Succeeds only if adding n does not exceed capacity.
// If successful, increments tokens by n.
func (lbl *LeakyBucketLimiter) TryAcquire(key string, n int64) bool {
	lbl.mu.Lock()
	defer lbl.mu.Unlock()
	if lbl.tokens+n <= lbl.capacity { // space available for n requests
		lbl.tokens += n // occupy n slots
		return true
	}
	return false
}

func (lbl *LeakyBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
	select {
	case <-ctx.Done():
		return nil, false
	default:
		lbl.mu.Lock()
		// if space available, occupy one immediately
		if lbl.tokens < lbl.capacity {
			lbl.tokens++
			lbl.mu.Unlock()
			res := &tokenReservation{
				ok:    true,
				delay: 0,
			}
			return res, true
		}
		// otherwise, estimate wait until next leak frees space
		if lbl.rate <= 0 {
			lbl.mu.Unlock()
			return nil, false
		}
		// Imagine you have 9 tokens in a 10-capacity bucket, and rate is 1 token/sec.
		// To add one more token, you need to wait until at least one token leaks out.
		// The time to wait is determined by how many tokens need to leak to make space.
		remaining := (float64(lbl.tokens + 1 - lbl.capacity)) // check that how much empty space is needed
		waitSec := remaining / lbl.rate
		waitDur := time.Duration(waitSec * float64(lbl.refillDur))
		lbl.mu.Unlock()

		res := &tokenReservation{
			ok:    true,
			delay: waitDur,
			cancel: func() {
				// no-op
			},
		}
		return res, true
	}
}

func (lbl *LeakyBucketLimiter) Wait(ctx context.Context, key string) error {
	go func() {
		<-ctx.Done()
		lbl.cond.Broadcast()
	}()

	lbl.mu.Lock()
	defer lbl.mu.Unlock()
	for lbl.tokens >= lbl.capacity {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lbl.cond.Wait()
	}
	lbl.tokens++
	return nil
}

func (lbl *LeakyBucketLimiter) Refill() { // background goroutine that leaks tokens (reduces fill)
	lbl.once.Do(func() {
		go func() {
			ticker := time.NewTicker(lbl.refillDur)
			defer ticker.Stop()
			for {
				select {
				case <-lbl.stopCh:
					return
				case <-ticker.C:
					lbl.mu.Lock()
					lbl.acc += lbl.rate * (float64(lbl.refillDur) / float64(time.Second))
					leaked := int64(lbl.acc)
					if leaked > 0 {
						if leaked >= lbl.tokens {
							lbl.tokens = 0
						} else {
							lbl.tokens -= leaked
						}
						lbl.acc -= float64(leaked)
						lbl.cond.Broadcast()
					}
					lbl.mu.Unlock()
				}
			}
		}()
	})
}

func (lbl *LeakyBucketLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (lbl *LeakyBucketLimiter) Close() error { //stop refill goroutine, broadcast cond.
	lbl.mu.Lock()
	if lbl.closed {
		lbl.mu.Unlock()
		return nil
	}
	lbl.closed = true
	close(lbl.stopCh)
	lbl.cond.Broadcast()
	lbl.mu.Unlock()
	return nil
}
