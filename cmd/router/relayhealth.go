package main

// What the relay in front asks before it sends this instance a customer, and the operator's switch.
//
// The relay checks each router on its plaintext listener with `GET /healthz/relay`:
//
//	200  it may be sent customers
//	503  it holds a certificate and serves, but asks to be left: the operator stepped it aside, or
//	     its certificate is about to expire (the relay sends its new customers to the other
//	     router, and to this one only when no router may be sent any)
//	410  it was told to stop: it serves what it has and is sent nobody, whatever the other does
//	409  it holds no valid certificate for a name its customers call (tlspair.go): it cannot serve
//
// A router of a release before this file answers the path 404, which the relay takes as "may be
// sent customers" (it always could), so one relay configuration serves an update from such a
// release. Nothing else on the listener changes: the attestation, the receipts and the routes
// answer as before, and the answers in flight finish.
//
// The switch is `/healthz/relay/drain` (RELAY_SWITCH_TOKEN). `POST {"drain": true}` steps the
// instance aside: the check answers 503 and every answer on the TLS listener closes its connection
// after it, so a customer who keeps one open comes back through the relay, to the other router.
// `GET` tells what is left on it (answers in flight, connections open), and when both are nothing
// the instance can be updated without anyone noticing. `POST {"drain": false}` opens it again.
//
// The switch is kept on the state volume (RELAY_STATE_FILE) with the release that was opened (the
// measured compose), and an instance that does not find its own release there starts stepped
// aside: a new machine, and a machine that comes back from an update however it was updated,
// takes no customer because it holds a certificate; it takes them when the operator has tried it
// and opened it. A machine that was open and restarts by itself, on the same release, comes back
// open. The release is the measured compose and nothing else: a restart that changes only sealed
// variables (a lab's key, the peer's address) is the same release, so step the instance aside
// first when what changes should be tried before it takes customers.
//
// `{"recycle": true}` only makes the answers close their connections, the check still answering
// 200: it empties a relay (the customers come back through the name, wherever it points by then)
// without moving anyone off this router. It ends by itself after a quarter of an hour.

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	relayHealthPath = "/healthz/relay"
	relayDrainPath  = "/healthz/relay/drain"
	relayOpen       = "open" // RELAY_STATE_FILE holds this word, and the release, while the instance takes customers
	recycleFor      = 15 * time.Minute
)

type relayHealth struct {
	ready     func() bool // a certificate for every name customers call, valid for a while yet; nil: always
	serves    func() bool // a certificate for every name customers call, valid now; nil: as ready says
	token     string      // opens the switch; "" for none
	inFlight  func() int64
	logger    *log.Logger
	stateFile string // where the switch is kept across restarts; "": nowhere, and the instance starts open
	release   string // what this instance runs (the measured compose): opened under another one, it starts stepped aside
	// peerTakes says whether the other router may be sent customers now (and why not): asked before
	// this one steps aside. nil: there is no other.
	peerTakes func(ctx context.Context) (bool, string)
	// detail is the rest of what the operator is told: the certificates held, the ACME account, the pair.
	detail func() map[string]any
	now    func() time.Time

	sw sync.Mutex // one order of the switch, or the stop, at a time: the file and the state change together

	mu           sync.Mutex
	draining     bool
	since        time.Time // when it began to step aside
	stopping     bool
	recycleUntil time.Time    // answers close their connections until then
	open         atomic.Int64 // connections open on the TLS listener
}

