package router

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lebrel-router/internal/auth"
)

// scriptedUpstream answers each call with the next answer of its script (the last one answers every
// call after it) and keeps when each call came.
type scriptedUpstream struct {
	*fakeUpstream
	mu    sync.Mutex
	calls []time.Time
}

func scripted(t *testing.T, script ...http.HandlerFunc) *scriptedUpstream {
	t.Helper()
	s := &scriptedUpstream{}
	s.fakeUpstream = newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.calls)
		s.calls = append(s.calls, time.Now())
		s.mu.Unlock()
		if n >= len(script) {
			n = len(script) - 1
		}
		script[n](w, r)
	})
	return s
}

func (s *scriptedUpstream) times() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.calls...)
}

// refusing answers with a status and a provider's own words about the account it serves, which is
// Lebrel's: none of them may reach a customer.
func refusing(status int, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":{"code":"1302","message":"High concurrency usage of this API, please reduce concurrency or contact customer service"}}`)
	}
}

var answering = okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":8}`)

type alertLog struct {
	mu    sync.Mutex
	texts []string
}

func (a *alertLog) add(text string) {
	a.mu.Lock()
	a.texts = append(a.texts, text)
	a.mu.Unlock()
}

func (a *alertLog) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.texts...)
}

// patient gives the route its patience, with waits short enough for a test and no jitter, so the
// waits are the ones the test names.
func patient(budget, first, longest time.Duration, alerts *alertLog) func(*Settings) {
	return func(s *Settings) {
		s.Patience = Patience{Budget: budget, First: first, Longest: longest, Jitter: func(wait time.Duration) time.Duration { return wait }}
		if alerts != nil {
			s.Alert = alerts.add
		}
	}
}

const patienceBody = `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi there"}],"temperature":0}`

// A provider that says "not now" twice and then answers costs the customer a moment: the answer
// arrives, charged once like any other, and nobody is alerted.
func TestARefusalOfTheMomentCostsAWaitNotTheAnswer(t *testing.T) {
	up := scripted(t, refusing(429), refusing(429), answering)
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 10*time.Millisecond, 40*time.Millisecond, &alerts))
	before := h.balance(t)
	started := time.Now()
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	waited := time.Since(started)
	if response.StatusCode != 200 || response.Header.Get(upstreamHeader) != "zai/api-flash" || !strings.Contains(string(raw), "hello world") {
		t.Fatalf("%d served by %q: %s", response.StatusCode, response.Header.Get(upstreamHeader), raw)
	}
	if calls := len(up.times()); calls != 3 {
		t.Fatalf("two refusals and the answer are three calls, got %d", calls)
	}
	if waited < 30*time.Millisecond {
		t.Fatalf("10 ms after the first refusal and 20 ms after the second, waited %s", waited)
	}
	if _, ok := h.receipts.Get(response.Header.Get("X-Request-ID")); !ok {
		t.Fatal("the answer has its receipt")
	}
	charged := before - h.balance(t)
	plain := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, plain.Body)
	if again := before - charged - h.balance(t); charged <= 0 || charged != again {
		t.Fatalf("the answer that waited is charged once, like one that did not: %d against %d", charged, again)
	}
	if got := alerts.all(); len(got) != 0 {
		t.Fatalf("the customer was answered, nobody is alerted: %q", got)
	}
}

// What an upstream asks for is what it gets: it is not called again before the time it named,
// however short the router's own waits are.
func TestAnUpstreamIsLeftAloneForAsLongAsItAsks(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After-Ms", "150"), answering)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 5*time.Millisecond, 20*time.Millisecond, nil))
	response := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, response.Body)
	times := up.times()
	if response.StatusCode != 200 || len(times) != 2 {
		t.Fatalf("status %d after %d calls", response.StatusCode, len(times))
	}
	if gap := times[1].Sub(times[0]); gap < 150*time.Millisecond {
		t.Fatalf("it asked for 150 ms and was called again after %s", gap)
	}
}

// The pause is the upstream's, not the request's: a request that arrives while the upstream is being
// left alone waits for it too instead of asking and being refused in its turn.
func TestEveryRequestLeavesAnUpstreamAloneWhileItAsksForIt(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After-Ms", "200"), answering)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 5*time.Millisecond, 400*time.Millisecond, nil))
	post := func() (int, error) {
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
		request.Header.Set("Authorization", "Bearer "+testKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode, nil
	}
	type outcome struct {
		status int
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		status, err := post()
		first <- outcome{status, err}
	}()
	for deadline := time.Now().Add(5 * time.Second); len(up.times()) == 0 && time.Now().Before(deadline); {
		time.Sleep(2 * time.Millisecond)
	}
	status, err := post() // arrives while the upstream is being left alone
	if err != nil || status != 200 {
		t.Fatalf("the request that arrived during the pause: %d %v", status, err)
	}
	if got := <-first; got.err != nil || got.status != 200 {
		t.Fatalf("the request that was refused: %d %v", got.status, got.err)
	}
	times := up.times()
	if len(times) != 3 {
		t.Fatalf("one refusal and one call for each answer; a request that came during the pause must not ask: %d calls", len(times))
	}
	for _, at := range times[1:] {
		if gap := at.Sub(times[0]); gap < 200*time.Millisecond {
			t.Fatalf("called again %s after it asked for 200 ms", gap)
		}
	}
}

// With another upstream to ask, nobody waits for the one being left alone: the next one answers, and
// the first is not called again until its time is over.
func TestAnUpstreamBeingLeftAloneIsPassedOverForTheNext(t *testing.T) {
	limited := scripted(t, refusing(429, "Retry-After-Ms", "5000"))
	backup := newFakeUpstream(t, answering)
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("alibaba/model-studio", limited.server.URL, 1), upstream("deepseek/api", backup.server.URL, 2)), board, patient(2*time.Second, 5*time.Millisecond, 10*time.Second, &alerts))
	started := time.Now()
	for i := 0; i < 3; i++ {
		response := h.post(t, patienceBody, testKey)
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 || response.Header.Get(upstreamHeader) != "deepseek/api" {
			t.Fatalf("call %d: %d served by %q: %s", i+1, response.StatusCode, response.Header.Get(upstreamHeader), raw)
		}
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("nobody waits for an upstream when the next one answers: %s", waited)
	}
	if calls := len(limited.times()); calls != 1 {
		t.Fatalf("the upstream that asked to be left alone was called %d times", calls)
	}
	got := alerts.all()
	if len(got) != 1 || !strings.Contains(got[0], "alibaba/model-studio (HTTP 429, código 1302)") || !strings.Contains(got[0], "deepseek/api") {
		t.Fatalf("one fallback alert naming the refusal and who answered, got %q", got)
	}
}

