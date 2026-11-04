package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/ledger"
	"lebrel-router/internal/meter"
	"lebrel-router/internal/model"
	"lebrel-router/internal/settle"
)

// serverWith runs a router over the harness's route, keys and signer with its own ledger and queue.
func serverWith(t *testing.T, h *harness, l ledger.Ledger, queue *settle.Queue, tune func(*Settings)) *httptest.Server {
	t.Helper()
	settings := Settings{Routes: Config{Routes: []Route{h.route}}, Auth: h.auth, Ledger: l, Signer: h.signer, Receipts: NewReceiptStore(time.Hour, 100),
		Settlements: queue, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return time.Unix(1_900_000_000, 0) }}
	if tune != nil {
		tune(&settings)
	}
	server, err := New(settings)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts
}

func complete(t *testing.T, base, body string) (*http.Response, error) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testKey)
	return http.DefaultClient.Do(request)
}

const streamBody = `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`

// holdLedger keeps every acceptance waiting until the test lets it through.
type holdLedger struct {
	*ledger.InMemory
	entered  chan struct{}
	release  chan struct{}
	accepted atomic.Int64
}

func (l *holdLedger) MarkAccepted(ctx context.Context, id string) error {
	l.entered <- struct{}{}
	<-l.release
	l.accepted.Add(1)
	return l.InMemory.MarkAccepted(ctx, id)
}

func TestTheAcceptanceIsRecordedBeforeTheFirstByte(t *testing.T) {
	up := newFakeUpstream(t, sseHandler)
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	held := &holdLedger{InMemory: h.ledger, entered: make(chan struct{}, 1), release: make(chan struct{})}
	queue := runningQueue(t, held)
	ts := serverWith(t, h, held, queue, nil)
	var once sync.Once
	open := func() { once.Do(func() { close(held.release) }) }
	t.Cleanup(open) // a failing test must not leave the request waiting
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	answered := make(chan *http.Response, 1)
	go func() {
		response, err := complete(t, ts.URL, streamBody)
		if err != nil {
			t.Error(err)
			close(answered)
			return
		}
		answered <- response
	}()
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the router never told the control plane the upstream accepted")
	}
	select {
	case <-answered:
		t.Fatal("the customer saw the answer before the acceptance was recorded")
	case <-time.After(100 * time.Millisecond):
	}
	open()
	response := <-answered
	if response == nil {
		t.FailNow()
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("status %d body %s", response.StatusCode, raw)
	}
	flushed(t, queue)
	after, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	if before-after != 4 {
		t.Fatalf("charged %d, want 4", before-after)
	}
	if stats := queue.Stats(); held.accepted.Load() != 1 || stats.Delivered != 1 {
		t.Fatalf("the acceptance happened inside the request, only the settlement went through the queue: accepted %d, %+v", held.accepted.Load(), stats)
	}
}

// slowLedger never answers the first acceptance in time; later calls go through.
type slowLedger struct {
	*ledger.InMemory
	mu    sync.Mutex
	calls []string
}

func (l *slowLedger) MarkAccepted(ctx context.Context, id string) error {
	l.mu.Lock()
	first := len(l.calls) == 0
	l.calls = append(l.calls, "accepted")
	l.mu.Unlock()
	if first {
		<-ctx.Done()
		return fmt.Errorf("%w: %w", ledger.ErrUnavailable, ctx.Err())
	}
	return l.InMemory.MarkAccepted(ctx, id)
}

func (l *slowLedger) Settle(ctx context.Context, id string, actual int64, usage model.Usage, outcome model.SettlementOutcome, provider string) (model.Settlement, error) {
	l.mu.Lock()
	l.calls = append(l.calls, "settle")
	l.mu.Unlock()
	return l.InMemory.Settle(ctx, id, actual, usage, outcome, provider)
}