func newRelayHealth(ready func() bool, token, stateFile, release string, inFlight func() int64, logger *log.Logger) (*relayHealth, error) {
	h := &relayHealth{ready: ready, token: strings.TrimSpace(token), inFlight: inFlight, logger: logger, stateFile: strings.TrimSpace(stateFile), release: strings.TrimSpace(release), now: time.Now}
	if h.stateFile == "" {
		return h, nil
	}
	if h.token == "" {
		// it would start stepped aside with no switch to open it: the check 503 for good, silently
		return nil, errors.New("an instance that keeps its switch needs RELAY_SWITCH_TOKEN, or nobody could open it")
	}
	if err := os.MkdirAll(filepath.Dir(h.stateFile), 0o700); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(h.stateFile)
	switch {
	case err == nil && string(data) == h.openWord():
		logger.Printf("relay health: open, as it was left")
		return h, nil
	case err == nil && strings.HasPrefix(string(data), relayOpen):
		logger.Printf("relay health: starts stepped aside: it was open under another release (%s), and this one has not been tried: it takes customers once it is opened (POST %s {\"drain\": false})",
			strings.TrimSpace(strings.TrimPrefix(string(data), relayOpen)), relayDrainPath)
		// what is on disk says what the instance does: a later start of the earlier release does not open by
		// itself either. A word that cannot be taken off stops nothing: the instance starts stepped aside all the same.
		if err := os.Remove(h.stateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("relay health: the earlier release's switch could not be taken off the disk (%v): if this instance is put back on that release it would start open", err)
		}
	case err == nil || errors.Is(err, os.ErrNotExist):
		logger.Printf("relay health: starts stepped aside (a new instance, or one that was before it stopped): it takes customers once it is opened (POST %s {\"drain\": false})", relayDrainPath)
	default:
		return nil, err
	}
	h.draining, h.since = true, h.now()
	return h, nil
}

// openWord is what the state file holds while this release is open.
func (h *relayHealth) openWord() string {
	if h.release == "" {
		return relayOpen + "\n"
	}
	return relayOpen + " " + h.release + "\n"
}

// state is why the relay may or may not send customers here: "ok", "draining", "expiring",
// "stopping" or "no_certificate".
func (h *relayHealth) state() string {
	ready := h.ready == nil || h.ready()
	serves := ready
	if !ready && h.serves != nil {
		serves = h.serves()
	}
	if !serves {
		return "no_certificate"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.stopping:
		return "stopping"
	case h.draining:
		return "draining"
	case !ready:
		// its certificate still serves, for minutes: the other router takes the customers while it can
		return "expiring"
	}
	return "ok"
}

// Stop makes the instance step aside for good: it is stopping. An order of the switch that was
// being carried out ends first.
func (h *relayHealth) Stop() {
	h.sw.Lock()
	defer h.sw.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopping = true
	if !h.draining {
		h.draining, h.since = true, h.now()
	}
}

// closing says whether the answers must close their connections: the instance steps aside, or it
// was asked to send its customers back through the relay.
func (h *relayHealth) closing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining || h.now().Before(h.recycleUntil)
}

// Closing wraps the TLS listener's handler: while the instance steps aside (or recycles), an answer
// closes its connection after it (HTTP/2: the connection is told to go away), so whoever keeps
// connections open returns through the relay.
func (h *relayHealth) Closing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.closing() {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

// ConnState counts the connections open on the TLS listener (an http.Server's ConnState).
func (h *relayHealth) ConnState(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		h.open.Add(1)
	case http.StateClosed, http.StateHijacked:
		h.open.Add(-1)
	}
}

// Handler wraps the plaintext listener's handler.
func (h *relayHealth) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case relayHealthPath:
			h.serveHealth(w, r)
		case relayDrainPath:
			h.serveSwitch(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (h *relayHealth) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := h.state()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	switch state {
	case "draining", "expiring":
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
	case "stopping":
		w.WriteHeader(http.StatusGone)
	case "no_certificate":
		w.WriteHeader(http.StatusConflict)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": state})
}

// report is what the operator is told.
func (h *relayHealth) report() map[string]any {
	state := h.state()
	h.mu.Lock()
	now := h.now()
	recycling := now.Before(h.recycleUntil)
	answer := map[string]any{"status": state, "draining": h.draining, "recycling": recycling, "stopping": h.stopping, "connections": h.open.Load()}
	if h.draining {
		answer["draining_for_seconds"] = int64(now.Sub(h.since) / time.Second)
	}
	if recycling {
		answer["recycling_ends_in_seconds"] = int64(h.recycleUntil.Sub(now) / time.Second)
	}
	if h.release != "" {
		answer["release"] = h.release
	}
	h.mu.Unlock()
	if h.inFlight != nil {
		answer["in_flight"] = h.inFlight()
	}
	if h.detail != nil {
		for key, value := range h.detail() {
			answer[key] = value
		}
	}
	return answer
}

func (h *relayHealth) refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}

