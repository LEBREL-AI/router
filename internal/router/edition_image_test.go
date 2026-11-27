package router

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tinfoilsh/encrypted-http-body-protocol/protocol"
)

func postImage(t *testing.T, h *harness, body, key string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+imageGenerationPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestAnImageIsAskedForThroughTheRouterEncryptedToItsRuntime(t *testing.T) {
	now := func() time.Time { return time.Unix(1_900_000_000, 0) }
	edge := newFakeEdge(t, "lebrel/test-uncensored", now)
	h := newHarnessWith(t, editionRoute(edge), "", func(s *Settings) { s.EdgeProxySecret = "edge-secret"; s.DirectClients = true })
	const asSent = `{"model":"lebrel/chroma1-hd","prompt":"Photograph of a lighthouse at dawn","size":"1536x1024"}`
	response := postImage(t, h, "  "+asSent+"\n", testKey, map[string]string{"Idempotency-Key": "my-image-1"})
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(raw), `"b64_json":"QUJDRA=="`) || !strings.Contains(string(raw), `"model":"lebrel/chroma1-hd"`) {
		t.Fatalf("image answer: %d %s", response.StatusCode, raw)
	}
	if response.Header.Get(receiptHeader) == "" || response.Header.Get("X-Lebrel-Encryption") != "router-to-runtime" ||
		response.Header.Get("X-Request-ID") != "img_hold_0001" || response.Header.Get(editionRequestIDHeader) != "img_hold_0001" {
		t.Fatalf("answer headers: %v", response.Header)
	}
	if response.Header.Get(protocol.ResponseNonceHeader) != "" {
		t.Fatal("the encryption ends in the router: no nonce reaches the client")
	}
	seen := edge.requests()
	if len(seen) != 1 {
		t.Fatalf("the edge saw %d requests", len(seen))
	}
	sent := seen[0]
	// The runtime opens the client's own bytes; the edge is told a digest of them and the request's name, and gets the key.
	digest := sha256.Sum256([]byte(asSent))
	if string(sent.raw) != asSent || sent.headers.Get(imageAskedHeader) != hex.EncodeToString(digest[:]) ||
		sent.headers.Get("Idempotency-Key") != "my-image-1" || sent.headers.Get("Authorization") != "Bearer "+testKey {
		t.Fatalf("the runtime decrypted %q; the edge's headers: %v", sent.raw, sent.headers)
	}
	// The receipt is fetched by the id the answer carried, like an answer's.
	receipt, err := http.Get(h.server.URL + "/v1/receipts/img_hold_0001")
	if err != nil {
		t.Fatal(err)
	}
	document, _ := io.ReadAll(receipt.Body)
	if receipt.StatusCode != 200 || !strings.Contains(string(document), `"request_id":"img_hold_0001"`) {
		t.Fatalf("receipt: %d %s", receipt.StatusCode, document)
	}
	// And the manifest the receipt names: the image model's, signed by the runtime that drew, a public document.
	manifest, err := http.Get(h.server.URL + imageManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	published, _ := io.ReadAll(manifest.Body)
	if manifest.StatusCode != 200 || !strings.Contains(string(published), `"id":"lebrel/chroma1-hd"`) || manifest.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("image manifest: %d %s", manifest.StatusCode, published)
	}
}

func TestAnImageRequestIsRefusedBeforeAnythingIsEncryptedOrRelayedAsTheEdgeRefusesIt(t *testing.T) {
	now := func() time.Time { return time.Unix(1_900_000_000, 0) }
	edge := newFakeEdge(t, "lebrel/test-uncensored", now)
	h := newHarnessWith(t, editionRoute(edge), "", func(s *Settings) { s.EdgeProxySecret = "edge-secret"; s.DirectClients = true })
	for name, c := range map[string]struct {
		body, key string
		status    int
		code      string
	}{
		"a wrong key":         {`{"prompt":"x"}`, "leb_live_wrong_0123456789abcdef_0123456789abcdef0123456789abcdef", 401, "invalid_api_key"},
		"no key":              {`{"prompt":"x"}`, "", 401, "invalid_api_key"},
		"a body that is none": {`[1,2]`, testKey, 400, "invalid_request_error"},
		"an empty body":       {``, testKey, 400, "invalid_request_error"},
		"too large a request": {`{"prompt":"` + strings.Repeat("a", imageRequestMax) + `"}`, testKey, 413, "request_too_large"},
	} {
		response := postImage(t, h, c.body, c.key, nil)
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != c.status || !strings.Contains(string(raw), `"code":"`+c.code+`"`) {
			t.Errorf("%s: %d %s", name, response.StatusCode, raw)
		}
	}
	if len(edge.requests()) != 0 {
		t.Fatalf("the edge was reached %d times", len(edge.requests()))
	}
	if response, err := http.Get(h.server.URL + imageGenerationPath); err != nil || response.StatusCode != 405 {
		t.Fatalf("GET: %v %v", err, response)
	}
	// The edge's own refusal (it charged, and the runtime refused to draw; or images are busy) reaches the client by status and code.
	edge.mu.Lock()
	edge.refuseOnce, edge.refuseBody = 429, `{"error":{"code":"image_busy","message":"Lebrel is drawing as many images as it can right now. Try again in a moment"}}`
	edge.mu.Unlock()
	busy := postImage(t, h, `{"prompt":"x"}`, testKey, nil)
	raw, _ := io.ReadAll(busy.Body)
	if busy.StatusCode != 429 || !strings.Contains(string(raw), `"code":"image_busy"`) || busy.Header.Get("Retry-After") != "2" || busy.Header.Get("X-Request-ID") != "img_hold_0001" {
		t.Fatalf("busy: %d %s %v", busy.StatusCode, raw, busy.Header)
	}
	// A key the runtime no longer takes: the router fetches the key again and asks once more, by itself.
	before := len(edge.requests())
	edge.mu.Lock()
	edge.refuseOnce, edge.refuseBody = 422, `{"error":{"code":"encryption_key_rotated","message":"fetch the key again"}}`
	hits := edge.metadataHits
	edge.mu.Unlock()
	again := postImage(t, h, `{"prompt":"x"}`, testKey, nil)
	_, _ = io.ReadAll(again.Body)
	edge.mu.Lock()
	refetched := edge.metadataHits > hits
	edge.mu.Unlock()
	if again.StatusCode != 200 || !refetched || len(edge.requests()) != before+1 {
		t.Fatalf("after a rotated key: %d refetched=%t", again.StatusCode, refetched)
	}
}

func TestARouterWithoutAnEditionServesNoImages(t *testing.T) {
	h := newHarnessWith(t, routeFor(upstream("deepseek/api", "http://127.0.0.1:1", 1)), "", func(s *Settings) { s.EdgeProxySecret = "edge-secret"; s.DirectClients = true })
	response := postImage(t, h, `{"prompt":"x"}`, testKey, nil)
	if response.StatusCode != 404 {
		t.Fatalf("status %d", response.StatusCode)
	}
}