// An upstream that asks for longer than the request may wait: the customer is told to come back, in
// Lebrel's words and with the time the upstream named. Its own words speak of Lebrel's account with
// it and never reach the customer; nothing is charged, and the operator hears of it.
func TestWhenTheWaitIsLongerThanThePatienceTheCustomerIsToldToRetryInLebrelsWords(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After", "7"))
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(300*time.Millisecond, 5*time.Millisecond, 10*time.Second, &alerts))
	before := h.balance(t)
	started := time.Now()
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "7" || !strings.Contains(string(raw), `"upstream_busy"`) {
		t.Fatalf("status %d, Retry-After %q, body %s", response.StatusCode, response.Header.Get("Retry-After"), raw)
	}
	if strings.Contains(string(raw), "1302") || strings.Contains(string(raw), "concurrency") || response.Header.Get(upstreamHeader) != "" {
		t.Fatalf("the upstream's words about Lebrel's account reached the customer: %s (%q)", raw, response.Header.Get(upstreamHeader))
	}
	if calls, waited := len(up.times()), time.Since(started); calls != 1 || waited > 250*time.Millisecond {
		t.Fatalf("a wait that does not fit is not begun: %d calls, %s", calls, waited)
	}
	if h.balance(t) != before {
		t.Fatalf("nothing is charged for no answer: %d", before-h.balance(t))
	}
	got := alerts.all()
	if len(got) != 1 || !strings.Contains(got[0], "ningún proveedor") || !strings.Contains(got[0], "zai/api-flash (HTTP 429, código 1302)") {
		t.Fatalf("one outage alert naming the refusal and the upstream's own code for it, got %q", got)
	}
	// A request that comes while the upstream is being left alone is told the same, without a call.
	second := h.post(t, patienceBody, testKey)
	raw, _ = io.ReadAll(second.Body)
	seconds, _ := strconv.Atoi(second.Header.Get("Retry-After"))
	if second.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"upstream_busy"`) || seconds < 6 || seconds > 7 || len(up.times()) != 1 {
		t.Fatalf("status %d, Retry-After %q, %d calls, body %s", second.StatusCode, second.Header.Get("Retry-After"), len(up.times()), raw)
	}
}

// An upstream that names no time and keeps refusing is asked a few times, each wait twice the one
// before, and when the patience is spent the route says it is busy.
func TestAnUpstreamThatKeepsRefusingIsAskedAFewTimesAndThenTheRouteSaysBusy(t *testing.T) {
	up := scripted(t, refusing(429))
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(100*time.Millisecond, 10*time.Millisecond, 40*time.Millisecond, &alerts))
	started := time.Now()
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	waited := time.Since(started)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"upstream_busy"`) || response.Header.Get("Retry-After") != "1" {
		t.Fatalf("status %d, Retry-After %q, body %s", response.StatusCode, response.Header.Get("Retry-After"), raw)
	}
	times := up.times()
	if len(times) < 3 || len(times) > 4 || waited > time.Second {
		t.Fatalf("10, 20 and 40 ms of waits fit in 100 ms and a second 40 does not: %d calls in %s", len(times), waited)
	}
	if first, second := times[1].Sub(times[0]), times[2].Sub(times[1]); first < 10*time.Millisecond || second < 20*time.Millisecond {
		t.Fatalf("each wait is twice the one before: %s then %s", first, second)
	}
	got := alerts.all()
	if len(got) != 1 || !strings.Contains(got[0], "zai/api-flash (HTTP 429, código 1302)") || !strings.Contains(got[0], fmt.Sprintf("%d intentos", len(times))) {
		t.Fatalf("one outage alert saying how many times the route was asked, got %q", got)
	}
}

// A failure of the moment that says nothing of a limit (a gateway error, a connection that broke) is
// asked again by the request that met it, and holds nobody else back.
func TestAFailureThatSaysNothingOfALimitIsAskedAgainAndHoldsNobodyBack(t *testing.T) {
	broken := func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}
	up := scripted(t, refusing(502), broken, answering)
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 5*time.Millisecond, 20*time.Millisecond, &alerts))
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || len(up.times()) != 3 {
		t.Fatalf("status %d after %d calls: %s", response.StatusCode, len(up.times()), raw)
	}
	if left := h.router.pauses[pauseKey(&h.route, "zai/api-flash")].left(time.Now()); left != 0 {
		t.Fatalf("an upstream that named no limit is left alone for %s", left)
	}
	if got := alerts.all(); len(got) != 0 {
		t.Fatalf("answered, nobody is alerted: %q", got)
	}
}

// What a wait cannot cure is not waited for: a request the upstream refuses as sent, a refused
// credential and a model it does not know answer at once, as they did, and an upstream that kept
// silent for its whole time is not given another.
func TestWhatAWaitCannotCureIsNotWaitedFor(t *testing.T) {
	for name, test := range map[string]struct {
		status int
		want   int
	}{
		"a request the upstream refuses as sent is relayed": {http.StatusBadRequest, http.StatusBadRequest},
		"a refused credential is Lebrel's outage":           {http.StatusUnauthorized, http.StatusBadGateway},
		"a model the upstream does not know is too":         {http.StatusNotFound, http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			up := scripted(t, refusing(test.status))
			h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 50*time.Millisecond, 200*time.Millisecond, nil))
			started := time.Now()
			response := h.post(t, patienceBody, testKey)
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode != test.want || len(up.times()) != 1 || time.Since(started) > time.Second {
				t.Fatalf("status %d after %d calls in %s", response.StatusCode, len(up.times()), time.Since(started))
			}
		})
	}
	t.Run("a silent upstream is not asked again", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		slow := scripted(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		only := upstream("slow/api", slow.server.URL, 1)
		only.TimeoutSeconds = 1
		h := newHarnessWith(t, routeFor(only), "", patient(10*time.Second, 5*time.Millisecond, 20*time.Millisecond, nil))
		started := time.Now()
		response := h.post(t, patienceBody, testKey)
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), "upstream_unavailable") || len(slow.times()) != 1 || time.Since(started) > 3*time.Second {
			t.Fatalf("status %d after %d calls in %s: %s", response.StatusCode, len(slow.times()), time.Since(started), raw)
		}
	})
}

// A customer who leaves while the route waits is no outage: nothing more is asked of the upstream,
// nothing is charged and nobody is alerted.
func TestACustomerWhoLeavesWhileTheRouteWaitsIsNoOutageAndNothingMoreIsAsked(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After-Ms", "300"), answering)
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 5*time.Millisecond, 20*time.Millisecond, &alerts))
	before := h.balance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
	request.Header.Set("Authorization", "Bearer "+testKey)
	if response, err := http.DefaultClient.Do(request); err == nil {
		response.Body.Close()
		t.Fatalf("the customer left before any answer, got %d", response.StatusCode)
	}
	for deadline := time.Now().Add(150 * time.Millisecond); h.router.InFlight() > 0 && time.Now().Before(deadline); {
		time.Sleep(2 * time.Millisecond)
	}
	if h.router.InFlight() != 0 {
		t.Fatal("the request ends when its customer leaves, not when the upstream's time is over")
	}
	if seated := h.router.waiting[h.route.ModelID].Load(); seated != 0 {
		t.Fatalf("a customer who leaves gives back the place in the waiting room: %d still taken", seated)
	}
	time.Sleep(400 * time.Millisecond) // past the time the upstream asked for
	if calls := len(up.times()); calls != 1 {
		t.Fatalf("the upstream was asked %d times for a customer who had left", calls)
	}
	if h.balance(t) != before {
		t.Fatalf("nothing is charged: %d", before-h.balance(t))
	}
	if got := alerts.all(); len(got) != 0 {
		t.Fatalf("a customer who hung up is no outage: %q", got)
	}
}

