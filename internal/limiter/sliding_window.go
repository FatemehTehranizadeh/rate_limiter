package internal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// About Sliding Window Limiter:
// SlidingWindowLimiter implements a sliding window rate limiter.
// It tracks requests over a defined time window and allows a maximum number of requests within that window.
// Imagine a sliding window that moves over time. The window has a fixed duration (e.g., 1 minute).
// As requests come in, they are recorded with their timestamps. The limiter checks how many requests have occurred within the current window.
// If the number of requests exceeds the defined capacity, new requests are denied until older requests fall outside the window.

// For example, if the rate is 10 requests per minute and capacity is 10 requests:
// - Initially, the window is empty.
// - If 7 requests come in at once, they are all allowed, filling the window to 7 requests.
// - If another 5 requests come in immediately, only 3 can be allowed (filling to capacity of 10), and the other 2 must wait.
// - As time passes and requests fall outside the sliding window, new requests can be allowed.

type SlidingWindowLimiter struct {
	rate       float64 // tokens per second
	window     time.Duration
	capacity   int64
	store      Store
	mu         sync.Mutex
	cond       *sync.Cond             // used to block Waiters when no tokens present (or per-key conds)
	once       sync.Once              // one-time initialization (e.g., starting refill goroutine)
	pool       *sync.Pool             // reuse Reservation objects or small buffers
	timemap    map[string][]time.Time // per-key timestamps of requests
	refillDur  time.Duration          // duration between refills
	closed     bool                   // whether limiter is closed
	stats      sync.Map               // per-key stats storage, map[string]*Stats
	lastRefill atomic.Pointer[time.Time]
	stopCh     chan struct{} // signal to stop refill goroutine
}

// NewSlidingWindowLimiter creates a new SlidingWindowLimiter with the specified rate (tokens per second), window duration, and capacity.
func NewSlidingWindowLimiter(rate float64, window time.Duration, capacity int64) *SlidingWindowLimiter {
	swl := &SlidingWindowLimiter{
		rate:     rate,
		window:   window,
		capacity: capacity,
		timemap:  make(map[string][]time.Time),
		refillDur: time.Second,
		stopCh:   make(chan struct{}),
	}
	swl.cond = sync.NewCond(&swl.mu)
	swl.Refill()
	return swl
}


func filterValid(timestamps []time.Time, now time.Time, window time.Duration) []time.Time {
    cutoff := now.Add(-window)
    var valid []time.Time
    for _, t := range timestamps {
        if t.After(cutoff) {
            valid = append(valid, t)
        }
    }
    return valid
}

// Allow tries to enqueue a request; succeeds only if within capacity in the sliding window and the request is received within the time window.
func (swl *SlidingWindowLimiter) Allow(ctx context.Context, key string) bool {
    select {
    case <-ctx.Done():
        return false
    default:
        swl.mu.Lock()
        defer swl.mu.Unlock()
        now := time.Now()
        timestamps := swl.timemap[key]
        valid := filterValid(timestamps, now, swl.window)

        if int64(len(valid)) < swl.capacity {
            valid = append(valid, now)
            swl.timemap[key] = valid
            return true
        }

        swl.timemap[key] = valid
        return false
    }
}

// TryAcquire attempts to add n requests to the sliding window.
// Succeeds only if adding n does not exceed capacity.
func (swl *SlidingWindowLimiter) TryAcquire(ctx context.Context, key string, n int) bool {
    select {
    case <-ctx.Done():
        return false
    default:
        swl.mu.Lock()
        defer swl.mu.Unlock()
        now := time.Now()
        timestamps := swl.timemap[key]
        valid := filterValid(timestamps, now, swl.window)
        if int64(len(valid))+int64(n) <= swl.capacity {
            for i := 0; i < n; i++ {
                valid = append(valid, now)
            }
            swl.timemap[key] = valid
            return true
        }
        swl.timemap[key] = valid
        return false
    }
}

func (swl *SlidingWindowLimiter) Wait(ctx context.Context, key string) error {
    doneCh := make(chan struct{})
    go func() {
        select {
        case <-ctx.Done():
            swl.cond.Broadcast()
        case <-doneCh:
            return
        }
    }()

    swl.mu.Lock()
    defer func() {
        close(doneCh)
        swl.mu.Unlock()
    }()

    for {
        now := time.Now()
        timestamps := swl.timemap[key]
        valid := filterValid(timestamps, now, swl.window)

        if int64(len(valid)) < swl.capacity {
            valid = append(valid, now)
            swl.timemap[key] = valid
            return nil
        }

        if ctx.Err() != nil {
            return ctx.Err()
        }

        swl.cond.Wait()
    }
}

func (swl *SlidingWindowLimiter) Refill() {
    swl.once.Do(func() {
        go func() {
            ticker := time.NewTicker(swl.refillDur)
            defer ticker.Stop()
            for {
                select {
                case <-swl.stopCh:
                    return
                case <-ticker.C:
                    swl.mu.Lock()
                    now := time.Now()
                    for key, timestamps := range swl.timemap {
                        valid := filterValid(timestamps, now, swl.window)
                        if len(valid) == 0 {
                            delete(swl.timemap, key)
                        } else {
                            swl.timemap[key] = valid
                        }
                    }
                    swl.lastRefill.Store(&now)
                    swl.cond.Broadcast()
                    swl.mu.Unlock()
                }
            }
        }()
    })
}

func (swl *SlidingWindowLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (swl *SlidingWindowLimiter) Close() error {
    swl.mu.Lock()
    defer swl.mu.Unlock()
    if !swl.closed {
        close(swl.stopCh)
        swl.closed = true
    }
    swl.cond.Broadcast()
    return nil
}