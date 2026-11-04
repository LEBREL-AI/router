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
