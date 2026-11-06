package flood

import (
	"testing"
	"time"
)

func TestABurstThenTheRefillRate(t *testing.T) {
	clock := time.Unix(1_900_000_000, 0)
	l := New(60, 3)
	l.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	if l.Allow("a") {
		t.Fatal("the fourth request at once must wait")
	}
	if !l.Allow("b") {
		t.Fatal("another address has its own bucket")
	}
	clock = clock.Add(time.Second) // one token a second at 60 a minute
	if !l.Allow("a") {
		t.Fatal("one second refills one request")
	}
	if l.Allow("a") {
		t.Fatal("and only one")
	}
}

func TestDisabledAndBounded(t *testing.T) {
	if !New(0, 0).Allow("x") || !(*Limiter)(nil).Allow("x") {
		t.Fatal("a limiter without a rate admits everything")
	}
	l := New(60, 1)
	l.maxKeys = 3
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		l.Allow(key)
	}
	if len(l.buckets) > 3 {
		t.Fatalf("buckets grow without bound: %d", len(l.buckets))
	}
}