// A stream refused for a moment streams whole after the wait and is settled for its exact usage.
func TestAStreamRefusedForAMomentStreamsWholeAfterTheWait(t *testing.T) {
	up := scripted(t, refusing(429), sseHandler)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), board, patient(2*time.Second, 5*time.Millisecond, 20*time.Millisecond, nil))
	before := h.balance(t)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, testKey)
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(response.Body)
	var lines []string
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 5 || lines[4] != "data: [DONE]" || len(up.times()) != 2 {
		t.Fatalf("stream lines %v after %d calls", lines, len(up.times()))
	}
	// 12 uncached * 0.1575 + 3 * 0.63 = 1.89 + 1.89 -> ceil 4, as for a stream that never waited
	if got := before - h.balance(t); got != 4 {
		t.Fatalf("stream charged %d", got)
	}
}

// The other formats say the same in their own shape: busy, with the time to come back.
func TestTheOtherFormatsSayBusyInTheirOwnShape(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After", "9"))
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(300*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, nil))
	for path, body := range map[string]string{
		anthropicMessagesPath: `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":"hi there"}]}`,
		responsesPath:         `{"model":"deepseek/deepseek-v4.1-flash","input":"hi there"}`,
	} {
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+testKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		seconds, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		if response.StatusCode != http.StatusServiceUnavailable || seconds < 8 || seconds > 9 || !strings.Contains(string(raw), "at capacity") || strings.Contains(string(raw), "1302") {
			t.Fatalf("%s: status %d, Retry-After %q, body %s", path, response.StatusCode, response.Header.Get("Retry-After"), raw)
		}
	}
}

// A shutdown that cuts the wait asks the customer to retry, like one that cuts the upstream's call:
// never accepted, nothing charged, the reservation listed as cut.
func TestAShutdownThatCutsTheWaitAsksTheCustomerToRetry(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After-Ms", "8000"))
	h := newHarness(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "")
	queue := runningQueue(t, h.ledger)
	server, err := New(Settings{Routes: Config{Routes: []Route{h.route}}, Auth: h.auth, Ledger: h.ledger, Signer: h.signer, Receipts: h.receipts,
		Settlements: queue, Logger: log.New(io.Discard, "", 0), Patience: Patience{Budget: 10 * time.Second, Longest: 9 * time.Second, Jitter: func(wait time.Duration) time.Duration { return wait }}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	answered := make(chan *http.Response, 1)
	go func() {
		response, err := complete(t, ts.URL, streamBody)
		if err != nil {
			t.Error(err)
		}
		answered <- response
	}()
	for deadline := time.Now().Add(5 * time.Second); len(up.times()) == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the route is waiting for the upstream's time
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cut := server.CutStreams(ctx)
	response := <-answered
	if response == nil {
		t.FailNow()
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "router_restarting") || response.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q, body %s", response.StatusCode, response.Header.Get("Retry-After"), raw)
	}
	if len(cut) != 1 || len(up.times()) != 1 {
		t.Fatalf("the reservation is listed as cut and the upstream is not asked again: %v, %d calls", cut, len(up.times()))
	}
	if seated := server.waiting[h.route.ModelID].Load(); seated != 0 {
		t.Fatalf("a cut wait gives back the place in the waiting room: %d still taken", seated)
	}
	flushed(t, queue)
	if after, _ := h.ledger.Balance(context.Background(), "acct_test_000001"); after != before {
		t.Fatalf("never accepted, nothing charged: %d", before-after)
	}
}

// An upstream that refuses a request now and then is not left alone: each request waits on its own.
// One that has refused pauseAfter times in a row is, for longer each time, until it answers; and
// several refusals of one busy moment (answers to calls made before it was left alone) count as one.
func TestAnUpstreamIsLeftAloneOnlyWhenItRefusesEveryone(t *testing.T) {
	patience := Patience{First: 100 * time.Millisecond, Longest: 400 * time.Millisecond}.withDefaults()
	var held pause
	now := time.Unix(1_900_000_000, 0)
	for i := 1; i < pauseAfter; i++ {
		if got := held.refuse(now, 0, patience); got != 0 || held.left(now) != 0 {
			t.Fatalf("refusal %d in a row: left alone for %s", i, got)
		}
		if i == 3 {
			held.served() // an answer between refusals: the count starts over
			for j := 1; j <= 3; j++ {
				held.refuse(now, 0, patience)
			}
		}
	}
	if got := held.refuse(now, 0, patience); got != 100*time.Millisecond {
		t.Fatalf("refusal %d in a row with no answer between: %s", pauseAfter, got)
	}
	if got := held.refuse(now.Add(10*time.Millisecond), 0, patience); got != 100*time.Millisecond || held.refused != pauseAfter {
		t.Fatalf("a refusal that comes while it is left alone is the same busy moment: %s, %d in a row", got, held.refused)
	}
	now = now.Add(110 * time.Millisecond)
	if held.left(now) != 0 {
		t.Fatalf("its time is over: %s", held.left(now))
	}
	for i, want := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond} {
		if got := held.refuse(now, 0, patience); got != want {
			t.Fatalf("refusal %d after it was left alone: %s, want %s", i+1, got, want)
		}
		now = now.Add(want)
	}
	held.served()
	if got := held.refuse(now, 0, patience); got != 0 {
		t.Fatalf("after an answer the next refusal is a first one: %s", got)
	}
	// A time the upstream names is kept as asked from its first refusal, within reason at both ends...
	for asked, want := range map[time.Duration]time.Duration{300 * time.Millisecond: 300 * time.Millisecond, time.Millisecond: 25 * time.Millisecond} {
		var fresh pause
		if got := fresh.refuse(now, asked, patience); got != want {
			t.Fatalf("asked for %s: left alone %s, want %s", asked, got, want)
		}
	}
	// ...but one longer than the router's own longest wait is kept by everyone only from the second
	// refusal in a row: a single odd refusal that names a minute closes the route to nobody else.
	var long pause
	if got := long.refuse(now, 7*time.Second, patience); got != 400*time.Millisecond {
		t.Fatalf("a first refusal that names 7 s leaves it alone for the router's longest wait: %s", got)
	}
	if got := long.refuse(now.Add(50*time.Millisecond), 7*time.Second, patience); got != 400*time.Millisecond || long.refused != 1 {
		t.Fatalf("an answer to a call made before is the same refusal, and no reason to trust the long time yet: %s, %d in a row", got, long.refused)
	}
	later := now.Add(460 * time.Millisecond)
	if got := long.refuse(later, 7*time.Second, patience); got != 7*time.Second {
		t.Fatalf("refused again when its time was over, it is left alone for what it names: %s", got)
	}
	if got := long.refuse(later.Add(time.Second), 100*time.Millisecond, patience); got != 6*time.Second {
		t.Fatalf("a shorter time named later does not shorten what it is left alone: %s", got)
	}
	long.served()
	if got := long.left(later.Add(time.Second)); got != 6*time.Second {
		t.Fatalf("an answer starts the count over and leaves the time it asked for as it is: %s", got)
	}
	var hour pause
	hour.refuse(now, time.Hour, patience)
	if got := hour.refuse(now.Add(400*time.Millisecond), time.Hour, patience); got != pauseLongest {
		t.Fatalf("an hour speaks of a quota: it is asked again after %s, not %s", pauseLongest, got)
	}
	var none *pause // a route that is not the router's own never panics
	if none.left(now) != 0 || none.refuse(now, time.Second, patience) != 0 {
		t.Fatal("no pause, nothing to wait for")
	}
	none.served()
}

