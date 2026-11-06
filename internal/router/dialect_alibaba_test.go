package router

import (
	"reflect"
	"testing"
)

func TestAlibabaDialectMapsEveryReasoningSwitchOntoEnableThinking(t *testing.T) {
	cases := []struct {
		name    string
		in      map[string]any
		fixed   bool
		wantKey bool
		want    bool
	}{
		{"deepseek thinking disabled", map[string]any{"thinking": map[string]any{"type": "disabled"}}, false, true, false},
		{"deepseek thinking enabled", map[string]any{"thinking": map[string]any{"type": "enabled"}}, false, true, true},
		{"openrouter reasoning off", map[string]any{"reasoning": map[string]any{"enabled": false}}, false, true, false},
		{"alibaba's own switch wins", map[string]any{"enable_thinking": true, "thinking": map[string]any{"type": "disabled"}}, false, true, true},
		{"effort alone: the host's default", map[string]any{"reasoning_effort": "low"}, false, false, false},
		{"a host that always reasons gets no switch", map[string]any{"thinking": map[string]any{"type": "disabled"}}, true, false, false},
	}
	for _, c := range cases {
		payload := map[string]any{"model": "x", "messages": []any{}}
		for k, v := range c.in {
			payload[k] = v
		}
		translateDialect(payload, "alibaba", c.fixed)
		for _, gone := range []string{"thinking", "reasoning", "reasoning_effort"} {
			if _, present := payload[gone]; present {
				t.Fatalf("%s: %s must be dropped", c.name, gone)
			}
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
