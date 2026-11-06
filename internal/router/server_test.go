package router

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lebrel-router/internal/auth"
	"lebrel-router/internal/ledger"
	"lebrel-router/internal/meter"
	"lebrel-router/internal/model"
	"lebrel-router/internal/settle"
)

const testKey = "leb_live_testkey_0123456789abcdef_0123456789abcdef0123456789abcdef"

type fakeUpstream struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	body    map[string]any
	headers http.Header
}

func (f *fakeUpstream) record(r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{body: body, headers: r.Header.Clone()})
	f.mu.Unlock()
}

func (f *fakeUpstream) last(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("upstream received no request")
	}
	return f.requests[len(f.requests)-1]
}

func newFakeUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		handler(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func okJSON(usage string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"up-1","object":"chat.completion","model":"deepseek-flash","provider":"HostOne","system_fingerprint":"fp_backend_a","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello world"}}],"usage":%s}`, usage)
	}
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher := w.(http.Flusher)
	for _, chunk := range []string{
		`{"id":"s","model":"deepseek-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
		`{"id":"s","model":"deepseek-flash","choices":[{"index":0,"delta":{"content":" world"}}]}`,
		`{"id":"s","model":"deepseek-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"s","model":"deepseek-flash","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

type harness struct {
	server      *httptest.Server
	ledger      *ledger.InMemory
	signer      *Signer
	route       Route
	receipts    *ReceiptStore
	auth        auth.Authenticator
	settlements *settle.Queue
}

// runningQueue delivers to “l“ in the background until the test ends; retries come fast.
func runningQueue(t *testing.T, l settle.Ledger) *settle.Queue {
	t.Helper()
	queue, err := settle.New("", l, settle.Options{MaxBackoff: 20 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go queue.Run(ctx)
	return queue
}

func flushed(t *testing.T, queue *settle.Queue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := queue.Flush(ctx); err != nil {
		t.Fatalf("settlements still owed after 5s: %+v", queue.Stats())
	}
}

func routeFor(upstreams ...Upstream) Route {
	return Route{
		ModelID: "deepseek/deepseek-v4.1-flash", Level: 2, Family: "deepseek-v4.1-flash", ClaimedRevision: "deepseek-flash", Upstreams: upstreams,
		Pricing: PricingConfig{InputMicrousdPerMillionTokens: 157_500, CachedInputMicrousdPerMillionTokens: 3_150, OutputMicrousdPerMillionTokens: 630_000,
			ReservationBPS: 15_000, Version: "router-v1", EffectiveAt: "2026-09-23T00:00:00.000Z", FeeBPS: 500},
		MaxContextTokens: 200_000, MaxOutputTokens: 8_192, DefaultMaxOutputTokens: 512, DataNote: "anonymised; processed by DeepSeek",
	}
}

func newHarness(t *testing.T, route Route, board string) *harness {
	return newHarnessWith(t, route, board, nil)
}

// newHarnessWith lets a test adjust the settings before the server is built.
func newHarnessWith(t *testing.T, route Route, board string, adjust func(*Settings)) *harness {
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
	verification := NewVerification("")
	if board != "" {
		path := filepath.Join(t.TempDir(), "board.json")
		if err := os.WriteFile(path, []byte(board), 0o600); err != nil {
			t.Fatal(err)
		}
		verification = NewVerification(path)
		if err := verification.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	receipts := NewReceiptStore(time.Hour, 100)
	settlements := runningQueue(t, inMemory)
	settings := Settings{Routes: Config{Routes: []Route{route}}, Auth: authenticator, Ledger: inMemory, Signer: signer, Receipts: receipts, Settlements: settlements,
		Verification: verification, BoardURL: "https://models.lebrel.ai/watch", Now: func() time.Time { return time.Unix(1_900_000_000, 0) }}
	if adjust != nil {
		adjust(&settings)
	}
	server, err := New(settings)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	return &harness{server: ts, ledger: inMemory, signer: signer, route: route, receipts: receipts, auth: authenticator, settlements: settlements}
}

func (h *harness) post(t *testing.T, body string, key string) *http.Response {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (h *harness) balance(t *testing.T) int64 {
	t.Helper()
	flushed(t, h.settlements) // the money is owed asynchronously; the balance is read once it is delivered
	balance, err := h.ledger.Balance(context.Background(), "acct_test_000001")
	if err != nil {
		t.Fatal(err)
	}
	return balance
}

func upstream(name, url string, priority int) Upstream {
	return Upstream{Name: name, BaseURL: url + "/v1", Model: "deepseek-flash", APIKeyEnv: "TEST_UPSTREAM_KEY", Priority: priority, UsageFormat: "deepseek",
		Body: map[string]any{"provider": map[string]any{"order": []any{"deepinfra"}, "allow_fallbacks": false}}, DataNote: "test"}
}

const board = `{"version":1,"run_id":"20260923T190000Z","generated_at":1758654000,"battery_id":"abc","entries":[{"target":"deepseek/api","status":"consistent"},{"target":"backup","status":"drift"}]}`

func TestBufferedCompletionBillsSignsAndVerifies(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":8}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), board)
	before := h.balance(t)
	body := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi there"}],"temperature":0}`
	response := h.post(t, body, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"hello world"`) {
		t.Fatalf("body not passed through: %s", raw)
	}
	if got := response.Header.Get(upstreamHeader); got != "deepseek/api" {
		t.Fatalf("upstream header %q", got)
	}
	if got := response.Header.Get(verificationHeader); got != "consistent" {
		t.Fatalf("verification header %q", got)
	}
	if got := response.Header.Get(contentRetentionHeader); got != "none" {
		t.Fatalf("retention header %q", got)
	}
	requestID := response.Header.Get("X-Request-ID")
	if !strings.HasPrefix(requestID, "req_") || len(requestID) != 36 {
		t.Fatalf("request id %q", requestID)
	}
	// the upstream saw the rewritten model, the pinned provider and our key, never the client's key
	seen := up.last(t)
	if seen.body["model"] != "deepseek-flash" || seen.body["provider"] == nil || seen.body["max_tokens"] == nil {
		t.Fatalf("upstream body %v", seen.body)
	}
	if seen.headers.Get("Authorization") != "Bearer sk-upstream-secret" {
		t.Fatalf("upstream auth %q", seen.headers.Get("Authorization"))
	}
	// receipt: header and store agree, signature verifies, digests match what the client sent and received
	encoded := response.Header.Get(receiptHeader)
	if encoded == "" {
		t.Fatal("no receipt header")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var document SignedDocument
	if err := json.Unmarshal(decoded, &document); err != nil {
		t.Fatal(err)
	}
	if !Verify(document, h.signer.Public) {
		t.Fatal("receipt signature does not verify")
	}
	payload := document.Payload
	if payload["prompt_sha256"] != SHA256Hex([]byte(body)) || payload["response_sha256"] != SHA256Hex([]byte("hello world")) {
		t.Fatalf("digests %v", payload)
	}
	if payload["upstream"] != "deepseek/api" || payload["served_model"] != "deepseek-flash" || payload["provider"] != "HostOne" || payload["usage_exact"] != true {
		t.Fatalf("route fields %v", payload)
	}
	verification := payload["verification"].(map[string]any)
	if verification["status"] != "consistent" || verification["run_id"] != "20260923T190000Z" {
		t.Fatalf("verification %v", verification)
	}
	// charge: 8 uncached * 0.1575 + 4 cached * 0.00315 + 3 out * 0.63 microUSD per token -> ceil(1.26 + 0.0126 + 1.89) = 4
	if payload["price_microusd"] != float64(4) || payload["cached_tokens"] != float64(4) {
		t.Fatalf("price %v cached %v", payload["price_microusd"], payload["cached_tokens"])
	}
	if got := before - h.balance(t); got != 4 {
		t.Fatalf("ledger charged %d, want 4", got)
	}
	stored, ok := h.receipts.Get(requestID)
	if !ok || stored.Signature != document.Signature {
		t.Fatal("stored receipt differs from the header")
	}
	// GET /v1/receipts/{id} is public (no key) and returns the same document
	get, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/receipts/"+requestID, nil)
	fetched, err := http.DefaultClient.Do(get)
	if err != nil || fetched.StatusCode != 200 {
		t.Fatalf("fetch receipt: %v %d", err, fetched.StatusCode)
	}
	var fetchedDocument SignedDocument
	_ = json.NewDecoder(fetched.Body).Decode(&fetchedDocument)
	if fetchedDocument.Signature != document.Signature {
		t.Fatal("fetched receipt differs")
	}
	if payload["manifest_sha256"] == "" {
		t.Fatal("receipt has no manifest identity")
	}
}

func TestFailoverSkipsFailingUpstream(t *testing.T) {
	failing := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"error":"overloaded"}`, 503) })
	backup := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", failing.server.URL, 1), upstream("backup", backup.server.URL, 2)), board)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != 200 || response.Header.Get(upstreamHeader) != "backup" || response.Header.Get(verificationHeader) != "drift" {
		t.Fatalf("status %d upstream %q verification %q", response.StatusCode, response.Header.Get(upstreamHeader), response.Header.Get(verificationHeader))
	}
	if len(failing.requests) != 1 || len(backup.requests) != 1 {
		t.Fatalf("calls: failing=%d backup=%d", len(failing.requests), len(backup.requests))
	}
}

