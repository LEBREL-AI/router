// Package settle delivers acceptances and settlements to the control plane without ever losing
// one. A request's money is decided the moment the upstream answers; telling the control plane is
// bookkeeping that must not depend on that instant's latency. Every operation is on disk (fsynced,
// in the journal) before it is attempted, attempted at once, retried with jittered backoff for as
// long as the control plane is unreachable, and replayed after a restart. It leaves the queue once
// the control plane has it (delivered, or already settled the same way) or once it can never be
// delivered (the control plane refuses it for good, or it could never be sent): then it is kept in
// dead/ and an alert is raised. When the journal cannot take an operation, the operation is kept
// in memory, counted, raised as an alert, and written again as soon as the journal takes lines.
//
// Operations of one reservation go in order: acceptance before settlement (the router adds them in
// that order from one goroutine). Different reservations go out concurrently, up to Concurrency
// calls in flight, in batches of up to BatchSize when the control plane offers its batch endpoint.
// When the control plane keeps failing, the queue probes it with one call at a time: an operation
// never attempted goes at once (it is as good a probe as any), a retry waits for the probe spacing,
// a quarter of a second at first, longer after each failed probe, never above five seconds.
package settle

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/ledger"
	"lebrel-router/internal/model"
)

const (
	KindAccepted = "accepted"
	KindSettle   = "settle"

	defaultAttemptTimeout = 10 * time.Second
	defaultMaxBackoff     = time.Minute
	defaultConcurrency    = 64
	defaultProbeAfter     = 8
	baseBackoff           = 500 * time.Millisecond
	// probes are never further apart than this: after an outage, settlements go out within
	// seconds of the control plane's first answer
	maxProbeSpacing = 5 * time.Second
	// a batch endpoint the control plane did not have is asked for again after this
	batchRetryAfter = 5 * time.Minute
)

// Ledger is the part of the control plane the queue talks to. A ledger that also implements
// ledger.Batcher is used in batches when Options.BatchSize is above 1.
type Ledger interface {
	MarkAccepted(context.Context, string) error
	Settle(context.Context, string, int64, model.Usage, model.SettlementOutcome, string) (model.Settlement, error)
}

