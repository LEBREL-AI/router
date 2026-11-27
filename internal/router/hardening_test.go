package router

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// What a request may carry upstream, and the cap every host is given (27 November 2026).

func postJSON(t *testing.T, h *harness, payload map[string]any) (*http.Response, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response := h.post(t, string(body), testKey)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response, string(raw)
}

func chat(fields map[string]any) map[string]any {
	payload := map[string]any{"model": "deepseek/deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for key, value := range fields {
		payload[key] = value
	}
	return payload
}

func TestTheOutputCapGoesUpstreamUnderBothOfItsNames(t *testing.T) {
	usage := `{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`
	for name, test := range map[string]struct {
		fields map[string]any
		want   float64
	}{
		"the client names neither":                {fields: nil, want: 512},
		"the client writes max_tokens":            {fields: map[string]any{"max_tokens": 40}, want: 40},
		"the client writes max_completion_tokens": {fields: map[string]any{"max_completion_tokens": 40}, want: 40},
	} {
		up := newFakeUpstream(t, okJSON(usage))
		h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
		if response, raw := postJSON(t, h, chat(test.fields)); response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, response.StatusCode, raw)
		}
		seen := up.last(t).body
		if seen["max_tokens"] != test.want || seen["max_completion_tokens"] != test.want {
			t.Errorf("%s: the host was sent max_tokens %v and max_completion_tokens %v, want %v under both", name, seen["max_tokens"], seen["max_completion_tokens"], test.want)
		}
	}
	// both at once stay a client's mistake, as before
	h := newHarness(t, routeFor(upstream("deepseek/api", newFakeUpstream(t, okJSON(usage)).server.URL, 1)), "")
	if response, _ := postJSON(t, h, chat(map[string]any{"max_tokens": 40, "max_completion_tokens": 40})); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("both names from a client: status %d", response.StatusCode)
	}
	// an endpoint measured, or documented, to read one name is sent that one
	for _, field := range capFieldNames {
		up := newFakeUpstream(t, okJSON(usage))
		one := upstream("deepseek/api", up.server.URL, 1)
		one.CapFields = []string{field}
		h := newHarness(t, routeFor(one), "")
		if response, raw := postJSON(t, h, chat(map[string]any{"max_completion_tokens": 40})); response.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", response.StatusCode, raw)
		}
		seen := up.last(t).body
		for _, name := range capFieldNames {
			if _, sent := seen[name]; sent != (name == field) {
				t.Errorf("an endpoint that reads %s was sent %v", field, seen)
			}
		}
		if seen[field] != float64(40) {
			t.Errorf("an endpoint that reads %s was sent %v", field, seen[field])
		}
	}
	for _, wrong := range [][]string{{"max_output_tokens"}, {"max_tokens", "max_tokens"}, {"max_tokens", "max_completion_tokens", "max_tokens"}} {
		config := Config{Routes: []Route{routeFor(Upstream{Name: "a", BaseURL: "https://example.com/v1", Model: "m", APIKeyEnv: "TEST_UPSTREAM_KEY", Priority: 1, CapFields: wrong})}}
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "cap_fields") {
			t.Errorf("cap_fields %v accepted: %v", wrong, err)
		}
	}
}

func TestProductionRoutesGiveEveryUpstreamACap(t *testing.T) {
	config, err := LoadConfig("../../routes.production.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range config.Routes {
		for _, up := range route.Upstreams {
			// DeepSeek's own API could not be measured with both names (27 November 2026) and is sent the one it documents
			wantOne := strings.HasPrefix(up.Name, "deepseek/api")
			if (len(up.CapFields) == 1 && up.CapFields[0] == "max_tokens") != wantOne || len(up.CapFields) > 1 {
				t.Errorf("%s on %s: cap_fields %v", up.Name, route.ModelID, up.CapFields)
			}
		}
	}
}

func TestOnlyListedFieldsGoUpstream(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	sent := map[string]any{
		"temperature": 0.2, "top_p": 0.9, "top_k": 40, "seed": 7, "stop": []any{"END"}, "reasoning_effort": "high", "enable_thinking": true, "thinking_budget": 2048,
		"tool_choice": "auto", "parallel_tool_calls": false, "response_format": map[string]any{"type": "json_object"}, "user": "u-1", "logprobs": true, "top_logprobs": 2,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
		// what is not forwarded: a relay's routing, a tier, picture options, what libraries add, a made-up field
		"models": []any{"another/model"}, "route": "fallback", "transforms": []any{"middle-out"}, "service_tier": "priority", "vl_high_resolution_images": true,
		"metadata": map[string]any{"a": "b"}, "enable_search": false, "notes for the model": "read this too", "zzz_made_up": 1,
	}
	response, raw := postJSON(t, h, chat(sent))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, raw)
	}
	seen := up.last(t).body
	for _, name := range []string{"temperature", "top_p", "top_k", "seed", "stop", "reasoning_effort", "enable_thinking", "thinking_budget", "tool_choice", "parallel_tool_calls", "response_format", "user", "logprobs", "top_logprobs", "tools", "messages", "max_tokens", "max_completion_tokens"} {
		if _, ok := seen[name]; !ok {
			t.Errorf("%s did not reach the host", name)
		}
	}
	for _, name := range []string{"models", "route", "transforms", "service_tier", "vl_high_resolution_images", "metadata", "enable_search", "notes for the model", "zzz_made_up"} {
		if _, ok := seen[name]; ok {
			t.Errorf("%s reached the host", name)
		}
	}
	// the provider behind a route stays the route's
	if provider, _ := seen["provider"].(map[string]any); provider == nil || provider["allow_fallbacks"] != false {
		t.Errorf("the route's own provider pin: %v", seen["provider"])
	}
	// the answer names what was left out, by plain names only; the operator's count keeps no made-up name
	header := response.Header.Get(ignoredFieldsHeader)
	if header != "enable_search, metadata, models, route, service_tier, transforms, vl_high_resolution_images, zzz_made_up, and 1 more" {
		t.Errorf("ignored fields header %q", header)
	}
	counts := h.router.leftOut.snapshot()
	if counts["models"] != 1 || counts["service_tier"] != 1 || counts["(other)"] != 2 || counts["zzz_made_up"] != 0 || len(counts) != 8 {
		t.Errorf("counts %v", counts)
	}
	// a request of listed fields alone says nothing
	if response, _ := postJSON(t, h, chat(map[string]any{"temperature": 0})); response.Header.Get(ignoredFieldsHeader) != "" {
		t.Errorf("header on a plain request: %q", response.Header.Get(ignoredFieldsHeader))
	}
}

