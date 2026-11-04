package settle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/ledger"
	"lebrel-router/internal/model"
)

// onDiskCheck is a control plane that, on every call, reads the journal as a restart would and
// notes whether the operation it is asked about was already there.
type onDiskCheck struct {
	dir     string
	mu      sync.Mutex
	missing []string
	calls   int
}

func (c *onDiskCheck) check(id string) {
	owed, _, _ := readJournal(filepath.Join(c.dir, journalName))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	for _, op := range owed {
		if op.ID == id {
			return
		}
	}
	c.missing = append(c.missing, id)
}

func (c *onDiskCheck) MarkAccepted(_ context.Context, reservation string) error {
	c.check(opID(reservation, KindAccepted))
	return nil
}

func (c *onDiskCheck) Settle(_ context.Context, reservation string, actual int64, _ model.Usage, outcome model.SettlementOutcome, _ string) (model.Settlement, error) {
	c.check(opID(reservation, KindSettle))
	return model.Settlement{ReservationID: reservation, Actual: actual, Outcome: outcome}, nil
}

func TestAnOperationIsOnDiskBeforeItIsAttempted(t *testing.T) {
	dir := t.TempDir()
	c := &onDiskCheck{dir: dir}
	q, err := New(dir, c, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	for i := 0; i < 40; i++ {
		reservation := fmt.Sprintf("res_%02d", i)
		q.Accepted(reservation, "req_"+reservation)
		q.Settle(reservation, "req_"+reservation, 9, model.Usage{}, model.SettlementSucceeded, "x")
	}
	flush(t, q)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls != 80 || len(c.missing) != 0 {
		t.Fatalf("every operation must be in the journal before its call: %d calls, not on disk: %v", c.calls, c.missing)
	}
}

func line(t *testing.T, record journalRecord) string {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func deadFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(dir, "dead"))
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestATornLastLineIsDroppedAndAnythingElseUnreadableGoesToDeadWithAnAlert(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	content := line(t, journalRecord{Op: &Op{ReservationID: "res_a", RequestID: "req_a", Kind: KindSettle, Actual: 5, Outcome: model.SettlementSucceeded, CreatedAt: at}}) +
		"{this is not json\n" +
		line(t, journalRecord{Op: &Op{ReservationID: "res_b", RequestID: "req_b", Kind: KindAccepted, CreatedAt: at}}) +
		line(t, journalRecord{Op: &Op{ReservationID: "res_c", RequestID: "req_c", Kind: KindSettle, Actual: 5, Outcome: "usage-missing", CreatedAt: at}}) +
		line(t, journalRecord{Op: &Op{ReservationID: "res_d", RequestID: "req_d", Kind: KindSettle, Actual: 7, Outcome: model.SettlementSucceeded, CreatedAt: at}}) +
		line(t, journalRecord{Done: opID("res_d", KindSettle)}) +
		`{"op":{"reservation_id":"res_torn","kind":"sett` // the write a crash cut short
	if err := os.WriteFile(filepath.Join(dir, journalName), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	l := &flaky{}
	q, err := New(dir, l, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.Stats().Replayed != 2 {
		t.Fatalf("res_a and res_b are owed (res_d was done, res_torn never finished): %+v", q.Stats())
	}
	letters := q.drainDead()
	if len(letters) != 2 || len(deadFiles(t, dir)) != 2 {
		t.Fatalf("the unreadable line and the impossible settlement go to dead/ with an alert: %+v, files %v", letters, deadFiles(t, dir))
	}
	codes := map[string]bool{}
	for _, letter := range letters {
		codes[letter.Code] = true
	}
	if !codes["unreadable_record"] || !codes["invalid_operation"] {
		t.Fatalf("codes %v", codes)
	}
	run(t, q)
	flush(t, q)
	calls := l.seen()
	sort.Strings(calls)
	if strings.Join(calls, ",") != "accepted:res_b,settle:res_a" {
		t.Fatalf("only what is owed is delivered: %v", calls)
	}
}

func TestTheOneFilePerOperationLayoutIsTakenOver(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	good, _ := json.Marshal(Op{ID: "res_old-settle", ReservationID: "res_old", RequestID: "req_old", Kind: KindSettle, Actual: 3, Outcome: model.SettlementSucceeded, CreatedAt: time.Now()})
	write("res_old-settle.json", good)
	write("res_empty-settle.json", nil) // a power cut before the data reached the disk
	bad, _ := json.Marshal(Op{ReservationID: "res_neg", RequestID: "req_neg", Kind: KindSettle, Actual: -4, Outcome: model.SettlementSucceeded})
	write("res_neg-settle.json", bad)
	l := &flaky{}
	q, err := New(dir, l, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if q.Stats().Replayed != 1 || len(q.drainDead()) != 2 {
		t.Fatalf("one owed, two for a human: %+v", q.Stats())
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(files) != 0 {
		t.Fatalf("the old files are gone once the journal holds them: %v", files)
	}
	q.Close()
	owed, _, err := readJournal(filepath.Join(dir, journalName))
	if err != nil || len(owed) != 1 || owed[0].ReservationID != "res_old" {
		t.Fatalf("the journal owes the old file's settlement: %v %v", owed, err)
	}
}

// stubborn delivers everything but one reservation, which it never answers about.
type stubborn struct{ flaky }

func (s *stubborn) Settle(ctx context.Context, reservation string, actual int64, usage model.Usage, outcome model.SettlementOutcome, provider string) (model.Settlement, error) {
	if reservation == "res_hold" {
		return model.Settlement{}, fmt.Errorf("%w: %w", ledger.ErrUnavailable, controlplane.NewHTTPError(503, nil))
	}
	return s.flaky.Settle(ctx, reservation, actual, usage, outcome, provider)
}

func TestCompactionKeepsExactlyWhatIsOwed(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir, &stubborn{}, Options{CompactAbove: 4 << 10, MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	run(t, q)
	q.Settle("res_hold", "req_hold", 1, model.Usage{}, model.SettlementSucceeded, "x")
	for i := 0; i < 300; i++ {
		q.Settle(fmt.Sprintf("res_%03d", i), fmt.Sprintf("req_%03d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	waitFor(t, "300 delivered", func() bool { return q.Stats().Delivered == 300 })
	q.Close()
	info, err := os.Stat(filepath.Join(dir, journalName))
	if err != nil || info.Size() > 16<<10 {
		t.Fatalf("600 lines were written; compaction keeps the file small: %v bytes, %v", info.Size(), err)
	}
	owed, bad, err := readJournal(filepath.Join(dir, journalName))
	if err != nil || len(bad) != 0 || len(owed) != 1 || owed[0].ReservationID != "res_hold" {
		t.Fatalf("after compaction the journal owes exactly res_hold: %d owed, %d bad, %v", len(owed), len(bad), err)
	}
}

func TestOwedOperationsSurviveAProcessThatDies(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir, &down{}, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		q.Settle(fmt.Sprintf("res_%02d", i), fmt.Sprintf("req_%02d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	// the process dies: no Close, nothing flushed after this point; only the lock goes with it
	q.lock.Close()
	l := &flaky{}
	again, err := New(dir, l, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Stats().Replayed != 20 {
		t.Fatalf("every operation whose Settle returned is on disk: %d replayed", again.Stats().Replayed)
	}
	run(t, again)
	flush(t, again)
	if len(l.seen()) != 20 {
		t.Fatalf("all delivered after the restart: %v", l.seen())
	}
}

func TestADeadLetterThatCannotBeWrittenStaysOwed(t *testing.T) {
	dir := t.TempDir()
	refused := fmt.Errorf("%w: %w", ledger.ErrReservationConflict, controlplane.NewHTTPError(409, []byte(`{"error":{"code":"settlement_outcome_mismatch"}}`)))
	q, err := New(dir, &flaky{refuse: map[string]error{"res_r": refused}}, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "dead"), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "dead"), 0o750)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(done)
	}()
	q.Settle("res_r", "req_r", 7, model.Usage{}, model.SettlementSucceeded, "x")
	waitFor(t, "the refusal", func() bool { return q.Stats().Dead == 1 })
	letters := q.drainDead()
	if len(letters) != 1 || !strings.Contains(letters[0].Error, "not written to dead/, still owed in the journal") {
		t.Fatalf("the alert must say the dead letter could not be written: %+v", letters)
	}
	cancel()
	<-done
	q.Close()
	owed, _, err := readJournal(filepath.Join(dir, journalName))
	if err != nil || len(owed) != 1 || owed[0].ReservationID != "res_r" {
		t.Fatalf("a settlement whose dead letter failed is still owed: %v %v", owed, err)
	}
	// dead/ writable again: the next start refuses it again and keeps it this time
	os.Chmod(filepath.Join(dir, "dead"), 0o750)
	again, err := New(dir, &flaky{refuse: map[string]error{"res_r": refused}}, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	run(t, again)
	waitFor(t, "the dead letter on disk", func() bool { return again.Health(time.Minute).DeadOnDisk == 1 })
	if files := deadFiles(t, dir); len(files) != 1 {
		t.Fatalf("dead letters %v", files)
	}
}

func TestAnOperationThatCanNeverBeSentGoesStraightToDead(t *testing.T) {
	dir := t.TempDir()
	l := &flaky{}
	q, err := New(dir, l, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	q.Settle("res_x", "req_x", 5, model.Usage{}, "usage-missing", "x")
	q.Settle("res_y", "req_y", -1, model.Usage{}, model.SettlementSucceeded, "x")
	waitFor(t, "two dead letters", func() bool { return q.Stats().Dead == 2 })
	if calls := l.seen(); len(calls) != 0 || q.Pending() != 0 || len(deadFiles(t, dir)) != 2 {
		t.Fatalf("never sent, never retried, kept for a human: calls %v, pending %d, files %v", calls, q.Pending(), deadFiles(t, dir))
	}
}

// envelope answers the first batch call as a whole with a 409, then delivers every item.
type envelope struct {
	flaky
	mu    sync.Mutex
	calls int
}

func (e *envelope) DeliverBatch(_ context.Context, items []ledger.BatchItem) ([]error, error) {
	e.mu.Lock()
	e.calls++
	first := e.calls == 1
	e.mu.Unlock()
	errs := make([]error, len(items))
	if first {
		whole := fmt.Errorf("%w: %w", ledger.ErrBatchEnvelope, fmt.Errorf("%w: %w", ledger.ErrReservationConflict, controlplane.NewHTTPError(409, nil)))
		for i := range errs {
			errs[i] = whole
		}
	}
	return errs, nil
}

func TestAnAnswerToTheBatchAsAWholeRefusesNoItem(t *testing.T) {
	e := &envelope{}
	q, err := New(t.TempDir(), e, Options{BatchSize: 50, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for i := 0; i < 50; i++ {
		q.Settle(fmt.Sprintf("res_%02d", i), fmt.Sprintf("req_%02d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	run(t, q)
	flush(t, q)
	if s := q.Stats(); s.Dead != 0 || s.Delivered != 50 || s.Retried != 50 {
		t.Fatalf("a 409 to the whole call is retried for every item, never buried: %+v", s)
	}
}

// A backlog bigger than the compaction threshold (an outage) must not be rewritten after every
// group: only once the journal has doubled since its last rewrite.
func TestALargeBacklogIsNotRewrittenAfterEveryGroup(t *testing.T) {
	dir := t.TempDir()
	q, err := New(dir, &down{}, Options{CompactAbove: 2 << 10, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	// about 25 KB owed: ten times the threshold
	for i := 0; i < 100; i++ {
		q.Settle(fmt.Sprintf("res_%03d", i), fmt.Sprintf("req_%03d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	compactions := q.journal.compactions.Load()
	// 100 groups of one line each, the file growing from 2 KB to 25 KB: doubling allows at most
	// a handful of rewrites, one per group would be about 90
	if compactions > 6 {
		t.Fatalf("%d rewrites for 100 appends", compactions)
	}
	owed, _, err := readJournal(filepath.Join(dir, journalName))
	if err != nil || len(owed) != 100 {
		t.Fatalf("all 100 owed: %d, %v", len(owed), err)
	}
}

// A full disk: the operation stays deliverable in memory, the first failure raises an alert, the
// health says lagging with the count only in memory; once the journal takes lines again the
// operation is written again and the episode ends with a second alert.
func TestAJournalThatCannotWriteRaisesAnAlertAndCatchesUp(t *testing.T) {
	dir := t.TempDir()
	d := &down{}
	q, err := New(dir, d, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	alerts := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Watch(ctx, 5*time.Millisecond, time.Minute, func(text string) { alerts <- text })
	full := error(syscall.ENOSPC)
	q.journal.fault.Store(&full)
	q.Settle("res_a", "req_a", 7, model.Usage{}, model.SettlementSucceeded, "x")
	q.Settle("res_b", "req_b", 8, model.Usage{}, model.SettlementSucceeded, "x")
	var disk, lagging int
	deadline := time.After(5 * time.Second)
	for disk == 0 || lagging == 0 {
		select {
		case text := <-alerts:
			switch {
			case strings.Contains(text, "el disco no admite el diario") && strings.Contains(text, "no space left"):
				disk++
			case strings.Contains(text, "con retraso") && strings.Contains(text, "2 solo en memoria"):
				lagging++
			default:
				t.Fatalf("unexpected alert %q", text)
			}
		case <-deadline:
			t.Fatalf("a journal that cannot write raises its alert and the lagging one: disk %d, lagging %d", disk, lagging)
		}
	}
	if h := q.Health(time.Minute); h.MemoryOnly != 2 || h.Status != "lagging" {
		t.Fatalf("two only in memory count as lagging: %+v", h)
	}
	time.Sleep(50 * time.Millisecond)
	for len(alerts) > 0 {
		if text := <-alerts; strings.Contains(text, "el disco no admite") {
			t.Fatalf("one disk alert per episode, not per operation: %q", text)
		}
	}
	q.journal.fault.Store(nil)
	deadline = time.After(5 * time.Second)
	for back := false; !back; {
		select {
		case text := <-alerts:
			back = strings.Contains(text, "vuelve a escribirse")
		case <-deadline:
			t.Fatal("the episode must end once the journal writes again")
		}
	}
	if h := q.Health(time.Minute); h.MemoryOnly != 0 {
		t.Fatalf("nothing only in memory once written again: %+v", h)
	}
	// both are on disk now: a crash here loses nothing
	q.lock.Close()
	again, err := New(dir, &flaky{}, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Stats().Replayed != 2 {
		t.Fatalf("both written again before the crash: %d replayed", again.Stats().Replayed)
	}
}

// Once the disk takes lines again, what only memory holds is written again by Watch alone: the
// settlements arriving meanwhile pay their own append, never the catch-up, and each line is
// written once however many of them there are.
func TestTheDiskCatchUpRunsInWatchAloneAndWritesEachLineOnce(t *testing.T) {
	const backlog = 300
	dir := t.TempDir()
	q, err := New(dir, &down{}, Options{Logger: quiet}) // lebrel.ai is down: nothing leaves memory by delivery
	if err != nil {
		t.Fatal(err)
	}
	run(t, q)
	full := error(syscall.ENOSPC)
	q.journal.fault.Store(&full)
	for i := 0; i < backlog; i++ {
		q.Settle(fmt.Sprintf("res_mem_%d", i), fmt.Sprintf("req_mem_%d", i), 1, model.Usage{}, model.SettlementSucceeded, "x")
	}
	if h := q.Health(time.Minute); h.MemoryOnly != backlog {
		t.Fatalf("memory only %d", h.MemoryOnly)
	}
	q.journal.fault.Store(nil)
	var callers sync.WaitGroup
	for k := 0; k < 4; k++ {
		callers.Add(1)
		go func(k int) {
			defer callers.Done()
			q.Settle(fmt.Sprintf("res_caller_%d", k), fmt.Sprintf("req_caller_%d", k), 1, model.Usage{}, model.SettlementSucceeded, "x")
		}(k)
	}
	callers.Wait()
	if h := q.Health(time.Minute); h.MemoryOnly != backlog {
		t.Fatalf("no settlement writes the catch-up itself: %d left in memory after them", h.MemoryOnly)
	}
	alerts := make(chan string, 64)
	ctx, cancel := context.WithCancel(context.Background())
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		q.Watch(ctx, time.Millisecond, time.Minute, func(text string) {
			select {
			case alerts <- text:
			default:
			}
		})
	}()
	waitFor(t, "the catch-up", func() bool { return q.Health(time.Minute).MemoryOnly == 0 })
	cancel()
	<-watching
	q.Close()
	raw, err := os.ReadFile(filepath.Join(dir, journalName))
	if err != nil {
		t.Fatal(err)
	}
	if written := strings.Count(string(raw), `"reservation_id":"res_mem_`); written != backlog {
		t.Fatalf("each line written once: %d lines for %d operations", written, backlog)
	}
	back := 0
	for len(alerts) > 0 {
		if strings.Contains(<-alerts, "vuelve a escribirse") {
			back++
		}
	}
	if back != 1 {
		t.Fatalf("the episode ends once, with its notice: %d", back)
	}
}

// An operation held only in memory that lebrel.ai refuses while dead/ cannot take its letter is not
// dropped: memory keeps it until the disk takes it, the alert says it is not in dead/, and the next
// start meets the refusal again and keeps it there.
func TestARefusedOperationHeldOnlyInMemoryIsKeptUntilTheDiskTakesIt(t *testing.T) {
	dir := t.TempDir()
	refusal := fmt.Errorf("%w: %w", ledger.ErrReservationConflict, controlplane.NewHTTPError(409, []byte(`{"error":{"code":"settlement_outcome_mismatch"}}`)))
	refuses := &flaky{refuse: map[string]error{"res_x": refusal}}
	q, err := New(dir, refuses, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	deadDir := filepath.Join(dir, "dead")
	if err := os.Chmod(deadDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(deadDir, 0o750) })
	full := error(syscall.ENOSPC)
	q.journal.fault.Store(&full)
	run(t, q)
	q.Settle("res_x", "req_x", 5, model.Usage{}, model.SettlementSucceeded, "x")
	waitFor(t, "the refusal", func() bool { return q.Stats().Dead == 1 })
	if h := q.Health(time.Minute); h.MemoryOnly != 1 || h.Status != "lagging" {
		t.Fatalf("still held, in memory, and lagging: %+v", h)
	}
	dead := q.drainDead()
	if len(dead) != 1 || dead[0].Saved {
		t.Fatalf("a letter that is not in dead/: %+v", dead)
	}
	if text := deadText(dead, 0); !strings.Contains(text, "no caben en dead/") {
		t.Fatalf("the alert says it is not in dead/: %q", text)
	}
	q.journal.fault.Store(nil)
	q.retryDisk() // what Watch does at its next check
	if h := q.Health(time.Minute); h.MemoryOnly != 0 {
		t.Fatalf("written once the disk takes it: %+v", h)
	}
	// a crash now: the next start finds it owed, meets the refusal again and keeps it in dead/
	q.lock.Close()
	if err := os.Chmod(deadDir, 0o750); err != nil {
		t.Fatal(err)
	}
	again, err := New(dir, refuses, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Stats().Replayed != 1 {
		t.Fatalf("owed at the next start: %d replayed", again.Stats().Replayed)
	}
	run(t, again)
	waitFor(t, "the dead letter", func() bool { return again.Stats().Dead == 1 })
	if _, err := os.Stat(filepath.Join(deadDir, "res_x-settle.json")); err != nil {
		t.Fatalf("kept in dead/ at last: %v", err)
	}
}

// A stopping Watch makes one last pass: a refusal from the router's last seconds is still told.
func TestWatchTellsTheLastRefusalsAsItStops(t *testing.T) {
	refusal := fmt.Errorf("%w: %w", ledger.ErrReservationConflict, controlplane.NewHTTPError(409, []byte(`{"error":{"code":"settlement_outcome_mismatch"}}`)))
	q, err := New(t.TempDir(), &flaky{refuse: map[string]error{"res_last": refusal}}, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	alerts := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		q.Watch(ctx, time.Hour, time.Minute, func(text string) { alerts <- text })
	}()
	q.Settle("res_last", "req_last", 5, model.Usage{}, model.SettlementSucceeded, "x")
	waitFor(t, "the refusal", func() bool { return q.Stats().Dead == 1 })
	cancel()
	<-watching
	select {
	case text := <-alerts:
		if !strings.Contains(text, "req_last") {
			t.Fatalf("the last refusal: %q", text)
		}
	default:
		t.Fatal("the last refusal was never told")
	}
}

// A crash after both operations of a reservation were delivered but before their done lines were
// on disk: the replayed acceptance meets 409 with the settlement the platform holds; it is the one
// still owed right behind it, so both are done, and nobody is called.
func TestAReplayedAcceptanceOfOurOwnSettlementIsDone(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().UTC()
	usage := model.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10}
	content := line(t, journalRecord{Op: &Op{ReservationID: "res_x", RequestID: "req_x", Kind: KindAccepted, CreatedAt: at}}) +
		line(t, journalRecord{Op: &Op{ReservationID: "res_x", RequestID: "req_x", Kind: KindSettle, Actual: 750, Usage: usage, Outcome: model.SettlementSucceeded, CreatedAt: at}}) +
		line(t, journalRecord{Op: &Op{ReservationID: "res_y", RequestID: "req_y", Kind: KindAccepted, CreatedAt: at}}) +
		line(t, journalRecord{Op: &Op{ReservationID: "res_y", RequestID: "req_y", Kind: KindSettle, Actual: 750, Usage: usage, Outcome: model.SettlementSucceeded, CreatedAt: at}})
	if err := os.WriteFile(filepath.Join(dir, journalName), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	remote := platform(t, map[string]func(w http.ResponseWriter){
		// ours, settled exactly as we owe it
		"res_x": refusal(409, `{"error":{"code":"reservation_not_open","message":"Reservation is not open","settled":{"outcome":"succeeded","actualMicrousd":750}}}`),
		// closed at zero by someone else while we owe 750: a human must look
		"res_y": refusal(409, `{"error":{"code":"reservation_not_open","message":"Reservation is not open","settled":{"outcome":"upstream_rejected","actualMicrousd":0}}}`),
	})
	q, err := New(dir, remote, Options{MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	run(t, q)
	waitFor(t, "every answer", func() bool { s := q.Stats(); return s.AlreadySettled+s.Dead >= 4 })
	letters := q.drainDead()
	dead := map[string]int{}
	for _, letter := range letters {
		dead[letter.RequestID]++
	}
	if dead["req_x"] != 0 || dead["req_y"] != 2 {
		t.Fatalf("res_x is exactly as we owe it, res_y is not: %+v", letters)
	}
}

// An acceptance whose line the catch-up wrote after its done line is replayed alone at the next
// start, its settlement long delivered: the platform holds the reservation settled, and an
// acceptance adds nothing to that. Done, never a dead letter.
func TestALoneReplayedAcceptanceOfASettledReservationIsDone(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().UTC()
	usage := model.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10}
	content := line(t, journalRecord{Op: &Op{ReservationID: "res_x", RequestID: "req_x", Kind: KindSettle, Actual: 750, Usage: usage, Outcome: model.SettlementSucceeded, CreatedAt: at}}) +
		line(t, journalRecord{Done: opID("res_x", KindAccepted)}) +
		line(t, journalRecord{Op: &Op{ReservationID: "res_x", RequestID: "req_x", Kind: KindAccepted, CreatedAt: at}}) +
		line(t, journalRecord{Done: opID("res_x", KindSettle)})
	if err := os.WriteFile(filepath.Join(dir, journalName), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	remote := platform(t, map[string]func(w http.ResponseWriter){
		"res_x": refusal(409, `{"error":{"code":"reservation_not_open","message":"Reservation is not open","settled":{"outcome":"succeeded","actualMicrousd":750,"usageExact":true}}}`),
	})
	q, err := New(dir, remote, Options{MaxBackoff: 10 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.Stats().Replayed != 1 {
		t.Fatalf("the acceptance alone is owed: %d replayed", q.Stats().Replayed)
	}
	run(t, q)
	waitFor(t, "its answer", func() bool { s := q.Stats(); return s.AlreadySettled+s.Dead >= 1 })
	if s := q.Stats(); s.AlreadySettled != 1 || s.Dead != 0 {
		t.Fatalf("done, not a dead letter: %+v, %+v", s, q.drainDead())
	}
}

// A corrupt record whose dead letter cannot be written is not dropped: it stays in the journal
// (through compaction too) and its alert says so; the next start with dead/ writable keeps it there.
func TestAnUnreadableRecordWithoutADeadLetterStaysInTheJournal(t *testing.T) {
	dir := t.TempDir()
	good := line(t, journalRecord{Op: &Op{ReservationID: "res_a", RequestID: "req_a", Kind: KindAccepted, CreatedAt: time.Now()}})
	if err := os.WriteFile(filepath.Join(dir, journalName), []byte("{broken record\n"+good), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dead"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "dead"), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "dead"), 0o750)
	q, err := New(dir, &flaky{}, Options{CompactAbove: 1, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	letters := q.drainDead()
	if len(letters) != 1 || !strings.Contains(letters[0].Error, "kept in its journal line for the next start") {
		t.Fatalf("the alert must say where the record stays: %+v", letters)
	}
	run(t, q)
	flush(t, q) // res_a delivered; with CompactAbove 1 the journal is rewritten after every group
	q.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, journalName))
	if !strings.Contains(string(raw), "{broken record") {
		t.Fatalf("the record must survive compaction: %q", raw)
	}
	os.Chmod(filepath.Join(dir, "dead"), 0o750)
	again, err := New(dir, &flaky{}, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if len(again.drainDead()) != 1 || len(deadFiles(t, dir)) != 1 {
		t.Fatalf("with dead/ writable the record is kept there: %v", deadFiles(t, dir))
	}
	raw, _ = os.ReadFile(filepath.Join(dir, journalName))
	if strings.Contains(string(raw), "{broken record") {
		t.Fatalf("once in dead/, it leaves the journal: %q", raw)
	}
}