// An upstream that refuses everyone: it is left alone, and once it has refused again when its time
// was over, a customer who comes while it is left alone is told at once, without a call and without
// waiting out a patience nothing suggests will be rewarded. The requests already waiting are the
// ones that ask it again, and an answer puts everything back.
func TestAnUpstreamThatRefusesEveryoneIsLeftAloneAndCustomersAreToldAtOnce(t *testing.T) {
	var open atomic.Bool
	up := scripted(t, func(w http.ResponseWriter, r *http.Request) {
		if open.Load() {
			answering(w, r)
			return
		}
		refusing(429)(w, r)
	})
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", func(s *Settings) {
		patient(3*time.Second, 100*time.Millisecond, 400*time.Millisecond, nil)(s)
		s.Auth = manyAtOnce(t)
	})
	ask := func() (int, string, time.Duration) {
		started := time.Now()
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
		request.Header.Set("Authorization", "Bearer "+testKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, err.Error(), time.Since(started)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(raw), time.Since(started)
	}
	const crowd = 12
	statuses := make(chan int, crowd)
	for i := 0; i < crowd; i++ {
		go func() {
			status, _, _ := ask()
			statuses <- status
		}()
	}
	// The crowd is refused, waits out the first pause (100 ms) and is refused again: now nothing shows
	// the upstream would take the next request, and it is left alone for 200 ms.
	for deadline := time.Now().Add(5 * time.Second); len(up.times()) <= crowd && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	held := h.router.pauses[pauseKey(&h.route, "zai/api-flash")]
	for deadline := time.Now().Add(time.Second); !held.refusing(time.Now()) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	before := len(up.times())
	status, raw, took := ask()
	if status != http.StatusServiceUnavailable || !strings.Contains(raw, `"upstream_busy"`) || took > 100*time.Millisecond {
		t.Fatalf("a customer who comes while it refuses everyone is told at once: status %d in %s: %s", status, took, raw)
	}
	if after := len(up.times()); after > before+crowd {
		t.Fatalf("the customer told at once made no call of its own: %d calls before, %d after", before, after)
	}
	open.Store(true)
	served := 0
	for i := 0; i < crowd; i++ {
		if <-statuses == 200 {
			served++
		}
	}
	if served != crowd {
		t.Fatalf("every request that was waiting asks again when its time is over and is answered; only one that has asked nobody yet is told at once: %d of %d served", served, crowd)
	}
	if held.refused != 0 {
		t.Fatalf("an answer starts the count over: %d in a row", held.refused)
	}
	for deadline := time.Now().Add(2 * time.Second); held.left(time.Now()) > 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if status, raw, _ := ask(); status != 200 {
		t.Fatalf("and the next customer is served: %d %s", status, raw)
	}
}

func TestRetryAfterIsReadInSecondsAsADateAndInMilliseconds(t *testing.T) {
	now := time.Date(2026, 11, 12, 13, 5, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		header http.Header
		want   time.Duration
	}{
		"seconds":             {http.Header{"Retry-After": {"2"}}, 2 * time.Second},
		"a fraction":          {http.Header{"Retry-After": {"1.5"}}, 1500 * time.Millisecond},
		"a date":              {http.Header{"Retry-After": {"Thu, 12 Nov 2026 13:05:05 GMT"}}, 5 * time.Second},
		"a date already past": {http.Header{"Retry-After": {"Thu, 12 Nov 2026 13:04:00 GMT"}}, 0},
		"zero":                {http.Header{"Retry-After": {"0"}}, 0},
		"not a time":          {http.Header{"Retry-After": {"soon"}}, 0},
		"nothing":             {http.Header{}, 0},
		"milliseconds first":  {http.Header{"Retry-After-Ms": {"250"}, "Retry-After": {"1"}}, 250 * time.Millisecond},
	} {
		if got := retryAfter(test.header, now); got != test.want {
			t.Errorf("%s: %s, want %s", name, got, test.want)
		}
	}
}

