package router

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteRouteFixture writes a signed route manifest and receipt for the Python
// verifier's cross-language test. Run with ROUTE_FIXTURE_DIR=<dir> go test ./internal/router -run WriteRouteFixture.
func TestWriteRouteFixture(t *testing.T) {
	dir := os.Getenv("ROUTE_FIXTURE_DIR")
	if dir == "" {
		t.Skip("ROUTE_FIXTURE_DIR not set")
	}
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = 7
	}
	signer, err := NewSigner(seed, "router-fixture")
	if err != nil {
		t.Fatal(err)
	}
	route := routeFor(Upstream{Name: "deepseek/api", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-flash", UsageFormat: "deepseek", DataNote: "the lab's own API"})
	now := time.Unix(1_900_000_000, 0)
	manifest, identity, err := signer.RouteManifest(route, "https://models.lebrel.ai/watch", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	prompt := []byte(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi there"}],"temperature":0}`)
	response := []byte("hello world")
	receipt, err := signer.RouteReceipt(ReceiptInput{
		RequestID: "req_0123456789abcdef0123456789abcdef", ModelID: route.ModelID, ManifestIdentity: identity, Upstream: "deepseek/api", UpstreamModel: "deepseek-flash",
		ServedModel: "deepseek-flash", Provider: "", Verification: VerificationStatus{Status: "consistent", RunID: "20260923T190000Z", CheckedAt: 1758654000},
		PromptSHA256: SHA256Hex(prompt), ResponseSHA256: SHA256Hex(response), PromptTokens: 12, CompletionTokens: 3, CachedTokens: 4, UsageExact: true,
		PriceMicrousd: 4, FeeBPS: 500, Streamed: false,
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) {
		var data []byte
		switch typed := value.(type) {
		case []byte:
			data = typed
		case string:
			data = []byte(typed)
		default:
			data, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", manifest)
	write("receipt.json", receipt)
	write("public_key.txt", hex.EncodeToString(signer.Public))
	write("request.json", prompt)
	write("response.txt", response)
}
