package router

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Patience is how a route waits when every upstream it may ask refuses for the moment. A provider's
// rate limit or overload passes in seconds, and so does a broken connection: the route is asked again
// after a wait, and the customer gets the answer a moment late instead of the provider's "not now".
type Patience struct {
	// Budget is how long after a request's first call another round may still begin (default 12 s).
	// Negative: one round and no pauses; the route answers with whatever that round got.
	Budget time.Duration
	// First is the wait after a failure that names none (default 500 ms). It doubles with each round
	// of a request, and with each refusal in a row of an upstream that is being left alone.
	First time.Duration
	// Longest caps a wait the router chooses itself (default 4 s). What an upstream asks for with
	// Retry-After is kept as asked, up to pauseLongest.
	Longest time.Duration
	// Jitter spreads the requests that wait for the same moment (nil: up to a quarter more, at random),
	// so a provider that reopens is not asked by all of them in the same instant.
	Jitter func(wait time.Duration) time.Duration
}

const (
	defaultPatienceBudget  = 12 * time.Second
	defaultPatienceFirst   = 500 * time.Millisecond
	defaultPatienceLongest = 4 * time.Second
	// pauseLongest caps what an upstream may ask for with Retry-After. A longer one (an hour, a date far
	// away) speaks of a quota, not of a moment: the upstream is asked again after this much, which costs
	// it two calls a minute while it keeps refusing.
	pauseLongest = 30 * time.Second
)

// busyStatuses are an upstream saying it is at its limit with Lebrel (429) or overloaded (503): the
// request waits and asks again, the upstream may be left alone by every request (pause), and what it
// said is never relayed as if it were about the customer. passingStatuses are failures of the moment
// that say nothing of a limit: the request that met one asks again after a wait.
var (
	busyStatuses    = map[int]bool{429: true, 503: true}
	passingStatuses = map[int]bool{408: true, 425: true, 500: true, 502: true, 504: true}
)

func (p Patience) withDefaults() Patience {
	if p.Budget == 0 {
		p.Budget = defaultPatienceBudget
	}
	if p.First <= 0 {
		p.First = defaultPatienceFirst
	}
	if p.Longest <= 0 {
		p.Longest = defaultPatienceLongest
	}
	if p.Longest < p.First {
		p.Longest = p.First
	}
	return p
}

// wait is the router's own wait after the nth failure in a row: First, doubled each time, up to Longest.
func (p Patience) wait(n int) time.Duration {
	wait := p.First
	for i := 1; i < n && wait < p.Longest; i++ {
		wait *= 2
	}
	if wait > p.Longest {
		wait = p.Longest
	}
	return wait
}

// spread adds the jitter to a wait.
func (p Patience) spread(wait time.Duration) time.Duration {
	if p.Jitter != nil {
		return p.Jitter(wait)
	}
	return wait + rand.N(wait/4+1)
}

// pauseAfter is how many refusals in a row, with no answer between them, show an upstream that is
// refusing everyone rather than the odd request. A provider near its capacity refuses a request now
// and then and takes the next: each request waits and asks again on its own, and nobody else is held
// back. One that refuses this many in a row is taking nothing: from then on it is left alone by every
// request, so that the customers are told at once instead of each waiting out its patience, and the
// provider is not asked by all of them again and again.
const pauseAfter = 8

// pause is what one upstream of one route has said about its limit: until when it is left alone, and
// how many times in a row it refused. Providers count per model, so there is one per route and
// upstream, read by every request. An upstream is left alone when it names a time itself
// (Retry-After), or when it has refused pauseAfter times in a row.
type pause struct {
	mu      sync.Mutex
	until   time.Time
	refused int
}

// left is how long from now the upstream is left alone (zero: it may be asked).
func (p *pause) left(now time.Time) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if left := p.until.Sub(now); left > 0 {
		return left
	}
	return 0
}

// refuse records a refusal and returns how long the upstream is left alone from now; zero when it is
// not, and the request that was refused waits on its own. A time the upstream names is kept as asked
// (never under a quarter of First, nor over pauseLongest). Without one, the upstream is left alone
// from its pauseAfter-th refusal in a row, for the router's own wait, doubled with each refusal after
// that. Refusals that arrive while it is already being left alone are answers to calls made before:
// they belong to the same refusal and are not counted again, or one busy moment met by many requests
// at once would read as many refusals in a row.
func (p *pause) refuse(now time.Time, asked time.Duration, patience Patience) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !now.Before(p.until) {
		p.refused++
	}
	wait := asked
	if wait <= 0 && p.refused >= pauseAfter {
		wait = patience.wait(p.refused - pauseAfter + 1)
	}
	if wait > 0 {
		if wait > pauseLongest {
			wait = pauseLongest
		}
		if floor := patience.First / 4; wait < floor {
			// an upstream that asks for no time at all would be asked again at once, and again
			wait = floor
		}
		if until := now.Add(wait); until.After(p.until) {
			p.until = until
		}
	}
	if left := p.until.Sub(now); left > 0 {
		return left
	}
	return 0
}

// served records an answer: the upstream is taking requests, and its next refusal is a first one.
func (p *pause) served() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.refused = 0
	p.mu.Unlock()
}

// retryAfter reads how long an upstream asked to be left alone: Retry-After in seconds or as a date,
// or the milliseconds some APIs send beside it. Zero when it names nothing usable.
func retryAfter(header http.Header, now time.Time) time.Duration {
	if value := strings.TrimSpace(header.Get("Retry-After-Ms")); value != "" {
		if ms, err := strconv.ParseFloat(value, 64); err == nil && ms > 0 && ms < 1e9 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds > 0 && seconds < 1e6 {
			return time.Duration(seconds * float64(time.Second))
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := at.Sub(now); wait > 0 {
			return wait
		}
	}
	return 0
}

// roundEnd is what a round that no upstream answered leaves behind: whether anything it was told may
// pass in a moment, the shortest wait after which some upstream may be asked again, and whether an
// upstream was at its limit.
type roundEnd struct {
	passing bool
	wait    time.Duration
	busy    bool
}

func (e *roundEnd) note(wait time.Duration, busy bool) {
	if !e.passing || wait < e.wait {
		e.wait = wait
	}
	e.passing = true
	e.busy = e.busy || busy
}
