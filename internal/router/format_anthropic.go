package router

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// The Anthropic Messages API, spoken for clients such as Claude Code: a Messages request becomes
// one chat completion, the answer comes back as a Messages response, streamed as Messages events
// when asked. This is the local proxy's translation (lebrel-encrypted, anthropic_messages.py),
// moved into the router so nothing has to run on the client's machine.

// formatError is a request an adapter cannot express as a chat completion: answered as the
// format's invalid request error, never relayed.
type formatError struct{ message string }

func (e *formatError) Error() string { return e.message }

func formatErrorf(format string, args ...any) error {
	return &formatError{message: fmt.Sprintf(format, args...)}
}

var anthropicStopReasons = map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "content_filter": "end_turn"}

// anthropicText is the text of a string or a list of text blocks; anything else is refused by name.
func anthropicText(content any, what string) (string, error) {
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		parts := make([]string, 0, len(value))
		for _, block := range value {
			object, ok := block.(map[string]any)
			text, hasText := object["text"].(string)
			if ok && object["type"] == "text" && hasText {
				parts = append(parts, text)
				continue
			}
			kind := "unknown"
			if ok {
				kind, _ = object["type"].(string)
			}
			return "", formatErrorf("%s content of type %q is not supported", what, kind)
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", formatErrorf("%s content must be text", what)
}

// anthropicUserMessages is a user turn: tool results first (each its own tool message), then the user's own text.
func anthropicUserMessages(content any) ([]map[string]any, error) {
	if text, ok := content.(string); ok {
		return []map[string]any{{"role": "user", "content": text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, formatErrorf("message content must be a string or a list of blocks")
	}
	var tools []map[string]any
	var texts []string
	for _, block := range blocks {
		object, ok := block.(map[string]any)
		if !ok {
			return nil, formatErrorf("message content blocks must be objects")
		}
		kind, _ := object["type"].(string)
		text, hasText := object["text"].(string)
		toolUseID, hasToolUse := object["tool_use_id"].(string)
		switch {
		case kind == "text" && hasText:
			texts = append(texts, text)
		case kind == "tool_result" && hasToolUse:
			result := ""
			if object["content"] != nil {
				var err error
				if result, err = anthropicText(object["content"], "tool_result"); err != nil {
					return nil, err
				}
			}
			if object["is_error"] == true && result != "" {
				result = "Error: " + result
			}
			tools = append(tools, map[string]any{"role": "tool", "tool_call_id": toolUseID, "content": result})
		default:
			return nil, formatErrorf("user content of type %q is not supported", kind)
		}
	}
	messages := tools
	if len(texts) > 0 {
		messages = append(messages, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
	}
	return messages, nil
}

// anthropicAssistantMessage is an assistant turn: its text and the tools it called; thinking blocks are the model's own and stay out.
func anthropicAssistantMessage(content any) (map[string]any, error) {
	if text, ok := content.(string); ok {
		return map[string]any{"role": "assistant", "content": text}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, formatErrorf("message content must be a string or a list of blocks")
	}
	var texts []string
	var calls []any
	for _, block := range blocks {
		object, ok := block.(map[string]any)
		if !ok {
			return nil, formatErrorf("message content blocks must be objects")
		}
		kind, _ := object["type"].(string)
		text, hasText := object["text"].(string)
		id, hasID := object["id"].(string)
		name, hasName := object["name"].(string)
		switch {
		case kind == "text" && hasText:
			texts = append(texts, text)
		case kind == "tool_use" && hasID && hasName:
			arguments, _ := object["input"].(map[string]any)
			if arguments == nil {
				arguments = map[string]any{}
			}
			encoded, _ := json.Marshal(arguments)
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}})
		case kind == "thinking" || kind == "redacted_thinking":
			continue
		default:
			return nil, formatErrorf("assistant content of type %q is not supported", kind)
		}
	}
	message := map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	return message, nil
}

func anthropicTools(tools any) ([]any, error) {
	list, ok := tools.([]any)
	if !ok {
		return nil, formatErrorf("tools must be a list")
	}
	out := make([]any, 0, len(list))
	for _, tool := range list {
		object, ok := tool.(map[string]any)
		name, hasName := object["name"].(string)
		if !ok || !hasName {
			return nil, formatErrorf("each tool needs a name")
		}
		if kind, present := object["type"]; present && kind != nil && kind != "custom" {
			return nil, formatErrorf("tool type %q is not supported", fmt.Sprint(kind))
		}
		function := map[string]any{"name": name}
		if schema, ok := object["input_schema"].(map[string]any); ok {
			function["parameters"] = schema
		} else {
			function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if description, ok := object["description"].(string); ok {
			function["description"] = description
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out, nil
}

func anthropicToolChoice(choice any) (any, error) {
	object, ok := choice.(map[string]any)
	if !ok {
		return nil, formatErrorf("tool_choice must be an object")
	}
	name, hasName := object["name"].(string)
	switch object["type"] {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		if hasName {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}, nil
		}
	}
	return nil, formatErrorf("tool_choice is not supported")
}

// positiveInteger reads a JSON number that is a whole number above zero.
func positiveInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		if number >= 1 && number == float64(int64(number)) {
			return int64(number), true
		}
	case json.Number:
		if integer, err := number.Int64(); err == nil && integer >= 1 {
			return integer, true
		}
	}
	return 0, false
}

// anthropicToChat is the chat completion that expresses a Messages request; max_tokens is capped at
// maxOutputTokens when that is above zero.
func anthropicToChat(request map[string]any, maxOutputTokens int64) (map[string]any, error) {
	messages := []map[string]any{}
	if system, present := request["system"]; present && system != nil {
		text, err := anthropicText(system, "system")
		if err != nil {
			return nil, err
		}
		if text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}
	turns, ok := request["messages"].([]any)
	if !ok || len(turns) == 0 {
		return nil, formatErrorf("messages must be a non-empty list")
	}
	for _, turn := range turns {
		object, ok := turn.(map[string]any)
		if !ok {
			return nil, formatErrorf("each message must be an object")
		}
		switch object["role"] {
		case "user":
			user, err := anthropicUserMessages(object["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, user...)
		case "assistant":
			assistant, err := anthropicAssistantMessage(object["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, assistant)
		case "system":
			text, err := anthropicText(object["content"], "system")
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "system", "content": text})
		default:
			return nil, formatErrorf("message role %q is not supported", fmt.Sprint(object["role"]))
		}
	}
	if len(messages) == 0 {
		return nil, formatErrorf("messages must be a non-empty list")
	}
	chat := map[string]any{"messages": messages, "stream": request["stream"] == true}
	maxTokens, ok := positiveInteger(request["max_tokens"])
	if !ok {
		return nil, formatErrorf("max_tokens must be a positive integer")
	}
	if maxOutputTokens > 0 && maxTokens > maxOutputTokens {
		maxTokens = maxOutputTokens
	}
	chat["max_tokens"] = maxTokens
	for _, name := range []string{"temperature", "top_p"} {
		if value, present := request[name]; present && value != nil {
			chat[name] = value
		}
	}
	if stop, ok := request["stop_sequences"].([]any); ok && len(stop) > 0 {
		chat["stop"] = stop
	}
	if tools, present := request["tools"]; present && tools != nil {
		if list, ok := tools.([]any); !ok || len(list) > 0 {
			converted, err := anthropicTools(tools)
			if err != nil {
				return nil, err
			}
			chat["tools"] = converted
			if choice, present := request["tool_choice"]; present && choice != nil {
				converted, err := anthropicToolChoice(choice)
				if err != nil {
					return nil, err
				}
				chat["tool_choice"] = converted
				if object, _ := choice.(map[string]any); object["disable_parallel_tool_use"] == true {
					chat["parallel_tool_calls"] = false
				}
			}
		}
	}
	if chat["stream"] == true {
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	return chat, nil
}

func integerOf(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		integer, _ := number.Int64()
		return integer
	}
	return 0
}

func anthropicUsage(usage any) map[string]any {
	object, _ := usage.(map[string]any)
	return map[string]any{
		"input_tokens":                integerOf(object["prompt_tokens"]),
		"output_tokens":               integerOf(object["completion_tokens"]),
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
	}
}

// toolInput is a tool call's arguments as an object: the JSON they carry, or an empty object.
func toolInput(arguments any) map[string]any {
	switch value := arguments.(type) {
	case map[string]any:
		return value
	case string:
		if strings.TrimSpace(value) != "" {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(value), &parsed); err == nil && parsed != nil {
				return parsed
			}
		}
	}
	return map[string]any{}
}

func randomHex(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

func anthropicMessageID(requestID string) string {
	base := strings.ReplaceAll(requestID, "-", "")
	if base == "" {
		base = randomHex(16)
	}
	return "msg_" + base
}

func firstChoice(completion map[string]any) map[string]any {
	choices, _ := completion["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return map[string]any{}
	}
	return choice
}

// anthropicFromChat is the Messages response for a finished chat completion.
func anthropicFromChat(completion map[string]any, model, requestID string) map[string]any {
	choice := firstChoice(completion)
	message, _ := choice["message"].(map[string]any)
	content := []any{}
	if text, ok := message["content"].(string); ok && text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, call := range calls {
			object, ok := call.(map[string]any)
			if !ok {
				continue
			}
			function, _ := object["function"].(map[string]any)
			id, _ := object["id"].(string)
			if id == "" {
				id = "toolu_" + randomHex(12)
			}
			name, _ := function["name"].(string)
			content = append(content, map[string]any{"type": "tool_use", "id": id, "name": name, "input": toolInput(function["arguments"])})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	stop := "end_turn"
	if reason, ok := choice["finish_reason"].(string); ok {
		if mapped, known := anthropicStopReasons[reason]; known {
			stop = mapped
		}
	}
	return map[string]any{
		"id":            anthropicMessageID(requestID),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         anthropicUsage(completion["usage"]),
	}
}

// formatEvent is one server-sent event of a translated stream: its name (empty for a bare data line) and its data.
type formatEvent struct {
	Name string
	Data map[string]any
}

// anthropicStream turns a chat completion stream into Messages events: text and tool calls become
// content blocks in order. Start, then Push per chunk, then Finish.
type anthropicStream struct {
	model, requestID string
	index            int
	openKind         string        // "text" or "tool" while a block is open
	tools            map[int64]int // tool call position in the chunk stream -> content block index
	finish           string
	usage            map[string]any
}

func newAnthropicStream(model, requestID string) *anthropicStream {
	return &anthropicStream{model: model, requestID: requestID, index: -1, tools: map[int64]int{}, usage: map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}}
}

func (s *anthropicStream) Start() []formatEvent {
	message := map[string]any{"id": anthropicMessageID(s.requestID), "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}
	return []formatEvent{{Name: "message_start", Data: map[string]any{"type": "message_start", "message": message}}}
}

func (s *anthropicStream) closeBlock() []formatEvent {
	if s.openKind == "" {
		return nil
	}
	s.openKind = ""
	return []formatEvent{{Name: "content_block_stop", Data: map[string]any{"type": "content_block_stop", "index": s.index}}}
}

func (s *anthropicStream) Push(chunk map[string]any) []formatEvent {
	var events []formatEvent
	if usage, ok := chunk["usage"].(map[string]any); ok {
		s.usage = anthropicUsage(usage)
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return events
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return events
	}
	delta, _ := choice["delta"].(map[string]any)
	if text, ok := delta["content"].(string); ok && text != "" {
		if s.openKind != "text" {
			events = append(events, s.closeBlock()...)
			s.index++
			s.openKind = "text"
			events = append(events, formatEvent{Name: "content_block_start", Data: map[string]any{"type": "content_block_start", "index": s.index, "content_block": map[string]any{"type": "text", "text": ""}}})
		}
		events = append(events, formatEvent{Name: "content_block_delta", Data: map[string]any{"type": "content_block_delta", "index": s.index, "delta": map[string]any{"type": "text_delta", "text": text}}})
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, call := range calls {
			object, ok := call.(map[string]any)
			if !ok {
				continue
			}
			position := int64(len(s.tools))
			if value, ok := object["index"]; ok {
				if number, isNumber := value.(float64); isNumber {
					position = int64(number)
				} else if number, isNumber := value.(json.Number); isNumber {
					position, _ = number.Int64()
				}
			}
			function, _ := object["function"].(map[string]any)
			if _, known := s.tools[position]; !known {
				events = append(events, s.closeBlock()...)
				s.index++
				s.tools[position] = s.index
				s.openKind = "tool"
				id, _ := object["id"].(string)
				if id == "" {
					id = "toolu_" + randomHex(12)
				}
				name, _ := function["name"].(string)
				events = append(events, formatEvent{Name: "content_block_start", Data: map[string]any{"type": "content_block_start", "index": s.index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}}})
			} else if s.tools[position] != s.index {
				// Arguments for an earlier call after another block opened: reopen that call's block in place.
				events = append(events, s.closeBlock()...)
				s.index = s.tools[position]
				s.openKind = "tool"
			}
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				events = append(events, formatEvent{Name: "content_block_delta", Data: map[string]any{"type": "content_block_delta", "index": s.tools[position], "delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}}})
			}
		}
	}
	if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
		s.finish = reason
	}
	return events
}

func (s *anthropicStream) Finish() []formatEvent {
	events := s.closeBlock()
	if s.index < 0 {
		// Nothing was said: one empty text block keeps the message well-formed.
		events = append(events,
			formatEvent{Name: "content_block_start", Data: map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}},
			formatEvent{Name: "content_block_stop", Data: map[string]any{"type": "content_block_stop", "index": 0}})
	}
	finish := s.finish
	if finish == "" {
		finish = "stop"
	}
	stop, known := anthropicStopReasons[finish]
	if !known {
		stop = "end_turn"
	}
	events = append(events,
		formatEvent{Name: "message_delta", Data: map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": s.usage["output_tokens"]}}},
		formatEvent{Name: "message_stop", Data: map[string]any{"type": "message_stop"}})
	return events
}

// anthropicCountTokensEstimate is a size-based estimate of the prompt (about four characters a
// token); the model's own count is in every answer's usage.
func anthropicCountTokensEstimate(request map[string]any) (int64, error) {
	copied := make(map[string]any, len(request)+1)
	for key, value := range request {
		copied[key] = value
	}
	copied["max_tokens"] = float64(1)
	chat, err := anthropicToChat(copied, 0)
	if err != nil {
		return 0, err
	}
	messages, _ := json.Marshal(chat["messages"])
	tools, _ := json.Marshal(chat["tools"])
	size := int64(len(messages) + len(tools))
	if chat["tools"] == nil {
		size = int64(len(messages)) + 2
	}
	if size/4 < 1 {
		return 1, nil
	}
	return size / 4, nil
}

var anthropicErrorKinds = map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 411: "invalid_request_error", 413: "invalid_request_error", 415: "invalid_request_error", 429: "rate_limit_error", 500: "api_error", 502: "api_error", 503: "overloaded_error", 504: "api_error"}

func anthropicErrorBody(status int, code, message string) map[string]any {
	kind, known := anthropicErrorKinds[status]
	if !known {
		kind = "api_error"
	}
	if message == "" {
		message = strings.ReplaceAll(code, "_", " ")
	}
	return map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}}
}
