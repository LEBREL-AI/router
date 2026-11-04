package router

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func openRouterUpstream(name, url string, priority int) Upstream {
	u := upstream(name, url, priority)
	u.Model = "deepseek/deepseek-v4.1-flash-1017"
	u.UsageFormat = "openai"
	u.Dialect = "openrouter"
	u.Body = map[string]any{"provider": map[string]any{"order": []any{"deepseek"}, "allow_fallbacks": false}}
	return u
}

func bodyFor(t *testing.T, client string, u Upstream) map[string]any {
	t.Helper()
	raw, err := upstreamBody([]byte(client), &u)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func reasoningEnabled(t *testing.T, payload map[string]any) (bool, bool) {
	t.Helper()
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		return false, false
	}
	enabled, ok := reasoning["enabled"].(bool)
	return enabled, ok
}

// A client of a DeepSeek route writes what DeepSeek documents; through OpenRouter the switch
// arrives as OpenRouter's own, and the lab-native key never reaches the relay.
func TestOpenRouterDialectMapsTheLabsReasoningSwitchesOntoReasoning(t *testing.T) {
	u := openRouterUpstream("openrouter/deepseek", "https://openrouter.ai/api", 1)
	cases := []struct {
		name    string
		client  string
		enabled bool
		mapped  bool
	}{
		{"deepseek thinking disabled", `{"model":"deepseek/deepseek-v4.1-flash","messages":[],"thinking":{"type":"disabled"}}`, false, true},
		{"deepseek thinking enabled", `{"model":"deepseek/deepseek-v4.1-flash","messages":[],"thinking":{"type":"enabled"}}`, true, true},
		{"alibaba enable_thinking false", `{"model":"qwen/qwen3.8-max","messages":[],"enable_thinking":false}`, false, true},
		{"alibaba enable_thinking true", `{"model":"qwen/qwen3.8-max","messages":[],"enable_thinking":true}`, true, true},
		{"no switch at all", `{"model":"deepseek/deepseek-v4.1-flash","messages":[]}`, false, false},
		{"an unknown thinking type", `{"model":"deepseek/deepseek-v4.1-flash","messages":[],"thinking":{"type":"auto"}}`, false, false},
	}
	for _, c := range cases {
		payload := bodyFor(t, c.client, u)
		if _, present := payload["thinking"]; present {
			t.Fatalf("%s: thinking reached the relay: %v", c.name, payload)
		}
		if _, present := payload["enable_thinking"]; present {
			t.Fatalf("%s: enable_thinking reached the relay: %v", c.name, payload)
		}
		enabled, mapped := reasoningEnabled(t, payload)
		if mapped != c.mapped || (mapped && enabled != c.enabled) {
			t.Fatalf("%s: reasoning mapped=%v enabled=%v, want mapped=%v enabled=%v (%v)", c.name, mapped, enabled, c.mapped, c.enabled, payload)
		}
		if payload["model"] != "deepseek/deepseek-v4.1-flash-1017" || payload["provider"] == nil {
			t.Fatalf("%s: model or provider pin lost: %v", c.name, payload)
		}
	}
}

// A client that already speaks OpenRouter's dialect is not second-guessed: its reasoning object
// stays, the lab-native key still goes.
func TestOpenRouterDialectKeepsTheClientsOwnReasoning(t *testing.T) {
	u := openRouterUpstream("openrouter/deepseek", "https://openrouter.ai/api", 1)
	payload := bodyFor(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[],"thinking":{"type":"disabled"},"reasoning":{"effort":"low"}}`, u)
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" || reasoning["enabled"] != nil {
		t.Fatalf("client's reasoning rewritten: %v", payload)
	}
	if _, present := payload["thinking"]; present {
		t.Fatalf("thinking reached the relay: %v", payload)
	}
}

// The labs' own APIs keep receiving the client's fields untouched.
func TestPlainDialectForwardsTheLabsFieldsAsTheyAre(t *testing.T) {
	u := upstream("deepseek/api", "https://api.deepseek.com", 1)
	payload := bodyFor(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[],"thinking":{"type":"disabled"}}`, u)
	thinking, _ := payload["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking rewritten for the lab's own API: %v", payload)
	}
	if _, present := payload["reasoning"]; present {
		t.Fatalf("reasoning invented for the lab's own API: %v", payload)
	}
}

