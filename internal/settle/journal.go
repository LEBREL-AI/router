package settle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// The journal is the queue's record on disk: one JSON line per operation owed ({"op": …}) and one
// per operation the queue is done with ({"done": id}). One writer appends the lines waiting and
// fsyncs once per group, so an operation is on disk before anyone attempts it without a disk flush
// per operation. When the file is past compactAbove and has doubled since its last rewrite, it is
// rewritten with the operations still owed (written to a temporary file, fsynced, renamed, the
// directory fsynced).
//
// Reading it back: a line cut short at the very end is a write that never completed (whoever was
// waiting for it never got its answer) and is dropped; any other line that does not read, or reads
// as an operation that could never be sent, is kept in dead/ and raised as an alert.

const (
	journalName         = "journal"
	defaultCompactAbove = 8 << 20
	groupLines          = 4096
)

type journalRecord struct {
	Op   *Op    `json:"op,omitempty"`
	Done string `json:"done,omitempty"`
}

type entry struct {
	line    []byte
	durable chan error // nil: nobody waits for this line
}

type journal struct {
	dir          string
	path         string
	logger       *log.Logger
	compactAbove int64
	owed         func() []*Op // the operations owed now, for compaction

	file *os.File
	size int64
	// base is the size right after the last rewrite: the journal is rewritten again only once it has
	// doubled, so a large backlog is not rewritten after every group
	base        int64
	compactions atomic.Int64
	// kept are unreadable lines whose dead letter could not be written: every rewrite keeps them, so
	// the next start tries again
	kept [][]byte
	// fault, set only by tests, fails every write the way a full disk does
	fault atomic.Pointer[error]

	mu      sync.Mutex
	pending []entry
	closed  bool
	wake    chan struct{}
	stopped chan struct{}
}

// corrupt is a line the journal could not take back: its raw bytes go to dead/.
type corrupt struct {
	name   string
	raw    []byte
	reason string
	op     *Op
	file   string // the one-file-per-operation file it came from; "" for a journal line
}

