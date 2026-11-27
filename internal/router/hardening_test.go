package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestAStreamIsChargedByItsLargestCountAndAHostBeyondTheHoldIsTold(t *testing.T) {
	var mu sync.Mutex
	var alerts []string
	// a host that sends its count, then a block of zeros
	twice := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"s","model":"deepseek-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
			`{"id":"s","model":"deepseek-flash","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
			`{"id":"s","model":"deepseek-flash","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	h := newHarnessWith(t, routeFor(upstream("deepseek/api", twice.server.URL, 1)), "", func(s *Settings) {
		s.Alert = func(text string) { mu.Lock(); alerts = append(alerts, text); mu.Unlock() }
	})
	before := h.balance(t)
	response, raw := postJSON(t, h, chat(map[string]any{"stream": true}))
	if response.StatusCode != http.StatusOK || !strings.Contains(raw, "[DONE]") {
		t.Fatalf("status %d: %s", response.StatusCode, raw)
	}
	// 12 prompt * 0.1575 + 3 output * 0.63 microUSD = 3.78, rounded up
	if charged := before - h.balance(t); charged != 4 {
		t.Fatalf("a stream whose last usage block is zeros was charged %d, want 4", charged)
	}
	// a host that writes past the cap it was given: the customer pays the reserved maximum, and the operator is told once
	past := newFakeUpstream(t, okJSON(`{"prompt_tokens":12,"completion_tokens":900,"total_tokens":912}`))
	h = newHarnessWith(t, routeFor(upstream("deepseek/api", past.server.URL, 1)), "", func(s *Settings) {
		s.Alert = func(text string) { mu.Lock(); alerts = append(alerts, text); mu.Unlock() }
	})
	for i := 0; i < 3; i++ {
		if response, raw := postJSON(t, h, chat(map[string]any{"max_tokens": 16})); response.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", response.StatusCode, raw)
		}
	}
	flushed(t, h.settlements)
	mu.Lock()
	defer mu.Unlock()
	if len(alerts) != 1 || !strings.Contains(alerts[0], "contó más de lo reservado") || !strings.Contains(alerts[0], "salida 900 de 16") || strings.Contains(alerts[0], "hi") && strings.Contains(alerts[0], "content") {
		t.Fatalf("alerts %q", alerts)
	}
}

func TestAKeysTurnsAreTakenBeforeATranslatedRequestIsRead(t *testing.T) {
	release := make(chan struct{})
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`)(w, r)
	})
	var fetches atomic.Int64
	h := newHarnessWith(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), "", func(s *Settings) {
		s.EdgeProxySecret = "edge-secret"
		s.DirectClients = true
	})
	h.router.mediaFetch = func(context.Context, string) ([]byte, string, error) {
		fetches.Add(1)
		return tinyPDF("hello"), "application/pdf", nil
	}
	post := func(path, body string) *http.Response {
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+testKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	// the key's four turns are taken by four answers still on their way
	done := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func() {
			response := post("/v1/chat/completions", `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
			response.Body.Close()
			done <- response.StatusCode
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		up.mu.Lock()
		reached := len(up.requests)
		up.mu.Unlock()
		if reached == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 4 requests reached the host", reached)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// a fifth, in each translated format and naming documents to fetch, is turned away before any of them is fetched
	document := `{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}}`
	for path, body := range map[string]string{
		anthropicMessagesPath:                   `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":[` + document + `,` + document + `]}]}`,
		anthropicMessagesPath + "/count_tokens": `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`,
		responsesPath:                           `{"model":"deepseek/deepseek-v4.1-flash","input":[{"role":"user","content":[{"type":"input_file","file_url":"https://example.com/a.pdf"}]}]}`,
	} {
		response := post(path, body)
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s with every turn taken: status %d: %.200s", path, response.StatusCode, raw)
		}
	}
	if fetches.Load() != 0 {
		t.Errorf("%d files were fetched for requests that had no turn", fetches.Load())
	}
	close(release)
	for i := 0; i < 4; i++ {
		if status := <-done; status != http.StatusOK {
			t.Errorf("a request that held a turn ended %d", status)
		}
	}
	// with a turn free the same request is read, and it takes one turn, not two
	response := post(anthropicMessagesPath, `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":[`+document+`,{"type":"text","text":"what does it say?"}]}]}`)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || fetches.Load() != 1 {
		t.Fatalf("with a turn free: status %d after %d fetches: %.200s", response.StatusCode, fetches.Load(), raw)
	}
}

func TestARequestsFilesAreBounded(t *testing.T) {
	megabyte := strings.Repeat("palabra ", 131_072)
	files := map[string][]byte{"https://example.com/a.png": append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 600_000)...), "https://example.com/a.txt": []byte(megabyte)}
	message := func(parts ...any) map[string]any {
		return map[string]any{"messages": []map[string]any{{"role": "user", "content": parts}}}
	}
	repeat := func(count int, part func(int) any) []any {
		parts := make([]any, count)
		for i := range parts {
			parts[i] = part(i)
		}
		return parts
	}
	route := visionRoute()
	route.MaxImages = 64
	for name, test := range map[string]struct {
		chat   map[string]any
		budget int64
		want   string
	}{
		"more files by address than a request may name": {chat: message(repeat(mediaMaxFetches+1, func(int) any { return imagePart("https://example.com/a.png") })...), want: "at most 32 files by address"},
		"more fetched than a request may carry":         {chat: message(repeat(4, func(int) any { return imagePart("https://example.com/a.png") })...), budget: 2 << 20, want: "at most 2 MB together"},
		"more documents than a request may carry":       {chat: message(repeat(documentsMax+1, func(int) any { return documentPart("", "text/plain", "", "hello", "") })...), want: "at most 32 documents"},
		"more document text than a request may carry":   {chat: message(repeat(9, func(int) any { return documentPart("", "text/plain", "", "", "https://example.com/a.txt") })...), want: "at most 8 MB of text together"},
	} {
		server := mediaServer(files)
		server.settings.MaxRequestBytes = test.budget
		err := server.resolveMedia(context.Background(), test.chat, route)
		var refused *formatError
		if !asFormatError(err, &refused) || !strings.Contains(refused.message, test.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// what fits is read as before
	server := mediaServer(files)
	if err := server.resolveMedia(context.Background(), message(imagePart("https://example.com/a.png"), documentPart("notes", "text/plain", "", "", "https://example.com/a.txt")), route); err != nil {
		t.Fatalf("an image and a document by address: %v", err)
	}
}

func TestOnlyThePublicWebIsFetched(t *testing.T) {
	for address, public := range map[string]bool{
		"93.184.216.34:443": true, "93.184.216.34:80": true, "93.184.216.34:8080": true, "[2606:2800:220:1:248:1893:25c8:1946]:443": true,
		"93.184.216.34:22": false, "93.184.216.34:25": false, "93.184.216.34:6379": false, "93.184.216.34:8090": false,
		"127.0.0.1:443": false, "10.0.0.1:443": false, "169.254.169.254:80": false, "100.64.0.1:443": false, "192.88.99.1:443": false,
		"[::1]:443": false, "[::ffff:10.0.0.1]:443": false, "[::10.0.0.1]:443": false, "[64:ff9b:1::a00:1]:443": false, "[2001::1]:443": false,
		"[2001:db8::1]:443": false, "[fec0::1]:443": false, "[fe80::1]:443": false, "[fd00::1]:443": false, "[3fff::1]:443": false, "[5f00::1]:443": false,
	} {
		if err := publicOnly("tcp", address, nil); (err == nil) != public {
			t.Errorf("%s: fetched %v, want %v", address, err == nil, public)
		}
	}
}
