package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const switchToken = "the-switch-token-of-the-test"

// behind is the router's own handler in these tests: it answers every path 200 with its name.
func behind() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "router:"+r.URL.Path) })
}

func call(t *testing.T, handler http.Handler, method, path, token, body string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func healthOf(t *testing.T, ready func() bool, token, stateFile string, inFlight func() int64) *relayHealth {
	t.Helper()
	return healthAt(t, ready, token, stateFile, "", inFlight)
}

// healthAt is healthOf for an instance that runs a named release.
func healthAt(t *testing.T, ready func() bool, token, stateFile, release string, inFlight func() int64) *relayHealth {
	t.Helper()
	health, err := newRelayHealth(ready, token, stateFile, release, inFlight, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	return health
}

func report(t *testing.T, body string) map[string]any {
	t.Helper()
	var answer map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return answer
}

func TestTheRelaysCheckSaysWhetherTheInstanceMayBeSentCustomersAndNothingElseChanges(t *testing.T) {
	var ready atomic.Bool
	health := healthOf(t, ready.Load, switchToken, "", nil)
	handler := health.Handler(behind())
	others := []string{"/.well-known/attestation", "/v1/receipts/req_0123", "/v1/chat/completions", "/v1/models", "/m/slug/v1/models", "/readyz", acmeChallengePath}
	unchanged := func(when string) {
		t.Helper()
		for _, path := range others {
			if status, body := call(t, handler, http.MethodGet, path, "", ""); status != http.StatusOK || body != "router:"+path {
				t.Fatalf("%s must answer as before %s: %d %s", path, when, status, body)
			}
		}
	}
	// no certificate yet: it cannot serve
	if status, body := call(t, handler, http.MethodGet, relayHealthPath, "", ""); status != http.StatusConflict || !strings.Contains(body, `"status":"no_certificate"`) {
		t.Fatalf("the relay's check without a certificate: %d %s", status, body)
	}
	unchanged("while it holds no certificate")
	// ready
	ready.Store(true)
	if status, body := call(t, handler, http.MethodGet, relayHealthPath, "", ""); status != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("the relay's check when ready: %d %s", status, body)
	}
	if status, _ := call(t, handler, http.MethodHead, relayHealthPath, "", ""); status != http.StatusOK {
		t.Fatalf("HEAD when ready: %d", status)
	}
	// stepped aside: it serves, and asks to be left
	if status, _ := call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":true}`); status != http.StatusOK {
		t.Fatalf("stepping aside: %d", status)
	}
	if status, body := call(t, handler, http.MethodGet, relayHealthPath, "", ""); status != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"draining"`) {
		t.Fatalf("the relay's check while stepped aside: %d %s", status, body)
	}
	unchanged("while stepped aside")
	// stepped aside and without a certificate: it cannot serve, which is what the relay must know
	ready.Store(false)
	if status, _ := call(t, handler, http.MethodGet, relayHealthPath, "", ""); status != http.StatusConflict {
		t.Fatalf("stepped aside and without a certificate: %d", status)
	}
	if status, _ := call(t, handler, http.MethodPost, relayHealthPath, "", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST on the check: %d", status)
	}
}

func TestAnInstanceStartsSteppedAsideUntilItIsOpenedAndComesBackAsItWasLeft(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "relay-open")
	// a new machine: it holds its certificate (ready) and still takes nobody
	first := healthOf(t, nil, switchToken, file, nil)
	if first.state() != "draining" {
		t.Fatalf("a new instance starts %q, not stepped aside", first.state())
	}
	handler := first.Handler(behind())
	if status, _ := call(t, handler, http.MethodGet, relayHealthPath, "", ""); status != http.StatusServiceUnavailable {
		t.Fatalf("the relay's check on a new instance: %d", status)
	}
	// the operator has tried it and opens it
	if status, body := call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`); status != http.StatusOK || report(t, body)["status"] != "ok" {
		t.Fatalf("opening: %d %s", status, body)
	}
	// it restarts by itself (the host, a crash): it comes back open
	if again := healthOf(t, nil, switchToken, file, nil); again.state() != "ok" {
		t.Fatalf("an open instance came back %q", again.state())
	}
	// the operator steps it aside before an update: it comes back from the update stepped aside
	if status, _ := call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":true}`); status != http.StatusOK {
		t.Fatalf("stepping aside: %d", status)
	}
	if updated := healthOf(t, nil, switchToken, file, nil); updated.state() != "draining" {
		t.Fatalf("an instance stepped aside before its update came back %q", updated.state())
	}
	// a stop does not change what is kept: an open instance told to stop comes back open
	_, _ = call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`)
	first.Stop()
	if back := healthOf(t, nil, switchToken, file, nil); back.state() != "ok" {
		t.Fatalf("an open instance that was stopped came back %q", back.state())
	}
	// without a place to keep it, an instance starts open, as before there was a switch
	if plain := healthOf(t, nil, switchToken, "", nil); plain.state() != "ok" {
		t.Fatalf("an instance without RELAY_STATE_FILE starts %q", plain.state())
	}
	// a switch that cannot be kept is not thrown: the operator is told, and nothing changes
	blocked := filepath.Join(t.TempDir(), "gone", "relay-open")
	kept := healthOf(t, nil, switchToken, blocked, nil)
	if err := os.RemoveAll(filepath.Dir(blocked)); err != nil {
		t.Fatal(err)
	}
	if status, _ := call(t, kept.Handler(behind()), http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`); status != http.StatusInternalServerError || kept.state() != "draining" {
		t.Fatalf("a switch that could not be kept: %d, state %s", status, kept.state())
	}
	// stepping aside is the other way round: the relay is told whatever the disk does, and the operator
	// is told that a restart would open it again
	stuck := filepath.Join(t.TempDir(), "relay-open")
	open := healthOf(t, nil, switchToken, stuck, nil)
	_, _ = call(t, open.Handler(behind()), http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`)
	if err := os.Remove(stuck); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stuck, "in-the-way"), 0o700); err != nil { // a directory with something in it cannot be removed as a file is
		t.Fatal(err)
	}
	status, body := call(t, open.Handler(behind()), http.MethodPost, relayDrainPath, switchToken, `{"drain":true}`)
	if status != http.StatusInternalServerError || open.state() != "draining" || !strings.Contains(body, "come back open") {
		t.Fatalf("a switch that could not be taken off the disk: %d %s, state %s", status, body, open.state())
	}
}

func TestAnInstanceOpenedUnderAnotherReleaseStartsSteppedAside(t *testing.T) {
	file := filepath.Join(t.TempDir(), "relay-open")
	first := healthAt(t, nil, switchToken, file, "compose aaaa", nil)
	_, _ = call(t, first.Handler(behind()), http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`)
	if data, _ := os.ReadFile(file); string(data) != "open compose aaaa\n" {
		t.Fatalf("what is kept: %q", data)
	}
	// the same release restarts by itself: open
	if again := healthAt(t, nil, switchToken, file, "compose aaaa", nil); again.state() != "ok" {
		t.Fatalf("the same release came back %q", again.state())
	}
	// it is updated without the switch (nobody stepped it aside first): the new release has not been
	// tried, so it takes nobody, and what was kept is gone
	updated := healthAt(t, nil, switchToken, file, "compose bbbb", nil)
	if updated.state() != "draining" {
		t.Fatalf("an instance updated while open came back %q", updated.state())
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the earlier release's switch is still on disk: %v", err)
	}
	// so going back to the earlier release does not open by itself either
	if back := healthAt(t, nil, switchToken, file, "compose aaaa", nil); back.state() != "draining" {
		t.Fatalf("back on the earlier release it came back %q", back.state())
	}
	// what a release before this kept (the bare word) is another release's too
	if err := os.WriteFile(file, []byte("open\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if old := healthAt(t, nil, switchToken, file, "compose bbbb", nil); old.state() != "draining" {
		t.Fatalf("a switch kept without its release opened the instance: %q", old.state())
	}
	// a word of another release that cannot be taken off the disk stops nothing: stepped aside all the same
	locked := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	jammed := filepath.Join(locked, "relay-open")
	if err := os.WriteFile(jammed, []byte("open compose aaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil { // nothing can be removed from it (unless the test runs as root, which removes it)
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o700)
	stuck, err := newRelayHealth(nil, switchToken, jammed, "compose bbbb", nil, quietLogger())
	if err != nil || stuck.state() != "draining" {
		t.Fatalf("a switch of another release that could not be removed: %v, state %v", err, stuck)
	}
	// the release is told on the switch
	_, body := call(t, updated.Handler(behind()), http.MethodGet, relayDrainPath, switchToken, "")
	if report(t, body)["release"] != "compose bbbb" {
		t.Fatalf("the switch does not tell the release: %s", body)
	}
}

func TestAnInstanceThatKeepsItsSwitchNeedsTheTokenThatOpensIt(t *testing.T) {
	// it would start stepped aside, the check 503 for good and the switch 404: refused at start
	if _, err := newRelayHealth(nil, "", filepath.Join(t.TempDir(), "relay-open"), "", nil, quietLogger()); err == nil || !strings.Contains(err.Error(), "RELAY_SWITCH_TOKEN") {
		t.Fatalf("a state file without a switch token: %v", err)
	}
	if _, err := newRelayHealth(nil, "", "", "", nil, quietLogger()); err != nil {
		t.Fatalf("no state file and no token is an instance without a switch: %v", err)
	}
}

func TestACertificateAboutToEndStillServesWhenNobodyElseCan(t *testing.T) {
	var ready, serves atomic.Bool
	health := healthOf(t, ready.Load, "", "", nil)
	health.serves = serves.Load
	handler := health.Handler(behind())
	check := func() (int, string) { return call(t, handler, http.MethodGet, relayHealthPath, "", "") }
	// no certificate at all: it cannot serve
	if status, body := check(); status != http.StatusConflict || !strings.Contains(body, `"status":"no_certificate"`) {
		t.Fatalf("no certificate: %d %s", status, body)
	}
	// its certificate ends in minutes: it asks to be left, and still serves if no router may be sent customers
	serves.Store(true)
	if status, body := check(); status != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"expiring"`) {
		t.Fatalf("a certificate about to end: %d %s", status, body)
	}
	ready.Store(true)
	if status, _ := check(); status != http.StatusOK {
		t.Fatalf("a good certificate: %d", status)
	}
	// stopping is told apart from stepping aside: the relay sends it nobody, whatever the other does
	health.Stop()
	if status, body := check(); status != http.StatusGone || !strings.Contains(body, `"status":"stopping"`) {
		t.Fatalf("stopping: %d %s", status, body)
	}
}

func TestAStopAndAnOrderOfTheSwitchNeverLeaveAStoppingInstanceOpen(t *testing.T) {
	for round := 0; round < 200; round++ {
		health := healthOf(t, nil, switchToken, filepath.Join(t.TempDir(), "relay-open"), nil)
		done := make(chan struct{})
		go func() {
			defer close(done)
			health.set(context.Background(), false, false)
		}()
		health.Stop()
		<-done
		if state := health.state(); state != "stopping" {
			t.Fatalf("round %d: a stopping instance answers %q", round, state)
		}
	}
}

func TestTheSwitchAnswersOnlyToItsTokenAndTellsWhatIsLeftOnTheInstance(t *testing.T) {
	flying := int64(3)
	health := healthOf(t, nil, switchToken, "", func() int64 { return flying })
	health.detail = func() map[string]any { return map[string]any{"paired": true} }
	handler := health.Handler(behind())
	if status, _ := call(t, healthOf(t, nil, "", "", nil).Handler(behind()), http.MethodGet, relayDrainPath, switchToken, ""); status != http.StatusNotFound {
		t.Fatalf("without a token configured the switch does not exist: %d", status)
	}
	for _, wrong := range []string{"", "x", switchToken + "x", switchToken[:len(switchToken)-1]} {
		if status, _ := call(t, handler, http.MethodPost, relayDrainPath, wrong, `{"drain":true}`); status != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", wrong, status)
		}
	}
	if health.state() != "ok" {
		t.Fatal("a wrong token stepped the instance aside")
	}
	code, body := call(t, handler, http.MethodGet, relayDrainPath, switchToken, "")
	answer := report(t, body)
	if code != http.StatusOK || answer["status"] != "ok" || answer["draining"] != false || answer["in_flight"] != float64(3) || answer["connections"] != float64(0) || answer["paired"] != true {
		t.Fatalf("reading: %d %s", code, body)
	}
	for _, bad := range []string{"", "{}", `{"drain":"yes"}`, `{"drain":true,"other":1}`, `{"force":true}`, "not json"} {
		if status, _ := call(t, handler, http.MethodPost, relayDrainPath, switchToken, bad); status != http.StatusBadRequest {
			t.Fatalf("body %q: %d", bad, status)
		}
	}
	code, body = call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":true}`)
	answer = report(t, body)
	if code != http.StatusOK || answer["status"] != "draining" || answer["draining"] != true || answer["draining_for_seconds"] == nil {
		t.Fatalf("stepping aside: %d %s", code, body)
	}
	flying = 0
	if answer = report(t, second(call(t, handler, http.MethodGet, relayDrainPath, switchToken, ""))); answer["in_flight"] != float64(0) {
		t.Fatalf("reading while stepped aside: %v", answer)
	}
	code, body = call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`)
	if answer = report(t, body); code != http.StatusOK || answer["status"] != "ok" || answer["draining"] != false {
		t.Fatalf("opening: %d %s", code, body)
	}
	// recycling: the answers close their connections, and the relay goes on sending customers
	code, body = call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"recycle":true}`)
	if answer = report(t, body); code != http.StatusOK || answer["status"] != "ok" || answer["recycling"] != true {
		t.Fatalf("recycling: %d %s", code, body)
	}
	if status, _ := call(t, handler, http.MethodGet, relayHealthPath, "", ""); status != http.StatusOK {
		t.Fatalf("the relay's check while recycling: %d", status)
	}
	if status, _ := call(t, handler, http.MethodDelete, relayDrainPath, switchToken, ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: %d", status)
	}
	// once it is stopping it does not open again
	health.Stop()
	if status, _ := call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`); status != http.StatusConflict || health.state() != "stopping" {
		t.Fatalf("opening a stopping instance: %d, state %s", status, health.state())
	}
}

func second(_ int, body string) string { return body }

func TestAnInstanceDoesNotStepAsideWhileTheOtherTakesNobodyUnlessForced(t *testing.T) {
	health := healthOf(t, nil, switchToken, "", nil)
	var takes atomic.Bool
	var asked atomic.Int64
	health.peerTakes = func(context.Context) (bool, string) {
		asked.Add(1)
		return takes.Load(), "it is stepped aside"
	}
	handler := health.Handler(behind())
	status, body := call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":true}`)
	if status != http.StatusConflict || health.state() != "ok" || !strings.Contains(body, "it is stepped aside") || !strings.Contains(body, "force") {
		t.Fatalf("stepping aside while the other takes nobody: %d %s (state %s)", status, body, health.state())
	}
	if status, _ := call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":true,"force":true}`); status != http.StatusOK || health.state() != "draining" {
		t.Fatalf("forced: %d, state %s", status, health.state())
	}
	// opening never asks the other, nor does stepping aside again
	before := asked.Load()
	_, _ = call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":true}`)
	_, _ = call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":false}`)
	if asked.Load() != before {
		t.Fatal("the other was asked although nothing depended on it")
	}
	takes.Store(true)
	if status, _ := call(t, handler, http.MethodPost, relayDrainPath, switchToken, `{"drain":true}`); status != http.StatusOK || health.state() != "draining" {
		t.Fatalf("stepping aside while the other takes customers: %d, state %s", status, health.state())
	}
}