// With no patience at all the route answers with what its one round got, except that a limit reached
// is still told in Lebrel's words: it never was about the customer.
func TestWithoutPatienceALimitReachedIsStillToldInLebrelsWords(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After", "5"))
	h := newHarness(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "")
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"upstream_busy"`) || strings.Contains(string(raw), "1302") || len(up.times()) != 1 {
		t.Fatalf("status %d after %d calls: %s", response.StatusCode, len(up.times()), raw)
	}
	again := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, again.Body)
	if len(up.times()) != 2 {
		t.Fatalf("without patience no upstream is left alone: %d calls", len(up.times()))
	}
}

// manyAtOnce is the test account with room for a crowd of requests at once.
func manyAtOnce(t *testing.T) auth.Authenticator {
	t.Helper()
	secret := bytes.Repeat([]byte{9}, 32)
	return auth.NewHMACAuthenticator(secret, []auth.Record{{AccountID: "acct_test_000001", Digest: auth.Digest(secret, testKey), Concurrency: 512, AccountConcurrency: 512}})
}

// The waiting room has a size: however many customers come to a route whose provider refuses, the
// requests that ask it again are never more than the room holds, and the rest are told at once.
func TestTheWaitingRoomBoundsTheCallsAProviderThatRefusesGets(t *testing.T) {
	up := scripted(t, refusing(429))
	const crowd, room = 60, 4
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", func(s *Settings) {
		patient(400*time.Millisecond, 20*time.Millisecond, 80*time.Millisecond, nil)(s)
		s.Patience.Waiting = room
		s.Auth = manyAtOnce(t)
	})
	type outcome struct {
		status int
		took   time.Duration
	}
	outcomes := make(chan outcome, crowd)
	for i := 0; i < crowd; i++ {
		go func() {
			started := time.Now()
			request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
			request.Header.Set("Authorization", "Bearer "+testKey)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				outcomes <- outcome{0, time.Since(started)}
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			outcomes <- outcome{response.StatusCode, time.Since(started)}
		}()
	}
	waited := 0
	for i := 0; i < crowd; i++ {
		got := <-outcomes
		if got.status != http.StatusServiceUnavailable {
			t.Fatalf("every customer is told the route is busy: %d", got.status)
		}
		if got.took >= 200*time.Millisecond {
			waited++
		}
	}
	// One call each at most for the customers told at once, and the room's requests asking again at
	// 20, 40, 80, 80, 80 and 80 ms: without the room the crowd would make seven calls each.
	if calls := len(up.times()); calls > crowd+room*8 {
		t.Fatalf("%d customers made %d calls; the waiting room holds %d", crowd, calls, room)
	}
	if waited > room*2 {
		t.Fatalf("%d customers waited; the room holds %d at a time", waited, room)
	}
	if left := h.router.waiting[h.route.ModelID].Load(); left != 0 {
		t.Fatalf("nobody is left counted in the waiting room: %d", left)
	}
}

// A single refusal that names a long time closes the route to nobody else: the customer it was said
// to is told to come back then, and the next one is asked for after the router's own longest wait.
func TestASingleRefusalThatNamesALongTimeClosesTheRouteToNobodyElse(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After", "20"), answering)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(300*time.Millisecond, 5*time.Millisecond, 40*time.Millisecond, nil))
	first := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, first.Body)
	if first.StatusCode != http.StatusServiceUnavailable || first.Header.Get("Retry-After") != "20" {
		t.Fatalf("the customer it was said to: %d, Retry-After %q", first.StatusCode, first.Header.Get("Retry-After"))
	}
	started := time.Now()
	second := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(second.Body)
	if second.StatusCode != 200 || len(up.times()) != 2 || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("the next customer: %d after %d calls in %s: %s", second.StatusCode, len(up.times()), time.Since(started), raw)
	}
}

// An upstream that names almost no time is not asked at every turn: a request that keeps being
// refused asks less and less often, whatever the upstream says.
func TestAnUpstreamThatNamesAlmostNoTimeIsNotAskedAtEveryTurn(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After-Ms", "1"))
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(300*time.Millisecond, 20*time.Millisecond, 80*time.Millisecond, nil))
	response := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, response.Body)
	// asked at 0, 20, 60, 140, 220 and 300 ms; every 5 ms would be sixty calls
	if calls := len(up.times()); response.StatusCode != http.StatusServiceUnavailable || calls < 4 || calls > 7 {
		t.Fatalf("status %d after %d calls", response.StatusCode, calls)
	}
}

// The jitter spreads a wait within what is left of the patience; it never decides whether a
// customer is answered.
func TestTheJitterNeverDecidesWhetherACustomerIsAnswered(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After-Ms", "100"), answering)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", func(s *Settings) {
		s.Patience = Patience{Budget: 150 * time.Millisecond, First: 5 * time.Millisecond, Longest: 200 * time.Millisecond, Jitter: func(wait time.Duration) time.Duration { return 10 * wait }}
	})
	started := time.Now()
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || time.Since(started) > 400*time.Millisecond {
		t.Fatalf("a wait that fits is waited, however the jitter falls: %d in %s: %s", response.StatusCode, time.Since(started), raw)
	}
	times := up.times()
	if gap := times[1].Sub(times[0]); gap < 100*time.Millisecond {
		t.Fatalf("and never cut short: asked again after %s", gap)
	}
	// The default: up to a quarter more, never less, and not always the same.
	defaults := Patience{}.withDefaults()
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		got := defaults.spread(time.Second)
		if got < time.Second || got > 1250*time.Millisecond {
			t.Fatalf("a second spread to %s", got)
		}
		seen[got] = true
	}
	if len(seen) < 20 {
		t.Fatalf("the default jitter gave %d different waits in 200", len(seen))
	}
}

// What production runs with, said once: a change of any of these is a change of what customers get.
func TestTheDefaultsAreTheOnesTheReadmeNames(t *testing.T) {
	got := Patience{}.withDefaults()
	if got.Budget != 8*time.Second || got.First != 500*time.Millisecond || got.Longest != 3*time.Second || got.Waiting != 32 {
		t.Fatalf("defaults: %+v", got)
	}
	if pauseLongest != 30*time.Second || pauseAfter != 8 {
		t.Fatalf("an upstream's own time is kept up to %s and it is left alone after %d refusals", pauseLongest, pauseAfter)
	}
	var waits []time.Duration
	for n := 1; n <= 6; n++ {
		waits = append(waits, got.wait(n))
	}
	if fmt.Sprint(waits) != "[500ms 1s 2s 3s 3s 3s]" {
		t.Fatalf("waits: %v", waits)
	}
	if fmt.Sprint(busyStatuses) != "map[429:true 503:true]" || fmt.Sprint(passingStatuses) != "map[408:true 425:true 500:true 502:true 504:true]" {
		t.Fatalf("busy %v passing %v", busyStatuses, passingStatuses)
	}
	if off := (Patience{Budget: -1}).withDefaults(); off.Budget != -1 {
		t.Fatalf("no patience stays no patience: %s", off.Budget)
	}
}

// With those defaults a route whose provider refuses everything answers inside the ten seconds many
// clients wait, having asked a handful of times.
func TestWithTheDefaultsARouteThatRefusesEverythingAnswersInsideTenSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("eight seconds of real waiting")
	}
	up := scripted(t, refusing(429))
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", func(s *Settings) {
		s.Patience = Patience{}
		s.Alert = alerts.add
	})
	started := time.Now()
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	took := time.Since(started)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"upstream_busy"`) {
		t.Fatalf("status %d: %s", response.StatusCode, raw)
	}
	// asked at 0, 0.5, 1.5, 3.5 and 6.5 s (each wait up to a quarter longer); a sixth would be past 8 s
	if calls := len(up.times()); calls != 5 || took < 6*time.Second || took > 8500*time.Millisecond {
		t.Fatalf("%d calls in %s", calls, took)
	}
	if got := alerts.all(); len(got) != 1 {
		t.Fatalf("one outage alert: %q", got)
	}
}