// serveSwitch is the operator's switch: GET reads it, POST sets it. It answers only to
// RELAY_SWITCH_TOKEN, and not at all when none is set.
func (h *relayHealth) serveSwitch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.token == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	received := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(received) != len(h.token) || !hmac.Equal([]byte(received), []byte(h.token)) {
		http.Error(w, "missing or wrong token", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var order struct {
			Drain   *bool `json:"drain"`
			Recycle *bool `json:"recycle"`
			Force   bool  `json:"force"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&order); err != nil || (order.Drain == nil && order.Recycle == nil) {
			h.refuse(w, http.StatusBadRequest, `the body is {"drain": true|false} or {"recycle": true|false}; {"drain": true, "force": true} steps aside although the other router takes no customers`)
			return
		}
		if order.Drain != nil {
			if status, message := h.set(r.Context(), *order.Drain, order.Force); status != 0 {
				h.refuse(w, status, message)
				return
			}
		}
		if order.Recycle != nil {
			h.mu.Lock()
			changed := h.now().Before(h.recycleUntil) != *order.Recycle
			h.recycleUntil = time.Time{}
			if *order.Recycle {
				h.recycleUntil = h.now().Add(recycleFor)
			}
			h.mu.Unlock()
			if changed && *order.Recycle {
				h.logger.Printf("relay health: answers close their connections for %s, by the operator's order: the customers come back through the relay", recycleFor)
			} else if changed {
				h.logger.Printf("relay health: answers keep their connections again, by the operator's order")
			}
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.report())
}

// set steps the instance aside or opens it, and keeps the switch for the next start. It returns a
// status and a reason when it does not: stepping aside while the other router takes no customers
// would leave them with nobody (force says to do it all the same), and a stopping instance does not
// open. One order at a time, and never across a stop: what is on disk and what the check answers
// change together.
func (h *relayHealth) set(ctx context.Context, drain, force bool) (int, string) {
	h.mu.Lock()
	was := h.draining
	h.mu.Unlock()
	if drain && !was && !force && h.peerTakes != nil {
		if ok, why := h.peerTakes(ctx); !ok {
			return http.StatusConflict, "the other router takes no customers now (" + why + "): stepping this one aside would leave them with nobody; open the other first, or pass \"force\": true"
		}
	}
	h.sw.Lock()
	defer h.sw.Unlock()
	h.mu.Lock()
	stopping, was := h.stopping, h.draining
	h.mu.Unlock()
	if !drain && stopping {
		return http.StatusConflict, "this instance is stopping: it does not open again"
	}
	if !drain {
		// opened on disk first: an instance that could not keep it stays as it was
		if h.stateFile != "" {
			if err := writeFileAtomic(h.stateFile, []byte(h.openWord())); err != nil {
				return http.StatusInternalServerError, "the switch could not be kept on disk, and the instance stays as it was: " + err.Error()
			}
		}
		h.mu.Lock()
		h.draining = false
		h.mu.Unlock()
		if was {
			h.logger.Printf("relay health: opened by the operator's order")
		}
		return 0, ""
	}
	// stepped aside in memory first: the relay is told whatever the disk does
	h.mu.Lock()
	if !h.draining {
		h.draining, h.since = true, h.now()
	}
	h.mu.Unlock()
	if !was {
		h.logger.Printf("relay health: stepped aside by the operator's order: the relay is told to send no new customers")
	}
	if h.stateFile != "" {
		if err := os.Remove(h.stateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			h.logger.Printf("relay health: stepped aside, but the switch could not be taken off the disk: %v", err)
			return http.StatusInternalServerError, "this instance stepped aside, but the switch could not be taken off the disk (" + err.Error() + "): after a restart it would come back open"
		}
	}
	return 0, ""
}

// writeFileAtomic replaces path with data, never leaving half of it.
func writeFileAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".relay-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