func TestAllUpstreamsDownRefundsEverything(t *testing.T) {
	down := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 502) })
	h := newHarness(t, routeFor(upstream("deepseek/api", down.server.URL, 1)), "")
	before := h.balance(t)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != 502 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if h.balance(t) != before {
		t.Fatalf("balance changed by %d", before-h.balance(t))
	}
	if response.Header.Get(upstreamHeader) != "deepseek/api" {
		t.Fatalf("the failing upstream's response is passed through with its name: %q", response.Header.Get(upstreamHeader))
	}
}

func TestClientErrorPassesThroughWithoutCharge(t *testing.T) {
	bad := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad temperature"}}`)
	})
	h := newHarness(t, routeFor(upstream("deepseek/api", bad.server.URL, 1)), "")
	before := h.balance(t)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 400 || !strings.Contains(string(raw), "bad temperature") {
		t.Fatalf("status %d body %s", response.StatusCode, raw)
	}
	if h.balance(t) != before {
		t.Fatal("client error must not charge")
	}
	if response.Header.Get(verificationHeader) != "unknown" {
		t.Fatalf("verification header %q", response.Header.Get(verificationHeader))
	}
}

func TestStreamingPassesLinesAndSettlesExact(t *testing.T) {
	up := newFakeUpstream(t, sseHandler)
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), board)
	before := h.balance(t)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`, testKey)
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(response.Body)
	var lines []string
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 5 || lines[4] != "data: [DONE]" {
		t.Fatalf("stream lines %v", lines)
	}
	seen := up.last(t)
	options, _ := seen.body["stream_options"].(map[string]any)
	if options == nil || options["include_usage"] != true {
		t.Fatalf("include_usage not forced: %v", seen.body)
	}
	requestID := response.Header.Get("X-Request-ID")
	deadline := time.Now().Add(2 * time.Second)
	var document SignedDocument
	var ok bool
	for time.Now().Before(deadline) {
		if document, ok = h.receipts.Get(requestID); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatal("no receipt stored after stream")
	}
	if !Verify(document, h.signer.Public) || document.Payload["streamed"] != true || document.Payload["usage_exact"] != true {
		t.Fatalf("stream receipt %v", document.Payload)
	}
	if document.Payload["response_sha256"] != SHA256Hex([]byte("hello world")) || document.Payload["completion_tokens"] != int64(3) {
		t.Fatalf("stream receipt digests %v", document.Payload)
	}
	// 12 uncached * 0.1575 + 3 * 0.63 = 1.89 + 1.89 -> ceil 4
	if got := before - h.balance(t); got != 4 {
		t.Fatalf("stream charged %d", got)
	}
}

func TestRejectsUnknownModelBadKeysAndOversizedContext(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	cases := []struct {
		name string
		body string
		key  string
		want int
	}{
		{"unknown model", `{"model":"other/model","messages":[]}`, testKey, 404},
		{"no key", `{"model":"deepseek/deepseek-v4.1-flash","messages":[]}`, "", 401},
		{"wrong key", `{"model":"deepseek/deepseek-v4.1-flash","messages":[]}`, "leb_live_wrong", 401},
		{"n must be 1", `{"model":"deepseek/deepseek-v4.1-flash","messages":[],"n":2}`, testKey, 400},
		{"too much output", `{"model":"deepseek/deepseek-v4.1-flash","messages":[],"max_tokens":9000}`, testKey, 400},
		{"context", fmt.Sprintf(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"%s"}]}`, strings.Repeat("a", 900_000)), testKey, 400},
	}
	for _, tc := range cases {
		response := h.post(t, tc.body, tc.key)
		io.Copy(io.Discard, response.Body)
		if response.StatusCode != tc.want {
			t.Errorf("%s: status %d want %d", tc.name, response.StatusCode, tc.want)
		}
	}
	if len(up.requests) != 0 {
		t.Fatalf("rejected requests reached the upstream: %d", len(up.requests))
	}
}

