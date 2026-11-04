package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"lebrel-router/internal/auth"
	"lebrel-router/internal/dstack"
	"lebrel-router/internal/ledger"
)

// A fake VM: hands back a quote that echoes the report data it was asked, and a fixed identity.
// With hold set, every quote waits until hold is closed (a slow agent).
type fakeAttester struct {
	mu    sync.Mutex
	asked [][]byte
	fail  bool
	hold  chan struct{}
}

func (f *fakeAttester) Quote(_ context.Context, reportData []byte) (dstack.Quote, error) {
	f.mu.Lock()
	f.asked = append(f.asked, append([]byte(nil), reportData...))
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if f.fail {
		return dstack.Quote{}, io.ErrUnexpectedEOF
	}
	return dstack.Quote{Quote: append([]byte{4, 0, 2, 0}, reportData...), EventLog: `[{"imr":3,"event":"compose-hash","event_payload":"50d6"}]`, ReportData: reportData}, nil
}

func (f *fakeAttester) Info(context.Context) (dstack.Info, error) {
	return dstack.Info{AppID: "4d72", InstanceID: "9fed", ComposeHash: "50d6", OSImageHash: "ec43"}, nil
}

// attestedServer is a router as the confidential VM runs it: the same server as the harness, with
// the VM's guest agent (or none) behind it.
func attestedServer(t *testing.T, attester dstack.Attester) (*httptest.Server, *Signer) {
	t.Helper()
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	authenticator := auth.NewHMACAuthenticator(secret, []auth.Record{{AccountID: "acct_test_000001", Digest: auth.Digest(secret, testKey), Concurrency: 4, AccountConcurrency: 4}})
	inMemory := ledger.NewInMemory(map[string]int64{"acct_test_000001": 10_000_000})
	signer, err := NewSigner(bytes.Repeat([]byte{7}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Settings{Routes: Config{Routes: []Route{routeFor(upstream("deepseek/api", "http://127.0.0.1:1", 1))}}, Auth: authenticator, Ledger: inMemory, Signer: signer,
		Receipts: NewReceiptStore(time.Hour, 100), Settlements: runningQueue(t, inMemory), Verification: NewVerification(""), BoardURL: "https://models.lebrel.ai/watch",
		Now: func() time.Time { return time.Unix(1_900_000_000, 0) }, Attester: attester, KeySource: "vm-kms"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts, signer
}

func getAttestation(t *testing.T, ts *httptest.Server, query string) (*http.Response, AttestationDocument) {
	t.Helper()
	response, err := http.Get(ts.URL + AttestationPath + query)
	if err != nil {
		t.Fatal(err)
	}
	var document AttestationDocument
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("not a document: %s", raw)
		}
	}
	return response, document
}

// The quote's report data commits to the receipt-signing key, so a receipt signed by that key was
// signed inside the measured machine; a verifier recomputes it from the document alone.
func TestTheAttestationBindsTheReceiptKeyIntoTheQuotesReportData(t *testing.T) {
	vm := &fakeAttester{}
	ts, signer := attestedServer(t, vm)
	response, document := getAttestation(t, ts, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	// v3: domain, then each field behind its one-byte length (no encryption key, no nonce here).
	expected := sha512.New()
	expected.Write([]byte("lebrel-router-attestation-v3"))
	expected.Write([]byte{32})
	expected.Write(signer.Public)
	expected.Write([]byte{0, 0})
	want := hex.EncodeToString(expected.Sum(nil))
	if document.ReportData != want || document.PublicKey != hex.EncodeToString(signer.Public) || document.SigningKeyID != signer.KeyID {
		t.Fatalf("binding wrong: %+v", document)
	}
	if len(vm.asked) != 1 || hex.EncodeToString(vm.asked[0]) != want {
		t.Fatalf("the VM was asked %x", vm.asked)
	}
	if document.Info.ComposeHash != "50d6" || !json.Valid(document.EventLog) || document.Quote[:8] != "04000200" || document.KeySource != "vm-kms" {
		t.Fatalf("identity, quote or key source missing: %+v", document)
	}
	if response.Header.Get("Access-Control-Allow-Origin") != "*" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("not served as a public document: %v", response.Header)
	}
}

// A verifier's nonce goes into the report data, so the quote is fresh; without one the last quote
// is reused for a while and the VM is not asked again.
func TestANonceMakesAFreshQuoteAndNoNonceReusesTheLastOne(t *testing.T) {
	vm := &fakeAttester{}
	ts, signer := attestedServer(t, vm)
	getAttestation(t, ts, "")
	getAttestation(t, ts, "")
	if len(vm.asked) != 1 {
		t.Fatalf("the VM was asked %d times for the same document", len(vm.asked))
	}
	nonce := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	_, document := getAttestation(t, ts, "?nonce="+nonce)
	nonceBytes, _ := hex.DecodeString(nonce)
	want := hex.EncodeToString(AttestationReportData(signer.Public, nil, nonceBytes))
	if document.Nonce != nonce || document.ReportData != want || len(vm.asked) != 2 {
		t.Fatalf("nonce not honoured: %+v asked=%d", document, len(vm.asked))
	}
	response, _ := getAttestation(t, ts, "?nonce="+nonce+"ff")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a 33-byte nonce accepted: %d", response.StatusCode)
	}
	response, _ = getAttestation(t, ts, "?nonce=zz")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a non-hex nonce accepted: %d", response.StatusCode)
	}
}

