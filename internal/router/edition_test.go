package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinfoilsh/encrypted-http-body-protocol/protocol"
)

// fakeEdge stands in for Lebrel's edge with the edition's runtime behind it: it publishes the
// runtime's signed encryption document, checks what the real edge checks, decrypts what the router
// sent, and answers encrypted (a completion or a stream), beside the public documents.
type fakeEdge struct {
	server     *httptest.Server
	encryption *Encryption
	signer     *Signer
	now        func() time.Time
	mu         sync.Mutex
	seen       []fakeEdgeRequest
	// refuseOnce answers the next completion in plaintext with this status and body, then serves;
	// refuseProblem makes it the runtime's own refusal (a problem document) rather than the edge's
	refuseOnce    int
	refuseBody    string
	refuseProblem bool
	metadataHits  int
}

type fakeEdgeRequest struct {
	headers http.Header
	body    map[string]any
	raw     []byte
}

func newFakeEdge(t *testing.T, modelID string, now func() time.Time) *fakeEdge {
	t.Helper()
	encryption, err := NewEncryption(bytes.Repeat([]byte{9}, 32), "edge-test")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewSigner(bytes.Repeat([]byte{3}, 32), "runtime-test")
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeEdge{encryption: encryption, signer: signer, now: now}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == EncryptionMetadataPath:
			e.mu.Lock()
			e.metadataHits++
			e.mu.Unlock()
			document, err := encryption.SignedMetadata(signer, modelID, now())
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(document)
		case r.URL.Path == "/.well-known/proof-of-edition":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"payload":{"kind":"serving","model_id":"` + modelID + `"},"signature":"c2ln"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/receipts/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/receipts/")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"payload":{"kind":"receipt","request_id":"` + id + `"},"signature":"c2ln"}`))
		case r.URL.Path == completionPath:
			e.mu.Lock()
			refuse, body, problem := e.refuseOnce, e.refuseBody, e.refuseProblem
			e.refuseOnce, e.refuseProblem = 0, false
			e.mu.Unlock()
			if refuse != 0 {
				w.Header().Set("Content-Type", "application/json")
				if problem {
					w.Header().Set("Content-Type", "application/problem+json")
				}
				w.WriteHeader(refuse)
				_, _ = w.Write([]byte(body))
				return
			}
			current, _ := encryption.keys()
			hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
			if !hex64.MatchString(r.Header.Get(protocol.EncapsulatedKeyHeader)) || r.Header.Get(editionKeyIDHeader) != current.keyID || !regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`).MatchString(r.Header.Get(editionRequestIDHeader)) || r.Header.Get("Content-Type") != "application/octet-stream" {
				http.Error(w, `{"error":{"code":"encryption_required","message":"use the encrypted client"}}`, 400)
				return
			}
			ciphertext, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(ciphertext))
			responseContext, err := current.identity.DecryptRequestWithContext(r)
			if err != nil {
				http.Error(w, `{"error":{"code":"invalid_ciphertext","message":"bad"}}`, 400)
				return
			}
			plaintext, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(plaintext, &payload)
			e.mu.Lock()
			e.seen = append(e.seen, fakeEdgeRequest{headers: r.Header.Clone(), body: payload, raw: plaintext})
			e.mu.Unlock()
			ew, err := current.identity.SetupDerivedResponseEncryption(w, responseContext)
			if err != nil {
				http.Error(w, "encryption failed", 500)
				return
			}
			ew.Header().Set(editionKeyIDHeader, current.keyID)
			ew.Header().Set(receiptHeader, base64.StdEncoding.EncodeToString([]byte(`{"payload":{"kind":"receipt"},"signature":"c2ln"}`)))
			ew.Header().Set("X-Request-Id", "runtime-request")
			if payload["stream"] == true {
				ew.Header().Set("Content-Type", "text/event-stream")
				ew.WriteHeader(200)
				for _, chunk := range []string{
					`{"id":"s","model":"deepseek-v41","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
					`{"id":"s","model":"deepseek-v41","choices":[{"index":0,"delta":{"content":" world"}}]}`,
					`{"id":"s","model":"deepseek-v41","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`{"id":"s","model":"deepseek-v41","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
				} {
					_, _ = fmt.Fprintf(ew, "data: %s\n\n", chunk)
					ew.Flush()
				}
				_, _ = io.WriteString(ew, "data: [DONE]\n\n")
				ew.Flush()
				return
			}
			ew.Header().Set("Content-Type", "application/json")
			ew.WriteHeader(200)
			_, _ = ew.Write([]byte(`{"id":"up-1","object":"chat.completion","model":"deepseek-v41","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello world"}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *fakeEdge) requests() []fakeEdgeRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]fakeEdgeRequest(nil), e.seen...)
}

func editionRoute(edge *fakeEdge) Route {
	return Route{
		ModelID: "lebrel/test-uncensored", DisplayName: "Test · Lebrel Uncensored", BaseModel: "Test", Level: 3, Family: "test", Kind: routeKindEdition,
		MaxContextTokens: 1_048_544, MaxOutputTokens: 8192,
		Edition:  &EditionConfig{Origin: edge.server.URL, SigningKeyB64: base64.StdEncoding.EncodeToString(edge.signer.Public)},
		DataNote: "Encrypted here to the edition's own GPUs.",
	}
}

func TestAnEditionIsServedThroughTheRouterEncryptedToItsRuntime(t *testing.T) {
	now := func() time.Time { return time.Unix(1_900_000_000, 0) }
	edge := newFakeEdge(t, "lebrel/test-uncensored", now)
	h := newHarnessWith(t, editionRoute(edge), "", func(s *Settings) { s.EdgeProxySecret = "edge-secret"; s.DirectClients = true })
	// A plain request over the router's own connection; the priority tier asked for as OpenAI clients ask.
	response := h.post(t, `{"model":"lebrel/test-uncensored","messages":[{"role":"user","content":"hi there"}],"service_tier":"priority","max_tokens":64}`, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(raw), `"hello world"`) {
		t.Fatalf("edition answer: %d %s", response.StatusCode, raw)
	}
	if response.Header.Get(receiptHeader) == "" || response.Header.Get("X-Lebrel-Encryption") != "router-to-runtime" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(response.Header.Get(editionRequestIDHeader)) {
		t.Fatalf("answer headers: %v", response.Header)
	}
	if nonce := response.Header.Get(protocol.ResponseNonceHeader); nonce != "" {
		t.Fatal("the encryption ends in the router: no nonce reaches the client")
	}
	seen := edge.requests()
	if len(seen) != 1 {
		t.Fatalf("the edge saw %d requests", len(seen))
	}
	sent := seen[0]
	if sent.headers.Get("Authorization") != "Bearer "+testKey || sent.headers.Get(editionTierHeader) != "priority" || sent.headers.Get(editionRequestIDHeader) != response.Header.Get(editionRequestIDHeader) {
		t.Fatalf("the edge's headers: %v", sent.headers)
	}
	if sent.body["model"] != "lebrel/test-uncensored" || sent.body["service_tier"] != nil || sent.body["max_tokens"] != float64(64) {
		t.Fatalf("the plaintext the runtime decrypts: %v", sent.body)
	}
	// Without a tier to take out, the runtime decrypts the client's own bytes: the receipt's prompt digest is theirs to check.
	asSent := `{"model":"lebrel/test-uncensored","messages":[{"role":"user","content":"hi there"}],"max_tokens":64}`
	if response := h.post(t, asSent, testKey); response.StatusCode != 200 {
		t.Fatalf("plain edition answer: %d", response.StatusCode)
	}
	if seen := edge.requests(); string(seen[len(seen)-1].raw) != asSent {
		t.Fatalf("the runtime decrypted %q, not the client's bytes", seen[len(seen)-1].raw)
	}
	// Streamed: the decrypted events reach the client as they come.
	response = h.post(t, `{"model":"lebrel/test-uncensored","messages":[{"role":"user","content":"hi there"}],"stream":true}`, testKey)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(string(raw), `"content":" world"`) || !strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("streamed edition answer: %d %q %s", response.StatusCode, response.Header.Get("Content-Type"), raw)
	}
	// The edge's own plaintext refusal is relayed by status and code, never trusted beyond that.
	edge.mu.Lock()
	edge.refuseOnce, edge.refuseBody = 402, `{"error":{"code":"insufficient_funds","message":"Top up your balance"}}`
	edge.mu.Unlock()
	response = h.post(t, `{"model":"lebrel/test-uncensored","messages":[{"role":"user","content":"hi there"}]}`, testKey)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 402 || !strings.Contains(string(raw), `"insufficient_funds"`) {
		t.Fatalf("edge refusal: %d %s", response.StatusCode, raw)
	}
	// A key the edge says has rotated is fetched again, once, and the request goes through.
	hits := edge.metadataHits
	edge.mu.Lock()
	edge.refuseOnce, edge.refuseBody = 400, `{"error":{"code":"encryption_key_rotated","message":"the key rotated"}}`
	edge.mu.Unlock()
	response = h.post(t, `{"model":"lebrel/test-uncensored","messages":[{"role":"user","content":"hi there"}]}`, testKey)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 || edge.metadataHits != hits+1 {
		t.Fatalf("after a rotation: %d (metadata fetched %d more times) %s", response.StatusCode, edge.metadataHits-hits, raw)
	}
	// The runtime's own refusal of the ciphertext (its key changed with a restart; the edge relays its
	// problem document as it is) is a rotation too: fetched again, once, and the request goes through.
	hits = edge.metadataHits
	edge.mu.Lock()
	edge.refuseOnce, edge.refuseBody, edge.refuseProblem = 422, `{"type":"about:blank","title":"Encrypted request could not be authenticated","status":422}`, true
	edge.mu.Unlock()
	response = h.post(t, `{"model":"lebrel/test-uncensored","messages":[{"role":"user","content":"hi there"}]}`, testKey)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 || edge.metadataHits != hits+1 {
		t.Fatalf("after the runtime's refusal: %d (metadata fetched %d more times) %s", response.StatusCode, edge.metadataHits-hits, raw)
	}
	// A wrong key is refused by the router before anything is encrypted.
	before := len(edge.requests())
	if response := h.post(t, `{"model":"lebrel/test-uncensored","messages":[{"role":"user","content":"x"}]}`, "leb_live_wrong"); response.StatusCode != 401 || len(edge.requests()) != before {
		t.Fatalf("wrong key: %d (edge saw %d more)", response.StatusCode, len(edge.requests())-before)
	}
	// The edition's public documents come from the edge: its manifest and its receipts, under the id the router handed out.
	for path, want := range map[string]string{"/v1/receipts/" + response.Header.Get(editionRequestIDHeader): "receipt", "/m/lebrel--test-uncensored/.well-known/proof-of-edition": "serving", "/v1/models": "lebrel/test-uncensored"} {
		document, err := http.Get(h.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(document.Body)
		if document.StatusCode != 200 || !strings.Contains(string(raw), want) {
			t.Fatalf("%s: %d %s", path, document.StatusCode, raw)
		}
	}
}

func TestClaudeCodeReachesTheEditionWithoutAnyProxy(t *testing.T) {
	now := func() time.Time { return time.Unix(1_900_000_000, 0) }
	edge := newFakeEdge(t, "lebrel/test-uncensored", now)
	h := newHarnessWith(t, editionRoute(edge), "", func(s *Settings) { s.EdgeProxySecret = "edge-secret"; s.DirectClients = true })
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+anthropicMessagesPath, strings.NewReader(`{"model":"lebrel/test-uncensored","max_tokens":64,"messages":[{"role":"user","content":"hi there"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("messages through the edition: %d %s", response.StatusCode, raw)
	}
	message := decodeJSON(t, string(raw))
	if message["type"] != "message" || message["content"].([]any)[0].(map[string]any)["text"] != "hello world" || response.Header.Get(receiptHeader) == "" {
		t.Fatalf("message: %s", raw)
	}
	seen := edge.requests()
	if len(seen) != 1 || seen[0].body["messages"] == nil || seen[0].body["max_tokens"] != float64(64) {
		t.Fatalf("the runtime decrypted a chat completion: %v", seen)
	}
	// Claude Code names no thinking switch: the edition's thinking is turned on for it, as the proxy did.
	if seen[0].body["thinking"] != true {
		t.Fatalf("the edition's thinking was not turned on: %v", seen[0].body)
	}
	// What Claude Code asks beyond the edition's output limit is capped to it (it asks 32,000 of any model
	// it does not know); the key may come as Anthropic's own header; the priority tier as a header too.
	request, _ = http.NewRequest(http.MethodPost, h.server.URL+anthropicMessagesPath, strings.NewReader(`{"model":"lebrel/test-uncensored","max_tokens":32000,"messages":[{"role":"user","content":"hi there"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", testKey)
	request.Header.Set(editionTierHeader, "priority")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("messages with x-api-key: %d %s", response.StatusCode, raw)
	}
	seen = edge.requests()
	last := seen[len(seen)-1]
	if last.body["max_tokens"] != float64(8192) || last.headers.Get("Authorization") != "Bearer "+testKey || last.headers.Get(editionTierHeader) != "priority" || last.body["service_tier"] != nil {
		t.Fatalf("capped, keyed and prioritised: %v %v", last.body, last.headers)
	}
	// A browser client can read the edition's own headers.
	preflight, _ := http.NewRequest(http.MethodOptions, h.server.URL+anthropicMessagesPath, nil)
	preflight.Header.Set("Origin", "https://app.example")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	response, err = http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	if exposed := response.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(exposed, editionRequestIDHeader) || !strings.Contains(exposed, "X-Lebrel-Encryption") {
		t.Fatalf("exposed headers: %q", exposed)
	}
}

func TestTheEditionsEncryptionDocumentIsCheckedLikeTheClientChecksIt(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	encryption, _ := NewEncryption(bytes.Repeat([]byte{9}, 32), "edge-test")
	signer, _ := NewSigner(bytes.Repeat([]byte{3}, 32), "runtime-test")
	document, err := encryption.SignedMetadata(signer, "lebrel/test-uncensored", now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := verifyEditionMetadata(document, signer.Public, now.Add(time.Minute), "lebrel/test-uncensored")
	if err != nil || key.keyID != encryption.KeyID() {
		t.Fatalf("a good document: %v", err)
	}
	other, _ := NewSigner(bytes.Repeat([]byte{4}, 32), "other")
	if _, err := verifyEditionMetadata(document, other.Public, now, "lebrel/test-uncensored"); err == nil {
		t.Fatal("another signer's key must be refused")
	}
	if _, err := verifyEditionMetadata(document, signer.Public, now, "lebrel/another"); err == nil {
		t.Fatal("another model must be refused")
	}
	if _, err := verifyEditionMetadata(document, signer.Public, now.Add(time.Hour), "lebrel/test-uncensored"); err == nil {
		t.Fatal("an expired document must be refused")
	}
	tampered := bytes.Replace(document, []byte(`"signature":"`), []byte(`"signature":"AA`), 1)
	if _, err := verifyEditionMetadata(tampered, signer.Public, now, "lebrel/test-uncensored"); err == nil {
		t.Fatal("a damaged signature must be refused")
	}
}