func TestWhatIsNotOfferedIsRefusedWhenAskedFor(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	h := newHarness(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "")
	before := h.balance(t)
	picture := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}}
	refused := map[string]map[string]any{
		"web search, Alibaba's switch":      chat(map[string]any{"enable_search": true}),
		"web search, its options":           chat(map[string]any{"search_options": map[string]any{"forced_search": true}}),
		"web search, OpenAI's options":      chat(map[string]any{"web_search_options": map[string]any{"search_context_size": "high"}}),
		"plugins":                           chat(map[string]any{"plugins": []any{map[string]any{"id": "web"}}}),
		"a code interpreter":                chat(map[string]any{"enable_code_interpreter": true}),
		"audio output":                      chat(map[string]any{"modalities": []any{"text", "audio"}, "audio": map[string]any{"voice": "a"}}),
		"audio as a modality alone":         chat(map[string]any{"modalities": []any{"audio"}}),
		"predicted outputs":                 chat(map[string]any{"prediction": map[string]any{"type": "content", "content": "x"}}),
		"a hosted tool":                     chat(map[string]any{"tools": []any{map[string]any{"type": "builtin_function", "function": map[string]any{"name": "$web_search"}}}}),
		"a web search tool":                 chat(map[string]any{"tools": []any{map[string]any{"type": "web_search"}}}),
		"a video":                           chat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/a.mp4"}}}}}}),
		"audio input":                       chat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "AAAA", "format": "wav"}}}}}}),
		"a file":                            chat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "file", "file": map[string]any{"file_data": "data:application/pdf;base64,AAAA"}}}}}}),
		"a part with no type":               chat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": "hi"}}}}}),
		"an image part with no address":     chat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"detail": "high"}}}}}}),
		"a video beside a picture and text": chat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look"}, picture, map[string]any{"type": "video", "video": []any{"https://example.com/1.jpg"}}}}}}),
	}
	for name, payload := range refused {
		response, raw := postJSON(t, h, payload)
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(raw, "invalid_request_error") {
			t.Errorf("%s: status %d: %.160s", name, response.StatusCode, raw)
		}
	}
	up.mu.Lock()
	reached := len(up.requests)
	up.mu.Unlock()
	if reached != 0 || h.balance(t) != before {
		t.Fatalf("%d refused requests reached the host; balance moved by %d", reached, before-h.balance(t))
	}
	// asking for nothing is no reason to refuse: the field is left out
	for name, payload := range map[string]map[string]any{
		"search switched off": chat(map[string]any{"enable_search": false}),
		"no plugins":          chat(map[string]any{"plugins": []any{}}),
		"text alone":          chat(map[string]any{"modalities": []any{"text"}}),
		"no prediction":       chat(map[string]any{"prediction": nil}),
	} {
		response, raw := postJSON(t, h, payload)
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d: %.160s", name, response.StatusCode, raw)
		}
		for _, field := range []string{"enable_search", "plugins", "modalities", "prediction"} {
			if _, ok := up.last(t).body[field]; ok {
				t.Errorf("%s: %s reached the host", name, field)
			}
		}
	}
}

func TestAPictureGoesUpstreamAsItsAddressAndNothingElse(t *testing.T) {
	up := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`))
	reads := upstream("deepseek/api", up.server.URL, 1)
	reads.Images = true
	route := routeFor(reads)
	route.ImageTokens, route.MaxImages = 1_000, 4
	h := newHarness(t, route, "")
	inline := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	response, raw := postJSON(t, h, chat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what are these?", "cache_control": map[string]any{"type": "ephemeral"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": inline, "detail": "high", "max_pixels": 99999999}, "min_pixels": 1},
		map[string]any{"type": "image_url", "image_url": "https://example.com/a.png"},
	}}}}))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, raw)
	}
	parts := up.last(t).body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 3 || parts[0].(map[string]any)["text"] != "what are these?" {
		t.Fatalf("parts %v", parts)
	}
	for i, want := range map[int]string{1: inline, 2: "https://example.com/a.png"} {
		part := parts[i].(map[string]any)
		inner, _ := part["image_url"].(map[string]any)
		if len(part) != 2 || part["type"] != "image_url" || len(inner) != 1 || inner["url"] != want {
			t.Errorf("image part %d reached the host as %v", i, part)
		}
	}
}