// Outside an attested VM the document does not exist, and a VM that cannot quote right now is a
// 503, never a document without a quote.
func TestWithoutAVMThereIsNoAttestationAndAFailingVMIsTold(t *testing.T) {
	ts, _ := attestedServer(t, nil)
	response, _ := getAttestation(t, ts, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("no VM: status %d", response.StatusCode)
	}
	ts, _ = attestedServer(t, &fakeAttester{fail: true})
	response, _ = getAttestation(t, ts, "")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failing VM: status %d", response.StatusCode)
	}
}

func TestTheReportDataIsDeterministicAndDomainSeparated(t *testing.T) {
	key := make([]byte, 32)
	a := AttestationReportData(key, nil, nil)
	b := AttestationReportData(key, nil, nil)
	c := AttestationReportData(key, nil, []byte{1})
	if hex.EncodeToString(a) != hex.EncodeToString(b) || hex.EncodeToString(a) == hex.EncodeToString(c) || len(a) != 64 {
		t.Fatal("report data is not a deterministic 64-byte commitment")
	}
	plain := sha512.Sum512(key)
	if hex.EncodeToString(a) == hex.EncodeToString(plain[:]) {
		t.Fatal("report data is a bare hash of the key: no domain separation")
	}
	// The fields are length-prefixed: moving bytes between them changes the digest.
	other := bytes.Repeat([]byte{2}, 32)
	if hex.EncodeToString(AttestationReportData(key, other, nil)) == hex.EncodeToString(AttestationReportData(append(append([]byte(nil), key...), other...), nil, nil)) {
		t.Fatal("two layouts of the same bytes share a digest")
	}
	if hex.EncodeToString(AttestationReportData(key, nil, other)) == hex.EncodeToString(AttestationReportData(key, other, nil)) {
		t.Fatal("the encryption key and the nonce are interchangeable")
	}
}

