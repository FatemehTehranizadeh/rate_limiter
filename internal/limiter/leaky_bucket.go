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
	tokens   int64
	requests chan struct{}
	store    Store
	mu       sync.Mutex
	cond     *sync.Cond // used to block Waiters when no tokens present (or per-key conds)
	once     sync.Once  // one-time initialization (e.g., starting refill goroutine)
	pool     *sync.Pool // reuse Reservation objects or small buffers
	stopCh   chan struct{}
}

func (lbl *LeakyBucketLimiter) Allow(ctx context.Context, key string) bool {
	select {
	case <-ctx.Done():
		return false
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.capacity > 0 && lbl.tokens <= lbl.capacity {
			lbl.capacity--
			return true
		}
	}
	return false
}

func (lbl *LeakyBucketLimiter) TryAcquire(key string, n int64) bool { //pick n tokens up
	if n <= lbl.tokens && lbl.tokens <= lbl.capacity {
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		lbl.tokens -= n
		return true
	}
	return false
}

func (lbl *LeakyBucketLimiter) Reserve(ctx context.Context, key string) (Reservation, bool) {
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
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		if lbl.tokens > 0 && lbl.tokens <= lbl.capacity {
			lbl.tokens--
			res := &tokenReservation{
				ok:    true,
				delay: 0,
			}
			return res, true
		} else {
			waitingTime := time.Duration(float64(1 / lbl.rate))
			res := &tokenReservation{
				ok:    true,
				delay: waitingTime,
			}
			return res, true
		}
	}
}

func (lbl *LeakyBucketLimiter) Wait(ctx context.Context, key string) error { //block until token available using Cond or channel
	select {
	case <-ctx.Done():
		fmt.Println("Request timeout.")
		return ctx.Err()
	default:
		lbl.mu.Lock()
		defer lbl.mu.Unlock()
		for lbl.tokens == 0 {
			lbl.cond.Wait()
		}
		lbl.tokens--
		fmt.Println("The request allows!")
		return nil
	}
}

func (lbl *LeakyBucketLimiter) Refill() { //background goroutine to periodically refill tokens
	lbl.once.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Second / time.Duration(lbl.rate))
			defer ticker.Stop()
			select {
			case <-lbl.stopCh:
				close(lbl.stopCh)
			case <-ticker.C:
				lbl.mu.Lock()
				leakRate := time.Duration(math.Round(lbl.rate))
				now := time.Now()
				elapsedTime := now.Sub(lbl.lastLeak)
				if elapsedTime >= leakRate {
					lbl.tokens += int64(lbl.rate)
					lbl.cond.Broadcast()
				}
				lbl.lastLeak = lbl.lastLeak.Add(leakRate)
				lbl.mu.Unlock()
			}
		}()
	})

}

func (lbl *LeakyBucketLimiter) Stats(key string) Stats { //uses store to read counters
	return Stats{}
}

func (lbl *LeakyBucketLimiter) Close() { //stop refill goroutine, broadcast cond.
	_, ok := <-lbl.stopCh
	if ok {
		close(lbl.stopCh)
	}
	lbl.cond.Broadcast()
}