// Op is one delivery owed to the control plane.
type Op struct {
	ID            string                  `json:"id"`
	ReservationID string                  `json:"reservation_id"`
	RequestID     string                  `json:"request_id"`
	Kind          string                  `json:"kind"`
	Actual        int64                   `json:"actual_microusd,omitempty"`
	Usage         model.Usage             `json:"usage,omitempty"`
	Outcome       model.SettlementOutcome `json:"outcome,omitempty"`
	Provider      string                  `json:"provider,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	Attempts      int                     `json:"attempts"`
	LastError     string                  `json:"last_error,omitempty"`

	notBefore time.Time
}

// Stats is what the queue has done since it started.
type Stats struct {
	Delivered      int64 `json:"delivered"`
	AlreadySettled int64 `json:"already_settled"`
	Retried        int64 `json:"retried"`
	Dead           int64 `json:"dead"`
	Batches        int64 `json:"batches"`
	Pending        int   `json:"pending"`
	Replayed       int   `json:"replayed"`
}

// DeadLetter is an operation that can never be delivered, kept in dead/ for a human.
type DeadLetter struct {
	RequestID string
	Kind      string
	Code      string
	Error     string
	// Saved is false when dead/ could not take it: it is then still owed on disk (or in memory
	// until the disk takes it) and comes back at the next start
	Saved bool
}

type Options struct {
	AttemptTimeout time.Duration
	MaxBackoff     time.Duration
	// Concurrency bounds the calls in flight, single or batch (default 64).
	Concurrency int
	// BatchSize is how many operations one call carries when the ledger can batch (default 1:
	// one call per operation, on the single endpoints).
	BatchSize int
	// ProbeAfter is how many failed calls in a row (a batch is one call) switch the queue to one
	// probe at a time (default 8).
	ProbeAfter int
	// CompactAbove is the journal size that triggers its rewrite (default 8 MiB).
	CompactAbove int64
	Now          func() time.Time
	Logger       *log.Logger
	Rand         *rand.Rand
}

// Queue owes the control plane the operations it holds until each is acknowledged.
type Queue struct {
	dir     string
	ledger  Ledger
	batcher ledger.Batcher
	options Options
	lock    *os.File
	journal *journal // set once in New, never changed: Close only closes it

	mu         sync.Mutex
	ops        map[string]*Op   // every operation owed, by id
	first      map[string]*Op   // per reservation, the operation that goes next (waiting, backing off or in flight)
	behind     map[string][]*Op // per reservation, the operations waiting for it (a settlement behind its acceptance)
	fresh      deque            // never attempted, deliverable now
	ready      deque            // retries whose backoff has passed
	delayed    delayHeap        // backing off, soonest first
	inFlight   int              // calls in flight
	failures   int              // failed calls in a row, across the queue
	probes     int              // failed probes since probing began
	probeAt    time.Time        // while probing, the earliest start of the next probe for a retry
	batchOff   time.Time        // the batch endpoint was missing: one call per operation until then
	memoryOnly map[string]*Op   // owed, but the journal could not take their line
	diskFails  bool             // in an episode of journal failures
	dead       []DeadLetter     // dead letters since the last check, for the alert
	notices    []string         // other alerts since the last check (the journal failing, then back)
	deadOnDisk int
	wake       chan struct{}
	stats      Stats
}

// New opens a queue over “dir“ (empty: memory only, for tests and local mode), takes the
// directory's lock (one router per directory) and replays what an earlier process left there.
func New(dir string, l Ledger, options Options) (*Queue, error) {
	if options.AttemptTimeout <= 0 {
		options.AttemptTimeout = defaultAttemptTimeout
	}
	if options.MaxBackoff <= 0 {
		options.MaxBackoff = defaultMaxBackoff
	}
	if options.Concurrency <= 0 {
		options.Concurrency = defaultConcurrency
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 1
	}
	if options.BatchSize > ledger.MaxBatchItems {
		options.BatchSize = ledger.MaxBatchItems
	}
	if options.ProbeAfter <= 0 {
		options.ProbeAfter = defaultProbeAfter
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = log.Default()
	}
	if options.Rand == nil {
		options.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	q := &Queue{dir: dir, ledger: l, options: options, ops: map[string]*Op{}, first: map[string]*Op{}, behind: map[string][]*Op{},
		memoryOnly: map[string]*Op{}, wake: make(chan struct{}, 1)}
	if batcher, ok := l.(ledger.Batcher); ok && options.BatchSize > 1 {
		q.batcher = batcher
	}
	if dir != "" {
		if err := os.MkdirAll(filepath.Join(dir, "dead"), 0o750); err != nil {
			return nil, fmt.Errorf("settlement directory: %w", err)
		}
		lock, err := lockDirectory(dir)
		if err != nil {
			return nil, err
		}
		q.lock = lock
		if err := q.replay(); err != nil {
			q.Close()
			return nil, err
		}
	}
	return q, nil
}

// lockDirectory makes sure no other router delivers from the same directory: two would replay
// and race on the same journal. Each instance needs its own SETTLEMENT_DIR.
func lockDirectory(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("settlement directory lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("settlement directory %s is in use by another router: each instance needs its own SETTLEMENT_DIR", dir)
	}
	return file, nil
}

// Close writes what the journal has queued and releases the directory; operations still owed stay
// on disk for the next start. A delivery still finishing after Close only finds the journal closed.
func (q *Queue) Close() error {
	var err error
	if q.journal != nil {
		err = q.journal.close()
	}
	q.mu.Lock()
	lock := q.lock
	q.lock = nil
	q.mu.Unlock()
	if lock != nil {
		if lockErr := lock.Close(); err == nil {
			err = lockErr
		}
	}
	return err
}

// replay takes back what the journal owes, plus any one-file-per-operation the previous router
// layout left (or a dead letter an operator moved back), writes a fresh journal with all of it,
// and keeps whatever cannot be taken back in dead/, with an alert. A record whose dead letter
// cannot be written stays where it was (its journal line, its file) for the next start.
func (q *Queue) replay() error {
	ops, bad, err := readJournal(filepath.Join(q.dir, journalName))
	if err != nil {
		return fmt.Errorf("settlement journal: %w", err)
	}
	legacy, files, legacyBad, err := legacyOps(q.dir)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(ops))
	for _, op := range ops {
		known[op.ID] = true
	}
	for _, op := range legacy {
		if !known[op.ID] {
			known[op.ID] = true
			ops = append(ops, op)
		}
	}
	var kept [][]byte
	keepFiles := map[string]bool{}
	for _, record := range append(bad, legacyBad...) {
		if q.keepUnreadable(record) {
			continue
		}
		if record.file != "" {
			keepFiles[record.file] = true
		} else {
			kept = append(kept, record.raw)
		}
	}
	// Reservations in the order they started; within one, acceptance before settlement whatever
	// the clock wrote.
	started := map[string]time.Time{}
	for _, op := range ops {
		if at, ok := started[op.ReservationID]; !ok || op.CreatedAt.Before(at) {
			started[op.ReservationID] = op.CreatedAt
		}
	}
	sort.SliceStable(ops, func(a, b int) bool {
		x, y := ops[a], ops[b]
		if sx, sy := started[x.ReservationID], started[y.ReservationID]; !sx.Equal(sy) {
			return sx.Before(sy)
		}
		if x.ReservationID != y.ReservationID {
			return x.ReservationID < y.ReservationID
		}
		return kindRank(x.Kind) < kindRank(y.Kind)
	})
	file, size, err := rewriteJournal(q.dir, ops, kept)
	if err != nil {
		return fmt.Errorf("settlement journal: %w", err)
	}
	// only now that the journal holds them durably can the old files go
	for _, name := range files {
		if !keepFiles[name] {
			_ = os.Remove(filepath.Join(q.dir, name))
		}
	}
	_ = os.Remove(filepath.Join(q.dir, journalName+".tmp"))
	if dead, err := os.ReadDir(filepath.Join(q.dir, "dead")); err == nil {
		q.deadOnDisk = 0
		for _, entry := range dead {
			if strings.HasSuffix(entry.Name(), ".json") {
				q.deadOnDisk++
			}
		}
	}
	for _, op := range ops {
		q.ops[op.ID] = op
		q.place(op)
		q.stats.Replayed++
	}
	if q.stats.Replayed > 0 {
		q.options.Logger.Printf("settlements: %d pending from before this start", q.stats.Replayed)
	}
	q.journal = newJournal(q.dir, file, size, kept, q.options.CompactAbove, q.options.Logger)
	q.journal.start(q.owed)
	return nil
}

// keepUnreadable copies a record the queue cannot take back into dead/ and raises it; false when
// the copy could not be written (the record then stays where it was).
func (q *Queue) keepUnreadable(record corrupt) bool {
	letter := DeadLetter{Kind: "unreadable", Code: "unreadable_record", Error: record.reason}
	if record.op != nil {
		letter.RequestID, letter.Kind, letter.Code = record.op.RequestID, record.op.Kind, "invalid_operation"
	}
	if letter.RequestID == "" {
		letter.RequestID = record.name
	}
	written := true
	if err := writeDurably(filepath.Join(q.dir, "dead"), record.name+".json", record.raw); err != nil {
		written = false
		where := "its journal line"
		if record.file != "" {
			where = "its file " + record.file
		}
		letter.Error += "; not copied to dead/, kept in " + where + " for the next start: " + err.Error()
	}
	letter.Saved = written
	q.dead = append(q.dead, letter)
	q.stats.Dead++
	q.options.Logger.Printf("settlement record %s: %s (copied to dead/: %t)", record.name, record.reason, written)
	return written
}

// owed is a copy of every operation the queue holds, for the journal's compaction (copies: the
// originals keep changing while the journal writes them).
func (q *Queue) owed() []*Op {
	q.mu.Lock()
	defer q.mu.Unlock()
	ops := make([]*Op, 0, len(q.ops))
	for _, op := range q.ops {
		copied := *op
		ops = append(ops, &copied)
	}
	sort.Slice(ops, func(a, b int) bool {
		if !ops[a].CreatedAt.Equal(ops[b].CreatedAt) {
			return ops[a].CreatedAt.Before(ops[b].CreatedAt)
		}
		return kindRank(ops[a].Kind) < kindRank(ops[b].Kind)
	})
	return ops
}

func opID(reservationID, kind string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, reservationID)
	return safe + "-" + kind
}

func kindRank(kind string) int {
	if kind == KindAccepted {
		return 0
	}
	return 1
}

// Accepted owes the control plane the note that the upstream accepted this request.
func (q *Queue) Accepted(reservationID, requestID string) {
	q.add(&Op{ReservationID: reservationID, RequestID: requestID, Kind: KindAccepted})
}

// Settle owes the control plane the final charge of this reservation.
func (q *Queue) Settle(reservationID, requestID string, actual int64, usage model.Usage, outcome model.SettlementOutcome, provider string) {
	q.add(&Op{ReservationID: reservationID, RequestID: requestID, Kind: KindSettle, Actual: actual, Usage: usage, Outcome: outcome, Provider: provider})
}

func (q *Queue) add(op *Op) {
	op.ID = opID(op.ReservationID, op.Kind)
	op.CreatedAt = q.options.Now()
	if problem := checkOp(op); problem != "" {
		// it could never be sent: kept for a human at once, never retried
		q.bury([]buried{{op: op, err: fmt.Errorf("%w: %s", ledger.ErrInvalidOperation, problem), unjournaled: true}})
		return
	}
	q.mu.Lock()
	if _, exists := q.ops[op.ID]; exists {
		q.mu.Unlock()
		return // the same delivery twice is the same delivery
	}
	q.ops[op.ID] = op
	q.mu.Unlock()
	// on disk before anyone can attempt it
	var err error
	if q.journal != nil {
		err = q.journal.append(journalRecord{Op: op}, true)
	}
	q.mu.Lock()
	if err != nil {
		q.keepInMemory(op, err)
	} else if q.diskFails && len(q.memoryOnly) == 0 {
		// the journal takes lines again and nothing waits to be written: the episode is over
		q.endDiskEpisode()
	}
	q.place(op)
	q.mu.Unlock()
	q.signal()
}

// keepInMemory holds an operation the journal could not take, until catchUp writes it; the first
// of an episode raises the disk alert. Callers hold q.mu.
func (q *Queue) keepInMemory(op *Op, err error) {
	q.memoryOnly[op.ID] = op
	if !q.diskFails {
		q.diskFails = true
		q.notices = append(q.notices, fmt.Sprintf("el disco no admite el diario de liquidaciones (%v): lo nuevo queda solo en memoria y se perdería si el router se reinicia antes de entregarlo", err))
	}
	q.options.Logger.Printf("request=%s settlement %s: not on disk, kept in memory only (%d so): %v", op.RequestID, op.Kind, len(q.memoryOnly), err)
}

// endDiskEpisode closes an episode of journal failures. Callers hold q.mu.
func (q *Queue) endDiskEpisode() {
	if q.diskFails {
		q.diskFails = false
		q.notices = append(q.notices, "el diario de liquidaciones vuelve a escribirse: todo lo pendiente está otra vez en disco.")
	}
}

// catchUpChunk is how many operations held only in memory one fsync writes again.
const catchUpChunk = 64

// catchUp writes again the operations the journal could not take, once it takes lines. Watch
// alone runs it, so no request ever waits for it and no caller repeats it: it takes them out of
// memory one by one, up to catchUpChunk per fsync, each only once its own line is on disk, and ends
// the episode when none is left. A failed write leaves the rest for the next check.
func (q *Queue) catchUp() {
	if q.journal == nil {
		return
	}
	for {
		q.mu.Lock()
		chunk := make([]*Op, 0, min(catchUpChunk, len(q.memoryOnly)))
		for _, op := range q.memoryOnly {
			copied := *op
			chunk = append(chunk, &copied)
			if len(chunk) == catchUpChunk {
				break
			}
		}
		if len(chunk) == 0 {
			q.endDiskEpisode()
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
		records := make([]journalRecord, len(chunk))
		for i, op := range chunk {
			records[i] = journalRecord{Op: op}
		}
		if err := q.journal.appendAll(records); err != nil {
			return
		}
		// One delivered while its line was being written may find that line after its done line
		// at the next start and go again: the same delivery, as after any lost done line (an
		// acceptance going alone meets its settled reservation and is done, see judge).
		q.mu.Lock()
		for _, op := range chunk {
			delete(q.memoryOnly, op.ID)
		}
		q.mu.Unlock()
	}
}

// place puts an operation in line: first for its reservation, or behind the one that is. Callers
// hold q.mu.
func (q *Queue) place(op *Op) {
	if _, busy := q.first[op.ReservationID]; busy {
		q.behind[op.ReservationID] = append(q.behind[op.ReservationID], op)
		return
	}
	q.first[op.ReservationID] = op
	q.line(op).PushBack(op)
}

// line is where an operation waits once deliverable: never attempted, or a retry.
func (q *Queue) line(op *Op) *deque {
	if op.Attempts == 0 {
		return &q.fresh
	}
	return &q.ready
}

// next is the operation waiting behind op's reservation, if any. Callers hold q.mu.
func (q *Queue) next(op *Op) *Op {
	if waiting := q.behind[op.ReservationID]; len(waiting) > 0 {
		return waiting[0]
	}
	return nil
}

// finish takes an operation out of the queue for good and lets its reservation's next one go,
// ahead of the line: it has waited already. Callers hold q.mu.
func (q *Queue) finish(op *Op) {
	delete(q.ops, op.ID)
	delete(q.memoryOnly, op.ID)
	if q.first[op.ReservationID] != op {
		return
	}
	waiting := q.behind[op.ReservationID]
	if len(waiting) == 0 {
		delete(q.first, op.ReservationID)
		return
	}
	next := waiting[0]
	if len(waiting) == 1 {
		delete(q.behind, op.ReservationID)
	} else {
		q.behind[op.ReservationID] = waiting[1:]
	}
	q.first[op.ReservationID] = next
	q.line(next).PushFront(next)
}

// done tells the journal the queue no longer owes op; nobody waits for that line, since an
// operation replayed after it was delivered is delivered again as the same delivery.
func (q *Queue) done(op *Op) {
	if q.journal != nil {
		_ = q.journal.append(journalRecord{Done: op.ID}, false)
	}
}

type buried struct {
	op          *Op
	err         error
	unjournaled bool // not in the journal: refused before it reached it, or held only in memory
}

// bury keeps operations that can never be delivered in dead/ (durably) and raises them; only then
// does the journal stop owing them, so one whose dead letter cannot be written is still owed at the
// next start (one that never reached the journal, refused at once or held only in memory, is
// written to it for that, or kept in memory until catchUp can). Called without q.mu.
func (q *Queue) bury(ops []buried) {
	for _, b := range ops {
		b.op.LastError = b.err.Error()
		letter := DeadLetter{RequestID: b.op.RequestID, Kind: b.op.Kind, Code: ledger.ErrorCode(b.err), Error: b.err.Error(), Saved: true}
		if errors.Is(b.err, ledger.ErrInvalidOperation) && letter.Code == "" {
			letter.Code = "invalid_operation"
		}
		var unwritten error // set when neither dead/ nor the journal took it
		if q.dir != "" {
			raw, err := json.Marshal(b.op)
			if err == nil {
				err = writeDurably(filepath.Join(q.dir, "dead"), b.op.ID+".json", raw)
			}
			if err != nil {
				letter.Saved = false
				letter.Error += "; not written to dead/, still owed in the journal for the next start: " + err.Error()
				if b.unjournaled && q.journal != nil {
					if err := q.journal.append(journalRecord{Op: b.op}, true); err != nil {
						unwritten = err
						letter.Error += "; nor written to the journal, held in memory until the disk takes it: " + err.Error()
					}
				}
			}
		}
		if letter.Saved && !b.unjournaled {
			q.done(b.op)
		}
		q.mu.Lock()
		if letter.Saved {
			q.deadOnDisk++
		}
		if unwritten != nil {
			q.keepInMemory(b.op, unwritten)
		}
		q.stats.Dead++
		q.dead = append(q.dead, letter)
		q.mu.Unlock()
		q.options.Logger.Printf("request=%s settlement %s can never be delivered, kept in dead/: %v", b.op.RequestID, b.op.Kind, b.err)
	}
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Run delivers until ctx ends, then waits for the calls in flight. Safe to call once.
func (q *Queue) Run(ctx context.Context) {
	var calls sync.WaitGroup
	defer calls.Wait()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		wait := q.dispatch(ctx, &calls)
		if ctx.Err() != nil {
			return
		}
		var due <-chan time.Time
		if wait > 0 {
			timer.Reset(wait)
			due = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-due:
		}
	}
}

// dispatch starts every call it can and returns how long until something scheduled is due (zero:
// nothing scheduled, wait for a new operation or a call to end).
func (q *Queue) dispatch(ctx context.Context, calls *sync.WaitGroup) time.Duration {
	for ctx.Err() == nil {
		call, wait := q.take(q.options.Now())
		if call.ops == nil {
			return wait
		}
		calls.Add(1)
		go func() {
			defer calls.Done()
			q.deliver(ctx, call)
		}()
	}
	return 0
}

// call is what one delivery carries: its operations, whether it goes as a batch, and whether it is
// a probe of a control plane that has been failing.
type call struct {
	ops     []*Op
	batched bool
	probe   bool
}

// take hands out the next call's operations, oldest first: up to BatchSize distinct reservations
// within the in-flight bound, or, while probing, one call at a time (an operation never attempted
// at once, a retry once the probe spacing has passed).
func (q *Queue) take(now time.Time) (call, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.delayed.Len() > 0 && !q.delayed[0].notBefore.After(now) {
		q.ready.PushBack(heap.Pop(&q.delayed).(*Op))
	}
	batched := q.batcher != nil && !now.Before(q.batchOff)
	if q.failures >= q.options.ProbeAfter {
		switch {
		case q.inFlight > 0:
			return call{}, q.nextDue(now, true)
		case q.fresh.Len() > 0:
			q.inFlight++
			return call{ops: []*Op{q.fresh.PopFront()}, batched: batched, probe: true}, 0
		case q.ready.Len() > 0 && !now.Before(q.probeAt):
			q.inFlight++
			return call{ops: []*Op{q.ready.PopFront()}, batched: batched, probe: true}, 0
		default:
			return call{}, q.nextDue(now, true)
		}
	}
	if q.inFlight >= q.options.Concurrency || q.fresh.Len()+q.ready.Len() == 0 {
		return call{}, q.nextDue(now, false)
	}
	size := 1
	if batched {
		size = q.options.BatchSize
	}
	ops := make([]*Op, 0, min(size, q.fresh.Len()+q.ready.Len()))
	for len(ops) < size && q.fresh.Len()+q.ready.Len() > 0 {
		ops = append(ops, q.popOldest())
	}
	q.inFlight++
	return call{ops: ops, batched: batched}, 0
}

// popOldest takes the operation created first from the two lines' heads.
func (q *Queue) popOldest() *Op {
	fresh, retry := q.fresh.Front(), q.ready.Front()
	if retry == nil || (fresh != nil && !retry.CreatedAt.Before(fresh.CreatedAt)) {
		return q.fresh.PopFront()
	}
	return q.ready.PopFront()
}

func (q *Queue) nextDue(now time.Time, probing bool) time.Duration {
	var wait time.Duration
	consider := func(at time.Time) {
		if d := at.Sub(now); d > 0 && (wait == 0 || d < wait) {
			wait = d
		}
	}
	if q.delayed.Len() > 0 {
		consider(q.delayed[0].notBefore)
	}
	if probing && q.ready.Len() > 0 {
		consider(q.probeAt)
	}
	return wait
}

func (q *Queue) deliver(ctx context.Context, c call) {
	attempt, cancel := context.WithTimeout(ctx, q.options.AttemptTimeout)
	errs := make([]error, len(c.ops))
	if c.batched {
		items := make([]ledger.BatchItem, len(c.ops))
		for i, op := range c.ops {
			items[i] = ledger.BatchItem{Kind: op.Kind, ReservationID: op.ReservationID, Actual: op.Actual, Usage: op.Usage, Outcome: op.Outcome, Provider: op.Provider}
		}
		results, err := q.batcher.DeliverBatch(attempt, items)
		switch {
		case errors.Is(err, ledger.ErrBatchUnsupported):
			cancel()
			q.withoutBatches(c.ops, err)
			return
		case err != nil:
			envelope := fmt.Errorf("%w: %w", ledger.ErrBatchEnvelope, err)
			for i := range errs {
				errs[i] = envelope
			}
		default:
			copy(errs, results)
		}
	} else {
		op := c.ops[0]
		switch op.Kind {
		case KindAccepted:
			errs[0] = q.ledger.MarkAccepted(attempt, op.ReservationID)
		default:
			_, errs[0] = q.ledger.Settle(attempt, op.ReservationID, op.Actual, op.Usage, op.Outcome, op.Provider)
		}
	}
	cancel()
	q.bury(q.resolve(ctx, c, errs))
	q.signal()
}

// withoutBatches puts a batch back at the head of its lines, unattempted, and delivers one call per
// operation until the batch endpoint is asked for again.
func (q *Queue) withoutBatches(batch []*Op, reason error) {
	q.mu.Lock()
	q.inFlight--
	now := q.options.Now()
	first := !now.Before(q.batchOff) // the calls in flight together all come back the same way
	q.batchOff = now.Add(batchRetryAfter)
	for i := len(batch) - 1; i >= 0; i-- {
		q.line(batch[i]).PushFront(batch[i])
	}
	q.mu.Unlock()
	if first {
		q.options.Logger.Printf("settlements: no batch endpoint (%v); one call per operation for %s", reason, batchRetryAfter)
	}
	q.signal()
}

type verdict int

const (
	delivered verdict = iota
	alreadySettled
	retry
	refused
)

// judge reads the control plane's answer to one operation; next is the operation waiting behind
// it for the same reservation (the settlement behind an acceptance), if any.
func judge(op *Op, err error, next *Op) verdict {
	if err == nil {
		return delivered
	}
	if Transient(err) {
		return retry
	}
	if stored, ok := ledger.StoredSettlement(err); ok {
		// the reservation is closed and the platform says how: done if that is what we owe for it,
		// the settlement itself or the one behind this acceptance. An acceptance with nothing owed
		// behind it adds nothing to a settled reservation (one replayed alone, its settlement long
		// delivered): done too.
		if op.Kind == KindAccepted && next == nil {
			return alreadySettled
		}
		owed := op
		if op.Kind == KindAccepted {
			owed = next
		}
		if owed != nil && owed.Kind == KindSettle && stored.Outcome == owed.Outcome && stored.Actual == owed.Actual {
			return alreadySettled
		}
		return refused
	}
	// A settlement that charges nothing finds the reservation gone (the platform's own 404), or
	// already closed at zero (a reservation that never had an acceptance can only have been closed
	// at zero): what it would have recorded is what stands.
	if op.Kind == KindSettle && op.Outcome == model.SettlementUpstreamRejected && op.Actual == 0 &&
		((errors.Is(err, ledger.ErrReservationMissing) && ledger.ErrorCode(err) == "reservation_not_found") ||
			ledger.ErrorCode(err) == "settlement_idempotency_conflict") {
		return alreadySettled
	}
	return refused
}

// resolve applies one call's answers and returns the operations to keep in dead/. A call failed
// when none of its operations got an answer about itself; the queue counts those in a row and
// probes after ProbeAfter of them: the first probe of a retry a quarter to half a second later,
// each failed probe pushing the next one further, never more than maxProbeSpacing.
func (q *Queue) resolve(ctx context.Context, c call, errs []error) []buried {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.inFlight--
	if c.batched {
		q.stats.Batches++
	}
	now := q.options.Now()
	answered, failed := false, false
	var toBury []buried
	for i, op := range c.ops {
		err := errs[i]
		if err != nil && ctx.Err() != nil {
			// shutting down: the operation stays in the journal for the next start
			q.line(op).PushFront(op)
			continue
		}
		switch judge(op, err, q.next(op)) {
		case delivered:
			answered = true
			q.finish(op)
			q.done(op)
			q.stats.Delivered++
			if op.Attempts > 0 {
				q.options.Logger.Printf("request=%s settlement %s delivered after %d retries", op.RequestID, op.Kind, op.Attempts)
			}
		case alreadySettled:
			answered = true
			q.finish(op)
			q.done(op)
			q.stats.AlreadySettled++
			q.options.Logger.Printf("request=%s settlement %s: the control plane already holds it the same way, done (%v)", op.RequestID, op.Kind, err)
		case refused:
			answered = true
			_, inMemory := q.memoryOnly[op.ID] // not in the journal: kept on disk some other way
			q.finish(op)
			toBury = append(toBury, buried{op: op, err: err, unjournaled: inMemory})
		default:
			failed = true
			op.Attempts++
			op.LastError = err.Error()
			op.notBefore = now.Add(q.backoff(op.Attempts))
			heap.Push(&q.delayed, op)
			q.stats.Retried++
			if op.Attempts == 1 || op.Attempts%10 == 0 {
				q.options.Logger.Printf("request=%s settlement %s attempt %d failed, retrying: %v", op.RequestID, op.Kind, op.Attempts, err)
			}
		}
	}
	switch {
	case answered:
		q.failures, q.probes = 0, 0
	case failed:
		q.failures++
		switch {
		case c.probe:
			q.probes++
			q.probeAt = now.Add(min(q.backoff(q.probes+1), maxProbeSpacing))
		case q.failures == q.options.ProbeAfter:
			q.probes = 0
			q.probeAt = now.Add(min(q.backoff(1), maxProbeSpacing))
			q.options.Logger.Printf("settlements: %d failed calls in a row, probing the control plane with one call at a time: %v", q.failures, errs[0])
		}
	}
	return toBury
}

// Transient is true when the control plane did not answer about the operation, or answered "not
// now": timeouts, transport errors, a redirect, any 404 but the platform's reservation_not_found (a
// page, a route not deployed, a catch-all's own not_found: nothing about the reservation), 5xx,
// throttling, 401/403 (a secret being rotated is fixed by an operator, never by dropping money),
// and any answer to a batch call as a whole. A refusal about the operation, or an operation that
// could never be sent, is final.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ledger.ErrBatchEnvelope) {
		return true
	}
	if errors.Is(err, ledger.ErrInvalidOperation) {
		return false
	}
	var httpError *controlplane.HTTPError
	if errors.As(err, &httpError) {
		code := httpError.StatusCode
		if code == http.StatusNotFound && httpError.Code != "reservation_not_found" {
			return true
		}
		return code >= 500 || (code >= 300 && code < 400) || code == http.StatusUnauthorized || code == http.StatusForbidden ||
			code == http.StatusRequestTimeout || code == http.StatusTooEarly || code == http.StatusTooManyRequests
	}
	return !errors.Is(err, ledger.ErrReservationMissing) && !errors.Is(err, ledger.ErrReservationConflict) && !errors.Is(err, ledger.ErrInsufficientCredit)
}

// backoff doubles from half a second up to MaxBackoff; half of it is random, so operations that
// failed together come back spread out.
func (q *Queue) backoff(attempts int) time.Duration {
	wait := baseBackoff
	for i := 1; i < attempts && wait < q.options.MaxBackoff; i++ {
		wait *= 2
	}
	if wait > q.options.MaxBackoff {
		wait = q.options.MaxBackoff
	}
	return wait/2 + time.Duration(q.options.Rand.Int63n(int64(wait)/2+1))
}

// Pending is how many deliveries are still owed.
func (q *Queue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.ops)
}

// Stats reports the queue's counters.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.stats
	s.Pending = len(q.ops)
	return s
}

// Flush waits until nothing is owed or ctx ends; for tests and orderly shutdowns.
func (q *Queue) Flush(ctx context.Context) error {
	for {
		if q.Pending() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
