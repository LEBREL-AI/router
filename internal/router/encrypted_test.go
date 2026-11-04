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
	encryption, err := NewEncryption(bytes.Repeat([]byte{9}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	ts, signer := encryptedServerWith(t, lab, encryption, nil)
	return ts, signer, encryption
}

// encryptedServerWith is encryptedServer with the given identity and any settings tuned.
func encryptedServerWith(t *testing.T, lab *fakeUpstream, encryption *Encryption, tune func(*Settings)) (*httptest.Server, *Signer) {
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
	route := routeFor(upstream("deepseek/api", lab.server.URL, 1))
	settings := Settings{Routes: Config{Routes: []Route{route}}, Auth: authenticator, Ledger: inMemory, Signer: signer,
		Receipts: NewReceiptStore(time.Hour, 100), Settlements: runningQueue(t, inMemory), Verification: NewVerification(""), BoardURL: "https://models.lebrel.ai/watch",
		Now: func() time.Time { return time.Unix(1_900_000_000, 0) }, Encryption: encryption, KeySource: "vm-kms", MaxRequestBytes: 8 << 20}
	if tune != nil {
		tune(&settings)
	}
	server, err := New(settings)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return ts, signer
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
	if document.ReportData != want || document.EncryptionPublicKey != hex.EncodeToString(encryption.PublicKey()) || document.EncryptionKeyID != encryption.KeyID() || !strings.Contains(document.Scheme, "len(encryption_public_key) || encryption_public_key") {
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

// receiptOf decodes the receipt a response carries and checks its signature.
func receiptOf(t *testing.T, response *http.Response, signer *Signer) map[string]any {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(response.Header.Get(receiptHeader))
	if err != nil || len(decoded) == 0 {
		t.Fatalf("no receipt on the answer: %v", response.Header)
	}
	var document SignedDocument
	if err := json.Unmarshal(decoded, &document); err != nil {
		t.Fatal(err)
	}
	if !Verify(document, signer.Public) {
		t.Fatal("receipt signature does not verify")
	}
	return document.Payload
}

func saltedDigest(salt []byte, body string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// A salt the client puts first inside the encrypted body is cut out before the lab sees the
// request and salts the receipt's digests: anyone reading the receipt can no longer confirm a
// guessed prompt or answer, while the client, who knows the salt, still can. Streamed answers too.
func TestAClientSaltKeepsTheReceiptsDigestsPrivateToTheTwoEnds(t *testing.T) {
	salt := bytes.Repeat([]byte{0xA5}, 16)
	inner := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	body := `{"` + ReceiptSaltField + `":"` + hex.EncodeToString(salt) + `",` + inner[1:]

	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	ts, signer, _ := encryptedServer(t, lab)
	client, keyID := clientIdentity(t, ts, signer)
	response, plain := encryptedPost(t, ts, client, keyID, body, testKey)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", response.StatusCode, plain)
	}
	if _, leaked := lab.last(t).body[ReceiptSaltField]; leaked {
		t.Fatal("the salt reached the lab")
	}
	payload := receiptOf(t, response, signer)
	if payload["hash_salt_scheme"] != ReceiptSaltScheme || payload["prompt_sha256"] != saltedDigest(salt, inner) || payload["response_sha256"] != saltedDigest(salt, "hello world") {
		t.Fatalf("digests not salted as the scheme says: %v", payload)
	}
	if payload["prompt_sha256"] == SHA256Hex([]byte(inner)) || payload["response_sha256"] == SHA256Hex([]byte("hello world")) {
		t.Fatal("a plain digest is still in the receipt")
	}

	// Without a salt the digests are plain and the receipt names no scheme.
	response, plain = encryptedPost(t, ts, client, keyID, inner, testKey)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", response.StatusCode, plain)
	}
	payload = receiptOf(t, response, signer)
	if _, named := payload["hash_salt_scheme"]; named || payload["prompt_sha256"] != SHA256Hex([]byte(inner)) {
		t.Fatalf("an unsalted receipt changed: %v", payload)
	}

	// A streamed answer: the receipt stored afterwards carries the salted digest of the whole text.
	streamed := newFakeUpstream(t, sseHandler)
	ts, signer, _ = encryptedServer(t, streamed)
	client, keyID = clientIdentity(t, ts, signer)
	streamBody := `{"` + ReceiptSaltField + `":"` + hex.EncodeToString(salt) + `","model":"deepseek/deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	response, plain = encryptedPost(t, ts, client, keyID, streamBody, testKey)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(plain), "[DONE]") {
		t.Fatalf("stream: %d %s", response.StatusCode, plain)
	}
	store := ts.Config.Handler.(*Server).settings.Receipts
	requestID := response.Header.Get("X-Request-ID")
	var document SignedDocument
	var stored bool
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !stored; {
		if document, stored = store.Get(requestID); !stored {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !stored || document.Payload["hash_salt_scheme"] != ReceiptSaltScheme || document.Payload["response_sha256"] != saltedDigest(salt, "hello world") || document.Payload["prompt_sha256"] != saltedDigest(salt, `{"model":"deepseek/deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`) {
		t.Fatalf("stream receipt not salted: stored=%v %v", stored, document.Payload)
	}
}

// A salt that is malformed, or not the first field, is the client's error, told inside the
// envelope before any lab is called.
func TestAMalformedOrMisplacedSaltIsRefusedInsideTheEnvelope(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{}`))
	ts, signer, _ := encryptedServer(t, lab)
	client, keyID := clientIdentity(t, ts, signer)
	for name, body := range map[string]string{
		"not hex":      `{"` + ReceiptSaltField + `":"zz","model":"deepseek/deepseek-v4.1-flash","messages":[]}`,
		"too short":    `{"` + ReceiptSaltField + `":"abcd","model":"deepseek/deepseek-v4.1-flash","messages":[]}`,
		"not a string": `{"` + ReceiptSaltField + `":12,"model":"deepseek/deepseek-v4.1-flash","messages":[]}`,
		"not first":    `{"model":"deepseek/deepseek-v4.1-flash","` + ReceiptSaltField + `":"` + strings.Repeat("ab", 16) + `","messages":[]}`,
	} {
		response, plain := encryptedPost(t, ts, client, keyID, body, testKey)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(plain), ReceiptSaltField) {
			t.Fatalf("%s: status %d body %s", name, response.StatusCode, plain)
		}
	}
	if len(lab.requests) != 0 {
		t.Fatal("the lab was called")
	}
}

// The encryption key is born per UTC day: after midnight the key document names the new key, a
// client still holding yesterday's key document is served, and one from two days ago is told to
// fetch the document again. Within a day Rotate changes nothing; a derivation that fails keeps the
// key in use.
func TestTheEncryptionKeyRotatesDailyAndYesterdaysKeyStillServes(t *testing.T) {
	derive := func(epoch string) ([]byte, error) {
		if epoch == "2026-11-07" {
			return nil, fmt.Errorf("kms unreachable")
		}
		secret := sha256.Sum256([]byte("secret:" + epoch))
		return secret[:], nil
	}
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 5, 0, time.UTC) }
	encryption, err := NewRotatingEncryption(derive, "router-test", day(4))
	if err != nil {
		t.Fatal(err)
	}
	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	ts, signer := encryptedServerWith(t, lab, encryption, nil)
	client, keyID := clientIdentity(t, ts, signer)
	first := encryption.KeyID()
	if keyID != first {
		t.Fatal("the published key is not today's")
	}
	if err := encryption.Rotate(day(4).Add(20 * time.Hour)); err != nil || encryption.KeyID() != first {
		t.Fatalf("rotated within the day: %v %s", err, encryption.KeyID())
	}
	if err := encryption.Rotate(day(5)); err != nil {
		t.Fatal(err)
	}
	second := encryption.KeyID()
	if second == first {
		t.Fatal("a new day did not bring a new key")
	}
	if _, published := clientIdentity(t, ts, signer); published != second {
		t.Fatal("the key document does not name the new key")
	}
	response, plain := encryptedPost(t, ts, client, first, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Lebrel-Encryption-Key-Id") != first {
		t.Fatalf("yesterday's key refused: %d %s", response.StatusCode, plain)
	}
	if err := encryption.Rotate(day(6)); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/m/deepseek--deepseek-v4.1-flash/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4.1-flash","messages":[]}`))
	request.Header.Set("Authorization", "Bearer "+testKey)
	request.Header.Set("X-Lebrel-Encryption-Key-Id", first)
	if _, err := client.EncryptRequestWithContext(request); err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(response.Header.Get("Content-Type"), "problem+json") {
		t.Fatalf("a key two days old: %d %s", response.StatusCode, response.Header.Get("Content-Type"))
	}
	third := encryption.KeyID()
	if err := encryption.Rotate(day(7)); err == nil || encryption.KeyID() != third {
		t.Fatalf("a failed derivation changed the key: %v %s", err, encryption.KeyID())
	}
	if len(lab.requests) != 1 {
		t.Fatalf("the lab was called %d times", len(lab.requests))
	}
}

