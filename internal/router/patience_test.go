package router

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(2*time.Second, 5*time.Millisecond, 20*time.Millisecond, nil))
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
	h := newHarnessWith(t, routeFor(upstream("alibaba/model-studio", limited.server.URL, 1), upstream("deepseek/api", backup.server.URL, 2)), board, patient(2*time.Second, 5*time.Millisecond, 20*time.Millisecond, &alerts))
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
	if len(got) != 1 || !strings.Contains(got[0], "alibaba/model-studio (HTTP 429)") || !strings.Contains(got[0], "deepseek/api") {
		t.Fatalf("one fallback alert naming the refusal and who answered, got %q", got)
	}
}

// An upstream that asks for longer than the request may wait: the customer is told to come back, in
// Lebrel's words and with the time the upstream named. Its own words speak of Lebrel's account with
// it and never reach the customer; nothing is charged, and the operator hears of it.
func TestWhenTheWaitIsLongerThanThePatienceTheCustomerIsToldToRetryInLebrelsWords(t *testing.T) {
	up := scripted(t, refusing(429, "Retry-After", "7"))
	var alerts alertLog
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(300*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, &alerts))
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
	if len(got) != 1 || !strings.Contains(got[0], "ningún proveedor") || !strings.Contains(got[0], "zai/api-flash (HTTP 429)") {
		t.Fatalf("one outage alert naming the refusal, got %q", got)
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
	if len(got) != 1 || !strings.Contains(got[0], "zai/api-flash (HTTP 429)") || !strings.Contains(got[0], fmt.Sprintf("%d intentos", len(times))) {
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
	time.Sleep(450 * time.Millisecond) // past the time the upstream asked for
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
		Settlements: queue, Logger: log.New(io.Discard, "", 0), Patience: Patience{Budget: 10 * time.Second}})
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
	// A time the upstream names is kept as asked from its first refusal, within reason at both ends.
	for asked, want := range map[time.Duration]time.Duration{7 * time.Second: 7 * time.Second, time.Hour: pauseLongest, time.Millisecond: 25 * time.Millisecond} {
		var fresh pause
		if got := fresh.refuse(now, asked, patience); got != want {
			t.Fatalf("asked for %s: left alone %s, want %s", asked, got, want)
		}
	}
	var none *pause // a route that is not the router's own never panics
	if none.left(now) != 0 || none.refuse(now, time.Second, patience) != 0 {
		t.Fatal("no pause, nothing to wait for")
	}
	none.served()
}

// An upstream that refuses everyone: from its pauseAfter-th refusal in a row it is left alone, and a
// customer who comes meanwhile is told at once to come back, without a call and without waiting out
// the patience. When its time is over it is asked again, and an answer puts everything back.
func TestAnUpstreamThatRefusesEveryoneIsLeftAloneAndCustomersAreToldAtOnce(t *testing.T) {
	script := make([]http.HandlerFunc, 0, pauseAfter+1)
	for i := 0; i < pauseAfter; i++ {
		script = append(script, refusing(429))
	}
	up := scripted(t, append(script, answering)...)
	// a patience too short for any wait: each request is one call
	h := newHarnessWith(t, routeFor(upstream("zai/api-flash", up.server.URL, 1)), "", patient(time.Millisecond, 300*time.Millisecond, time.Second, nil))
	ask := func() (int, string) {
		response := h.post(t, patienceBody, testKey)
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(raw)
	}
	for i := 1; i <= pauseAfter; i++ {
		if status, raw := ask(); status != http.StatusServiceUnavailable || len(up.times()) != i {
			t.Fatalf("refusal %d: status %d after %d calls: %s", i, status, len(up.times()), raw)
		}
	}
	started := time.Now()
	if status, raw := ask(); status != http.StatusServiceUnavailable || !strings.Contains(raw, `"upstream_busy"`) || len(up.times()) != pauseAfter || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("while it is left alone the customer is told at once and it is not called: status %d, %d calls, %s: %s", status, len(up.times()), time.Since(started), raw)
	}
	time.Sleep(320 * time.Millisecond) // its time is over
	if status, raw := ask(); status != 200 || len(up.times()) != pauseAfter+1 {
		t.Fatalf("asked again when its time is over: status %d after %d calls: %s", status, len(up.times()), raw)
	}
	if held := h.router.pauses[pauseKey(&h.route, "zai/api-flash")]; held.refused != 0 || held.left(time.Now()) != 0 {
		t.Fatalf("an answer puts everything back: %d in a row, left alone %s", held.refused, held.left(time.Now()))
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
	up := scripted(t, refusing(429))
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