func TestASlowControlPlaneOwesTheAcceptanceThroughTheQueue(t *testing.T) {
	up := newFakeUpstream(t, sseHandler)
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	slow := &slowLedger{InMemory: h.ledger}
	queue := runningQueue(t, slow)
	ts := serverWith(t, h, slow, queue, func(s *Settings) { s.AcceptTimeout = 50 * time.Millisecond })
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	started := time.Now()
	response, err := complete(t, ts.URL, streamBody)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("the answer must reach the customer: status %d body %s", response.StatusCode, raw)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the customer waited %s, the acceptance waits at most its timeout", elapsed)
	}
	flushed(t, queue)
	after, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	if before-after != 4 {
		t.Fatalf("charged %d, want 4", before-after)
	}
	slow.mu.Lock()
	defer slow.mu.Unlock()
	if strings.Join(slow.calls, ",") != "accepted,accepted,settle" {
		t.Fatalf("the late acceptance goes through the queue before the settlement: %v", slow.calls)
	}
}

// refusingLedger answers every acceptance the way the platform answers for a closed reservation.
type refusingLedger struct{ *ledger.InMemory }

func (l *refusingLedger) MarkAccepted(context.Context, string) error {
	return fmt.Errorf("%w: %w", ledger.ErrReservationConflict,
		controlplane.NewHTTPError(409, []byte(`{"error":{"code":"reservation_not_open","message":"Reservation cannot accept new upstream evidence"}}`)))
}

func TestARefusedAcceptanceWithholdsTheAnswerAndChargesNothing(t *testing.T) {
	up := newFakeUpstream(t, sseHandler)
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	refusing := &refusingLedger{InMemory: h.ledger}
	queue := runningQueue(t, refusing)
	ts := serverWith(t, h, refusing, queue, nil)
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	response, err := complete(t, ts.URL, streamBody)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "billing_unavailable") || strings.Contains(string(raw), "hello") {
		t.Fatalf("nothing can be charged, so nothing is answered: status %d body %s", response.StatusCode, raw)
	}
	flushed(t, queue)
	if after, _ := h.ledger.Balance(context.Background(), "acct_test_000001"); after != before {
		t.Fatalf("the hold must be released in full, balance moved by %d", before-after)
	}
}

