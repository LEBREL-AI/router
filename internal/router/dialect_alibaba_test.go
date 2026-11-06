package router

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAlibabaDialectMapsEveryReasoningSwitchOntoEnableThinking(t *testing.T) {
	disabled := map[string]any{"thinking": map[string]any{"type": "disabled"}}
	cases := []struct {
		name    string
		in      map[string]any
		fixed   bool
		efforts []string
		wantKey bool
		want    bool
		effort  string // the reasoning_effort forwarded, "" for none
	}{
		{"deepseek thinking disabled", disabled, false, nil, true, false, ""},
		{"deepseek thinking enabled", map[string]any{"thinking": map[string]any{"type": "enabled"}}, false, nil, true, true, ""},
		{"openrouter reasoning off", map[string]any{"reasoning": map[string]any{"enabled": false}}, false, nil, true, false, ""},
		{"alibaba's own switch wins", map[string]any{"enable_thinking": true, "thinking": map[string]any{"type": "disabled"}}, false, nil, true, true, ""},
		{"effort alone goes through", map[string]any{"reasoning_effort": "low"}, false, nil, false, false, "low"},
		{"openrouter's reasoning.effort is the same ask", map[string]any{"reasoning": map[string]any{"effort": "low"}}, false, nil, false, false, "low"},
		{"reasoning_effort wins over reasoning.effort", map[string]any{"reasoning": map[string]any{"effort": "high", "enabled": true}, "reasoning_effort": "low"}, false, nil, true, true, "low"},
		{"an effort the endpoint does not take is dropped, not refused", map[string]any{"reasoning_effort": "high"}, false, []string{"max"}, false, false, ""},
		{"an effort the endpoint takes goes through with the switch", map[string]any{"reasoning_effort": "max", "enable_thinking": true}, false, []string{"max"}, true, true, "max"},
		{"an effort that is not a word is dropped", map[string]any{"reasoning_effort": 3}, false, nil, false, false, ""},
		{"a host that always reasons gets no switch", disabled, true, nil, false, false, ""},
		{"a host that always reasons gives its lowest effort to a client asking for none", disabled, true, []string{"low", "high", "max"}, false, false, "low"},
		{"unless the client named the effort", map[string]any{"thinking": map[string]any{"type": "disabled"}, "reasoning_effort": "high"}, true, []string{"low", "high", "max"}, false, false, "high"},
	}
	for _, c := range cases {
		payload := map[string]any{"model": "x", "messages": []any{}}
		for k, v := range c.in {
			payload[k] = v
		}
		translateDialect(payload, "alibaba", c.fixed, c.efforts)
		for _, gone := range []string{"thinking", "reasoning"} {
			if _, present := payload[gone]; present {
				t.Fatalf("%s: %s must be dropped", c.name, gone)
			}
		}
		if effort, _ := payload["reasoning_effort"].(string); effort != c.effort {
			t.Fatalf("%s: reasoning_effort %q, want %q", c.name, effort, c.effort)
		}
		value, present := payload["enable_thinking"]
		if present != c.wantKey || (present && value != c.want) {
			t.Fatalf("%s: enable_thinking present=%v value=%v, want present=%v value=%v", c.name, present, value, c.wantKey, c.want)
		}
		if !reflect.DeepEqual(payload["messages"], []any{}) {
			t.Fatalf("%s: the rest of the payload is untouched", c.name)
		}
	}
}

func TestAnUpstreamMayTakeItsBaseURLFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_BASE_URL", " https://ws-123.example.test/compatible-mode/v1 ")
	u := Upstream{BaseURL: "https://fallback.example.test/v1", BaseURLEnv: "TEST_BASE_URL"}
	if got := u.Endpoint(); got != "https://ws-123.example.test/compatible-mode/v1" {
		t.Fatalf("endpoint from the environment: %q", got)
	}
	t.Setenv("TEST_BASE_URL", "")
	if got := u.Endpoint(); got != "https://fallback.example.test/v1" {
		t.Fatalf("empty variable falls back to base_url: %q", got)
	}
}

func TestAnEndpointFromTheEnvironmentStaysWithinTheServiceAndTheManifestNamesThePublicOne(t *testing.T) {
	route := routeFor(Upstream{Name: "alibaba/model-studio", BaseURL: "https://fallback.example.test/v1", BaseURLEnv: "TEST_BASE_URL", Model: "m", Priority: 1})
	for _, bad := range []string{
		"http://ws-123.example.test/compatible-mode/v1",
		"https://ws-123.other.test/compatible-mode/v1",
		"https://user:secret@ws-123.example.test/compatible-mode/v1",
		"https://ws-123.example.test/compatible-mode/v1?workspace=1",
	} {
		t.Setenv("TEST_BASE_URL", bad)
		if err := (Config{Routes: []Route{route}}).Validate(); err == nil || !strings.Contains(err.Error(), "TEST_BASE_URL") {
			t.Fatalf("%q must be refused, got %v", bad, err)
		}
	}
	t.Setenv("TEST_BASE_URL", "https://ws-123.example.test/compatible-mode/v1")
	if err := (Config{Routes: []Route{route}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if got := route.Upstreams[0].Endpoint(); got != "https://ws-123.example.test/compatible-mode/v1" {
		t.Fatalf("requests go to the environment's endpoint, got %q", got)
	}
	signer, err := NewSigner(bytes.Repeat([]byte{7}, 32), "router-test")
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := signer.RouteManifest(route, "https://models.lebrel.ai/watch", time.Unix(1_900_000_000, 0), time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	upstreams, _ := document.Payload["upstreams"].([]any)
	if len(upstreams) != 1 {
		t.Fatalf("upstreams in the manifest: %v", document.Payload["upstreams"])
	}
	if got := upstreams[0].(map[string]any)["base_url"]; got != "https://fallback.example.test/v1" {
		t.Fatalf("the public manifest names the service's public endpoint, never the account's form, got %v", got)
	}
}
