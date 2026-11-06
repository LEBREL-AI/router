// Package flood is the pre-authentication ceiling of the router's direct clients: a token bucket
// per client address, so a flood of requests without a valid key costs the machine a handshake and
// a lookup, never the labs. The customer's real limit stays the concurrency of their key.
package flood

import (
	"sync"
	"time"
)

// Limiter admits up to Burst requests at once per key and refills at PerMinute requests a minute.
type Limiter struct {
	perMinute float64
	burst     float64
	maxKeys   int
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// New makes a limiter; perMinute and burst below 1 disable it (every request admitted).
func New(perMinute, burst int) *Limiter {
	return &Limiter{perMinute: float64(perMinute), burst: float64(burst), maxKeys: 100_000, now: time.Now, buckets: map[string]*bucket{}}
}

// Allow reports whether one more request from key is admitted now.
func (l *Limiter) Allow(key string) bool {
	if l == nil || l.perMinute < 1 || l.burst < 1 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.sweep(now)
		}
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.seen).Minutes() * l.perMinute
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.seen = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep forgets the keys that would be full again, and if that is not enough, the oldest ones.
func (l *Limiter) sweep(now time.Time) {
	refill := time.Duration(float64(time.Minute) * l.burst / l.perMinute)
	for key, b := range l.buckets {
		if now.Sub(b.seen) >= refill {
			delete(l.buckets, key)
		}
	}
	for len(l.buckets) >= l.maxKeys {
		var oldest string
		var at time.Time
		for key, b := range l.buckets {
			if oldest == "" || b.seen.Before(at) {
				oldest, at = key, b.seen
			}
		}
		delete(l.buckets, oldest)
	}
}