// The agent's error text never reaches the outside: a failing VM is told as a plain 503.
func TestAFailingVMsErrorTextStaysInside(t *testing.T) {
	ts, _ := attestedServer(t, &fakeAttester{fail: true})
	response, err := http.Get(ts.URL + AttestationPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || bytes.Contains(raw, []byte("unexpected EOF")) {
		t.Fatalf("status %d body %s", response.StatusCode, raw)
	}
}

// A route manifest says whether its signer runs inside an attested VM: true from the attested
// machine, false from a plain deployment, so a signed data note never implies an attestation that
// the signer cannot show.
func TestTheRouteManifestSaysWhetherItsSignerIsAttested(t *testing.T) {
	attested, _ := attestedServer(t, &fakeAttester{})
	plain := newHarness(t, routeFor(upstream("deepseek/api", "http://127.0.0.1:1", 1)), board)
	for name, c := range map[string]struct {
		url  string
		want bool
	}{"attested": {attested.URL, true}, "plain": {plain.server.URL, false}} {
		response, err := http.Get(c.url + "/m/deepseek--deepseek-v4.1-flash/.well-known/proof-of-edition")
		if err != nil {
			t.Fatal(err)
		}
		var document SignedDocument
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		response.Body.Close()
		if document.Payload["attested"] != c.want {
			t.Fatalf("%s manifest says attested=%v, want %v", name, document.Payload["attested"], c.want)
		}
	}
}

// fetch asks for the attestation from any goroutine (t.Fatal belongs to the test's).
func fetchAttestation(ts *httptest.Server, query string) (int, error) {
	response, err := http.Get(ts.URL + AttestationPath + query)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode, nil
}

// The VM quotes one at a time: verifiers that arrive together wait their turn in a short line and
// are all served, while the cached document (no nonce) keeps answering meanwhile.
func TestFreshQuotesWaitTheirTurnAndTheCachedDocumentIsServedMeanwhile(t *testing.T) {
	vm := &fakeAttester{}
	ts, _ := attestedServer(t, vm)
	if response, _ := getAttestation(t, ts, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("first document: %d", response.StatusCode)
	}
	hold := make(chan struct{})
	vm.mu.Lock()
	vm.hold = hold
	vm.mu.Unlock()
	const askers = attestationInFlight + 3
	results := make(chan int, askers)
	for i := 0; i < askers; i++ {
		go func(i int) {
			status, err := fetchAttestation(ts, fmt.Sprintf("?nonce=%02x", i+1))
			if err != nil {
				status = -1
			}
			results <- status
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		vm.mu.Lock()
		inFlight := len(vm.asked) - 1
		vm.mu.Unlock()
		if inFlight == attestationInFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d quotes in flight after the wait, want %d", inFlight, attestationInFlight)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cached, _ := getAttestation(t, ts, ""); cached.StatusCode != http.StatusOK {
		t.Fatalf("the cached document was refused while the machine quotes: %d", cached.StatusCode)
	}
	select {
	case status := <-results:
		t.Fatalf("an asker in line was answered %d before its turn", status)
	case <-time.After(50 * time.Millisecond):
	}
	close(hold)
	for i := 0; i < askers; i++ {
		if status := <-results; status != http.StatusOK {
			t.Fatalf("an asker in line was refused: %d", status)
		}
	}
	vm.mu.Lock()
	quotes := len(vm.asked)
	vm.mu.Unlock()
	if quotes != askers+1 {
		t.Fatalf("the VM was asked %d times, want %d", quotes, askers+1)
	}
}

// When the line is full, or the wait for a slot runs out, a fresh quote is refused with a 429 and
// a Retry-After; the cached document is still served.
func TestAFullLineOrAWaitThatRunsOutIsToldAsBusy(t *testing.T) {
	vm := &fakeAttester{}
	ts, _ := attestedServer(t, vm)
	getAttestation(t, ts, "")
	cache := &ts.Config.Handler.(*Server).attestations
	cache.mu.Lock()
	cache.waiting = attestationWaiting
	cache.mu.Unlock()
	response, _ := getAttestation(t, ts, "?nonce=0102")
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "5" {
		t.Fatalf("a full line admitted a fresh quote: %d %v", response.StatusCode, response.Header)
	}
	cache.mu.Lock()
	cache.waiting = 0
	cache.wait = 30 * time.Millisecond
	if cache.slots == nil {
		cache.slots = make(chan struct{}, attestationInFlight)
	}
	for i := 0; i < attestationInFlight; i++ {
		cache.slots <- struct{}{}
	}
	cache.mu.Unlock()
	started := time.Now()
	response, _ = getAttestation(t, ts, "?nonce=0304")
	if response.StatusCode != http.StatusTooManyRequests || time.Since(started) < 30*time.Millisecond {
		t.Fatalf("a wait that ran out was not told as busy: %d after %s", response.StatusCode, time.Since(started))
	}
	if cached, _ := getAttestation(t, ts, ""); cached.StatusCode != http.StatusOK {
		t.Fatalf("the cached document was refused while busy: %d", cached.StatusCode)
	}
	// The shared document is renewed outside the line: with every slot taken it is still served.
	cache.invalidate()
	if renewed, _ := getAttestation(t, ts, ""); renewed.StatusCode != http.StatusOK {
		t.Fatalf("the shared document was not renewed while the line was full: %d", renewed.StatusCode)
	}
	cache.mu.Lock()
	for i := 0; i < attestationInFlight; i++ {
		<-cache.slots
	}
	cache.mu.Unlock()
	vm.mu.Lock()
	quotes := len(vm.asked)
	vm.mu.Unlock()
	if quotes != 2 {
		t.Fatalf("the VM was asked %d times, want 2 (the first document and its renewal; no refused request)", quotes)
	}
}