// Every status that says "busy" or "for the moment" is waited for, and the two that say "busy" are
// never relayed.
func TestTheStatusesOfTheMomentAreWaitedFor(t *testing.T) {
	for _, status := range []int{408, 425, 429, 500, 502, 503, 504} {
		up := scripted(t, refusing(status), answering)
		h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(time.Second, 5*time.Millisecond, 20*time.Millisecond, nil))
		response := h.post(t, patienceBody, testKey)
		_, _ = io.Copy(io.Discard, response.Body)
		if response.StatusCode != 200 || len(up.times()) != 2 {
			t.Fatalf("%d: status %d after %d calls", status, response.StatusCode, len(up.times()))
		}
	}
	for status, want := range map[int]int{429: 503, 503: 503, 500: 500, 504: 504} {
		up := scripted(t, refusing(status))
		h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(30*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, nil))
		response := h.post(t, patienceBody, testKey)
		raw, _ := io.ReadAll(response.Body)
		relayed := strings.Contains(string(raw), "1302")
		if response.StatusCode != want || relayed != (want != 503) || (want == 503) != strings.Contains(string(raw), `"upstream_busy"`) {
			t.Fatalf("an upstream that keeps answering %d: status %d, its words relayed %v: %s", status, response.StatusCode, relayed, raw)
		}
	}
}

// Providers count per model: an upstream left alone on one route is asked as ever on another route
// that names the same upstream.
func TestAnUpstreamLeftAloneOnOneRouteIsAskedOnAnother(t *testing.T) {
	limited := scripted(t, refusing(429, "Retry-After-Ms", "3000"))
	other := scripted(t, answering)
	second := routeFor(upstream("alibaba/model-studio", other.server.URL, 1))
	second.ModelID, second.Family, second.ClaimedRevision = "qwen/qwen3.8-flash", "qwen3.8-flash", "qwen3.8-flash"
	h := newHarnessWith(t, routeFor(upstream("alibaba/model-studio", limited.server.URL, 1)), "", func(s *Settings) {
		patient(100*time.Millisecond, 5*time.Millisecond, 10*time.Second, nil)(s)
		s.Routes.Routes = append(s.Routes.Routes, second)
	})
	first := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, first.Body)
	if first.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the route whose upstream refuses: %d", first.StatusCode)
	}
	started := time.Now()
	response := h.post(t, strings.Replace(patienceBody, "deepseek/deepseek-v4.1-flash", "qwen/qwen3.8-flash", 1), testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || len(other.times()) != 1 || time.Since(started) > 80*time.Millisecond {
		t.Fatalf("the other route: %d after %d calls in %s: %s", response.StatusCode, len(other.times()), time.Since(started), raw)
	}
}

// A round waits for the upstream that may be asked soonest, not for the one that asked for longest.
func TestARoundWaitsForTheUpstreamThatMayBeAskedSoonest(t *testing.T) {
	limited := scripted(t, refusing(429, "Retry-After-Ms", "400"))
	flaky := scripted(t, refusing(502), answering)
	h := newHarnessWith(t, routeFor(upstream("alibaba/model-studio", limited.server.URL, 1), upstream("deepseek/api", flaky.server.URL, 2)), board, patient(2*time.Second, 10*time.Millisecond, 500*time.Millisecond, nil))
	started := time.Now()
	response := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || response.Header.Get(upstreamHeader) != "deepseek/api" || time.Since(started) > 300*time.Millisecond {
		t.Fatalf("%d served by %q in %s: %s", response.StatusCode, response.Header.Get(upstreamHeader), time.Since(started), raw)
	}
	if len(limited.times()) != 1 || len(flaky.times()) != 2 {
		t.Fatalf("the one left alone is not asked again meanwhile: %d and %d calls", len(limited.times()), len(flaky.times()))
	}
}

// The operator's alert says an upstream was being left alone, so an outage during a pause reads as
// what it is.
func TestAnAlertSaysWhichUpstreamWasBeingLeftAlone(t *testing.T) {
	limited := scripted(t, refusing(429, "Retry-After-Ms", "5000"))
	backup := scripted(t, answering, refusing(502))
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("alibaba/model-studio", limited.server.URL, 1), upstream("deepseek/api", backup.server.URL, 2)), board, patient(20*time.Millisecond, 5*time.Millisecond, 10*time.Second, &alerts))
	first := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, first.Body)
	second := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, second.Body)
	if first.StatusCode != 200 || second.StatusCode != http.StatusBadGateway || len(limited.times()) != 1 {
		t.Fatalf("statuses %d and %d, %d calls to the upstream left alone", first.StatusCode, second.StatusCode, len(limited.times()))
	}
	got := alerts.all()
	if len(got) != 2 || !strings.Contains(got[1], "alibaba/model-studio (en pausa por su límite) y deepseek/api (HTTP 502)") {
		t.Fatalf("the outage names the upstream left alone and the one that failed, got %q", got)
	}
}

// closeTracking is a transport that knows, at every call, whether the calls before it were ended:
// their bodies closed and their contexts done.
type closeTracking struct {
	inner http.RoundTripper
	mu    sync.Mutex
	calls []*trackedCall
	open  []int
}

type trackedCall struct {
	ctx    context.Context
	closed atomic.Bool
}

type trackedBody struct {
	io.ReadCloser
	call *trackedCall
}

func (b *trackedBody) Close() error {
	b.call.closed.Store(true)
	return b.ReadCloser.Close()
}

func (c *closeTracking) RoundTrip(request *http.Request) (*http.Response, error) {
	c.mu.Lock()
	for i, earlier := range c.calls {
		if !earlier.closed.Load() || earlier.ctx.Err() == nil {
			c.open = append(c.open, i+1)
		}
	}
	call := &trackedCall{ctx: request.Context()}
	c.calls = append(c.calls, call)
	c.mu.Unlock()
	response, err := c.inner.RoundTrip(request)
	if err != nil {
		call.closed.Store(true)
		return nil, err
	}
	response.Body = &trackedBody{ReadCloser: response.Body, call: call}
	return response, nil
}

// A refused call is over before the route is asked again: its body closed and its context ended, so
// rounds never pile up connections or goroutines.
func TestARefusedCallIsOverBeforeTheRouteIsAskedAgain(t *testing.T) {
	up := scripted(t, refusing(429), refusing(502), refusing(503), answering)
	tracking := &closeTracking{inner: &http.Transport{}}
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", func(s *Settings) {
		patient(time.Second, 5*time.Millisecond, 20*time.Millisecond, nil)(s)
		s.HTTPClient = &http.Client{Transport: tracking}
	})
	response := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || len(up.times()) != 4 {
		t.Fatalf("status %d after %d calls", response.StatusCode, len(up.times()))
	}
	for deadline := time.Now().Add(time.Second); h.router.InFlight() > 0 && time.Now().Before(deadline); {
		time.Sleep(2 * time.Millisecond)
	}
	tracking.mu.Lock()
	defer tracking.mu.Unlock()
	if len(tracking.open) != 0 {
		t.Fatalf("calls still open when the route was asked again: %v", tracking.open)
	}
	for i, call := range tracking.calls {
		if !call.closed.Load() || call.ctx.Err() == nil {
			t.Fatalf("call %d is not over after the answer: closed %v, context %v", i+1, call.closed.Load(), call.ctx.Err())
		}
	}
}