func TestThePeersCheckIsReadAsTheRelayReadsIt(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != relayHealthPath {
			t.Errorf("asked %s", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	other := peerAt(t, server.URL, pairSecret)
	for code, want := range map[int]bool{http.StatusOK: true, http.StatusNotFound: true, http.StatusServiceUnavailable: false, http.StatusGone: false, http.StatusConflict: false, http.StatusInternalServerError: false} {
		status = code
		if got, why := other.takes(context.Background()); got != want || (!got && why == "") {
			t.Fatalf("a peer answering %d: takes %v (%q)", code, got, why)
		}
	}
	server.Close()
	if got, why := other.takes(context.Background()); got || why == "" {
		t.Fatalf("a peer that does not answer: takes %v (%q)", got, why)
	}
}

func TestWhileSteppedAsideTheAnswersCloseTheirConnectionsAndTheConnectionsOpenAreCounted(t *testing.T) {
	health := healthOf(t, nil, switchToken, "", nil)
	server := httptest.NewUnstartedServer(health.Closing(behind()))
	server.Config.ConnState = health.ConnState
	server.Start()
	defer server.Close()
	var dials atomic.Int64
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	get := func() *http.Response {
		t.Helper()
		response, err := client.Get(server.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response
	}
	// open: a customer keeps its connection
	for i := 0; i < 3; i++ {
		if response := get(); response.Close {
			t.Fatal("an open instance closed a customer's connection")
		}
	}
	if dials.Load() != 1 || health.open.Load() != 1 {
		t.Fatalf("%d dials, %d connections counted, for one customer that keeps its connection", dials.Load(), health.open.Load())
	}
	// stepped aside: the answer closes the connection, and the customer's next call opens another (through the relay)
	health.mu.Lock()
	health.draining = true
	health.mu.Unlock()
	if response := get(); !response.Close {
		t.Fatal("a stepped-aside instance kept the customer's connection")
	}
	deadline := time.Now().Add(2 * time.Second)
	for health.open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if health.open.Load() != 0 {
		t.Fatalf("%d connections counted after the customer's was closed", health.open.Load())
	}
	// the customer's next call comes on a new connection, and that one is closed after its answer too
	if response := get(); !response.Close || dials.Load() != 2 {
		t.Fatalf("%d dials: after its connection was closed the customer's next call opens another", dials.Load())
	}
	if response := get(); !response.Close || dials.Load() != 3 {
		t.Fatalf("%d dials: every call to a stepped-aside instance comes on a connection of its own", dials.Load())
	}
	// recycling does the same without stepping aside
	health.mu.Lock()
	health.draining, health.recycleUntil = false, time.Now().Add(time.Minute)
	health.mu.Unlock()
	if response := get(); !response.Close {
		t.Fatal("a recycling instance kept the customer's connection")
	}
	// and it ends by itself: an order nobody took back does not close connections for ever
	health.mu.Lock()
	health.recycleUntil = time.Now().Add(-time.Second)
	health.mu.Unlock()
	if response := get(); response.Close {
		t.Fatal("recycling went on after its time")
	}
	if health.report()["recycling"] != false {
		t.Fatal("the switch says it still recycles")
	}
}

func TestAStopTellsTheRelayAndGoesOnServingUntilItHasNoticed(t *testing.T) {
	again := make(chan os.Signal, 1)
	// the relay is told and given its time
	health := healthOf(t, nil, "", "", nil)
	started := time.Now()
	tellRelay(health, 150*time.Millisecond, again, quietLogger())
	if waited := time.Since(started); waited < 140*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("waited %s for the relay, not 150ms", waited)
	}
	if health.state() != "stopping" {
		t.Fatal("the relay was not told")
	}
	// an instance that had stepped aside waits the whole time too, however long ago it did: when no
	// router is open the relay still sends it customers, and it has to leave that list as well
	old := healthOf(t, nil, switchToken, "", nil)
	_, _ = call(t, old.Handler(behind()), http.MethodPost, relayDrainPath, switchToken, `{"drain":true,"force":true}`)
	old.mu.Lock()
	old.since = time.Now().Add(-time.Minute)
	old.mu.Unlock()
	started = time.Now()
	tellRelay(old, 200*time.Millisecond, again, quietLogger())
	if waited := time.Since(started); waited < 190*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("stepped aside a minute before the stop, it waited %s of the relay's 200ms", waited)
	}
	if old.state() != "stopping" {
		t.Fatalf("a stepped-aside instance that stops answers %q", old.state())
	}
	// no wait configured: told all the same, at once
	none := healthOf(t, nil, "", "", nil)
	started = time.Now()
	tellRelay(none, 0, again, quietLogger())
	if waited := time.Since(started); waited > time.Second || none.state() != "stopping" {
		t.Fatalf("no wait: %s, state %s", waited, none.state())
	}
	// a second stop signal ends the wait
	hurried := healthOf(t, nil, "", "", nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		again <- syscall.SIGTERM
	}()
	started = time.Now()
	tellRelay(hurried, 10*time.Second, again, quietLogger())
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("a second stop signal did not end the wait (%s)", waited)
	}
}
