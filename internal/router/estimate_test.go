package router

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lebrel-router/internal/meter"
	"lebrel-router/internal/model"
)

// A request is held for all of it and charged what its host counted (27 November 2026). The host here counts as
// a lab does: every byte it is sent is text it may read, a tool's parameter names as much as a message.

type countingHost struct {
	server *httptest.Server
	prompt atomic.Int64
}

func newCountingHost(t *testing.T) *countingHost {
	t.Helper()
	host := &countingHost{}
	host.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		prompt := int64(len(raw)) / 4
		host.prompt.Store(prompt)
		var sent struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &sent)
		usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":%d}`, prompt, prompt+1)
		if !sent.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"up-1","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":%s}`, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"id\":\"s\",\"model\":\"deepseek-flash\",\"choices\":[],\"usage\":%s}\n\n", usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(host.server.Close)
	return host
}

func TestARequestIsChargedInFullWhereverItsTextSits(t *testing.T) {
	document := strings.Repeat("palabra ", 30_000) // 240,000 bytes, about 60,000 tokens
	names := map[string]any{}
	for i := 0; i+64 <= len(document); i += 64 {
		names[fmt.Sprintf("%06d", i)+document[i:i+58]] = map[string]any{}
	}
	lookup := func(properties map[string]any) []any {
		return []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": properties}}}}
	}
	message := func(content any) []any { return []any{map[string]any{"role": "user", "content": content}} }
	base := func(fields map[string]any) map[string]any {
		payload := map[string]any{"model": "deepseek/deepseek-v4.1-flash", "max_tokens": 1}
		for key, value := range fields {
			payload[key] = value
		}
		return payload
	}
	cases := []struct {
		name    string
		payload map[string]any
		images  bool
	}{
		{name: "in a message", payload: base(map[string]any{"messages": message(document)})},
		{name: "in a message, streamed", payload: base(map[string]any{"messages": message(document), "stream": true})},
		{name: "in a tool's parameter names", payload: base(map[string]any{"messages": message("hi"), "tools": lookup(names)})},
		{name: "in a tool's parameter names, streamed", payload: base(map[string]any{"messages": message("hi"), "tools": lookup(names), "stream": true})},
		{name: "in one parameter name", payload: base(map[string]any{"messages": message("hi"), "tools": lookup(map[string]any{document: map[string]any{}})})},
		{name: "in a response schema's field names", payload: base(map[string]any{"messages": message("hi"), "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "schema": map[string]any{"type": "object", "properties": names}}}})},
		{name: "in a request marked as an image", payload: base(map[string]any{"messages": message(document), "type": "image_url"})},
		{name: "in a message marked as an image", payload: base(map[string]any{"messages": []any{map[string]any{"role": "user", "type": "image_url", "content": document}}})},
		{name: "in a request marked as an image, on a route that reads images", payload: base(map[string]any{"messages": message(document), "type": "image_url"}), images: true},
		{name: "in a message marked as an image, on a route that reads images", payload: base(map[string]any{"messages": []any{map[string]any{"role": "user", "type": "image_url", "content": document}}}), images: true},
	}
	var control int64
	for _, test := range cases {
		host := newCountingHost(t)
		up := upstream("deepseek/api", host.server.URL, 1)
		route := routeFor(up)
		if test.images {
			route.ImageTokens, route.MaxImages = 1_000, 4
			route.Upstreams[0].Images = true
		}
		h := newHarness(t, route, "")
		before := h.balance(t)
		body, err := json.Marshal(test.payload)
		if err != nil {
			t.Fatal(err)
		}
		response := h.post(t, string(body), testKey)
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d: %.300s", test.name, response.StatusCode, raw)
		}
		counted := host.prompt.Load()
		if counted < int64(len(document))/4 {
			t.Fatalf("%s: the host counted %d tokens of a %d-byte document", test.name, counted, len(document))
		}
		fair, err := route.MeterPricing().ActualScaled(model.Usage{PromptTokens: counted, CompletionTokens: 1, TotalTokens: counted + 1}, model.QueueShort, 100)
		if err != nil {
			t.Fatal(err)
		}
		charged := before - h.balance(t)
		if charged != fair {
			t.Errorf("%s: charged %d microUSD for %d prompt tokens, which cost %d", test.name, charged, counted, fair)
		}
		receipt, ok := h.receipts.Get(response.Header.Get("X-Request-ID"))
		if !ok {
			t.Fatalf("%s: no receipt", test.name)
		}
		if receipt.Payload["usage_exact"] != true || fmt.Sprint(receipt.Payload["prompt_tokens"]) != fmt.Sprint(counted) || fmt.Sprint(receipt.Payload["price_microusd"]) != fmt.Sprint(fair) {
			t.Errorf("%s: receipt says exact %v, %v prompt tokens, %v microUSD", test.name, receipt.Payload["usage_exact"], receipt.Payload["prompt_tokens"], receipt.Payload["price_microusd"])
		}
		if control == 0 {
			control = charged
		}
		if charged < control {
			t.Errorf("%s: charged %d, less than the %d a message carrying the same text costs", test.name, charged, control)
		}
	}
}

func TestAPictureIsHeldAsAPictureAndItsTextAsText(t *testing.T) {
	// a real PNG header, then data: what a picture's base64 looks like
	picture := "data:image/png;base64," + base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 300_000)...))
	host := newCountingHost(t)
	route := routeFor(upstream("deepseek/api", host.server.URL, 1))
	route.ImageTokens, route.MaxImages = 1_000, 4
	route.Upstreams[0].Images = true
	body, _ := json.Marshal(map[string]any{"model": route.ModelID, "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is this?"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": picture}},
	}}}})
	_, info, err := meter.InspectAndRequireUsage(completionPath, body, route.DefaultMaxOutputTokens, route.MaxOutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	if info.Images != 1 || info.EstimatedPromptTokens > 100 {
		t.Fatalf("a picture and a question: %d images, %d tokens of text", info.Images, info.EstimatedPromptTokens)
	}
	// the same data where no picture is read is text, and the request no longer fits the model
	h := newHarness(t, route, "")
	hidden, _ := json.Marshal(map[string]any{"model": route.ModelID, "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "image_url", "image_url": map[string]any{"url": strings.Repeat(picture, 3)}}}}}})
	response := h.post(t, string(hidden), testKey)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "context_length_exceeded") {
		t.Fatalf("a picture's data outside a message: status %d: %.200s", response.StatusCode, raw)
	}
}