func TestReadyManifestAndModels(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), board)
	get := func(path string) (int, map[string]any) {
		response, err := http.Get(h.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var document map[string]any
		_ = json.NewDecoder(response.Body).Decode(&document)
		return response.StatusCode, document
	}
	status, ready := get("/m/deepseek--deepseek-v4.1-flash/readyz")
	hash, _ := h.route.BillingContractHash()
	if status != 200 || ready["billingContractHash"] != hash {
		t.Fatalf("readyz %d %v", status, ready)
	}
	if status, _ := get("/m/nope/readyz"); status != 404 {
		t.Fatalf("unknown slug %d", status)
	}
	status, manifest := get("/.well-known/proof-of-edition")
	if status != 200 {
		t.Fatalf("manifest %d", status)
	}
	manifests := manifest["manifests"].([]any)
	first := manifests[0].(map[string]any)
	document := SignedDocument{Payload: first["payload"].(map[string]any), Signature: first["signature"].(string)}
	if !Verify(document, h.signer.Public) {
		t.Fatal("manifest signature invalid")
	}
	if document.Payload["kind"] != "route" || document.Payload["billing_contract_hash"] != hash || document.Payload["verification_board"] != "https://models.lebrel.ai/watch" {
		t.Fatalf("manifest payload %v", document.Payload)
	}
	identity, _ := ManifestIdentity(document.Payload)
	// Under /m/{slug}/ the same endpoint answers like a runtime: the bare signed manifest.
	status, scopedManifest := get("/m/deepseek--deepseek-v4.1-flash/.well-known/proof-of-edition")
	if status != 200 {
		t.Fatalf("scoped manifest %d", status)
	}
	scopedPayload, _ := scopedManifest["payload"].(map[string]any)
	scopedSignature, _ := scopedManifest["signature"].(string)
	if scopedPayload == nil || scopedSignature == "" || !Verify(SignedDocument{Payload: scopedPayload, Signature: scopedSignature}, h.signer.Public) {
		t.Fatalf("scoped manifest is not a bare signed document: %v", scopedManifest)
	}
	if scopedIdentity, _ := ManifestIdentity(scopedPayload); scopedIdentity != identity {
		t.Fatal("scoped manifest differs from the listed one")
	}
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	decoded, _ := base64.StdEncoding.DecodeString(response.Header.Get(receiptHeader))
	var receipt SignedDocument
	_ = json.Unmarshal(decoded, &receipt)
	if receipt.Payload["manifest_sha256"] != identity {
		t.Fatalf("receipt references %v, manifest identity %s", receipt.Payload["manifest_sha256"], identity)
	}
	status, models := get("/v1/models")
	data := models["data"].([]any)
	entry := data[0].(map[string]any)
	if status != 200 || entry["id"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("models %v", models)
	}
	lebrel := entry["lebrel"].(map[string]any)
	upstreams := lebrel["upstreams"].([]any)
	if upstreams[0].(map[string]any)["verification"].(map[string]any)["status"] != "consistent" {
		t.Fatalf("models verification %v", upstreams)
	}
}

