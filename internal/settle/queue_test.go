package settle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/ledger"
	"lebrel-router/internal/model"
)

var quiet = log.New(io.Discard, "", 0)

// flaky answers the control plane's way: the first “failures“ calls fail as unavailable, then
// every call succeeds; every call is recorded in order.
type flaky struct {
	mu       sync.Mutex
	failures int
	calls    []string
	refuse   map[string]error
}

func (f *flaky) record(kind, reservation string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kind+":"+reservation)
	if err, ok := f.refuse[reservation]; ok {
		return err
	}
	if f.failures > 0 {
		f.failures--
		return ledger.ErrUnavailable
	}
	return nil
}

func (f *flaky) MarkAccepted(_ context.Context, reservation string) error {
	return f.record(KindAccepted, reservation)
}

func (f *flaky) Settle(_ context.Context, reservation string, actual int64, _ model.Usage, outcome model.SettlementOutcome, _ string) (model.Settlement, error) {
	if err := f.record(KindSettle, reservation); err != nil {
		return model.Settlement{}, err
	}
	return model.Settlement{ReservationID: reservation, Actual: actual, Outcome: outcome}, nil
}

func (f *flaky) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func run(t *testing.T, q *Queue) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

func flush(t *testing.T, q *Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := q.Flush(ctx); err != nil {
		t.Fatalf("still owed after 10s: %d pending, stats %+v", q.Pending(), q.Stats())
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDeliversInOrderAndRetriesUntilTheControlPlaneAnswers(t *testing.T) {
	l := &flaky{failures: 3}
	q, err := New(t.TempDir(), l, Options{MaxBackoff: 20 * time.Millisecond, Logger: log.New(os.Stderr, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	run(t, q)
	q.Accepted("res_1", "req_1")
	q.Settle("res_1", "req_1", 750, model.Usage{PromptTokens: 90, CompletionTokens: 30, TotalTokens: 120}, model.SettlementSucceeded, "moonshot/api")
	flush(t, q)
	calls := l.seen()
	if len(calls) < 5 {
		t.Fatalf("expected retries before success, got %v", calls)
	}
	var settled int
	for _, call := range calls {
		switch call {
		case "accepted:res_1":
			if settled > 0 {
				t.Fatalf("acceptance delivered after settlement: %v", calls)
			}
		case "settle:res_1":
			settled++
		}
	}
	if calls[len(calls)-1] != "settle:res_1" || calls[len(calls)-2] != "accepted:res_1" {
		t.Fatalf("the last two deliveries must be the successful acceptance then the settlement: %v", calls)
	}
	stats := q.Stats()
	if stats.Delivered != 2 || stats.Retried != 3 || stats.Dead != 0 || stats.Pending != 0 {
		t.Fatalf("stats %+v", stats)
	}
	q.Close()
	if owed, bad, err := readJournal(filepath.Join(q.dir, journalName)); err != nil || len(owed) != 0 || len(bad) != 0 {
		t.Fatalf("the journal must owe nothing once both are delivered: %d owed, %d unreadable, %v", len(owed), len(bad), err)
	}
}

func TestReplaysWhatAnEarlierProcessLeft(t *testing.T) {
	dir := t.TempDir()
	stuck := &flaky{failures: 1 << 30}
	q, err := New(dir, stuck, Options{MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	q.Settle("res_2", "req_2", 5, model.Usage{}, model.SettlementSucceeded, "zai/api")
	q.Accepted("res_3", "req_3")
	// both are on disk the moment they are owed, before anything is attempted
	if owed, _, err := readJournal(filepath.Join(dir, journalName)); err != nil || len(owed) != 2 {
		t.Fatalf("two operations expected in the journal, got %d (%v)", len(owed), err)
	}
	// the process dies here; a new one opens the same directory
	q.Close()
	healthy := &flaky{}
	again, err := New(dir, healthy, Options{MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Stats().Replayed != 2 || again.Pending() != 2 {
		t.Fatalf("replayed %d, pending %d", again.Stats().Replayed, again.Pending())
	}
	run(t, again)
	flush(t, again)
	if calls := healthy.seen(); len(calls) != 2 {
		t.Fatalf("both replayed operations must be delivered: %v", calls)
	}
}

func TestReplayPutsTheAcceptanceFirstWhateverTheFilesSay(t *testing.T) {
	dir := t.TempDir()
	created := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	write := func(op Op) {
		op.ID = opID(op.ReservationID, op.Kind)
		raw, _ := json.Marshal(op)
		if err := os.WriteFile(filepath.Join(dir, op.ID+".json"), raw, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// a clock step made the settlement look older than its acceptance
	write(Op{ReservationID: "res_x", RequestID: "req_x", Kind: KindSettle, Actual: 9, Outcome: model.SettlementSucceeded, CreatedAt: created})
	write(Op{ReservationID: "res_x", RequestID: "req_x", Kind: KindAccepted, CreatedAt: created.Add(time.Second)})
	l := &flaky{}
	q, err := New(dir, l, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	flush(t, q)
	if calls := l.seen(); len(calls) != 2 || calls[0] != "accepted:res_x" {
		t.Fatalf("the acceptance must go first: %v", calls)
	}
}

func TestTheDirectoryBelongsToOneRouter(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir, &flaky{}, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, &flaky{}, Options{Logger: quiet}); err == nil || !strings.Contains(err.Error(), "in use by another router") {
		t.Fatalf("a second router on the same directory must be refused: %v", err)
	}
	first.Close()
	again, err := New(dir, &flaky{}, Options{Logger: quiet})
	if err != nil {
		t.Fatalf("a closed directory is free again: %v", err)
	}
	again.Close()
}

func TestARefusalIsRecordedDeadAndNeverRetried(t *testing.T) {
	l := &flaky{refuse: map[string]error{
		"res_gone":     fmt.Errorf("%w: %w", ledger.ErrReservationMissing, controlplane.NewHTTPError(404, []byte(`{"error":{"code":"reservation_not_found"}}`))),
		"res_settled":  fmt.Errorf("%w: %w", ledger.ErrReservationConflict, controlplane.NewHTTPError(409, []byte(`{"error":{"code":"settlement_outcome_mismatch"}}`))),
		"res_bad":      fmt.Errorf("%w: %w", ledger.ErrUnavailable, controlplane.NewHTTPError(400, []byte(`{"error":{"code":"invalid_usage","message":"bad"}}`))),
		"res_throttle": controlplane.NewHTTPError(429, nil),
		"res_secret":   controlplane.NewHTTPError(401, nil),
	}}
	dir := t.TempDir()
	q, err := New(dir, l, Options{MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	q.Settle("res_gone", "req_a", 1, model.Usage{}, model.SettlementSucceeded, "x")
	q.Settle("res_settled", "req_b", 1, model.Usage{}, model.SettlementSucceeded, "x")
	q.Accepted("res_bad", "req_c")
	waitFor(t, "three dead letters", func() bool { return q.Stats().Dead == 3 })
	waitFor(t, "the three dead letters on disk", func() bool { return q.Health(time.Minute).DeadOnDisk == 3 })
	stats := q.Stats()
	if stats.Dead != 3 || stats.Pending != 0 {
		t.Fatalf("three refusals must be dead and nothing pending: %+v", stats)
	}
	dead, _ := os.ReadDir(filepath.Join(dir, "dead"))
	if len(dead) != 3 {
		t.Fatalf("dead letters on disk: %d", len(dead))
	}
	letters := q.drainDead()
	if len(letters) != 3 || q.Health(time.Minute).DeadOnDisk != 3 {
		t.Fatalf("every refusal is kept for the alert: %+v", letters)
	}
	codes := map[string]string{}
	for _, letter := range letters {
		codes[letter.RequestID] = letter.Code
	}
	if codes["req_b"] != "settlement_outcome_mismatch" || codes["req_c"] != "invalid_usage" {
		t.Fatalf("the alert names the control plane's code: %v", codes)
	}
	// throttling and a refused secret are not refusals of the operation: they are retried
	q.Accepted("res_throttle", "req_d")
	q.Accepted("res_secret", "req_e")
	waitFor(t, "retries", func() bool { return q.Stats().Retried >= 4 })
	if s := q.Stats(); s.Dead != 3 || s.Pending != 2 {
		t.Fatalf("a 429 and a 401 must be retried, not buried: %+v", s)
	}
}

func TestTheSameDeliveryTwiceIsOneDelivery(t *testing.T) {
	l := &flaky{}
	q, err := New("", l, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	run(t, q)
	q.Settle("res_4", "req_4", 1, model.Usage{}, model.SettlementSucceeded, "x")
	q.Settle("res_4", "req_4", 1, model.Usage{}, model.SettlementSucceeded, "x")
	flush(t, q)
	if calls := l.seen(); len(calls) != 1 {
		t.Fatalf("expected one delivery, got %v", calls)
	}
}

func TestTransientClassification(t *testing.T) {
	for _, tc := range []struct {
		err       error
		transient bool
	}{
		{ledger.ErrUnavailable, true},
		{errors.New("context deadline exceeded"), true},
		{controlplane.NewHTTPError(503, nil), true},
		{controlplane.NewHTTPError(500, nil), true},
		{controlplane.NewHTTPError(429, nil), true},
		{controlplane.NewHTTPError(408, nil), true},
		{controlplane.NewHTTPError(401, nil), true},
		{controlplane.NewHTTPError(403, nil), true},
		{controlplane.NewHTTPError(404, []byte(`{"error":{"code":"reservation_not_found"}}`)), false},
		// any other 404 says nothing about the reservation: a catch-all's own code, a page, a route
		// not deployed yet
		{controlplane.NewHTTPError(404, []byte(`{"error":{"code":"not_found"}}`)), true},
		{controlplane.NewHTTPError(404, nil), true},
		{controlplane.NewHTTPError(404, []byte("<html>404 Not Found</html>")), true},
		{controlplane.NewHTTPError(409, nil), false},
		{controlplane.NewHTTPError(400, nil), false},
		{controlplane.NewHTTPError(422, nil), false},
		{fmt.Errorf("%w: %w", ledger.ErrUnavailable, controlplane.NewHTTPError(400, nil)), false},
		{ledger.ErrReservationConflict, false},
		{ledger.ErrReservationMissing, false},
		{controlplane.NewHTTPError(308, nil), true},
		{controlplane.NewHTTPError(301, nil), true},
		{fmt.Errorf("%w: %w", ledger.ErrBatchEnvelope, fmt.Errorf("%w: %w", ledger.ErrReservationConflict, controlplane.NewHTTPError(409, nil))), true},
		{fmt.Errorf("%w: bad outcome", ledger.ErrInvalidOperation), false},
	} {
		if got := Transient(tc.err); got != tc.transient {
			t.Errorf("%v: transient=%t, want %t", tc.err, got, tc.transient)
		}
	}
}

// platform is a control plane answering every settlement with the platform's own refusal bodies.
func platform(t *testing.T, answers map[string]func(w http.ResponseWriter)) *ledger.Remote {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		reservation := parts[len(parts)-2]
		w.Header().Set("Content-Type", "application/json")
		if answer, ok := answers[reservation]; ok {
			answer(w)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(server.Close)
	client, err := controlplane.New(server.URL, "secret", 5*time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return ledger.NewRemote(client)
}

func refusal(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func TestAlreadySettledTheSameWayIsDoneAndAnythingElseIsDead(t *testing.T) {
	remote := platform(t, map[string]func(w http.ResponseWriter){
		// the platform names how it settled (docs/platform-settlement-batch.md): the same as ours
		"res_same": refusal(409, `{"error":{"code":"settlement_idempotency_conflict","message":"settled","settled":{"outcome":"succeeded","actualMicrousd":750}}}`),
		// the same, but for a different amount: a real conflict
		"res_other": refusal(409, `{"error":{"code":"settlement_idempotency_conflict","message":"settled","settled":{"outcome":"upstream_rejected","actualMicrousd":0}}}`),
		// today's platform, no details: a zero settlement that finds the hold already closed at zero
		"res_zero": refusal(409, `{"error":{"code":"settlement_idempotency_conflict","message":"Reservation was settled with different usage or billing parameters"}}`),
		// a zero settlement whose reservation is gone: nothing was ever charged
		"res_gone": refusal(404, `{"error":{"code":"reservation_not_found","message":"Credit reservation not found"}}`),
		// a zero settlement the platform says was accepted: the hold would be charged, a human must look
		"res_mismatch": refusal(409, `{"error":{"code":"settlement_outcome_mismatch","message":"Settlement outcome does not match upstream admission evidence"}}`),
		// an answered request whose hold was closed at zero meanwhile: lost revenue, a human must look
		"res_late": refusal(409, `{"error":{"code":"settlement_outcome_mismatch","message":"Settlement outcome does not match upstream admission evidence"}}`),
	})
	q, err := New(t.TempDir(), remote, Options{MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	q.Settle("res_same", "req_same", 750, model.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10}, model.SettlementSucceeded, "deepseek/api")
	q.Settle("res_other", "req_other", 750, model.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10}, model.SettlementSucceeded, "deepseek/api")
	q.Settle("res_zero", "req_zero", 0, model.Usage{}, model.SettlementUpstreamRejected, "deepseek/api")
	q.Settle("res_gone", "req_gone", 0, model.Usage{}, model.SettlementUpstreamRejected, "deepseek/api")
	q.Settle("res_mismatch", "req_mismatch", 0, model.Usage{}, model.SettlementUpstreamRejected, "deepseek/api")
	q.Settle("res_late", "req_late", 750, model.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10}, model.SettlementSucceeded, "deepseek/api")
	flush(t, q)
	waitFor(t, "three dead letters", func() bool { return q.Stats().Dead >= 3 })
	stats := q.Stats()
	if stats.AlreadySettled != 3 || stats.Dead != 3 || stats.Delivered != 0 {
		t.Fatalf("three already settled the same way, three that need a human: %+v", stats)
	}
	dead := map[string]bool{}
	for _, letter := range q.drainDead() {
		dead[letter.RequestID] = true
	}
	if !dead["req_other"] || !dead["req_mismatch"] || !dead["req_late"] {
		t.Fatalf("dead letters %v", dead)
	}
}

// A 404 that is not the platform's reservation_not_found (a page, a route not deployed yet, a
// wrong CONTROL_PLANE_URL, a catch-all answering with its own code) proves nothing about the
// reservation: it is retried under the lagging alert, never counted as settled and never buried.
func TestA404ThatIsNotThePlatformsIsRetried(t *testing.T) {
	var pages, catchAll atomic.Int64
	remote := platform(t, map[string]func(w http.ResponseWriter){
		"res_page": func(w http.ResponseWriter) {
			if pages.Add(1) <= 3 {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(404)
				fmt.Fprint(w, "<html><body>404 Not Found</body></html>")
				return
			}
			fmt.Fprint(w, `{}`) // the route is deployed again
		},
		"res_catchall": func(w http.ResponseWriter) {
			if catchAll.Add(1) <= 3 {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"error":{"code":"not_found","message":"unknown path"}}`)
				return
			}
			fmt.Fprint(w, `{}`)
		},
	})
	q, err := New(t.TempDir(), remote, Options{MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	q.Settle("res_page", "req_page", 0, model.Usage{}, model.SettlementUpstreamRejected, "deepseek/api")
	q.Settle("res_catchall", "req_catchall", 0, model.Usage{}, model.SettlementUpstreamRejected, "deepseek/api")
	flush(t, q)
	if s := q.Stats(); s.Delivered != 2 || s.Dead != 0 || s.AlreadySettled != 0 || s.Retried != 6 {
		t.Fatalf("three 404s each, then delivered: %+v", s)
	}
}

// gate is a control plane whose calls wait until released; it counts the calls in flight.
type gate struct {
	release  chan struct{}
	once     sync.Once
	inFlight atomic.Int64
	peak     atomic.Int64
	mu       sync.Mutex
	order    map[string][]string
	together map[string]int
}

func newGate() *gate {
	return &gate{release: make(chan struct{}), order: map[string][]string{}, together: map[string]int{}}
}

// open lets every waiting and later call through; register it with t.Cleanup after run, so a test
// that fails with calls held still ends.
func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

func (g *gate) enter(kind, reservation string) {
	now := g.inFlight.Add(1)
	for {
		peak := g.peak.Load()
		if now <= peak || g.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	g.mu.Lock()
	g.order[reservation] = append(g.order[reservation], kind)
	g.together[reservation]++
	if g.together[reservation] > 1 {
		g.order[reservation] = append(g.order[reservation], "OVERLAP")
	}
	g.mu.Unlock()
	<-g.release
	g.mu.Lock()
	g.together[reservation]--
	g.mu.Unlock()
	g.inFlight.Add(-1)
}

func (g *gate) MarkAccepted(_ context.Context, reservation string) error {
	g.enter(KindAccepted, reservation)
	return nil
}

func (g *gate) Settle(_ context.Context, reservation string, actual int64, _ model.Usage, outcome model.SettlementOutcome, _ string) (model.Settlement, error) {
	g.enter(KindSettle, reservation)
	return model.Settlement{ReservationID: reservation, Actual: actual, Outcome: outcome}, nil
}

func TestConcurrencyIsBoundedAndReservationsStayInOrder(t *testing.T) {
	g := newGate()
	q, err := New(t.TempDir(), g, Options{Concurrency: 16, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	t.Cleanup(g.open)
	for i := 0; i < 100; i++ {
		reservation := fmt.Sprintf("res_%03d", i)
		q.Accepted(reservation, "req_"+reservation)
		q.Settle(reservation, "req_"+reservation, 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	waitFor(t, "16 calls in flight", func() bool { return g.inFlight.Load() == 16 })
	time.Sleep(20 * time.Millisecond)
	if peak := g.peak.Load(); peak != 16 {
		t.Fatalf("calls in flight peaked at %d with a bound of 16", peak)
	}
	g.open()
	flush(t, q)
	if peak := g.peak.Load(); peak > 16 {
		t.Fatalf("calls in flight peaked at %d with a bound of 16", peak)
	}
	for reservation, kinds := range g.order {
		if len(kinds) != 2 || kinds[0] != KindAccepted || kinds[1] != KindSettle {
			t.Fatalf("%s: one at a time, acceptance first: %v", reservation, kinds)
		}
	}
	if s := q.Stats(); s.Delivered != 200 {
		t.Fatalf("stats %+v", s)
	}
}

// batcher is a control plane with the batch endpoint: it records every batch and can fail items.
type batcher struct {
	flaky
	mu      sync.Mutex
	batches [][]ledger.BatchItem
	answer  func(ledger.BatchItem) error
	missing bool
}

func (b *batcher) DeliverBatch(_ context.Context, items []ledger.BatchItem) ([]error, error) {
	if b.missing {
		return nil, fmt.Errorf("%w: %w", ledger.ErrBatchUnsupported, controlplane.NewHTTPError(404, nil))
	}
	b.mu.Lock()
	b.batches = append(b.batches, append([]ledger.BatchItem(nil), items...))
	b.mu.Unlock()
	errs := make([]error, len(items))
	for i, item := range items {
		if b.answer != nil {
			errs[i] = b.answer(item)
		}
	}
	return errs, nil
}

func TestBatchesCarryDistinctReservationsAndEachItemIsJudgedAlone(t *testing.T) {
	var throttled atomic.Bool
	b := &batcher{answer: func(item ledger.BatchItem) error {
		switch item.ReservationID {
		case "res_007":
			if throttled.CompareAndSwap(false, true) {
				return fmt.Errorf("%w: %w", ledger.ErrUnavailable, controlplane.NewHTTPError(503, nil))
			}
		case "res_013":
			return fmt.Errorf("%w: %w", ledger.ErrReservationConflict, controlplane.NewHTTPError(409, []byte(`{"error":{"code":"settlement_outcome_mismatch"}}`)))
		}
		return nil
	}}
	q, err := New(t.TempDir(), b, Options{BatchSize: 10, Concurrency: 2, MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := 0; i < 50; i++ {
		reservation := fmt.Sprintf("res_%03d", i)
		if i%5 == 0 {
			q.Accepted(reservation, "req_"+reservation)
		}
		q.Settle(reservation, "req_"+reservation, int64(i), model.Usage{}, model.SettlementSucceeded, "x")
	}
	run(t, q)
	flush(t, q)
	waitFor(t, "the refused one kept in dead/", func() bool { return q.Stats().Dead == 1 })
	b.mu.Lock()
	defer b.mu.Unlock()
	accepted := map[string]bool{}
	for _, batch := range b.batches {
		if len(batch) > 10 {
			t.Fatalf("a batch of %d with a size of 10", len(batch))
		}
		reservations := map[string]bool{}
		for _, item := range batch {
			if reservations[item.ReservationID] {
				t.Fatalf("one batch carried two operations of %s", item.ReservationID)
			}
			reservations[item.ReservationID] = true
			var index int
			fmt.Sscanf(item.ReservationID, "res_%d", &index)
			switch {
			case item.Kind == KindAccepted:
				accepted[item.ReservationID] = true
			case index%5 == 0 && !accepted[item.ReservationID]:
				// reservations 0, 5, 10... carry an acceptance that must land in an earlier call
				t.Fatalf("%s settled before its acceptance", item.ReservationID)
			}
		}
	}
	stats := q.Stats()
	if stats.Delivered != 59 || stats.Dead != 1 || stats.Retried != 1 || stats.Batches < 6 {
		t.Fatalf("60 operations: one refused, one retried then delivered: %+v", stats)
	}
	if calls := b.seen(); len(calls) != 0 {
		t.Fatalf("with the batch endpoint no single call is made: %v", calls)
	}
}

func TestAMissingBatchEndpointFallsBackToOneCallPerOperation(t *testing.T) {
	b := &batcher{missing: true}
	q, err := New(t.TempDir(), b, Options{BatchSize: 25, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := 0; i < 30; i++ {
		q.Settle(fmt.Sprintf("res_%02d", i), fmt.Sprintf("req_%02d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	run(t, q)
	flush(t, q)
	if calls := b.seen(); len(calls) != 30 {
		t.Fatalf("every operation delivered on its own endpoint: %d calls", len(calls))
	}
	if s := q.Stats(); s.Delivered != 30 || s.Retried != 0 || s.Dead != 0 {
		t.Fatalf("falling back costs no attempt: %+v", s)
	}
	if h := q.Health(time.Minute); h.Batching {
		t.Fatal("the health must say batching is off")
	}
}

// down is a control plane that fails every call until it comes back; it counts calls in flight
// and notes when each failed call arrived.
type down struct {
	up       atomic.Bool
	inFlight atomic.Int64
	peak     atomic.Int64
	calls    atomic.Int64
	mu       sync.Mutex
	failed   []time.Time
}

func (d *down) call() error {
	now := d.inFlight.Add(1)
	defer d.inFlight.Add(-1)
	d.calls.Add(1)
	if d.up.Load() {
		return nil
	}
	for {
		peak := d.peak.Load()
		if now <= peak || d.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	d.mu.Lock()
	d.failed = append(d.failed, time.Now())
	d.mu.Unlock()
	time.Sleep(time.Millisecond)
	return fmt.Errorf("%w: %w", ledger.ErrUnavailable, controlplane.NewHTTPError(503, nil))
}

func (d *down) failedSince(at time.Time) []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	var times []time.Time
	for _, t := range d.failed {
		if t.After(at) {
			times = append(times, t)
		}
	}
	return times
}

func (d *down) MarkAccepted(context.Context, string) error { return d.call() }
func (d *down) Settle(_ context.Context, reservation string, actual int64, _ model.Usage, outcome model.SettlementOutcome, _ string) (model.Settlement, error) {
	return model.Settlement{ReservationID: reservation, Actual: actual, Outcome: outcome}, d.call()
}

// With production backoff (up to a minute): the calls that fail together count as failed calls and
// switch the queue to one call at a time; an operation never attempted goes at once even then (it
// is as good a probe as any); retries wait for the probe spacing, which never passes five seconds;
// and the first answer puts everything back at full speed.
func TestAFailingControlPlaneIsProbedWithOneCallAtATime(t *testing.T) {
	d := &down{}
	q, err := New(t.TempDir(), d, Options{Concurrency: 32, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := 0; i < 100; i++ {
		q.Settle(fmt.Sprintf("res_%03d", i), fmt.Sprintf("req_%03d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	run(t, q)
	waitFor(t, "probing", func() bool { return q.Health(time.Minute).Probing })
	time.Sleep(20 * time.Millisecond) // the calls started before probing come back
	d.peak.Store(0)
	waitFor(t, "every operation tried once", func() bool { return d.calls.Load() >= 100 })
	if peak := d.peak.Load(); peak > 1 {
		t.Fatalf("while probing at most one call is in flight, saw %d", peak)
	}
	q.mu.Lock()
	spacing, failed := time.Until(q.probeAt), q.probes
	q.mu.Unlock()
	if failed < 50 || spacing > maxProbeSpacing+100*time.Millisecond {
		t.Fatalf("after %d failed probes the next retry waits %s; never more than %s", failed, spacing, maxProbeSpacing)
	}
	// a settlement that arrives while the queue probes goes at once
	before := d.calls.Load()
	arrived := time.Now()
	q.Settle("res_new", "req_new", 1, model.Usage{}, model.SettlementSucceeded, "x")
	waitFor(t, "the new settlement's call", func() bool { return d.calls.Load() > before })
	if took := time.Since(arrived); took > 500*time.Millisecond {
		t.Fatalf("a never-attempted settlement waited %s for the probe slot", took)
	}
	// the control plane answers again: the next new settlement is the probe that finds out
	d.up.Store(true)
	recovered := time.Now()
	q.Settle("res_after", "req_after", 1, model.Usage{}, model.SettlementSucceeded, "x")
	flush(t, q)
	if took := time.Since(recovered); took > 3*time.Second {
		t.Fatalf("once the control plane answers, everything goes out in seconds: took %s", took)
	}
	if h := q.Health(time.Minute); h.Probing || h.Delivered != 102 {
		t.Fatalf("back to full speed once the control plane answers: %+v", h)
	}
}

// Without new traffic, retries alone find the way back: the probe spacing is capped at five seconds,
// so an outage costs its own length plus at most that.
func TestRetriesAloneRecoverWithinTheProbeCap(t *testing.T) {
	d := &down{}
	q, err := New(t.TempDir(), d, Options{Concurrency: 32, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := 0; i < 40; i++ {
		q.Settle(fmt.Sprintf("res_%03d", i), fmt.Sprintf("req_%03d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	run(t, q)
	waitFor(t, "every operation tried once", func() bool { return d.calls.Load() >= 40 })
	d.up.Store(true)
	recovered := time.Now()
	flush(t, q)
	if took := time.Since(recovered); took > maxProbeSpacing+1500*time.Millisecond {
		t.Fatalf("the next probe comes within the cap: took %s", took)
	}
}

// One operation late with slots to spare is the platform or the network, not capacity: the router
// keeps it on disk and retries it, so it is told only after two minutes, without asking for an
// instance, and cleared when it is through.
func TestBacklogAgeRaisesAndClearsTheAlert(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	g := newGate()
	q, err := New(t.TempDir(), g, Options{Now: now, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var mu sync.Mutex
	var alerts []string
	notify := func(text string) {
		mu.Lock()
		alerts = append(alerts, text)
		mu.Unlock()
	}
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), alerts...)
	}
	run(t, q)
	t.Cleanup(g.open)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Watch(ctx, 5*time.Millisecond, 5*time.Second, notify)
	q.Settle("res_slow", "req_slow", 1, model.Usage{}, model.SettlementSucceeded, "x")
	waitFor(t, "the call in flight", func() bool { return g.inFlight.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if len(got()) != 0 {
		t.Fatalf("a fresh operation raises nothing: %v", got())
	}
	clock.Add(int64(6 * time.Second))
	time.Sleep(30 * time.Millisecond)
	if len(got()) != 0 {
		t.Fatalf("a delay with slots to spare waits two minutes before it is told: %v", got())
	}
	if h := q.Health(5 * time.Second); h.Status != "lagging" || h.OverAlertAge != 1 {
		t.Fatalf("health %+v", h)
	}
	clock.Add(int64(2 * time.Minute))
	waitFor(t, "the lagging alert", func() bool { return len(got()) == 1 })
	if text := got()[0]; !strings.Contains(text, "retraso") || !strings.Contains(text, "umbral 5,0 s") || !strings.Contains(text, "quedan llamadas libres") || strings.Contains(text, "añadir una instancia") {
		t.Fatalf("lagging alert %q", text)
	}
	time.Sleep(30 * time.Millisecond)
	if len(got()) != 1 {
		t.Fatalf("one alert per change, not one per check: %v", got())
	}
	g.open()
	waitFor(t, "the recovery", func() bool { return len(got()) == 2 })
	if text := got()[1]; !strings.Contains(text, "al día") {
		t.Fatalf("recovery %q", text)
	}
}

// Every call in use and answering while the backlog ages: that is capacity, told at the alert age.
func TestADelayAtTheCeilingAsksForAnInstance(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC).UnixNano())
	g := newGate()
	q, err := New(t.TempDir(), g, Options{Concurrency: 2, Now: func() time.Time { return time.Unix(0, clock.Load()) }, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	t.Cleanup(g.open)
	for i := 0; i < 3; i++ {
		q.Settle(fmt.Sprintf("res_%d", i), fmt.Sprintf("req_%d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	waitFor(t, "two calls in flight", func() bool { return g.inFlight.Load() == 2 })
	clock.Add(int64(6 * time.Second))
	alerts := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Watch(ctx, 2*time.Millisecond, 5*time.Second, func(text string) { alerts <- text })
	deadline := time.After(5 * time.Second)
	for {
		select {
		case text := <-alerts:
			if strings.Contains(text, "retraso") {
				if !strings.Contains(text, "toca añadir una instancia") || !strings.Contains(text, "2 de 2 llamadas") {
					t.Fatalf("a delay at the ceiling asks for an instance: %q", text)
				}
				return
			}
		case <-deadline:
			t.Fatal("no lagging alert at the ceiling")
		}
	}
}

// Calls failing with slots to spare (the platform does not answer, or the network fails): told after
// two minutes as that, never as capacity.
func TestFailingCallsAreToldAsThePlatformOrTheNetwork(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 26, 16, 5, 0, 0, time.UTC).UnixNano())
	f := &flaky{failures: 1 << 30}
	q, err := New(t.TempDir(), f, Options{ProbeAfter: 1 << 30, MaxBackoff: 10 * time.Millisecond, Now: func() time.Time { return time.Unix(0, clock.Load()) }, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	alerts := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Watch(ctx, 2*time.Millisecond, 5*time.Second, func(text string) { alerts <- text })
	q.Settle("res_f", "req_f", 1, model.Usage{}, model.SettlementSucceeded, "x")
	waitFor(t, "a failed call", func() bool { return q.Health(5*time.Second).Failing > 0 })
	clock.Add(int64(6 * time.Second))
	select {
	case text := <-alerts:
		t.Fatalf("failing calls with slots to spare are not told before two minutes: %q", text)
	case <-time.After(50 * time.Millisecond):
	}
	clock.Add(int64(2 * time.Minute))
	select {
	case text := <-alerts:
		if !strings.Contains(text, "llamadas seguidas sin respuesta") || !strings.Contains(text, "no es capacidad") || strings.Contains(text, "añadir una instancia") {
			t.Fatalf("failing calls alert %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failing calls for two minutes must be told")
	}
}

func TestARefusalRaisesAnAlert(t *testing.T) {
	l := &flaky{refuse: map[string]error{"res_r": fmt.Errorf("%w: %w", ledger.ErrReservationConflict,
		controlplane.NewHTTPError(409, []byte(`{"error":{"code":"settlement_outcome_mismatch"}}`)))}}
	q, err := New(t.TempDir(), l, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	alerts := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Watch(ctx, 5*time.Millisecond, time.Minute, func(text string) { alerts <- text })
	q.Settle("res_r", "req_r", 7, model.Usage{}, model.SettlementSucceeded, "x")
	select {
	case text := <-alerts:
		if !strings.Contains(text, "req_r settle (rechazada por la plataforma: settlement_outcome_mismatch)") || !strings.Contains(text, "1 operación de cobro no se puede entregar") || !strings.Contains(text, "dead/") {
			t.Fatalf("dead letter alert %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a refusal must raise an alert")
	}
}

func TestNearTheCeilingRaisesTheBusyAlertBeforeAnythingIsLate(t *testing.T) {
	g := newGate()
	q, err := New(t.TempDir(), g, Options{Concurrency: 4, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	t.Cleanup(g.open)
	alerts := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// every 2 ms, a 1-minute alert age: only the busy alert can fire
	go q.Watch(ctx, 2*time.Millisecond, time.Minute, func(text string) { alerts <- text })
	for i := 0; i < 8; i++ {
		q.Settle(fmt.Sprintf("res_%d", i), fmt.Sprintf("req_%d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	waitFor(t, "four calls in flight", func() bool { return g.inFlight.Load() == 4 })
	select {
	case text := <-alerts:
		if !strings.Contains(text, "de su capacidad") || !strings.Contains(text, "4 de 4 llamadas") || !strings.Contains(text, "todavía sin retraso") {
			t.Fatalf("busy alert %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a queue at its bound for the whole window must say so")
	}
	g.open()
	select {
	case text := <-alerts:
		if !strings.Contains(text, "vuelve a tener margen") {
			t.Fatalf("recovery %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the busy alert must clear once the queue has room")
	}
	if h := q.Health(time.Minute); h.Status != "ok" {
		t.Fatalf("health %+v", h)
	}
}

func TestTheBusyAlertSaysSoWhenTheBacklogIsAlreadyLate(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC).UnixNano())
	g := newGate()
	q, err := New(t.TempDir(), g, Options{Concurrency: 2, Now: func() time.Time { return time.Unix(0, clock.Load()) }, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	t.Cleanup(g.open)
	for i := 0; i < 4; i++ {
		q.Settle(fmt.Sprintf("res_%d", i), fmt.Sprintf("req_%d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	waitFor(t, "two calls in flight", func() bool { return g.inFlight.Load() == 2 })
	clock.Add(int64(30 * time.Second))
	alerts := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Watch(ctx, 2*time.Millisecond, 5*time.Second, func(text string) { alerts <- text })
	deadline := time.After(5 * time.Second)
	for {
		select {
		case text := <-alerts:
			if strings.Contains(text, "capacidad") {
				if !strings.Contains(text, "ya con retraso") || strings.Contains(text, "sin retraso") {
					t.Fatalf("a busy alert during a delay must not say there is none: %q", text)
				}
				return
			}
		case <-deadline:
			t.Fatal("no busy alert")
		}
	}
}
