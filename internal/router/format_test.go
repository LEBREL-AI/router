package router

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	return value
}

func TestAnthropicMessagesBecomeOneChatCompletionAndBack(t *testing.T) {
	request := decodeJSON(t, `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":9000,"system":[{"type":"text","text":"Be brief."}],
		"messages":[
			{"role":"user","content":"Read the file."},
			{"role":"assistant","content":[{"type":"text","text":"Reading."},{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.txt"}},{"type":"thinking","thinking":"hmm"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hello","is_error":true},{"type":"text","text":"Thanks"}]}
		],
		"tools":[{"name":"read","description":"Reads","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"stop_sequences":["END"],"temperature":0.2,"stream":true}`)
	chat, err := anthropicToChat(request, 8192)
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]map[string]any)
	if len(messages) != 5 || messages[0]["role"] != "system" || messages[2]["role"] != "assistant" || messages[3]["role"] != "tool" || messages[4]["content"] != "Thanks" {
		t.Fatalf("messages: %v", messages)
	}
	if messages[3]["content"] != "Error: hello" || messages[3]["tool_call_id"] != "toolu_1" {
		t.Fatalf("tool result: %v", messages[3])
	}
	calls := messages[2]["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("tool call: %v", calls)
	}
	if chat["max_tokens"] != int64(8192) || chat["tool_choice"] != "auto" || chat["parallel_tool_calls"] != false || chat["temperature"] != 0.2 || chat["stream"] != true {
		t.Fatalf("chat: %v", chat)
	}
	if _, present := chat["stream_options"]; !present {
		t.Fatal("a streamed chat asks for usage")
	}
	if _, err := anthropicToChat(decodeJSON(t, `{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}],"max_tokens":5}`), 0); err == nil {
		t.Fatal("an image block is refused by name")
	}
	if _, err := anthropicToChat(decodeJSON(t, `{"messages":[{"role":"user","content":"x"}]}`), 0); err == nil {
		t.Fatal("max_tokens is required")
	}

	completion := decodeJSON(t, `{"choices":[{"finish_reason":"tool_calls","message":{"content":"Let me look.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":7}}`)
	message := anthropicFromChat(completion, "deepseek/deepseek-v4.1-flash", "req_abc")
	content := message["content"].([]any)
	if message["id"] != "msg_req_abc" || message["stop_reason"] != "tool_use" || len(content) != 2 {
		t.Fatalf("message: %v", message)
	}
	tool := content[1].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["input"].(map[string]any)["path"] != "a.txt" {
		t.Fatalf("tool use: %v", tool)
	}
	if message["usage"].(map[string]any)["input_tokens"] != int64(12) {
		t.Fatalf("usage: %v", message["usage"])
	}

	stream := newAnthropicStream("deepseek/deepseek-v4.1-flash", "req_abc")
	var names []string
	for _, event := range stream.Start() {
		names = append(names, event.Name)
	}
	for _, chunk := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Let me"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":" look."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7}}`,
	} {
		for _, event := range stream.Push(decodeJSON(t, chunk)) {
			names = append(names, event.Name)
		}
	}
	var last []formatEvent
	for _, event := range stream.Finish() {
		names = append(names, event.Name)
		last = append(last, event)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n got %v\nwant %v", names, want)
	}
	delta := last[len(last)-2].Data["delta"].(map[string]any)
	if delta["stop_reason"] != "tool_use" || last[len(last)-2].Data["usage"].(map[string]any)["output_tokens"] != int64(7) {
		t.Fatalf("message_delta: %v", last[len(last)-2].Data)
	}
	if body := anthropicErrorBody(429, "rate_limit_exceeded", ""); body["error"].(map[string]any)["type"] != "rate_limit_error" {
		t.Fatalf("error body: %v", body)
	}
	if estimate, err := anthropicCountTokensEstimate(decodeJSON(t, `{"messages":[{"role":"user","content":"four score and seven years ago"}]}`)); err != nil || estimate < 10 || estimate > 40 {
		t.Fatalf("estimate %d %v", estimate, err)
	}
}

func TestResponsesBecomeOneChatCompletionAndBack(t *testing.T) {
	request := decodeJSON(t, `{"model":"deepseek/deepseek-v4-pro","instructions":"Be brief.","max_output_tokens":500000,"stream":true,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"List files."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Listing."}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"a.txt"},
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"developer","content":"Note."}
		],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"web_search"}],"tool_choice":"auto","parallel_tool_calls":false}`)
	chat, err := responsesToChat(request, 131072)
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]map[string]any)
	if len(messages) != 5 || messages[0]["role"] != "system" || messages[2]["role"] != "assistant" || messages[3]["role"] != "tool" || messages[4]["role"] != "system" {
		t.Fatalf("messages: %v", messages)
	}
	if messages[2]["content"] != "Listing." || len(messages[2]["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant turn joins its text and calls: %v", messages[2])
	}
	if _, closed := messages[2]["_closed"]; closed {
		t.Fatal("the bookkeeping field must not travel")
	}
	if chat["max_tokens"] != int64(131072) || len(chat["tools"].([]any)) != 1 || chat["tool_choice"] != "auto" || chat["parallel_tool_calls"] != false {
		t.Fatalf("chat: %v", chat)
	}
	if _, err := responsesToChat(decodeJSON(t, `{"previous_response_id":"resp_1","input":"hi"}`), 0); err == nil {
		t.Fatal("previous_response_id is refused")
	}

	completion := decodeJSON(t, `{"choices":[{"finish_reason":"length","message":{"content":"a.txt b.txt"}}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`)
	response := responsesFromChat(completion, request, "deepseek/deepseek-v4-pro", "req_xyz", 1_900_000_000)
	if response["id"] != "resp_req_xyz" || response["status"] != "incomplete" || response["created_at"] != int64(1_900_000_000) {
		t.Fatalf("response: %v", response)
	}
	output := response["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "a.txt b.txt" {
		t.Fatalf("output: %v", output)
	}
	if response["usage"].(map[string]any)["total_tokens"] != int64(8) {
		t.Fatalf("usage: %v", response["usage"])
	}

	stream := newResponsesStream(request, "deepseek/deepseek-v4-pro", "req_xyz", 1_900_000_000)
	var names []string
	collect := func(events []formatEvent) {
		for _, event := range events {
			names = append(names, event.Name)
			if event.Data["sequence_number"] == nil || event.Data["type"] != event.Name {
				t.Fatalf("event %v lacks its sequence or type", event)
			}
		}
	}
	collect(stream.Start())
	for _, chunk := range []string{
		`{"choices":[{"index":0,"delta":{"content":"Running"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"shell","arguments":"{\"cmd\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5}}`,
	} {
		collect(stream.Push(decodeJSON(t, chunk)))
	}
	final := stream.Finish()
	collect(final)
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.completed"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n got %v\nwant %v", names, want)
	}
	completed := final[len(final)-1].Data["response"].(map[string]any)
	items := completed["output"].([]any)
	if completed["status"] != "completed" || len(items) != 2 || items[1].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("completed response: %v", completed)
	}
}

// readSSE collects the events of a streamed answer as (name, data) pairs.
func readSSE(t *testing.T, body io.Reader) []formatEvent {
	t.Helper()
	var events []formatEvent
	var name string
	var data []string
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	flush := func() {
		if name == "" && len(data) == 0 {
			return
		}
		payload := strings.Join(data, "\n")
		var value map[string]any
		if payload != "" && payload != "[DONE]" {
			if err := json.Unmarshal([]byte(payload), &value); err != nil {
				t.Fatalf("event %q data is not JSON: %s", name, payload)
			}
		}
		events = append(events, formatEvent{Name: name, Data: value})
		name, data = "", nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return events
}

func TestClaudeCodeAndCodexSpeakTheirFormatsToTheRouter(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			sseHandler(w, r)
			return
		}
		okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_cache_hit_tokens":4,"prompt_cache_miss_tokens":8}`)(w, r)
	})
	h := newHarnessWith(t, routeFor(upstream("deepseek/api", up.server.URL, 1)), board, func(s *Settings) {
		s.EdgeProxySecret = "edge-secret"
		s.DirectClients = true
	})
	post := func(path, body string) *http.Response {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+testKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	// Claude Code: a Messages request, answered as a Message, charged and signed like any completion.
	response := post(anthropicMessagesPath, `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":"hi there"}]}`)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("messages: %d %s", response.StatusCode, raw)
	}
	message := decodeJSON(t, string(raw))
	if message["type"] != "message" || message["content"].([]any)[0].(map[string]any)["text"] != "hello world" || response.Header.Get(receiptHeader) == "" {
		t.Fatalf("message %s (receipt header %q)", raw, response.Header.Get(receiptHeader))
	}
	if got := up.requests[len(up.requests)-1].body; got["model"] != "deepseek-flash" || got["max_tokens"] != float64(64) || got["stream"] != false {
		t.Fatalf("the lab saw a chat completion: %v", got)
	}
	// Streamed Messages events.
	response = post(anthropicMessagesPath, `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi there"}]}`)
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("streamed messages: %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	events := readSSE(t, response.Body)
	var names []string
	text := ""
	for _, event := range events {
		names = append(names, event.Name)
		if event.Name == "content_block_delta" {
			text += event.Data["delta"].(map[string]any)["text"].(string)
		}
	}
	if text != "hello world" || names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatalf("events %v text %q", names, text)
	}
	// The token count is an estimate, answered without a lab call.
	before := len(up.requests)
	response = post(anthropicCountPath, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi there"}]}`)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 || decodeJSON(t, string(raw))["input_tokens"] == nil || len(up.requests) != before {
		t.Fatalf("count: %d %s", response.StatusCode, raw)
	}
	// Codex: a Responses request, answered as a Response, and streamed as Responses events.
	response = post(responsesPath, `{"model":"deepseek/deepseek-v4.1-flash","input":"hi there","max_output_tokens":64}`)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("responses: %d %s", response.StatusCode, raw)
	}
	object := decodeJSON(t, string(raw))
	if object["object"] != "response" || object["status"] != "completed" || object["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "hello world" {
		t.Fatalf("response %s", raw)
	}
	response = post(responsesPath, `{"model":"deepseek/deepseek-v4.1-flash","input":"hi there","stream":true}`)
	events = readSSE(t, response.Body)
	if response.StatusCode != 200 || events[0].Name != "response.created" || events[len(events)-1].Name != "response.completed" {
		t.Fatalf("streamed responses: %d %v", response.StatusCode, events)
	}
	// Refusals come in the format's own shape.
	response = post(anthropicMessagesPath, `{"model":"nobody/nothing","max_tokens":5,"messages":[{"role":"user","content":"x"}]}`)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 404 || decodeJSON(t, string(raw))["error"].(map[string]any)["type"] != "not_found_error" {
		t.Fatalf("unknown model: %d %s", response.StatusCode, raw)
	}
	response = post(responsesPath, `{"model":"deepseek/deepseek-v4.1-flash","input":"x","previous_response_id":"resp_1"}`)
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 400 || decodeJSON(t, string(raw))["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Fatalf("unsupported field: %d %s", response.StatusCode, raw)
	}
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+anthropicMessagesPath, strings.NewReader(`{"model":"deepseek/deepseek-v4.1-flash","max_tokens":5,"messages":[{"role":"user","content":"x"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer leb_live_wrong")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(response.Body)
	if response.StatusCode != 401 || decodeJSON(t, string(raw))["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("wrong key: %d %s", response.StatusCode, raw)
	}
}