func TestParseUsageFormats(t *testing.T) {
	deepseek := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}}`)
	usage, ok := ParseUsage("deepseek", deepseek)
	if !ok || usage.CachedTokens != 60 || usage.TotalTokens != 110 {
		t.Fatalf("deepseek usage %v %v", usage, ok)
	}
	if usage, ok := ParseUsage("openai", deepseek); !ok || usage.CachedTokens != 0 {
		t.Fatalf("openai format must ignore deepseek cache fields: %v", usage)
	}
	openai := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":999,"prompt_tokens_details":{"cached_tokens":30}}}`)
	if usage, ok := ParseUsage("openai", openai); !ok || usage.CachedTokens != 30 || usage.TotalTokens != 110 {
		t.Fatalf("openai usage %v", usage)
	}
	if _, ok := ParseUsage("openai", []byte(`{"usage":{"prompt_tokens":1}}`)); ok {
		t.Fatal("incomplete usage must not parse")
	}
	if _, ok := ParseUsage("openai", []byte(`{"usage":{"prompt_tokens":-1,"completion_tokens":1}}`)); ok {
		t.Fatal("negative usage must not parse")
	}
}

func TestCanonicalJSONMatchesPythonSchema(t *testing.T) {
	payload := map[string]any{"b": 1, "a": map[string]any{"z": "é", "y": []any{1, 2}}, "c": "<&>"}
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != `{"a":{"y":[1,2],"z":"é"},"b":1,"c":"<&>"}` {
		t.Fatalf("canonical %s", canonical)
	}
}

