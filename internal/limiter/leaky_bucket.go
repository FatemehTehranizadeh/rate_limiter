package internal

import (
	"context"
	"sync"
	"time"
)

// LeakyBucketLimiter implements a leaky bucket where `tokens` represents current fill (queued requests).
// Incoming request is allowed (added to bucket) only when fill < capacity.
// The bucket leaks at fixed `rate` tokens/second which reduces fill over time.
type LeakyBucketLimiter struct {
	rate      float64
	capacity  int64
	tokens    int64   // current fill
	acc       float64 // fractional accumulator for leak
	store     Store
	mu        sync.Mutex
	cond      *sync.Cond // used to block Waiters when bucket is full
	once      sync.Once  // one-time initialization (e.g., starting leak goroutine)
	pool      *sync.Pool // reuse Reservation objects or small buffers
	stopCh    chan struct{}
	refillDur time.Duration
	closed    bool
}

func NewLeakyBucketLimiter(rate float64, capacity int64) *LeakyBucketLimiter {
	lbl := &LeakyBucketLimiter{
		rate:      rate,
		capacity:  capacity,
		tokens:    0,
		stopCh:    make(chan struct{}),
		refillDur: time.Second,
	}
	lbl.cond = sync.NewCond(&lbl.mu)
	lbl.Refill()
	return lbl
}

// Allow tries to enqueue a request; succeeds only if bucket not full.
func (lbl *LeakyBucketLimiter) Allow(ctx context.Context, key string) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.tokens < lbl.capacity {
			lbl.tokens++
			return true
		}
		return false
	}
}

func (lbl *LeakyBucketLimiter) TryAcquire(key string, n int64) bool { //attempt to add n to bucket
	lbl.mu.Lock()
	defer lbl.mu.Unlock()
	if lbl.tokens+n <= lbl.capacity {
		lbl.tokens += n
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