// The time a customer is told to come back in is never earlier than the upstream's.
func TestTheCustomersRetryAfterIsRoundedUp(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After-Ms", "6200"))
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(300*time.Millisecond, 5*time.Millisecond, 10*time.Second, nil))
	response := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "7" {
		t.Fatalf("status %d, Retry-After %q for 6.2 s", response.StatusCode, response.Header.Get("Retry-After"))
	}
}

// An upstream skipped because it does not take a field of the request refused nothing: the route is
// not busy for that. With the upstream that was asked failing, the customer is told no upstream could
// serve, not to come back because a provider is at capacity.
func TestAnUpstreamSkippedForAFieldIsNotAnUpstreamAtItsLimit(t *testing.T) {
	never := scripted(t, answering)
	down := scripted(t, func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	})
	first := upstream("alibaba/model-studio", never.server.URL, 1)
	first.Unsupported = []string{"logprobs"}
	h := newHarnessWith(t, routeFor(first, upstream("deepseek/api", down.server.URL, 2)), board, patient(40*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, nil))
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi there"}],"logprobs":true}`, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), "upstream_unavailable") || response.Header.Get("Retry-After") != "" {
		t.Fatalf("status %d, Retry-After %q: %s", response.StatusCode, response.Header.Get("Retry-After"), raw)
	}
	if len(never.times()) != 0 || len(down.times()) < 2 {
		t.Fatalf("the upstream that does not take the field is never asked, the other is asked again: %d and %d calls", len(never.times()), len(down.times()))
	}
}

// Only an answer starts the count of refusals over: a request the upstream refused as sent (relayed
// to its customer) says nothing about the upstream taking requests.
func TestOnlyAnAnswerStartsTheCountOfRefusalsOver(t *testing.T) {
	script := make([]http.HandlerFunc, 0, pauseAfter+1)
	for i := 1; i < pauseAfter; i++ {
		script = append(script, refusing(429))
	}
	script = append(script, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad temperature"}}`)
	}, refusing(429))
	up := scripted(t, script...)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(time.Millisecond, 300*time.Millisecond, time.Second, nil))
	for i := 0; i <= pauseAfter; i++ {
		response := h.post(t, patienceBody, testKey)
		_, _ = io.Copy(io.Discard, response.Body)
	}
	held := h.router.pauses[pauseKey(&h.route, "zai/api-flash")]
	if len(up.times()) != pauseAfter+1 || held.refused != pauseAfter || held.left(time.Now()) <= 0 {
		t.Fatalf("%d calls, %d refusals in a row, left alone %s", len(up.times()), held.refused, held.left(time.Now()))
	}
}

