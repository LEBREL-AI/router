package limit

import (
	"context"
	"errors"
	"sync"
)

var ErrConcurrencyExceeded = errors.New("concurrency limit exceeded")

type ReleaseFunc func()

type ConcurrencyLimiter interface {
	Acquire(context.Context, string, int) (ReleaseFunc, error)
}

type InMemory struct {
	mu     sync.Mutex
	active map[string]int
}

func NewInMemory() *InMemory {
	return &InMemory{active: make(map[string]int)}
}

// AcquireN takes n slots of key at once, all or none, under one lock: two callers near the cap
// cannot each hold a part and both fail.
func (l *InMemory) AcquireN(ctx context.Context, key string, n, maximum int) (ReleaseFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 1 || maximum < n {
		return nil, ErrConcurrencyExceeded
	}
	l.mu.Lock()
	if l.active[key]+n > maximum {
		l.mu.Unlock()
		return nil, ErrConcurrencyExceeded
	}
	l.active[key] += n
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active[key] -= n
			if l.active[key] <= 0 {
				delete(l.active, key)
			}
		})
	}, nil
}

func (l *InMemory) Acquire(ctx context.Context, key string, maximum int) (ReleaseFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maximum < 1 {
		return nil, ErrConcurrencyExceeded
	}
	l.mu.Lock()
	if l.active[key] >= maximum {
		l.mu.Unlock()
		return nil, ErrConcurrencyExceeded
	}
	l.active[key]++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active[key]--
			if l.active[key] == 0 {
				delete(l.active, key)
			}
		})
	}, nil
}
