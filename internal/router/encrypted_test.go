package router

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tinfoilsh/encrypted-http-body-protocol/identity"
	"github.com/tinfoilsh/encrypted-http-body-protocol/protocol"
	"lebrel-router/internal/auth"
	"lebrel-router/internal/ledger"
)

// encryptedServer is the router as the attested VM runs it: with an encryption identity born from
// a fixed secret, one route to a fake lab, and a known API key.
func encryptedServer(t *testing.T, lab *fakeUpstream) (*httptest.Server, *Signer, *Encryption) {
	t.Helper()
	t.Setenv("TEST_UPSTREAM_KEY", "sk-upstream-secret")
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	authenticator := auth.NewHMACAuthenticator(secret, []auth.Record{{AccountID: "acct_test_000001", Digest: auth.Digest(secret, testKey), Concurrency: 4, AccountConcurrency: 4}})
	inMemory := ledger.NewInMemory(map[string]int64{"acct_test_000001": 10_000_000})
	signer, err := NewSigner(bytes.Repeat([]byte{7}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	encryption, err := NewEncryption(bytes.Repeat([]byte{9}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	route := routeFor(upstream("deepseek/api", lab.server.URL, 1))
	server, err := New(Settings{Routes: Config{Routes: []Route{route}}, Auth: authenticator, Ledger: inMemory, Signer: signer,
		Receipts: NewReceiptStore(time.Hour, 100), Settlements: runningQueue(t, inMemory), Verification: NewVerification(""), BoardURL: "https://models.lebrel.ai/watch",
		Now: func() time.Time { return time.Unix(1_900_000_000, 0) }, Encryption: encryption, KeySource: "vm-kms", MaxRequestBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts, signer, encryption
}

// A client as the SDK behaves: fetches the route's signed key document, checks the signature with
// the receipt key the attestation names, and encrypts to the key it carries.
func clientIdentity(t *testing.T, ts *httptest.Server, signer *Signer) (*identity.Identity, string) {
	t.Helper()
	response, err := http.Get(ts.URL + "/m/deepseek--deepseek-v4.1-flash" + EncryptionMetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("metadata: status %d headers %v", response.StatusCode, response.Header)
	}
	var envelope struct {
		Version   int    `json:"version"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.StdEncoding.DecodeString(envelope.Payload)
	signature, _ := base64.StdEncoding.DecodeString(envelope.Signature)
	if envelope.Version != 1 || !ed25519.Verify(signer.Public, payload, signature) {
		t.Fatalf("the key document is not signed by the receipt key: %+v", envelope)
	}
	var metadata encryptionMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ModelID != "deepseek/deepseek-v4.1-flash" || metadata.SigningKeyID != signer.KeyID || metadata.Version != 1 || metadata.ExpiresAt-metadata.IssuedAt != 300 || len(metadata.ServerInstanceID) != 32 {
		t.Fatalf("metadata wrong: %+v", metadata)
	}
	config, _ := base64.StdEncoding.DecodeString(metadata.HPKEConfig)
	digest := sha256.Sum256(config)
	if hex.EncodeToString(digest[:]) != metadata.KeyID {
		t.Fatal("key id is not the digest of the configuration")
	}
	id, err := identity.UnmarshalPublicConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return id, metadata.KeyID
}

func encryptedPost(t *testing.T, ts *httptest.Server, client *identity.Identity, keyID string, body string, key string) (*http.Response, []byte) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/m/deepseek--deepseek-v4.1-flash/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Lebrel-Encryption-Key-Id", keyID)
	context, err := client.EncryptRequestWithContext(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get(protocol.ResponseNonceHeader) == "" {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("answer not encrypted: status %d body %s", response.StatusCode, raw)
	}
	if err := context.DecryptResponse(response); err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, plain
}

// The whole answer, headers aside, travels encrypted: the lab receives the plaintext request, the
// client gets the plaintext answer, and nothing in between can read either.
func TestAnEncryptedRequestIsServedAndAnsweredEncrypted(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	ts, signer, encryption := encryptedServer(t, lab)
	client, keyID := clientIdentity(t, ts, signer)
	if keyID != encryption.KeyID() {
		t.Fatal("the published key is not the router's")
	}
	response, plain := encryptedPost(t, ts, client, keyID, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", response.StatusCode, plain)
	}
	if response.Header.Get("X-Lebrel-Encryption-Key-Id") != keyID || response.Header.Get(upstreamHeader) != "deepseek/api" || response.Header.Get("X-Request-Id") == "" {
		t.Fatalf("headers: %v", response.Header)
	}
	var answer map[string]any
	if err := json.Unmarshal(plain, &answer); err != nil || answer["usage"] == nil {
		t.Fatalf("the decrypted answer is not the lab's: %s", plain)
	}
	if len(lab.requests) != 1 || !strings.Contains(fmt.Sprint(lab.requests[0].body["messages"]), "hi") {
		t.Fatalf("the lab did not get the plaintext request: %+v", lab.requests)
	}
	if lab.requests[0].headers.Get(protocol.EncapsulatedKeyHeader) != "" {
		t.Fatal("the EHBP header leaked to the lab")
	}
}

// Errors after decryption are encrypted too: a wrong model is told inside the envelope.
func TestErrorsAfterDecryptionAreEncrypted(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	ts, signer, _ := encryptedServer(t, lab)
	client, keyID := clientIdentity(t, ts, signer)
	response, plain := encryptedPost(t, ts, client, keyID, `{"model":"other/model","messages":[]}`, testKey)
	if response.StatusCode != http.StatusNotFound || !strings.Contains(string(plain), "model_not_found") {
		t.Fatalf("status %d body %s", response.StatusCode, plain)
	}
	if len(lab.requests) != 0 {
		t.Fatal("the lab was called")
	}
}

// A replayed encapsulation is refused, a key the client no longer holds is told as a key problem,
// and bad framing never reaches the decryption.
func TestReplayStaleKeyAndBadFramingAreRefused(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	ts, signer, _ := encryptedServer(t, lab)
	client, keyID := clientIdentity(t, ts, signer)
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/m/deepseek--deepseek-v4.1-flash/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+testKey)
	context, err := client.EncryptRequestWithContext(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(request.Body)
	encrypted := request
	first, _ := http.NewRequest(http.MethodPost, encrypted.URL.String(), bytes.NewReader(body))
	first.Header = encrypted.Header.Clone()
	first.Header.Del("Transfer-Encoding")
	response, err := http.DefaultClient.Do(first)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("first: %d %s", response.StatusCode, raw)
	}
	_ = context.DecryptResponse(response)
	io.Copy(io.Discard, response.Body)
	second, _ := http.NewRequest(http.MethodPost, encrypted.URL.String(), bytes.NewReader(body))
	second.Header = encrypted.Header.Clone()
	second.Header.Del("Transfer-Encoding")
	replay, err := http.DefaultClient.Do(second)
	if err != nil {
		t.Fatal(err)
	}
	// The replay's answer is encrypted to the same request context: readable with it.
	_ = context.DecryptResponse(replay)
	raw, _ := io.ReadAll(replay.Body)
	if replay.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "replayed_request") {
		t.Fatalf("replay: %d %s", replay.StatusCode, raw)
	}
	if len(lab.requests) != 1 {
		t.Fatalf("the lab was called %d times", len(lab.requests))
	}
	stale, _ := http.NewRequest(http.MethodPost, encrypted.URL.String(), bytes.NewReader(body))
	stale.Header = encrypted.Header.Clone()
	stale.Header.Del("Transfer-Encoding")
	stale.Header.Set("X-Lebrel-Encryption-Key-Id", strings.Repeat("0", 64))
	response, _ = http.DefaultClient.Do(stale)
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(response.Header.Get("Content-Type"), "problem+json") {
		t.Fatalf("stale key: %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	frame := make([]byte, 4+8)
	binary.BigEndian.PutUint32(frame, 8)
	bad, _ := http.NewRequest(http.MethodPost, encrypted.URL.String(), bytes.NewReader(frame))
	bad.Header = encrypted.Header.Clone()
	bad.Header.Set(protocol.EncapsulatedKeyHeader, strings.Repeat("ab", 32))
	response, _ = http.DefaultClient.Do(bad)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "invalid_ciphertext") || response.Header.Get(protocol.ResponseNonceHeader) != "" {
		t.Fatalf("bad framing: %d %s", response.StatusCode, raw)
	}
	_ = keyID
}

// Outside the attested VM there is no key: the key document does not exist and an encrypted
// request is refused in clear, so a client never encrypts to nothing.
func TestWithoutAnEncryptionIdentityEncryptedRequestsAreRefused(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", lab.server.URL, 1)), board)
	response, err := http.Get(h.server.URL + "/m/deepseek--deepseek-v4.1-flash" + EncryptionMetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("metadata without identity: %d", response.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader("x"))
	request.Header.Set(protocol.EncapsulatedKeyHeader, strings.Repeat("ab", 32))
	response, _ = http.DefaultClient.Do(request)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "encryption_unavailable") {
		t.Fatalf("encrypted request without identity: %d %s", response.StatusCode, raw)
	}
}

// The attestation document binds the encryption key beside the receipt key (v2 scheme).
func TestTheAttestationBindsTheEncryptionKeyToo(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{}`))
	ts, signer, encryption := encryptedServer(t, lab)
	server := ts.Config.Handler.(*Server)
	server.settings.Attester = &fakeAttester{}
	response, document := getAttestation(t, ts, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	want := hex.EncodeToString(AttestationReportData(signer.Public, encryption.PublicKey(), nil))
	if document.ReportData != want || document.EncryptionPublicKey != hex.EncodeToString(encryption.PublicKey()) || document.EncryptionKeyID != encryption.KeyID() || !strings.Contains(document.Scheme, "encryption_public_key") {
		t.Fatalf("encryption key not bound: %+v", document)
	}
}

// A streamed answer is encrypted frame by frame and decrypts back into the lab's event stream,
// [DONE] included.
func TestAStreamedAnswerIsEncryptedFrameByFrame(t *testing.T) {
	lab := newFakeUpstream(t, sseHandler)
	ts, signer, _ := encryptedServer(t, lab)
	client, keyID := clientIdentity(t, ts, signer)
	response, plain := encryptedPost(t, ts, client, keyID, `{"model":"deepseek/deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testKey)
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %q body %s", response.StatusCode, response.Header.Get("Content-Type"), plain)
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(plain)), "\n") {
		if strings.HasPrefix(line, "data: ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 5 || lines[4] != "data: [DONE]" || !strings.Contains(lines[0], "hello") {
		t.Fatalf("decrypted stream: %q", lines)
	}
}