// A remembered encapsulation is forgotten once its key is gone: the table never grows without
// bound and a restart's empty table is no weaker than a table whose entries have expired.
func TestAReplayIsRememberedWhileItsKeyLivesAndForgottenAfter(t *testing.T) {
	encryption, err := NewEncryption(bytes.Repeat([]byte{9}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	encapsulation := bytes.Repeat([]byte{1}, 32)
	start := time.Unix(1_900_000_000, 0)
	if fresh, _ := encryption.remember(encapsulation, start); !fresh {
		t.Fatal("first sight not fresh")
	}
	if fresh, _ := encryption.remember(encapsulation, start.Add(encryptedReplayFor-time.Minute)); fresh {
		t.Fatal("a replay within the key's life was admitted")
	}
	if fresh, _ := encryption.remember(encapsulation, start.Add(encryptedReplayFor+time.Minute)); !fresh {
		t.Fatal("an encapsulation whose key is gone is still remembered")
	}
	if len(encryption.replays) != 1 {
		t.Fatalf("expired entries are kept: %d", len(encryption.replays))
	}
}

// The edge is checked before the body is read or anything remembered: an outsider's ciphertext is
// refused in clear and leaves no trace, so the same request through the edge is not a replay.
func TestTheEdgeIsCheckedBeforeAnythingIsReadOrRemembered(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	encryption, err := NewEncryption(bytes.Repeat([]byte{9}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	ts, signer := encryptedServerWith(t, lab, encryption, func(s *Settings) { s.EdgeProxySecret = "edge-secret" })
	client, keyID := clientIdentity(t, ts, signer)
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/m/deepseek--deepseek-v4.1-flash/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+testKey)
	request.Header.Set("X-Lebrel-Encryption-Key-Id", keyID)
	context, err := client.EncryptRequestWithContext(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(request.Body)
	send := func(edge bool) *http.Response {
		again, _ := http.NewRequest(http.MethodPost, request.URL.String(), bytes.NewReader(body))
		again.Header = request.Header.Clone()
		again.Header.Del("Transfer-Encoding")
		if edge {
			again.Header.Set(edgeAuthorizationHeader, "v1="+DeriveEdgeProxyToken("edge-secret"))
		}
		response, err := http.DefaultClient.Do(again)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	outsider := send(false)
	raw, _ := io.ReadAll(outsider.Body)
	if outsider.StatusCode != http.StatusUnauthorized || !strings.Contains(string(raw), "edge_unauthorized") || outsider.Header.Get(protocol.ResponseNonceHeader) != "" {
		t.Fatalf("outsider: %d %s", outsider.StatusCode, raw)
	}
	if len(encryption.replays) != 0 {
		t.Fatal("the outsider's ciphertext was remembered")
	}
	through := send(true)
	if err := context.DecryptResponse(through); err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(through.Body)
	if through.StatusCode != http.StatusOK {
		t.Fatalf("through the edge: %d %s", through.StatusCode, raw)
	}
	if len(lab.requests) != 1 {
		t.Fatalf("the lab was called %d times", len(lab.requests))
	}
}

// An encrypted body beyond the limit is refused before any decryption is tried.
func TestAnEncryptedBodyBeyondTheLimitIsRefusedBeforeDecryption(t *testing.T) {
	lab := newFakeUpstream(t, okJSON(`{}`))
	encryption, err := NewEncryption(bytes.Repeat([]byte{9}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := encryptedServerWith(t, lab, encryption, func(s *Settings) { s.MaxRequestBytes = 1024 })
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/m/deepseek--deepseek-v4.1-flash/v1/chat/completions", bytes.NewReader(make([]byte, 2048)))
	request.Header.Set("Authorization", "Bearer "+testKey)
	request.Header.Set(protocol.EncapsulatedKeyHeader, strings.Repeat("ab", 32))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), "request_too_large") || response.Header.Get(protocol.ResponseNonceHeader) != "" {
		t.Fatalf("over the limit: %d %s", response.StatusCode, raw)
	}
	if len(lab.requests) != 0 {
		t.Fatal("the lab was called")
	}
}