func TestUnknownDialectIsRefused(t *testing.T) {
	u := upstream("deepseek/api", "https://api.deepseek.com", 1)
	u.Dialect = "anthropic"
	config := Config{Routes: []Route{routeFor(u)}}
	if err := config.Validate(); err == nil {
		t.Fatal("an unknown dialect was accepted")
	}
	u.Dialect = "openrouter"
	config = Config{Routes: []Route{routeFor(u)}}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}

// OpenRouter refusing Lebrel's credential or credit, or finding no endpoint for the model Lebrel
// named (a regional filter on the account, a model retired), is an outage of that upstream for the
// customer: the request goes to the lab's own API behind it, and the receipt says so.
func TestCredentialOrCreditRefusalFailsOverToTheNextUpstream(t *testing.T) {
	for _, status := range []int{401, 402, 403} {
		refusing := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"insufficient credits"}}`, status)
		})
		lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
		h := newHarness(t, routeFor(openRouterUpstream("openrouter/deepseek", refusing.server.URL, 1), upstream("deepseek/api", lab.server.URL, 2)), board)
		response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
		io.Copy(io.Discard, response.Body)
		if response.StatusCode != 200 || response.Header.Get(upstreamHeader) != "deepseek/api" {
			t.Fatalf("%d: status %d upstream %q", status, response.StatusCode, response.Header.Get(upstreamHeader))
		}
		if len(refusing.requests) != 1 || len(lab.requests) != 1 {
			t.Fatalf("%d: calls: refusing=%d lab=%d", status, len(refusing.requests), len(lab.requests))
		}
	}
}

// A 4xx that is the client's own (a bad request the lab refused) is still passed through, never
// retried on another upstream.
func TestAClientsBadRequestIsNotRetriedElsewhere(t *testing.T) {
	refusing := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad request"}}`, 400)
	})
	lab := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	h := newHarness(t, routeFor(openRouterUpstream("openrouter/deepseek", refusing.server.URL, 1), upstream("deepseek/api", lab.server.URL, 2)), board)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != 400 || len(lab.requests) != 0 {
		t.Fatalf("status %d, lab calls %d", response.StatusCode, len(lab.requests))
	}
}

// With no upstream left, a refusal of Lebrel's credential or credit is told as Lebrel's outage, in
// Lebrel's words, and nothing is charged: the relay's "insufficient credits" never reaches a customer
// as if it were theirs.
func TestCredentialRefusalWithNoUpstreamLeftIsLebrelsOutage(t *testing.T) {
	refusing := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"insufficient credits"}}`, 402)
	})
	h := newHarness(t, routeFor(openRouterUpstream("openrouter/alibaba", refusing.server.URL, 1)), board)
	before := h.balance(t)
	response := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, testKey)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 502 || !strings.Contains(string(raw), "upstream_unavailable") || strings.Contains(string(raw), "insufficient credits") {
		t.Fatalf("status %d body %s", response.StatusCode, raw)
	}
	if after := h.balance(t); after != before {
		t.Fatalf("charged %d for a refused request", before-after)
	}
}

// An endpoint that cannot reason less than always (Alibaba's qwen3.8-max) turns a client's "off"
// into the lowest effort, which is what the endpoint offers; "on" stays on.
func TestAFixedReasoningEndpointTurnsOffIntoTheLowestEffort(t *testing.T) {
	u := openRouterUpstream("openrouter/alibaba", "https://openrouter.ai/api", 1)
	u.Model = "qwen/qwen3.8-max-0902"
	u.ReasoningFixed = true
	off := bodyFor(t, `{"model":"qwen/qwen3.8-max","messages":[],"enable_thinking":false}`, u)
	reasoning, _ := off["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" || reasoning["enabled"] != nil {
		t.Fatalf("off not turned into the lowest effort: %v", off)
	}
	on := bodyFor(t, `{"model":"qwen/qwen3.8-max","messages":[],"enable_thinking":true}`, u)
	if enabled, mapped := reasoningEnabled(t, on); !mapped || !enabled {
		t.Fatalf("on rewritten: %v", on)
	}
	silent := bodyFor(t, `{"model":"qwen/qwen3.8-max","messages":[]}`, u)
	if _, present := silent["reasoning"]; present {
		t.Fatalf("reasoning invented for a client that said nothing: %v", silent)
	}
}