func TestReceiptsAreFoundOnTheOtherInstances(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	// instance A answers the request; B is the other instance behind the same address
	a := h.server
	var peerCalls atomic.Int64
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerCalls.Add(1)
		if r.Header.Get(receiptPeerHeader) != "1" {
			t.Errorf("a peer lookup must say so: %v", r.Header)
		}
		proxy, _ := http.NewRequest(r.Method, a.URL+r.URL.Path, nil)
		proxy.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(proxy)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer counting.Close()
	b := serverWith(t, h, h.ledger, runningQueue(t, h.ledger), func(s *Settings) { s.ReceiptPeers = []string{counting.URL} })
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	requestID := response.Header.Get("X-Request-ID")
	original, _ := h.receipts.Get(requestID)
	for _, path := range []string{"/v1/receipts/" + requestID, "/m/deepseek--deepseek-v4.1-flash/v1/receipts/" + requestID} {
		fetched, err := http.Get(b.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var document SignedDocument
		_ = json.NewDecoder(fetched.Body).Decode(&document)
		fetched.Body.Close()
		if fetched.StatusCode != 200 || document.Signature != original.Signature {
			t.Fatalf("%s on the other instance: status %d", path, fetched.StatusCode)
		}
	}
	if calls := peerCalls.Load(); calls != 1 {
		t.Fatalf("a receipt found on a peer is kept: %d peer calls for two reads", calls)
	}
	// ids that newRequestID never makes are not asked of anyone
	if missing, _ := http.Get(b.URL + "/v1/receipts/req_../../readyz"); missing.StatusCode != 404 {
		t.Fatalf("status %d", missing.StatusCode)
	}
	if calls := peerCalls.Load(); calls != 1 {
		t.Fatalf("an invalid id reached a peer: %d calls", calls)
	}
	// a peer that signs with another key is not believed
	stranger, err := NewSigner(bytes.Repeat([]byte{9}, 32), "stranger")
	if err != nil {
		t.Fatal(err)
	}
	strangerReceipts := NewReceiptStore(time.Hour, 10)
	forged, _ := stranger.RouteReceipt(ReceiptInput{RequestID: "req_0123456789abcdef0123456789abcdef", ModelID: h.route.ModelID}, time.Unix(1_900_000_000, 0))
	strangerReceipts.Put("req_0123456789abcdef0123456789abcdef", forged)
	strangerServer := serverWith(t, &harness{route: h.route, auth: h.auth, signer: stranger}, h.ledger, runningQueue(t, h.ledger), func(s *Settings) { s.Receipts = strangerReceipts })
	c := serverWith(t, h, h.ledger, runningQueue(t, h.ledger), func(s *Settings) { s.ReceiptPeers = []string{strangerServer.URL} })
	if refused, _ := http.Get(c.URL + "/v1/receipts/req_0123456789abcdef0123456789abcdef"); refused.StatusCode != 404 {
		t.Fatalf("a receipt signed with another key must not be served: %d", refused.StatusCode)
	}
	// two instances that list each other answer a missing receipt once, without a loop
	y := httptest.NewUnstartedServer(nil)
	t.Cleanup(y.Close)
	x := serverWith(t, h, h.ledger, runningQueue(t, h.ledger), func(s *Settings) { s.ReceiptPeers = []string{"http://" + y.Listener.Addr().String()} })
	yServer, err := New(Settings{Routes: Config{Routes: []Route{h.route}}, Auth: h.auth, Ledger: h.ledger, Signer: h.signer, Receipts: NewReceiptStore(time.Hour, 10),
		Settlements: runningQueue(t, h.ledger), ReceiptPeers: []string{x.URL}, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	y.Config.Handler = yServer
	y.Start()
	started := time.Now()
	if gone, _ := http.Get(x.URL + "/v1/receipts/req_ffffffffffffffffffffffffffffffff"); gone.StatusCode != 404 || time.Since(started) > time.Second {
		t.Fatalf("missing receipt: %d after %s", gone.StatusCode, time.Since(started))
	}
}

// stuckLedger never answers a settlement until the test ends.
type stuckLedger struct {
	*ledger.InMemory
	done chan struct{}
}

func (l *stuckLedger) Settle(ctx context.Context, _ string, _ int64, _ model.Usage, _ model.SettlementOutcome, _ string) (model.Settlement, error) {
	select {
	case <-ctx.Done():
		return model.Settlement{}, ctx.Err()
	case <-l.done:
		return model.Settlement{}, ledger.ErrUnavailable
	}
}

func TestSettlementHealthIsForMonitorsAndNeverTakesTheRoutesOffline(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	stuck := &stuckLedger{InMemory: h.ledger, done: make(chan struct{})}
	defer close(stuck.done)
	var clock atomic.Int64
	clock.Store(time.Unix(1_900_000_000, 0).UnixNano())
	queue, err := settle.New("", stuck, settle.Options{Now: func() time.Time { return time.Unix(0, clock.Load()) }, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go queue.Run(ctx)
	edge := "v1=" + DeriveEdgeProxyToken("edge-secret")
	get := func(base, path string, headers map[string]string) (int, map[string]any) {
		request, _ := http.NewRequest(http.MethodGet, base+path, nil)
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var document map[string]any
		_ = json.NewDecoder(bufio.NewReader(response.Body)).Decode(&document)
		return response.StatusCode, document
	}
	// without its own token the endpoint does not exist, whatever the caller holds
	closed := serverWith(t, h, stuck, queue, func(s *Settings) { s.EdgeProxySecret = "edge-secret" })
	if status, _ := get(closed.URL, settlementHealthPath, map[string]string{edgeAuthorizationHeader: edge}); status != http.StatusNotFound {
		t.Fatalf("no HEALTH_TOKEN, no endpoint: %d", status)
	}
	ts := serverWith(t, h, stuck, queue, func(s *Settings) { s.EdgeProxySecret, s.HealthToken = "edge-secret", "health-token-for-monitors" })
	for name, headers := range map[string]map[string]string{
		"nothing":       nil,
		"the edge's":    {edgeAuthorizationHeader: edge},
		"a wrong token": {"Authorization": "Bearer health-token-for-monitorz"},
		"a prefix":      {"Authorization": "Bearer health-token"},
	} {
		if status, _ := get(ts.URL, settlementHealthPath, headers); status != http.StatusUnauthorized {
			t.Fatalf("%s must not open the backlog: %d", name, status)
		}
	}
	monitor := map[string]string{"Authorization": "Bearer health-token-for-monitors"}
	status, document := get(ts.URL, settlementHealthPath, monitor)
	if status != 200 || document["settlements"].(map[string]any)["status"] != "ok" || document["signing_key_id"] != h.signer.KeyID || document["instance_id"] != h.signer.InstanceID {
		t.Fatalf("health %d %v", status, document)
	}
	queue.Settle("res_late", "req_late", 4, model.Usage{}, model.SettlementSucceeded, "deepseek/api")
	clock.Add(int64(8 * time.Second))
	status, document = get(ts.URL, settlementHealthPath, monitor)
	settlements := document["settlements"].(map[string]any)
	if status != http.StatusServiceUnavailable || settlements["status"] != "lagging" || settlements["oldest_age_seconds"].(float64) < 8 {
		t.Fatalf("a settlement owed for 8 s is lagging: %d %v", status, document)
	}
	if status, _ := get(ts.URL, "/m/deepseek--deepseek-v4.1-flash/readyz", map[string]string{edgeAuthorizationHeader: edge}); status != 200 {
		t.Fatalf("the routes stay ready while bookkeeping lags: %d", status)
	}
}

func TestAStreamedLineLongerThanTheBufferArrivesWhole(t *testing.T) {
	long := strings.Repeat("x", 3<<20)
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", long)
		fmt.Fprint(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	before := h.balance(t)
	response := h.post(t, streamBody, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(raw), long) || !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("status %d, %d bytes: a 3 MB event must pass whole", response.StatusCode, len(raw))
	}
	if got := before - h.balance(t); got != 4 {
		t.Fatalf("the usage after the long line still settles exactly: charged %d", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if document, ok := h.receipts.Get(response.Header.Get("X-Request-ID")); ok {
			if document.Payload["response_sha256"] != SHA256Hex([]byte(long)) {
				t.Fatal("the receipt hashes the whole answer")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no receipt")
}

// slowStream is a lab whose answer takes long: one one-character event every 20 ms for 10 s.
func slowStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for i := 0; i < 500; i++ {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
		fmt.Fprint(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
		flusher.Flush()
	}
	fmt.Fprint(w, "data: {\"id\":\"s\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":500,\"total_tokens\":512}}\n\ndata: [DONE]\n\n")
	flusher.Flush()
}

// A shutdown whose budget is spent cuts the answers still streaming: each is settled for what it
// relayed (an estimate, said so in its receipt), never left accepted for the reconciler's maximum,
// and reported by reservation.
func TestAShutdownCutStreamIsChargedForWhatItRelayed(t *testing.T) {
	up := newFakeUpstream(t, slowStream)
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	queue := runningQueue(t, h.ledger)
	server, err := New(Settings{Routes: Config{Routes: []Route{h.route}}, Auth: h.auth, Ledger: h.ledger, Signer: h.signer, Receipts: h.receipts,
		Settlements: queue, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	response, err := complete(t, ts.URL, streamBody)
	if err != nil {
		t.Fatal(err)
	}
	requestID := response.Header.Get("X-Request-ID")
	received := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		events := 0
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), `"content":"x"`) {
				events++
			}
		}
		received <- events
	}()
	time.Sleep(300 * time.Millisecond) // about 15 events relayed
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cut := server.CutStreams(ctx)
	if len(cut) != 1 || server.InFlight() != 0 {
		t.Fatalf("one answer cut and none left in flight: %v, %d", cut, server.InFlight())
	}
	events := <-received
	if events < 5 || events > 100 {
		t.Fatalf("the customer got the start of the answer, then the end of the stream: %d events", events)
	}
	flushed(t, queue)
	after, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	document, ok := h.receipts.Get(requestID)
	if !ok {
		t.Fatal("the cut answer still has its receipt")
	}
	completion, _ := document.Payload["completion_tokens"].(int64)
	if document.Payload["usage_exact"] != false || completion != int64(events) {
		t.Fatalf("the receipt says what was relayed, as an estimate: exact %v, completion %v for %d events", document.Payload["usage_exact"], document.Payload["completion_tokens"], events)
	}
	charged := before - after
	price, _ := document.Payload["price_microusd"].(int64)
	maximum, _ := h.route.MeterPricing().MaximumScaled(meter.RequestInfo{EstimatedPromptTokens: 64, MaxOutputTokens: 512}, model.QueueShort, 100)
	if charged <= 0 || charged != price || charged >= maximum {
		t.Fatalf("charged %d (receipt %d) for what was relayed, below the reserved maximum %d", charged, price, maximum)
	}
}

// A whole answer the shutdown cut before it was read reached nobody: nothing is charged.
func TestAShutdownCutBufferedAnswerIsNotCharged(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the answer is never finished
	})
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	queue := runningQueue(t, h.ledger)
	server, err := New(Settings{Routes: Config{Routes: []Route{h.route}}, Auth: h.auth, Ledger: h.ledger, Signer: h.signer, Receipts: h.receipts,
		Settlements: queue, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	answered := make(chan *http.Response, 1)
	go func() {
		response, err := complete(t, ts.URL, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
		if err != nil {
			t.Error(err)
		}
		answered <- response
	}()
	waitAccepted := time.Now().Add(5 * time.Second)
	for server.InFlight() == 0 && time.Now().Before(waitAccepted) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cut := server.CutStreams(ctx); len(cut) != 1 {
		t.Fatalf("cut %v", cut)
	}
	response := <-answered
	if response == nil {
		t.FailNow()
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "router_restarting") {
		t.Fatalf("status %d body %s", response.StatusCode, raw)
	}
	flushed(t, queue)
	if after, _ := h.ledger.Balance(context.Background(), "acct_test_000001"); after != before {
		t.Fatalf("nothing reached the customer, nothing is charged: %d", before-after)
	}
}

// cutHarness is a router over one fake lab with a running queue, for the shutdown tests.
func cutHarness(t *testing.T, lab func(w http.ResponseWriter, r *http.Request)) (*harness, *Server, *settle.Queue, *httptest.Server) {
	t.Helper()
	up := newFakeUpstream(t, lab)
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	queue := runningQueue(t, h.ledger)
	server, err := New(Settings{Routes: Config{Routes: []Route{h.route}}, Auth: h.auth, Ledger: h.ledger, Signer: h.signer, Receipts: h.receipts,
		Settlements: queue, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return h, server, queue, ts
}

// A customer who stopped reading leaves the handler blocked in a write a cancelled context does
// not end: the cut's write deadline ends it, and the answer is settled for what it relayed (an
// estimate, never the reserved maximum), listed with the others the shutdown cut.
func TestAShutdownCutStreamWhoseCustomerStoppedReadingIsChargedForWhatItRelayed(t *testing.T) {
	h, server, queue, ts := cutHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunk := strings.Repeat("x", 16384)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
			fmt.Fprintf(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%s\"}}]}\n\n", chunk)
			flusher.Flush()
		}
	})
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	response, err := complete(t, ts.URL, streamBody) // the headers are read, the body never is
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	time.Sleep(1500 * time.Millisecond) // the socket buffers fill and the handler blocks in a write
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cut := server.CutStreams(ctx)
	if len(cut) != 1 || server.InFlight() != 0 {
		t.Fatalf("the stalled answer is cut within the write grace and listed: %v, %d in flight", cut, server.InFlight())
	}
	flushed(t, queue)
	after, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	document, ok := h.receipts.Get(requestID)
	if !ok {
		t.Fatal("the cut answer still has its receipt")
	}
	completion, _ := document.Payload["completion_tokens"].(int64)
	price, _ := document.Payload["price_microusd"].(int64)
	maximum, _ := h.route.MeterPricing().MaximumScaled(meter.RequestInfo{EstimatedPromptTokens: 64, MaxOutputTokens: 512}, model.QueueShort, 100)
	charged := before - after
	if document.Payload["usage_exact"] != false || completion <= 0 || charged != price || charged <= 0 || charged >= maximum {
		t.Fatalf("settled for what was relayed, as an estimate: exact %v, completion %d, charged %d (receipt %d), maximum %d",
			document.Payload["usage_exact"], completion, charged, price, maximum)
	}
}

// A cut while the lab has not sent its first byte is a restart, not a lab failure: the customer
// gets the router's own 503 to retry, nothing is charged, and the reservation is listed as cut.
func TestAShutdownCutBeforeTheLabAnswersAsksTheCustomerToRetry(t *testing.T) {
	h, server, queue, ts := cutHarness(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // the headers never come
	})
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	answered := make(chan *http.Response, 1)
	go func() {
		response, err := complete(t, ts.URL, streamBody)
		if err != nil {
			t.Error(err)
		}
		answered <- response
	}()
	for deadline := time.Now().Add(5 * time.Second); server.InFlight() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
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
	if len(cut) != 1 {
		t.Fatalf("the reservation is listed as cut: %v", cut)
	}
	flushed(t, queue)
	if after, _ := h.ledger.Balance(context.Background(), "acct_test_000001"); after != before {
		t.Fatalf("never accepted, nothing charged: %d", before-after)
	}
}

// Reasoning the lab streamed reached the customer and is billed by the lab: a cut stream that
// relayed only reasoning is charged for it, once, even from a lab that sends it under both names.
func TestAShutdownCutStreamOfReasoningIsChargedForItOnce(t *testing.T) {
	h, server, queue, ts := cutHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 500; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			fmt.Fprint(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":null,\"reasoning_content\":\"thinking \",\"reasoning\":\"thinking \"}}]}\n\n")
			flusher.Flush()
		}
	})
	before, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	response, err := complete(t, ts.URL, streamBody)
	if err != nil {
		t.Fatal(err)
	}
	requestID := response.Header.Get("X-Request-ID")
	received := make(chan int64, 1)
	go func() {
		scanner := bufio.NewScanner(response.Body)
		var events int64
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "reasoning_content") {
				events++
			}
		}
		received <- events
	}()
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cut := server.CutStreams(ctx); len(cut) != 1 {
		t.Fatalf("cut %v", cut)
	}
	events := <-received
	flushed(t, queue)
	after, _ := h.ledger.Balance(context.Background(), "acct_test_000001")
	document, ok := h.receipts.Get(requestID)
	if !ok {
		t.Fatal("no receipt")
	}
	completion, _ := document.Payload["completion_tokens"].(int64)
	// "thinking " is 9 characters: one token per four, at least one per event, counted once
	if want := max(events, (9*events+3)/4); events < 5 || completion != want || before-after <= 0 {
		t.Fatalf("the relayed reasoning is charged once: %d events, completion %d (want %d), charged %d", events, completion, want, before-after)
	}
}

// Only a cut that ended the call to the labs is answered as a restart and listed: a customer who
// hung up first, or labs that failed on their own, keep their usual answer (and their zero charge).
func TestOnlyTheCutItselfIsAnsweredAsARestart(t *testing.T) {
	cutFirst, stop := context.WithCancelCause(context.Background())
	stop(errShutdownCut)
	if !cutCaused(cutFirst, fmt.Errorf("deepseek/api: %w", context.Canceled)) {
		t.Error("the cut ended the call: a restart")
	}
	if cutCaused(cutFirst, errors.New("deepseek/api: connection refused")) {
		t.Error("the labs failed on their own at the same instant: not a restart")
	}
	customer, leave := context.WithCancel(context.Background())
	goneFirst, stop := context.WithCancelCause(customer)
	leave()
	stop(errShutdownCut) // too late: the customer had already gone
	if cutCaused(goneFirst, fmt.Errorf("deepseek/api: %w", context.Canceled)) {
		t.Error("the customer hung up before the cut: not a restart")
	}
}

// deadlineRecorder is a response that notes the write deadline it is given.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu       sync.Mutex
	deadline time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline = at
	return nil
}

// A cut gives a stream's writes 1 s, and a whole answer being written (already settled exactly)
// 5 s: a slower reader still gets all of it.
func TestACutGivesAStreamOneSecondAndAWholeAnswerFive(t *testing.T) {
	for _, tc := range []struct {
		body  string
		grace time.Duration
	}{
		{streamBody, time.Second},
		{`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, 5 * time.Second},
	} {
		_, server, _, _ := cutHarness(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // the answer never ends: the cut ends it
		})
		recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer "+testKey)
		served := make(chan struct{})
		go func() {
			defer close(served)
			server.ServeHTTP(recorder, request)
		}()
		for deadline := time.Now().Add(5 * time.Second); server.InFlight() == 0 && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		cutAt := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		server.CutStreams(ctx)
		cancel()
		<-served
		recorder.mu.Lock()
		given := recorder.deadline.Sub(cutAt)
		recorder.mu.Unlock()
		if given < tc.grace-200*time.Millisecond || given > tc.grace+200*time.Millisecond {
			t.Fatalf("%s: the cut gave its writes %s, want %s", tc.body, given, tc.grace)
		}
	}
}
