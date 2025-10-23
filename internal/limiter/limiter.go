package internal

import (
	"context"
	"sync/atomic"
	"time"
)

type Limiter interface {
	Allow(ctx context.Context, key string) bool                  // Check if request to API is allowable or not
	Reserve(ctx context.Context, key string) (Reservation, bool) // It reserves the consumed token, if it's successful we can use Reservation for the rest.
	Wait(ctx context.Context, key string)                        // A blocking method, which waits until the request allowed or context cancelled.
	Stats(key string) Stats                                      // returns snapshot metrics for key
	Close() error                                                // Graceful shutdown, releasing resources.
}

type Stats struct {
	Allowed    atomic.Uint64
	Denied     atomic.Uint64
	Pending    atomic.Uint64
	LastRefill atomic.Pointer[time.Time]
}

type Reservation interface {
	OK() bool
	Cancel()
	Delay() time.Duration // how long caller should wait before proceeding
}
