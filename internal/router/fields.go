package router

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// What of a chat completion goes upstream (27 November 2026).
//
// A request used to go to its host as it came, apart from the few fields the router writes. A host reads more
// than the fields of a chat completion: features it sells apart (a web search on every call, a code interpreter,
// audio), routing of its own, options that make a picture cost more than the route holds for one, kinds of content
// the route has no price for. None of that is in a route's price and none of it was measured, so what a request
// may carry is written here. What is listed goes on as sent. A feature that is not offered is refused when it is
// asked for, so nobody takes an answer for what they asked. Any other field is left out, and the answer names it
// (X-Lebrel-Ignored-Fields). A message's content is text and images, and a tool is a function.

// forwardedFields go upstream as sent.
var forwardedFields = map[string]bool{
	// what the router reads or writes itself
	"model": true, "messages": true, "stream": true, "stream_options": true, "max_tokens": true, "max_completion_tokens": true, "n": true, "best_of": true,
	// sampling
	"temperature": true, "top_p": true, "top_k": true, "top_a": true, "min_p": true, "do_sample": true, "seed": true, "stop": true,
	"presence_penalty": true, "frequency_penalty": true, "repetition_penalty": true, "logit_bias": true, "logprobs": true, "top_logprobs": true,
	// tools and the shape of the answer
	"tools": true, "tool_choice": true, "parallel_tool_calls": true, "tool_stream": true, "functions": true, "function_call": true, "response_format": true,
	// reasoning, as each lab switches it; the output cap bounds it whatever these say
	"thinking": true, "enable_thinking": true, "thinking_budget": true, "preserve_thinking": true, "reasoning": true, "reasoning_effort": true, "include_reasoning": true,
	// who asks, for a host's own abuse checks and its cache
	"user": true, "user_id": true, "request_id": true, "prompt_cache_key": true,
}

// notOfferedFields are features a host sells apart, or answers in another form: a request asking for one is refused.
var notOfferedFields = map[string]string{
	"enable_search": "web search", "search_options": "web search", "web_search_options": "web search", "web_search": "web search",
	"plugins": "plugins", "enable_code_interpreter": "a code interpreter", "audio": "audio output", "prediction": "predicted outputs",
}

const (
	ignoredFieldsHeader = "X-Lebrel-Ignored-Fields"
	// ignoredFieldsNamed is how many left-out fields an answer names.
	ignoredFieldsNamed = 8
)

// plainFieldName is a field name an answer may repeat: an identifier, nothing a client could write a text into.
var plainFieldName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,39}$`)

// asked is false for the values that ask for nothing: absent, null, false, zero, empty.
func asked(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		text := fmt.Sprint(typed)
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return number != 0
		}
		return text != ""
	}
}

// chatPolicy settles what of a chat completion goes upstream, in place: it refuses what is not offered, makes
// every image part an address or inline picture and nothing more, and takes out the fields that are not listed,
// which it returns.
func chatPolicy(payload map[string]any) ([]string, error) {
	var left []string
	for name, value := range payload {
		if forwardedFields[name] {
			continue
		}
		if feature, sold := notOfferedFields[name]; sold && asked(value) {
			return nil, fmt.Errorf("%s asks for %s, which this API does not offer", name, feature)
		}
		if name == "modalities" {
			kinds, _ := value.([]any)
			for _, kind := range kinds {
				if kind != "text" {
					return nil, fmt.Errorf("modalities asks for more than text, which this API does not offer")
				}
			}
		}
		delete(payload, name)
		left = append(left, name)
	}
	sort.Strings(left)
	if tools, ok := payload["tools"].([]any); ok {
		for _, entry := range tools {
			tool, _ := entry.(map[string]any)
			if kind, named := tool["type"]; named && kind != "function" {
				return nil, fmt.Errorf("tools are functions: a tool of another type is not taken")
			}
		}
	}
	messages, _ := payload["messages"].([]any)
	for _, entry := range messages {
		message, _ := entry.(map[string]any)
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for i, entry := range parts {
			part, _ := entry.(map[string]any)
			switch part["type"] {
			case "text":
			case "image_url":
				var address string
				switch inner := part["image_url"].(type) {
				case map[string]any:
					address, _ = inner["url"].(string)
				case string:
					address = inner
				}
				if address == "" {
					return nil, fmt.Errorf("an image_url part needs a url")
				}
				// the picture and nothing else: an option beside it could make it cost more than the route holds for one
				parts[i] = map[string]any{"type": "image_url", "image_url": map[string]any{"url": address}}
			default:
				return nil, fmt.Errorf("a message's content is text and images: a part of another type is not taken")
			}
		}
	}
	return left, nil
}

// ignoredFieldsValue names the fields left out of a request, for the answer's header: plain names, a few of them.
func ignoredFieldsValue(left []string) string {
	named := make([]string, 0, ignoredFieldsNamed)
	others := 0
	for _, name := range left {
		if len(named) < ignoredFieldsNamed && plainFieldName.MatchString(name) {
			named = append(named, name)
		} else {
			others++
		}
	}
	value := strings.Join(named, ", ")
	if others > 0 {
		if value != "" {
			value += ", "
		}
		value += "and " + strconv.Itoa(others) + " more"
	}
	return value
}

// leftOutFields counts, for the operator, how often requests carry a field that is not forwarded. It keeps a
// number per name it already knows (the fields other APIs document) and one number for all the rest: a name a
// client made up is never kept.
type leftOutFields struct {
	mu     sync.Mutex
	counts map[string]int64
}

// knownUnlistedFields are fields other chat APIs document, which the router does not forward.
var knownUnlistedFields = map[string]bool{
	"models": true, "route": true, "transforms": true, "preset": true, "provider": true, "service_tier": true, "usage": true, "metadata": true, "store": true,
	"vl_high_resolution_images": true, "max_pixels": true, "min_pixels": true, "modalities": true, "verbosity": true, "structured_outputs": true,
	"incremental_output": true, "result_format": true, "translation_options": true, "typical_p": true, "min_tokens": true, "ignore_eos": true,
	"skip_special_tokens": true, "guided_json": true, "guided_regex": true, "guided_choice": true, "guided_grammar": true, "echo": true, "suffix": true,
	"safe_prompt": true, "random_seed": true, "profile": true, "chat_template_kwargs": true, "custom_params": true, "extra_body": true, "debug": true,
	"enable_search": true, "search_options": true, "web_search_options": true, "web_search": true, "plugins": true, "enable_code_interpreter": true,
	"audio": true, "prediction": true, "mirostat": true, "tfs": true, "num_ctx": true, "keep_alive": true, "options": true, "format": true,
}

func (l *leftOutFields) note(left []string) {
	if len(left) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil {
		l.counts = make(map[string]int64)
	}
	for _, name := range left {
		if !knownUnlistedFields[name] {
			name = "(other)"
		}
		l.counts[name]++
	}
}

func (l *leftOutFields) snapshot() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	copied := make(map[string]int64, len(l.counts))
	for name, count := range l.counts {
		copied[name] = count
	}
	return copied
}
