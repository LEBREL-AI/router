package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The OpenAI Responses API, spoken for clients such as Codex: a Responses request becomes one chat
// completion, the answer comes back as a Response object, streamed as Responses events when asked.
// Conversation state is the client's: every request carries its full input, as Codex sends it, and
// nothing is stored here. This is the local proxy's translation (lebrel-encrypted,
// openai_responses.py), moved into the router.

func responsesTextParts(content any, what string) (string, error) {
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		parts := make([]string, 0, len(value))
		for _, part := range value {
			object, ok := part.(map[string]any)
			kind, _ := object["type"].(string)
			text, hasText := object["text"].(string)
			refusal, hasRefusal := object["refusal"].(string)
			switch {
			case ok && (kind == "input_text" || kind == "output_text" || kind == "text") && hasText:
				parts = append(parts, text)
			case ok && kind == "refusal" && hasRefusal:
				parts = append(parts, refusal)
			default:
				return "", formatErrorf("%s content of type %q is not supported", what, kind)
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", formatErrorf("%s content must be text", what)
}

// responsesAppendAssistant: assistant text and the calls that followed it form one assistant message, as chat completions expect.
func responsesAppendAssistant(messages []map[string]any, text string, call map[string]any) []map[string]any {
	var last map[string]any
	if len(messages) > 0 {
		last = messages[len(messages)-1]
	}
	if last == nil || last["role"] != "assistant" || last["_closed"] == true {
		last = map[string]any{"role": "assistant", "content": ""}
		messages = append(messages, last)
	}
	if text != "" {
		if current, _ := last["content"].(string); current != "" {
			last["content"] = current + "\n" + text
		} else {
			last["content"] = text
		}
	}
	if call != nil {
		calls, _ := last["tool_calls"].([]any)
		last["tool_calls"] = append(calls, call)
	}
	return messages
}

// responsesToChat is the chat completion that expresses a Responses request; max_output_tokens is
// capped at maxOutputTokens when that is above zero.
func responsesToChat(request map[string]any, maxOutputTokens int64) (map[string]any, error) {
	if previous, present := request["previous_response_id"]; present && previous != nil && previous != "" {
		return nil, formatErrorf("previous_response_id is not supported: send the full conversation as input")
	}
	messages := []map[string]any{}
	if instructions, ok := request["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	var items []any
	switch input := request["input"].(type) {
	case string:
		items = []any{map[string]any{"type": "message", "role": "user", "content": input}}
	case []any:
		items = input
	}
	if len(items) == 0 {
		return nil, formatErrorf("input must be a non-empty string or list")
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, formatErrorf("each input item must be an object")
		}
		kind, _ := object["type"].(string)
		if kind == "" {
			kind = "message"
		}
		switch kind {
		case "message":
			role, _ := object["role"].(string)
			switch role {
			case "user", "system", "developer":
				text, err := responsesTextParts(object["content"], role)
				if err != nil {
					return nil, err
				}
				if role == "developer" {
					role = "system"
				}
				messages = append(messages, map[string]any{"role": role, "content": text})
			case "assistant":
				text, err := responsesTextParts(object["content"], "assistant")
				if err != nil {
					return nil, err
				}
				messages = responsesAppendAssistant(messages, text, nil)
				messages[len(messages)-1]["_closed"] = false
			default:
				return nil, formatErrorf("message role %q is not supported", role)
			}
		case "function_call":
			callID, hasCall := object["call_id"].(string)
			name, hasName := object["name"].(string)
			if !hasCall || !hasName {
				return nil, formatErrorf("function_call items need call_id and name")
			}
			arguments, isString := object["arguments"].(string)
			if !isString {
				encoded, _ := json.Marshal(object["arguments"])
				arguments = string(encoded)
				if object["arguments"] == nil {
					arguments = "{}"
				}
			}
			messages = responsesAppendAssistant(messages, "", map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}})
		case "function_call_output":
			callID, hasCall := object["call_id"].(string)
			if !hasCall {
				return nil, formatErrorf("function_call_output items need call_id")
			}
			output, isString := object["output"].(string)
			if !isString {
				var err error
				if output, err = responsesTextParts(object["output"], "function_call_output"); err != nil {
					return nil, err
				}
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
			if len(messages) > 1 && messages[len(messages)-2]["role"] == "assistant" {
				messages[len(messages)-2]["_closed"] = true
			}
		case "reasoning":
			continue
		default:
			return nil, formatErrorf("input item type %q is not supported", kind)
		}
	}
	for _, message := range messages {
		delete(message, "_closed")
	}
	chat := map[string]any{"messages": messages, "stream": request["stream"] == true}
	if limit, present := request["max_output_tokens"]; present && limit != nil {
		maxTokens, ok := positiveInteger(limit)
		if !ok {
			return nil, formatErrorf("max_output_tokens must be a positive integer")
		}
		if maxOutputTokens > 0 && maxTokens > maxOutputTokens {
			maxTokens = maxOutputTokens
		}
		chat["max_tokens"] = maxTokens
	} else if maxOutputTokens > 0 {
		chat["max_tokens"] = maxOutputTokens
	}
	for _, name := range []string{"temperature", "top_p"} {
		if value, present := request[name]; present && value != nil {
			chat[name] = value
		}
	}
	if tools, ok := request["tools"].([]any); ok && len(tools) > 0 {
		functions := []any{}
		for _, tool := range tools {
			object, ok := tool.(map[string]any)
			if !ok {
				return nil, formatErrorf("each tool must be an object")
			}
			if object["type"] != "function" {
				// Hosted tools (web_search, the computer tools) and tool namespaces are the platform's own;
				// the model keeps every function tool and simply does not have the others.
				continue
			}
			name, hasName := object["name"].(string)
			if !hasName {
				return nil, formatErrorf("each function tool needs a name")
			}
			function := map[string]any{"name": name}
			if parameters, ok := object["parameters"].(map[string]any); ok {
				function["parameters"] = parameters
			} else {
				function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			if description, ok := object["description"].(string); ok {
				function["description"] = description
			}
			functions = append(functions, map[string]any{"type": "function", "function": function})
		}
		if len(functions) > 0 {
			chat["tools"] = functions
			switch choice := request["tool_choice"].(type) {
			case nil:
			case string:
				if choice == "auto" || choice == "none" || choice == "required" {
					chat["tool_choice"] = choice
				} else {
					return nil, formatErrorf("tool_choice is not supported")
				}
			case map[string]any:
				name, hasName := choice["name"].(string)
				if choice["type"] == "function" && hasName {
					chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
				} else {
					return nil, formatErrorf("tool_choice is not supported")
				}
			default:
				return nil, formatErrorf("tool_choice is not supported")
			}
			if request["parallel_tool_calls"] == false {
				chat["parallel_tool_calls"] = false
			}
		}
	}
	if chat["stream"] == true {
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	return chat, nil
}

func responsesUsage(usage any) map[string]any {
	object, _ := usage.(map[string]any)
	prompt := integerOf(object["prompt_tokens"])
	completion := integerOf(object["completion_tokens"])
	return map[string]any{"input_tokens": prompt, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens": completion, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": prompt + completion}
}

func responsesID(requestID string) string {
	base := strings.ReplaceAll(requestID, "-", "")
	if base == "" {
		base = randomHex(16)
	}
	return "resp_" + base
}

func responsesSkeleton(request map[string]any, model, requestID string, createdAt int64) map[string]any {
	var instructions, maxOutput any
	if value, ok := request["instructions"].(string); ok {
		instructions = value
	}
	if value, ok := positiveInteger(request["max_output_tokens"]); ok {
		maxOutput = value
	}
	toolChoice := request["tool_choice"]
	if toolChoice == nil {
		toolChoice = "auto"
	}
	tools, _ := request["tools"].([]any)
	if tools == nil {
		tools = []any{}
	}
	metadata, _ := request["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	return map[string]any{
		"id":                   responsesID(requestID),
		"object":               "response",
		"created_at":           createdAt,
		"status":               "in_progress",
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         instructions,
		"max_output_tokens":    maxOutput,
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  request["parallel_tool_calls"] != false,
		"previous_response_id": nil,
		"reasoning":            nil,
		"store":                false,
		"temperature":          request["temperature"],
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          toolChoice,
		"tools":                tools,
		"top_p":                request["top_p"],
		"truncation":           "disabled",
		"usage":                nil,
		"user":                 nil,
		"metadata":             metadata,
	}
}

func responsesMessageItem(itemID, text, status string) map[string]any {
	return map[string]any{"id": itemID, "type": "message", "status": status, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func responsesCallItem(itemID string, call map[string]any, status string) map[string]any {
	function, _ := call["function"].(map[string]any)
	callID, _ := call["id"].(string)
	if callID == "" {
		callID = "call_" + randomHex(12)
	}
	name, _ := function["name"].(string)
	arguments, isString := function["arguments"].(string)
	if !isString {
		if function["arguments"] == nil {
			arguments = ""
		} else {
			encoded, _ := json.Marshal(function["arguments"])
			arguments = string(encoded)
		}
	}
	return map[string]any{"id": itemID, "type": "function_call", "status": status, "call_id": callID, "name": name, "arguments": arguments}
}

// responsesFromChat is the Response object for a finished chat completion.
func responsesFromChat(completion, request map[string]any, model, requestID string, createdAt int64) map[string]any {
	response := responsesSkeleton(request, model, requestID, createdAt)
	base := strings.TrimPrefix(response["id"].(string), "resp_")
	choice := firstChoice(completion)
	message, _ := choice["message"].(map[string]any)
	output := []any{}
	if text, ok := message["content"].(string); ok && text != "" {
		output = append(output, responsesMessageItem("msg_"+base, text, "completed"))
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for position, call := range calls {
			if object, ok := call.(map[string]any); ok {
				output = append(output, responsesCallItem(fmt.Sprintf("fc_%s_%d", base, position), object, "completed"))
			}
		}
	}
	if len(output) == 0 {
		output = append(output, responsesMessageItem("msg_"+base, "", "completed"))
	}
	response["output"] = output
	if choice["finish_reason"] == "length" {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	} else {
		response["status"] = "completed"
	}
	response["usage"] = responsesUsage(completion["usage"])
	return response
}

// responsesStream turns a chat completion stream into Responses events, ending in
// response.completed with the whole response. Start, then Push per chunk, then Finish.
type responsesStream struct {
	response  map[string]any
	base      string
	sequence  int64
	output    []map[string]any
	textIndex int // output index of the open message item, -1 for none
	textParts []string
	calls     map[int64]int // tool call position in the chunk stream -> output index
	arguments map[int64][]string
	order     []int64 // call positions in the order they opened
	finish    string
	usage     map[string]any
}

func newResponsesStream(request map[string]any, model, requestID string, createdAt int64) *responsesStream {
	response := responsesSkeleton(request, model, requestID, createdAt)
	return &responsesStream{response: response, base: strings.TrimPrefix(response["id"].(string), "resp_"), textIndex: -1, calls: map[int64]int{}, arguments: map[int64][]string{}, usage: responsesUsage(nil)}
}

func (s *responsesStream) event(name string, payload map[string]any) formatEvent {
	s.sequence++
	data := make(map[string]any, len(payload)+2)
	for key, value := range payload {
		data[key] = value
	}
	data["type"] = name
	data["sequence_number"] = s.sequence
	return formatEvent{Name: name, Data: data}
}

func (s *responsesStream) snapshot() map[string]any {
	copied := make(map[string]any, len(s.response))
	for key, value := range s.response {
		copied[key] = value
	}
	return copied
}

func copyItem(item map[string]any) map[string]any {
	copied := make(map[string]any, len(item))
	for key, value := range item {
		copied[key] = value
	}
	return copied
}

func (s *responsesStream) Start() []formatEvent {
	return []formatEvent{s.event("response.created", map[string]any{"response": s.snapshot()}), s.event("response.in_progress", map[string]any{"response": s.snapshot()})}
}

func (s *responsesStream) closeText() []formatEvent {
	if s.textIndex < 0 {
		return nil
	}
	text := strings.Join(s.textParts, "")
	item := s.output[s.textIndex]
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	part["text"] = text
	item["status"] = "completed"
	index := s.textIndex
	s.textIndex = -1
	return []formatEvent{
		s.event("response.output_text.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": 0, "text": text, "logprobs": []any{}}),
		s.event("response.content_part.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": 0, "part": part}),
		s.event("response.output_item.done", map[string]any{"output_index": index, "item": item}),
	}
}

func (s *responsesStream) Push(chunk map[string]any) []formatEvent {
	var events []formatEvent
	if usage, ok := chunk["usage"].(map[string]any); ok {
		s.usage = responsesUsage(usage)
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
		if s.textIndex < 0 {
			s.textIndex = len(s.output)
			item := responsesMessageItem(fmt.Sprintf("msg_%s_%d", s.base, s.textIndex), "", "in_progress")
			s.output = append(s.output, item)
			s.textParts = s.textParts[:0]
			added := copyItem(item)
			added["content"] = []any{}
			events = append(events,
				s.event("response.output_item.added", map[string]any{"output_index": s.textIndex, "item": added}),
				s.event("response.content_part.added", map[string]any{"item_id": item["id"], "output_index": s.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
		}
		s.textParts = append(s.textParts, text)
		events = append(events, s.event("response.output_text.delta", map[string]any{"item_id": s.output[s.textIndex]["id"], "output_index": s.textIndex, "content_index": 0, "delta": text, "logprobs": []any{}}))
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, call := range calls {
			object, ok := call.(map[string]any)
			if !ok {
				continue
			}
			position := int64(len(s.calls))
			if value, ok := object["index"]; ok {
				if number, isNumber := value.(float64); isNumber {
					position = int64(number)
				} else if number, isNumber := value.(json.Number); isNumber {
					position, _ = number.Int64()
				}
			}
			function, _ := object["function"].(map[string]any)
			if _, known := s.calls[position]; !known {
				events = append(events, s.closeText()...)
				s.calls[position] = len(s.output)
				s.order = append(s.order, position)
				s.arguments[position] = nil
				name, _ := function["name"].(string)
				item := responsesCallItem(fmt.Sprintf("fc_%s_%d", s.base, position), map[string]any{"id": object["id"], "function": map[string]any{"name": name, "arguments": ""}}, "in_progress")
				s.output = append(s.output, item)
				events = append(events, s.event("response.output_item.added", map[string]any{"output_index": s.calls[position], "item": copyItem(item)}))
			}
			if piece, ok := function["arguments"].(string); ok && piece != "" {
				s.arguments[position] = append(s.arguments[position], piece)
				events = append(events, s.event("response.function_call_arguments.delta", map[string]any{"item_id": s.output[s.calls[position]]["id"], "output_index": s.calls[position], "delta": piece}))
			}
		}
	}
	if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
		s.finish = reason
	}
	return events
}

func (s *responsesStream) Finish() []formatEvent {
	events := s.closeText()
	for _, position := range s.order {
		index := s.calls[position]
		item := s.output[index]
		item["arguments"] = strings.Join(s.arguments[position], "")
		item["status"] = "completed"
		events = append(events,
			s.event("response.function_call_arguments.done", map[string]any{"item_id": item["id"], "output_index": index, "arguments": item["arguments"]}),
			s.event("response.output_item.done", map[string]any{"output_index": index, "item": item}))
	}
	if len(s.output) == 0 {
		item := responsesMessageItem(fmt.Sprintf("msg_%s_0", s.base), "", "completed")
		s.output = append(s.output, item)
		added := copyItem(item)
		added["content"] = []any{}
		events = append(events,
			s.event("response.output_item.added", map[string]any{"output_index": 0, "item": added}),
			s.event("response.output_item.done", map[string]any{"output_index": 0, "item": item}))
	}
	output := make([]any, 0, len(s.output))
	for _, item := range s.output {
		output = append(output, item)
	}
	s.response["output"] = output
	s.response["usage"] = s.usage
	if s.finish == "length" {
		s.response["status"] = "incomplete"
		s.response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		events = append(events, s.event("response.incomplete", map[string]any{"response": s.response}))
	} else {
		s.response["status"] = "completed"
		events = append(events, s.event("response.completed", map[string]any{"response": s.response}))
	}
	return events
}

// Abort ends a stream that did not complete: the format's error event, in sequence, so the tool
// never takes a cut answer for a whole one.
func (s *responsesStream) Abort(message string) []formatEvent {
	return []formatEvent{s.event("error", map[string]any{"code": "incomplete_stream", "message": message, "param": nil})}
}

var responsesErrorKinds = map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 411: "invalid_request_error", 413: "invalid_request_error", 415: "invalid_request_error", 429: "rate_limit_error", 500: "server_error", 502: "server_error", 503: "server_error", 504: "server_error"}

func responsesErrorBody(status int, code, message string) map[string]any {
	kind, known := responsesErrorKinds[status]
	if !known {
		kind = "server_error"
	}
	if message == "" {
		message = strings.ReplaceAll(code, "_", " ")
	}
	return map[string]any{"error": map[string]any{"type": kind, "code": code, "message": message, "param": nil}}
}

// nowUnix is the moment for a Response object's created_at.
func nowUnix(now func() time.Time) int64 {
	if now == nil {
		return time.Now().Unix()
	}
	return now().Unix()
}