func TestAProvidersOwnCodeForARefusalIsReadForTheOperator(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"code":"1302","message":"Rate limit reached for requests"}}`:                         "1302",
		`{"error":{"message":"Rate limit reached","type":"rate_limit_error","param":null,"code":null}}`: "rate_limit_error",
		`{"error":{"code":429,"message":"Provider returned error"}}`:                                    "429",
		`{"error":{"type":"invalid_request_error","code":"rate_limit_exceeded"}}`:                       "rate_limit_exceeded",
		`{"code":"Throttling.RateQuota","message":"Requests rate limit exceeded","request_id":"x"}`:     "Throttling.RateQuota",
		`{"error":"overloaded"}`:                                 "",
		`{"error":{"code":"too many words here","message":"x"}}`: "",
		`{"error":{"code":"` + strings.Repeat("a", 41) + `"}}`:   "",
		`nope`: "",
		``:     "",
	} {
		if got := refusalCode([]byte(body)); got != want {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
	if got := statusReason(429, "1302"); got != "HTTP 429, código 1302" {
		t.Errorf("reason: %q", got)
	}
	if got := statusReason(502, ""); got != "HTTP 502" {
		t.Errorf("reason: %q", got)
	}
}

// A request that has waited keeps its place: with the room full of requests that wait, a newcomer is
// told at once, and it is told what its own round said. A route whose upstream fails, with no word
// of a limit, is not "busy" for a newcomer either.
func TestAFullWaitingRoomTellsANewcomerWhatItsRoundSaid(t *testing.T) {
	for name, test := range map[string]struct {
		status int
		want   int
		code   string
	}{
		"an upstream at its limit": {429, http.StatusServiceUnavailable, "upstream_busy"},
		"an upstream that fails":   {502, http.StatusBadGateway, "upstream_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			var open atomic.Bool
			up := scripted(t, func(w http.ResponseWriter, r *http.Request) {
				if open.Load() {
					answering(w, r)
					return
				}
				refusing(test.status)(w, r)
			})
			h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", func(s *Settings) {
				patient(2*time.Second, 150*time.Millisecond, 150*time.Millisecond, nil)(s)
				s.Patience.Waiting = 1
				s.Auth = manyAtOnce(t)
			})
			seated := make(chan int, 1)
			go func() {
				request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
				request.Header.Set("Authorization", "Bearer "+testKey)
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					seated <- 0
					return
				}
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				seated <- response.StatusCode
			}()
			room := h.router.waiting[h.route.ModelID]
			for deadline := time.Now().Add(2 * time.Second); room.Load() == 0 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			started := time.Now()
			newcomer := h.post(t, patienceBody, testKey)
			raw, _ := io.ReadAll(newcomer.Body)
			if newcomer.StatusCode != test.want || !strings.Contains(string(raw), test.code) || time.Since(started) > 100*time.Millisecond {
				t.Fatalf("the newcomer: %d in %s: %s", newcomer.StatusCode, time.Since(started), raw)
			}
			if (newcomer.Header.Get("Retry-After") != "") != (test.want == http.StatusServiceUnavailable) {
				t.Fatalf("a time to come back goes with busy only: %q", newcomer.Header.Get("Retry-After"))
			}
			// The one that waits keeps its place through its rounds and is served when the upstream answers.
			time.Sleep(200 * time.Millisecond)
			if room.Load() != 1 {
				t.Fatalf("the request that waits still has its place after another round: %d", room.Load())
			}
			open.Store(true)
			if status := <-seated; status != 200 {
				t.Fatalf("the request that waited: %d", status)
			}
			for deadline := time.Now().Add(time.Second); room.Load() != 0 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			if room.Load() != 0 {
				t.Fatalf("its place is given back once it is answered: %d still taken", room.Load())
			}
		})
	}
}

// Each route has its own waiting room: a route full of waiting requests does not keep another
// route's request from waiting for its own upstream.
func TestEachRouteHasItsOwnWaitingRoom(t *testing.T) {
	stuck := scripted(t, refusing(429))
	other := scripted(t, refusing(429), answering)
	second := routeFor(upstream("moonshot/api", other.server.URL, 1))
	second.ModelID, second.Family, second.ClaimedRevision = "moonshotai/kimi-k3", "kimi-k3", "kimi-k3"
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", stuck.server.URL, 1)), "", func(s *Settings) {
		patient(time.Second, 20*time.Millisecond, 100*time.Millisecond, nil)(s)
		s.Patience.Waiting = 1
		s.Routes.Routes = append(s.Routes.Routes, second)
		s.Auth = manyAtOnce(t)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
		request.Header.Set("Authorization", "Bearer "+testKey)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	for deadline := time.Now().Add(2 * time.Second); h.router.waiting[h.route.ModelID].Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	response := h.post(t, strings.Replace(patienceBody, "deepseek/deepseek-v4.1-flash", "moonshotai/kimi-k3", 1), testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || len(other.times()) != 2 {
		t.Fatalf("the other route's request waits for its own upstream and is served: %d after %d calls: %s", response.StatusCode, len(other.times()), raw)
	}
	<-done
}

// Each upstream is sent its own body, round after round: the model it serves under its own name.
func TestEachUpstreamIsSentItsOwnBodyInEveryRound(t *testing.T) {
	first := scripted(t, refusing(502))
	second := scripted(t, refusing(502), refusing(502), answering)
	a, b := upstream("alibaba/model-studio", first.server.URL, 1), upstream("deepseek/api", second.server.URL, 2)
	a.Model, b.Model = "deepseek-v4.1-flash-at-alibaba", "deepseek-flash"
	h := newHarnessWith(t, routeFor(a, b), board, patient(time.Second, 5*time.Millisecond, 20*time.Millisecond, nil))
	response := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != 200 || len(first.requests) != 3 || len(second.requests) != 3 {
		t.Fatalf("status %d after %d and %d calls", response.StatusCode, len(first.requests), len(second.requests))
	}
	for i := 0; i < 3; i++ {
		if got := first.requests[i].body["model"]; got != "deepseek-v4.1-flash-at-alibaba" {
			t.Fatalf("round %d: the first upstream was sent model %v", i+1, got)
		}
		if got := second.requests[i].body["model"]; got != "deepseek-flash" {
			t.Fatalf("round %d: the second upstream was sent model %v", i+1, got)
		}
	}
}

// Told at once only once the upstream has refused again after being left alone, and with the time
// its pause has left; a named hour is an upstream's quota, and the customer is told thirty seconds.
func TestWhatACustomerToldAtOnceIsTold(t *testing.T) {
	patience := Patience{First: 100 * time.Millisecond, Longest: 400 * time.Millisecond}.withDefaults()
	var held pause
	now := time.Unix(1_900_000_000, 0)
	for i := 0; i < pauseAfter; i++ {
		held.refuse(now, 0, patience)
	}
	if held.left(now) == 0 || held.refusing(now) {
		t.Fatalf("left alone after %d refusals in a row, but it has not been asked again yet: left %s, refusing %v", pauseAfter, held.left(now), held.refusing(now))
	}
	now = now.Add(100 * time.Millisecond)
	if held.refusing(now) {
		t.Fatal("its time is over: it may be asked")
	}
	held.refuse(now, 0, patience)
	if !held.refusing(now) || held.refusing(now.Add(200*time.Millisecond)) {
		t.Fatalf("asked again, it refused again: nothing shows it would take the next request until its time is over")
	}
	var end roundEnd
	end.closed(3 * time.Second)
	if !end.busy || end.passing || end.wait != 3*time.Second {
		t.Fatalf("busy, no reason to wait, and the pause's time for the customer: %+v", end)
	}
	end.closed(time.Second)
	if end.wait != time.Second {
		t.Fatalf("the upstream that may be asked soonest: %s", end.wait)
	}
	end.note(10*time.Millisecond, false)
	end.closed(5 * time.Millisecond)
	if !end.passing || end.wait != 10*time.Millisecond {
		t.Fatalf("something worth waiting for keeps its wait: %+v", end)
	}

	up := scripted(t, refusing(429, "Retry-After", "3600"))
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(300*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, nil))
	response := h.post(t, patienceBody, testKey)
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "30" {
		t.Fatalf("an hour named: status %d, Retry-After %q", response.StatusCode, response.Header.Get("Retry-After"))
	}
}

// Only a request that has asked nobody yet is told at once. One that is already waiting, and finds
// on its next round that the upstream is being left alone after refusing everyone again, keeps
// waiting: it is one of the requests that ask the upstream when its time is over.
func TestARequestAlreadyWaitingIsNotToldAtOnce(t *testing.T) {
	up := scripted(t, refusing(429), answering)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 100*time.Millisecond, 400*time.Millisecond, nil))
	started := time.Now()
	done := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
		request.Header.Set("Authorization", "Bearer "+testKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- 0
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		done <- response.StatusCode
	}()
	for deadline := time.Now().Add(2 * time.Second); len(up.times()) == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	// While it waits its 100 ms, the upstream refuses everyone else, is left alone, and refuses again
	// when asked: by the time the request comes back the upstream is closed for another 250 ms or so.
	held := h.router.pauses[pauseKey(&h.route, "zai/api-flash")]
	now := time.Now()
	for i := 1; i < pauseAfter; i++ {
		held.refuse(now, 0, h.router.settings.Patience)
	}
	held.refuse(now.Add(150*time.Millisecond), 0, h.router.settings.Patience)
	if !held.refusing(time.Now()) {
		t.Fatal("the upstream is refusing everyone")
	}
	if status := <-done; status != 200 || len(up.times()) != 2 {
		t.Fatalf("the request that was waiting: %d after %d calls", status, len(up.times()))
	}
	if waited := time.Since(started); waited < 300*time.Millisecond {
		t.Fatalf("it waited for the upstream's time to be over: %s", waited)
	}
}

// A request that has waited keeps its place while it asks: a newcomer that comes during its round
// does not take the place and leave it to be turned away after its wait.
func TestARequestKeepsItsPlaceWhileItAsks(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
		refusing(429)(w, r)
	}
	up := scripted(t, refusing(429), slow, refusing(429), answering)
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", func(s *Settings) {
		patient(2*time.Second, 50*time.Millisecond, 50*time.Millisecond, nil)(s)
		s.Patience.Waiting = 1
		s.Auth = manyAtOnce(t)
	})
	first := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(patienceBody))
		request.Header.Set("Authorization", "Bearer "+testKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			first <- 0
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		first <- response.StatusCode
	}()
	// the first request has waited once and is asking again (the slow call)
	for deadline := time.Now().Add(2 * time.Second); len(up.times()) < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	newcomer := h.post(t, patienceBody, testKey)
	raw, _ := io.ReadAll(newcomer.Body)
	if newcomer.StatusCode != http.StatusServiceUnavailable || time.Since(started) > 150*time.Millisecond {
		t.Fatalf("the newcomer finds the room taken and is told at once: %d in %s: %s", newcomer.StatusCode, time.Since(started), raw)
	}
	if status := <-first; status != 200 {
		t.Fatalf("the request that had waited is answered, not turned away for the newcomer: %d", status)
	}
}
