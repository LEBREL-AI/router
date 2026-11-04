package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lebrel-router/internal/auth"
	"lebrel-router/internal/dstack"
	"lebrel-router/internal/ledger"
)

// A fake VM: hands back a quote that echoes the report data it was asked, and a fixed identity.
type fakeAttester struct {
	asked [][]byte
	fail  bool
}

func (f *fakeAttester) Quote(_ context.Context, reportData []byte) (dstack.Quote, error) {
	f.asked = append(f.asked, append([]byte(nil), reportData...))
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
	expected := sha512.New()
	expected.Write([]byte("lebrel-router-attestation-v1"))
	expected.Write(signer.Public)
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
	want := hex.EncodeToString(AttestationReportData(signer.Public, nonceBytes))
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
	a := AttestationReportData(key, nil)
	b := AttestationReportData(key, nil)
	c := AttestationReportData(key, []byte{1})
	if hex.EncodeToString(a) != hex.EncodeToString(b) || hex.EncodeToString(a) == hex.EncodeToString(c) || len(a) != 64 {
		t.Fatal("report data is not a deterministic 64-byte commitment")
	}
	plain := sha512.Sum512(key)
	if hex.EncodeToString(a) == hex.EncodeToString(plain[:]) {
		t.Fatal("report data is a bare hash of the key: no domain separation")
	}
}

// The VM quotes one at a time: a client asking fresh quotes faster than a dozen a minute is told
// to slow down, and the cached document (no nonce) keeps answering meanwhile.
func TestFreshQuotesPerClientAreLimitedAndTheCachedDocumentIsNot(t *testing.T) {
	vm := &fakeAttester{}
	ts, _ := attestedServer(t, vm)
	nonce := "0102030405060708"
	var last int
	for i := 0; i < attestationPerClient+1; i++ {
		response, _ := getAttestation(t, ts, "?nonce="+nonce)
		last = response.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("the %dth fresh quote in a minute was admitted: %d", attestationPerClient+1, last)
	}
	if len(vm.asked) != attestationPerClient {
		t.Fatalf("the VM was asked %d times, want %d", len(vm.asked), attestationPerClient)
	}
	response, _ := getAttestation(t, ts, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the cached document was refused: %d", response.StatusCode)
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

// A refusal for a full machine is not counted against the client, and a fresh cached document is
// served even while the machine is busy quoting.
func TestARefusalDoesNotCountAndTheCacheIsServedWhileBusy(t *testing.T) {
	vm := &fakeAttester{}
	ts, _ := attestedServer(t, vm)
	getAttestation(t, ts, "")
	cache := &ts.Config.Handler.(*Server).attestations
	cache.mu.Lock()
	cache.inUse = attestationInFlight
	cache.mu.Unlock()
	response, _ := getAttestation(t, ts, "?nonce=0102")
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a full machine admitted a fresh quote: %d", response.StatusCode)
	}
	cached, _ := getAttestation(t, ts, "")
	if cached.StatusCode != http.StatusOK {
		t.Fatalf("the cached document was refused while busy: %d", cached.StatusCode)
	}
	cache.mu.Lock()
	count := cache.clients["127.0.0.1"].count
	cache.inUse = 0
	cache.mu.Unlock()
	if count != 0 {
		t.Fatalf("the refused request was counted: %d", count)
	}
}