func TestConfigValidation(t *testing.T) {
	route := routeFor(Upstream{Name: "x", BaseURL: "http://insecure.example/v1", Model: "m"})
	if err := (Config{Routes: []Route{route}}).Validate(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected https error, got %v", err)
	}
	route = routeFor(Upstream{Name: "x", BaseURL: "https://api.example/v1", Model: "m"})
	route.Pricing.EffectiveAt = "2026-09-23T00:00:00Z"
	if err := (Config{Routes: []Route{route}}).Validate(); err == nil || !strings.Contains(err.Error(), "effective_at") {
		t.Fatalf("expected effective_at error, got %v", err)
	}
	route = routeFor(Upstream{Name: "x", BaseURL: "https://api.example/v1", Model: "m"})
	if err := (Config{Routes: []Route{route, route}}).Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
	if route.Slug() != "deepseek--deepseek-v4.1-flash" {
		t.Fatalf("slug %q", route.Slug())
	}
}

func TestReceiptStoreExpiresAndCaps(t *testing.T) {
	store := NewReceiptStore(time.Minute, 2)
	now := time.Unix(1000, 0)
	store.now = func() time.Time { return now }
	store.Put("a", SignedDocument{Signature: "a"})
	store.Put("b", SignedDocument{Signature: "b"})
	store.Put("c", SignedDocument{Signature: "c"})
	if _, ok := store.Get("a"); ok {
		t.Fatal("cap not enforced")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := store.Get("c"); ok {
		t.Fatal("ttl not enforced")
	}
	_ = model.Usage{}
}

type tierLedger struct {
	*ledger.InMemory
	expectedAmount int64
	reserves       int
}

func (l *tierLedger) Reserve(ctx context.Context, accountID, apiKeyID, modelID, hash, requestID string, amount int64, queue model.QueueClass, estimate, maxOutput int64) (model.Reservation, error) {
	l.reserves++
	if amount != l.expectedAmount {
		return model.Reservation{}, ledger.ErrReservationConflict
	}
	return l.InMemory.Reserve(ctx, accountID, apiKeyID, modelID, hash, requestID, amount, queue, estimate, maxOutput)
}

func TestPeakPricingPassesTheSurchargeThrough(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	route := routeFor(upstream("deepseek/api", up.server.URL, 1))
	route.Pricing.Peak = &PeakPricing{MultiplierPercent: 200, Weekdays: []int{}, WindowsUTC: [][2]string{{"00:00", "24:00"}}}
	h := newHarness(t, route, "")
	before := h.balance(t)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	decoded, _ := base64.StdEncoding.DecodeString(response.Header.Get(receiptHeader))
	var document SignedDocument
	_ = json.Unmarshal(decoded, &document)
	// standard charge would be ceil(1.89 + 1.89) = 4; at 200% it is ceil(7.56) = 8
	if document.Payload["price_microusd"] != float64(8) || document.Payload["pricing_tier"] != "peak" || document.Payload["multiplier_percent"] != float64(200) {
		t.Fatalf("peak receipt %v", document.Payload)
	}
	if got := before - h.balance(t); got != 8 {
		t.Fatalf("charged %d, want 8", got)
	}
	hashWith, _ := route.BillingContractHash()
	plain := routeFor(upstream("deepseek/api", up.server.URL, 1))
	hashWithout, _ := plain.BillingContractHash()
	if hashWith == hashWithout {
		t.Fatal("the schedule is part of the billing contract")
	}
}

func TestPeakScheduleWindowsWeekdaysAndHolidays(t *testing.T) {
	peak := &PeakPricing{MultiplierPercent: 200, Weekdays: []int{1, 2, 3, 4, 5}, WindowsUTC: [][2]string{{"01:00", "04:00"}, {"06:00", "10:00"}}, HolidaysUTC: []string{"2026-10-01"}}
	cases := []struct {
		at   time.Time
		open bool
		why  string
	}{
		{time.Date(2026, 9, 28, 7, 30, 0, 0, time.UTC), true, "Monday 07:30 is inside 06:00-10:00"},
		{time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC), false, "windows are half-open"},
		{time.Date(2026, 9, 28, 0, 59, 0, 0, time.UTC), false, "before the first window"},
		{time.Date(2026, 9, 27, 7, 30, 0, 0, time.UTC), false, "Sunday is not a peak day"},
		{time.Date(2026, 10, 1, 7, 30, 0, 0, time.UTC), false, "a listed holiday is never peak"},
		{time.Date(2026, 9, 28, 9, 59, 59, 0, time.FixedZone("CEST", 2*3600)), true, "07:59:59 UTC counts, zones are converted"},
	}
	for _, tc := range cases {
		if peak.Open(tc.at) != tc.open {
			t.Errorf("%s: open=%t", tc.why, !tc.open)
		}
	}
	route := routeFor(Upstream{Name: "x", BaseURL: "https://api.example/v1", Model: "m"})
	route.Pricing.Peak = &PeakPricing{MultiplierPercent: 200, WindowsUTC: [][2]string{{"09:00", "08:00"}}}
	if err := (Config{Routes: []Route{route}}).Validate(); err == nil {
		t.Fatal("a window that ends before it starts must be rejected")
	}
	route.Pricing.Peak = &PeakPricing{MultiplierPercent: 100, WindowsUTC: [][2]string{{"01:00", "02:00"}}}
	if err := (Config{Routes: []Route{route}}).Validate(); err == nil {
		t.Fatal("a multiplier of 100% is not a surcharge")
	}
}