// readJournal returns, in the order they were first written, the operations the journal still
// owes, and the lines it cannot take back.
func readJournal(path string) ([]*Op, []corrupt, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	owed := map[string]*Op{}
	var order []string
	var bad []corrupt
	reader := bufio.NewReader(file)
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			complete := line[len(line)-1] == '\n'
			at := offset
			offset += int64(len(line))
			trimmed := bytes.TrimSpace(line)
			if !complete && err == io.EOF {
				if len(trimmed) > 0 {
					// the last write never completed: nobody was told it was on disk
					break
				}
			} else if len(trimmed) > 0 {
				var record journalRecord
				switch {
				case json.Unmarshal(trimmed, &record) != nil || (record.Op == nil) == (record.Done == ""):
					bad = append(bad, corrupt{name: fmt.Sprintf("journal-line-at-%d", at), raw: trimmed, reason: "an unreadable line in the settlement journal"})
				case record.Done != "":
					delete(owed, record.Done)
				default:
					op := record.Op
					if problem := checkOp(op); problem != "" {
						bad = append(bad, corrupt{name: fmt.Sprintf("journal-line-at-%d", at), raw: trimmed, reason: problem, op: op})
						continue
					}
					op.ID = opID(op.ReservationID, op.Kind)
					if _, seen := owed[op.ID]; !seen {
						order = append(order, op.ID)
					}
					owed[op.ID] = op
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
	}
	ops := make([]*Op, 0, len(owed))
	for _, id := range order {
		if op, ok := owed[id]; ok {
			ops = append(ops, op)
			delete(owed, id) // an id written twice is listed once
		}
	}
	return ops, bad, nil
}

// checkOp is what an operation needs to be sendable: "" when it is, else why not.
func checkOp(op *Op) string {
	if op == nil || op.ReservationID == "" {
		return "an operation without a reservation id"
	}
	switch op.Kind {
	case KindAccepted:
		return ""
	case KindSettle:
		if op.Actual < 0 || !op.Outcome.Valid() || !op.Usage.Valid() {
			return fmt.Sprintf("a settlement that can never be sent (outcome %q, %d microUSD)", op.Outcome, op.Actual)
		}
		return ""
	default:
		return fmt.Sprintf("an operation of unknown kind %q", op.Kind)
	}
}

// rewriteJournal writes a fresh journal holding exactly kept and ops, durably, in place of the old
// one, and returns it open for appending: the same file it wrote, so nothing is ever appended to a
// journal the rename has already replaced.
func rewriteJournal(dir string, ops []*Op, kept [][]byte) (*os.File, int64, error) {
	var buffer bytes.Buffer
	for _, line := range kept {
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	for _, op := range ops {
		line, err := json.Marshal(journalRecord{Op: op})
		if err != nil {
			return nil, 0, err
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	final := filepath.Join(dir, journalName)
	temp := final + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o640)
	if err != nil {
		return nil, 0, err
	}
	fail := func(err error) (*os.File, int64, error) {
		file.Close()
		return nil, 0, err
	}
	if _, err := file.Write(buffer.Bytes()); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := os.Rename(temp, final); err != nil {
		return fail(err)
	}
	if err := syncDirectory(dir); err != nil {
		return fail(err)
	}
	return file, int64(buffer.Len()), nil
}

// writeDurably puts name in dir with content so that it survives a power cut: a temporary file,
// fsynced, renamed over name, and the directory fsynced.
func writeDurably(dir, name string, content []byte) error {
	final := filepath.Join(dir, name)
	temp := final + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, final); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func newJournal(dir string, file *os.File, size int64, kept [][]byte, compactAbove int64, logger *log.Logger) *journal {
	if compactAbove <= 0 {
		compactAbove = defaultCompactAbove
	}
	return &journal{dir: dir, path: filepath.Join(dir, journalName), logger: logger, compactAbove: compactAbove, file: file, size: size,
		base: size, kept: kept, wake: make(chan struct{}, 1), stopped: make(chan struct{})}
}

// start runs the writer; owed lists the operations owed when the journal is compacted.
func (j *journal) start(owed func() []*Op) {
	j.owed = owed
	go j.write()
}

// append queues a line; with wait, it returns once the line is on disk (or why it is not).
func (j *journal) append(record journalRecord, wait bool) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	e := entry{line: line}
	if wait {
		e.durable = make(chan error, 1)
	}
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return errors.New("settlement journal closed")
	}
	j.pending = append(j.pending, e)
	j.mu.Unlock()
	select {
	case j.wake <- struct{}{}:
	default:
	}
	if !wait {
		return nil
	}
	return <-e.durable
}

// appendAll queues lines in order and returns once every one is on disk (or the first reason one
// is not): one fsync for as many as fit a group.
func (j *journal) appendAll(records []journalRecord) error {
	entries := make([]entry, len(records))
	for i, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			return err
		}
		entries[i] = entry{line: line, durable: make(chan error, 1)}
	}
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return errors.New("settlement journal closed")
	}
	j.pending = append(j.pending, entries...)
	j.mu.Unlock()
	select {
	case j.wake <- struct{}{}:
	default:
	}
	var first error
	for _, e := range entries {
		if err := <-e.durable; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (j *journal) write() {
	defer close(j.stopped)
	for {
		j.mu.Lock()
		for len(j.pending) == 0 && !j.closed {
			j.mu.Unlock()
			<-j.wake
			j.mu.Lock()
		}
		if len(j.pending) == 0 && j.closed {
			j.mu.Unlock()
			return
		}
		group := j.pending
		if len(group) > groupLines {
			group = group[:groupLines]
		}
		j.pending = j.pending[len(group):]
		j.mu.Unlock()
		var buffer bytes.Buffer
		for _, e := range group {
			buffer.Write(e.line)
			buffer.WriteByte('\n')
		}
		before := j.size
		var err error
		if fault := j.fault.Load(); fault != nil {
			err = *fault
		} else if _, err = j.file.Write(buffer.Bytes()); err == nil {
			err = j.file.Sync()
		}
		if err != nil {
			// leave no half group behind: the next group starts on a clean line
			_ = j.file.Truncate(before)
			var pathError *os.PathError
			if errors.As(err, &pathError) {
				// the file was written as journal.tmp before its rename: name it as the operator sees it
				err = &os.PathError{Op: pathError.Op, Path: j.path, Err: pathError.Err}
			}
			j.logger.Printf("settlement journal: %d lines not written: %v", len(group), err)
		} else {
			j.size += int64(buffer.Len())
		}
		for _, e := range group {
			if e.durable != nil {
				e.durable <- err
			}
		}
		if err == nil && j.size > j.compactAbove && j.size > 2*j.base {
			j.compact()
		}
	}
}

// compact rewrites the journal with the operations owed now. Lines queued meanwhile are written to
// the new file after it, so nothing owed is left out and a finished operation's line never matters.
func (j *journal) compact() {
	file, size, err := rewriteJournal(j.dir, j.owed(), j.kept)
	if err != nil {
		j.logger.Printf("settlement journal: compaction failed, kept as is: %v", err)
		return
	}
	j.file.Close()
	j.file, j.size, j.base = file, size, size
	j.compactions.Add(1)
}

// close writes what is queued and stops the writer.
func (j *journal) close() error {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return nil
	}
	j.closed = true
	j.mu.Unlock()
	select {
	case j.wake <- struct{}{}:
	default:
	}
	<-j.stopped
	return j.file.Close()
}

// legacyOps reads the one-file-per-operation layout an earlier router left (and dead letters an
// operator moved back to replay them): the operations it owes, the files it cannot take back.
func legacyOps(dir string) ([]*Op, []string, []corrupt, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	var ops []*Op
	var files []string
	var bad []corrupt
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("settlement %s: %w", name, err)
		}
		files = append(files, name)
		var op Op
		if err := json.Unmarshal(raw, &op); err != nil {
			bad = append(bad, corrupt{name: strings.TrimSuffix(name, ".json"), raw: raw, reason: "an unreadable settlement file", file: name})
			continue
		}
		if problem := checkOp(&op); problem != "" {
			bad = append(bad, corrupt{name: strings.TrimSuffix(name, ".json"), raw: raw, reason: problem, op: &op, file: name})
			continue
		}
		op.ID = opID(op.ReservationID, op.Kind)
		ops = append(ops, &op)
	}
	return ops, files, bad, nil
}