func TestReserveRetriesOnTheOtherSideOfAPeakBoundary(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	route := routeFor(upstream("deepseek/api", up.server.URL, 1))
	// our clock (Sunday) says standard; the control plane insists on the peak amount
	route.Pricing.Peak = &PeakPricing{MultiplierPercent: 200, Weekdays: []int{1, 2, 3, 4, 5}, WindowsUTC: [][2]string{{"01:00", "04:00"}}}
	h := newHarness(t, route, "")
	info := meter.RequestInfo{EstimatedPromptTokens: 0, MaxOutputTokens: 512}
	_ = info
	wrapped := &tierLedger{InMemory: h.ledger}
	// compute the peak reservation the control plane would expect for this request
	body := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	_, requestInfo, err := meter.InspectAndRequireUsage("/v1/chat/completions", []byte(body), route.DefaultMaxOutputTokens, route.MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	requestInfo.EstimatedPromptTokens = generousEstimate(requestInfo.EstimatedPromptTokens)
	expected, _ := route.MeterPricing().ReservationScaled(requestInfo, model.QueueShort, 200)
	wrapped.expectedAmount = expected
	server, err := New(Settings{Routes: Config{Routes: []Route{route}}, Auth: h.auth, Ledger: wrapped, Signer: h.signer, Receipts: h.receipts,
		Now: func() time.Time { return time.Unix(1_900_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != 200 || wrapped.reserves != 2 {
		t.Fatalf("status %d reserves %d: the router must retry once with the peak amount", response.StatusCode, wrapped.reserves)
	}
	decoded, _ := base64.StdEncoding.DecodeString(response.Header.Get(receiptHeader))
	var document SignedDocument
	_ = json.Unmarshal(decoded, &document)
	if document.Payload["pricing_tier"] != "peak" {
		t.Fatalf("the receipt must carry the tier the control plane priced: %v", document.Payload["pricing_tier"])
	}
}

func TestUpstreamDefaultsApplyOnlyWhenTheClientIsSilent(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`))
	u := upstream("deepseek/api", up.server.URL, 1)
	u.Defaults = map[string]any{"thinking": map[string]any{"type": "disabled"}}
	h := newHarness(t, routeFor(u), "")
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	seen := up.last(t)
	if thinking, _ := seen.body["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Fatalf("default not applied: %v", seen.body)
	}
	response = h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`, testKey)
	io.Copy(io.Discard, response.Body)
	seen = up.last(t)
	if thinking, _ := seen.body["thinking"].(map[string]any); thinking["type"] != "enabled" {
		t.Fatalf("the client's own choice must win over a default: %v", seen.body)
	}
	if seen.body["provider"] == nil {
		t.Fatal("forced body fields still apply")
	}
}

func TestPublicDocumentsAllowBrowsers(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":12}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), board)
	for _, path := range []string{"/.well-known/proof-of-edition", "/m/deepseek--deepseek-v4.1-flash/.well-known/proof-of-edition", "/v1/models", "/v1/receipts/req_missing"} {
		response, err := http.Get(h.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: no CORS header", path)
		}
		preflight, _ := http.NewRequest(http.MethodOptions, h.server.URL+path, nil)
		answer, err := http.DefaultClient.Do(preflight)
		if err != nil {
			t.Fatal(err)
		}
		answer.Body.Close()
		if answer.StatusCode != http.StatusNoContent || answer.Header.Get("Access-Control-Allow-Methods") != "GET, OPTIONS" {
			t.Fatalf("%s: preflight %d", path, answer.StatusCode)
		}
	}
	// Completions stay behind the edge: no cross-origin allowance there.
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("completions must not carry a wildcard CORS header")
	}
}

const fingerprintBoard = `{"version":1,"run_id":"20260924T020444Z","generated_at":1790220000,"entries":[
 {"target":"deepseek/api","status":"divergent","fingerprint":{"verdict":"anchor","run_id":"20260924T140000Z","checked_at":1790220000,
  "reference":{"verdict":"within_margin","median_mean_gap":0.2,"measured_at":1790219000}}},
 {"target":"lebrel/route-flash","status":"consistent","fingerprint_only":true,"route_model_id":"deepseek/deepseek-v4.1-flash",
  "fingerprint":{"verdict":"match","similarity_percent":99.98,"compared":13,"anchor":"deepseek/api","run_id":"20260924T140000Z","checked_at":1790220100,"receipts":{"verified":48,"failed":0,"missing":0}}},
 {"target":"lebrel/route-pro","status":"consistent","fingerprint_only":true,"route_model_id":"deepseek/deepseek-v4-pro",
  "fingerprint":{"verdict":"match","similarity_percent":100.0,"run_id":"20260924T120000Z","checked_at":1790210000,"stale":true}}]}`

func TestRouteFingerprintTravelsInReceiptsAndListing(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":12}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), fingerprintBoard)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	decoded, _ := base64.StdEncoding.DecodeString(response.Header.Get(receiptHeader))
	var receipt SignedDocument
	if err := json.Unmarshal(decoded, &receipt); err != nil {
		t.Fatal(err)
	}
	if !Verify(receipt, h.signer.Public) {
		t.Fatal("receipt signature")
	}
	if receipt.Payload["upstream_response_id"] != "up-1" || receipt.Payload["upstream_system_fingerprint"] != "fp_backend_a" {
		t.Fatalf("the lab's own record of the answer: %v %v", receipt.Payload["upstream_response_id"], receipt.Payload["upstream_system_fingerprint"])
	}
	verification := receipt.Payload["verification"].(map[string]any)
	fp, ok := verification["fingerprint"].(map[string]any)
	if !ok || fp["verdict"] != "match" || fp["similarity_bps"] != float64(9998) || fp["receipts_verified"] != float64(48) || fp["receipts_total"] != float64(48) || fp["compared"] != float64(13) {
		t.Fatalf("fingerprint in the receipt: %v", verification)
	}
	if fp["weights_verdict"] != "within_margin" || fp["weights_checked_at"] != float64(1790219000) {
		t.Fatalf("the anchor's published-weights verdict must reach the route's receipt: %v", fp)
	}
	if verification["status"] != "consistent" {
		t.Fatalf("the route's own row decides its status, not the lab's row (which is divergent here): %v", verification["status"])
	}
	listing, err := http.Get(h.server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Data []struct {
			Lebrel struct {
				Upstreams []struct {
					Verification VerificationStatus `json:"verification"`
				} `json:"upstreams"`
			} `json:"lebrel"`
		} `json:"data"`
	}
	_ = json.NewDecoder(listing.Body).Decode(&models)
	listing.Body.Close()
	if got := models.Data[0].Lebrel.Upstreams[0].Verification.Fingerprint; got == nil || got.SimilarityBPS != 9998 {
		t.Fatalf("listing fingerprint %+v", got)
	}
	// a stale check is not carried
	if status := h.server.Config.Handler.(*Server).settings.Verification.ForRoute("deepseek/api", "deepseek/deepseek-v4-pro"); status.Fingerprint != nil {
		t.Fatal("a stale fingerprint must not speak for the present")
	}
}

func TestOnlySignedBoardsAreApplied(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	path := filepath.Join(dir, "board.json")
	write := func(board string, signWith ed25519.PrivateKey) {
		if err := os.WriteFile(path, []byte(board), 0o600); err != nil {
			t.Fatal(err)
		}
		signature := base64.StdEncoding.EncodeToString(ed25519.Sign(signWith, []byte(board)))
		if err := os.WriteFile(path+".sig", []byte(signature), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	verification := NewVerification(path)
	verification.RequireSignature(public)
	write(fingerprintBoard, private)
	if err := verification.Refresh(context.Background()); err != nil {
		t.Fatalf("a signed board is applied: %v", err)
	}
	if verification.Get("deepseek/api").Status != "divergent" {
		t.Fatal("status not applied")
	}
	if verification.ForRoute("deepseek/api", "deepseek/deepseek-v4.1-flash").Status != "consistent" {
		t.Fatal("the route's own row is applied for the route")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	write(strings.Replace(fingerprintBoard, `"status":"divergent"`, `"status":"consistent"`, 1), other)
	if err := verification.Refresh(context.Background()); err == nil {
		t.Fatal("a board signed by another key must be refused")
	}
	if verification.Get("deepseek/api").Status != "divergent" {
		t.Fatal("a refused board leaves the previous one in force")
	}
	os.Remove(path + ".sig")
	if err := verification.Refresh(context.Background()); err == nil {
		t.Fatal("an unsigned board must be refused when a key is pinned")
	}
}

// outageLedger is the control plane during a rough patch: the first calls of each kind fail as
// unavailable, then it answers; every call is recorded in order.
type outageLedger struct {
	*ledger.InMemory
	mu             sync.Mutex
	acceptFailures int
	settleFailures int
	calls          []string
}

func (l *outageLedger) MarkAccepted(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "accepted")
	if l.acceptFailures > 0 {
		l.acceptFailures--
		return ledger.ErrUnavailable
	}
	return l.InMemory.MarkAccepted(ctx, id)
}

func (l *outageLedger) Settle(ctx context.Context, id string, actual int64, usage model.Usage, outcome model.SettlementOutcome, provider string) (model.Settlement, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, "settle")
	if l.settleFailures > 0 {
		l.settleFailures--
		return model.Settlement{}, ledger.ErrUnavailable
	}
	return l.InMemory.Settle(ctx, id, actual, usage, outcome, provider)
}

func (l *outageLedger) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func TestAnAnsweredRequestIsDeliveredAndSettledThroughAControlPlaneOutage(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	route := routeFor(upstream("deepseek/api", up.server.URL, 1))
	h := newHarness(t, route, "")
	wrapped := &outageLedger{InMemory: h.ledger, acceptFailures: 2, settleFailures: 3}
	queue := runningQueue(t, wrapped)
	server, err := New(Settings{Routes: Config{Routes: []Route{route}}, Auth: h.auth, Ledger: wrapped, Signer: h.signer, Receipts: h.receipts, Settlements: queue,
		Now: func() time.Time { return time.Unix(1_900_000_000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	before, _ := wrapped.Balance(context.Background(), "acct_test_000001")
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+testKey)
	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || response.Header.Get(receiptHeader) == "" {
		t.Fatalf("the answer must reach the customer whatever the control plane does: status %d receipt %q body %s", response.StatusCode, response.Header.Get(receiptHeader), body)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the customer waited %s for bookkeeping", elapsed)
	}
	flushed(t, queue)
	after, _ := wrapped.Balance(context.Background(), "acct_test_000001")
	if before-after != 4 {
		t.Fatalf("charged %d after the outage, want the exact 4", before-after)
	}
	calls := wrapped.seen()
	// the acceptance inside the request fails, so the queue owes it: 1 failed in the request,
	// 1 failed + 1 delivered by the queue, then 3 failed + 1 settled
	if len(calls) != 7 {
		t.Fatalf("calls %v", calls)
	}
	if calls[0] != "accepted" || calls[2] != "accepted" || calls[len(calls)-1] != "settle" {
		t.Fatalf("acceptance must be delivered before the settlement: %v", calls)
	}
	for _, call := range calls[3:] {
		if call != "settle" {
			t.Fatalf("no settlement may be attempted before the acceptance is delivered: %v", calls)
		}
	}
	if stats := queue.Stats(); stats.Delivered != 2 || stats.Retried != 4 || stats.Dead != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestDirectClientsAreAdmittedWithoutTheEdgeAndNeverCappedByAddress(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":8}`))
	body := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi there"}],"temperature":0}`
	// The edge alone, as deployed behind lebrel.ai: a direct call is refused before anything is read.
	closed := newHarnessWith(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), board, func(s *Settings) { s.EdgeProxySecret = "edge-secret" })
	if response := closed.post(t, body, testKey); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without direct clients a call without the edge header must be 401, got %d", response.StatusCode)
	}
	// The attested endpoint: direct clients admitted; many calls from one address are just calls (limits are by key).
	h := newHarnessWith(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), board, func(s *Settings) {
		s.EdgeProxySecret = "edge-secret"
		s.DirectClients = true
	})
	for i := 0; i < 12; i++ {
		response := h.post(t, body, testKey)
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("direct call %d: status %d: %s", i+1, response.StatusCode, raw)
		}
		if response.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal("direct completions carry CORS for browsers")
		}
	}
	// The platform's edge keeps working through the same server.
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testKey)
	request.Header.Set(edgeAuthorizationHeader, "v1="+DeriveEdgeProxyToken("edge-secret"))
	viaEdge, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if viaEdge.StatusCode != 200 {
		t.Fatalf("the edge's own call: %d", viaEdge.StatusCode)
	}
	// A browser's preflight.
	preflight, _ := http.NewRequest(http.MethodOptions, h.server.URL+"/v1/chat/completions", nil)
	response, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Access-Control-Allow-Headers") == "" {
		t.Fatalf("preflight: %d", response.StatusCode)
	}
	// A wrong key is still a wrong key.
	if response := h.post(t, body, "leb_live_wrong"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong key is refused: %d", response.StatusCode)
	}
}
