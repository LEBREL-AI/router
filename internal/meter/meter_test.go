package meter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// The estimate behind every hold measures a request whole (27 November 2026). These tests say that no place in
// a request holds text for less than a message does, and that only a picture's own data is left out.

const estimateTestPath = "/v1/chat/completions"

func inspect(t *testing.T, payload map[string]any) RequestInfo {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	_, info, err := InspectAndRequireUsage(estimateTestPath, body, 512, 8192)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return info
}

func request(extra map[string]any, messages ...any) map[string]any {
	payload := map[string]any{"model": "lab/model", "max_tokens": 1, "messages": messages}
	for key, value := range extra {
		payload[key] = value
	}
	return payload
}

func user(content any) map[string]any {
	return map[string]any{"role": "user", "content": content}
}

func tool(parameters map[string]any) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": parameters}}
}

func TestTextCostsTheSameWhereverItSits(t *testing.T) {
	document := strings.Repeat("palabra ", 50_000) // 400,000 bytes, about 100,000 tokens
	digits := strings.Repeat("7", len(document))
	fair := int64(len(document)) / 4
	control := inspect(t, request(nil, user(document)))
	if control.EstimatedPromptTokens < fair || control.EstimatedPromptTokens > fair+64 {
		t.Fatalf("a document in a message is estimated at %d tokens, want about %d", control.EstimatedPromptTokens, fair)
	}
	names := map[string]any{}
	for i := 0; i+64 <= len(document); i += 64 {
		names[fmt.Sprintf("%06d", i)+document[i:i+58]] = map[string]any{}
	}
	var number any = json.Number(digits)
	cases := map[string]map[string]any{
		"a tool's parameter name":                      request(map[string]any{"tools": []any{tool(map[string]any{"type": "object", "properties": map[string]any{document: map[string]any{}}})}}, user("hi")),
		"many parameter names":                         request(map[string]any{"tools": []any{tool(map[string]any{"type": "object", "properties": names})}}, user("hi")),
		"a field nobody documents":                     request(map[string]any{"notes": document}, user("hi")),
		"the name of a field":                          request(map[string]any{document: true}, user("hi")),
		"a request marked as an image":                 request(map[string]any{"type": "image_url"}, user(document)),
		"a message marked as an image":                 request(nil, map[string]any{"role": "user", "type": "image_url", "content": document}),
		"a tool marked as an image":                    request(map[string]any{"tools": []any{map[string]any{"type": "image_url", "function": map[string]any{"name": "lookup", "description": document}}}}, user("hi")),
		"text beside an image's address":               request(nil, user([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}, "text": document}})),
		"an image's address":                           request(nil, user([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": document}}})),
		"a data: URL that is no picture":               request(nil, user([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:text/plain;base64," + strings.ReplaceAll(document, " ", "+")}}})),
		"a data: URL whose data is not base64":         request(nil, user([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + document + "!"}}})),
		"a data: URL with a long header":               request(nil, user([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;note=" + document + ";base64,AAAA"}}})),
		"an SVG, which is text":                        request(nil, user([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/svg+xml;base64," + strings.ReplaceAll(document, " ", "+")}}})),
		"a number":                                     request(map[string]any{"tools": []any{tool(map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "number", "default": number}}})}}, user("hi")),
		"the options of a stream":                      request(map[string]any{"stream": true, "stream_options": map[string]any{"note": document}}, user("hi")),
		"a response schema's field names":              request(map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "schema": map[string]any{"type": "object", "properties": names}}}}, user("hi")),
		"a message's content given as a number":        request(nil, user(number)),
		"a message's name":                             request(nil, map[string]any{"role": "user", "name": document, "content": "hi"}),
		"the arguments of a tool call":                 request(nil, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": document}}}}, map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"}),
		"an image part outside a message":              request(map[string]any{"attachments": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.ReplaceAll(document, " ", "+")}}}}, user("hi")),
		"an image part nested inside a message's part": request(nil, user([]any{map[string]any{"type": "text", "text": "hi", "more": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.ReplaceAll(document, " ", "+")}}}}})),
	}
	for name, payload := range cases {
		info := inspect(t, payload)
		if info.EstimatedPromptTokens < fair {
			t.Errorf("%s: estimated at %d tokens, a message carrying the same text at %d", name, info.EstimatedPromptTokens, control.EstimatedPromptTokens)
		}
	}
}

func TestOnlyAPicturesDataIsLeftOut(t *testing.T) {
	data := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAA", 40_000) // 960,000 characters of base64
	part := func(url any) map[string]any {
		return map[string]any{"type": "image_url", "image_url": url}
	}
	for name, url := range map[string]any{
		"a picture":                         map[string]any{"url": "data:image/png;base64," + data},
		"a picture with a detail":           map[string]any{"url": "data:image/jpeg;base64," + data, "detail": "high"},
		"a picture whose type is uppercase": map[string]any{"url": "DATA:IMAGE/PNG;BASE64," + data},
		"a picture in lines":                map[string]any{"url": "data:image/png;base64," + strings.ReplaceAll(data, "iVBOR", "iVBOR\r\n")},
		"a picture in URL-safe base64":      map[string]any{"url": "data:image/webp;base64," + strings.ReplaceAll(data, "0K", "-_")},
		"a picture given as the url alone":  "data:image/gif;base64," + data,
	} {
		info := inspect(t, request(nil, user([]any{map[string]any{"type": "text", "text": "what is this?"}, part(url)})))
		if info.Images != 1 {
			t.Errorf("%s: %d images counted, want 1", name, info.Images)
		}
		if info.EstimatedPromptTokens > 100 {
			t.Errorf("%s: its data was read as text, %d tokens", name, info.EstimatedPromptTokens)
		}
	}
	// by address: an image all the same, and its address is text
	info := inspect(t, request(nil, user([]any{part(map[string]any{"url": "https://example.com/picture.png"})})))
	if info.Images != 1 || info.EstimatedPromptTokens > 100 {
		t.Fatalf("an image by address: %d images, %d tokens", info.Images, info.EstimatedPromptTokens)
	}
	// two pictures in two messages are two images
	info = inspect(t, request(nil, user([]any{part(map[string]any{"url": "data:image/png;base64," + data})}), user([]any{part(map[string]any{"url": "data:image/png;base64," + data})})))
	if info.Images != 2 || info.EstimatedPromptTokens > 100 {
		t.Fatalf("two pictures: %d images, %d tokens", info.Images, info.EstimatedPromptTokens)
	}
}

func TestAnImageIsOneOnlyInAMessagesContent(t *testing.T) {
	picture := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}}
	for name, payload := range map[string]map[string]any{
		"the request itself":       request(map[string]any{"type": "image_url"}, user("hi")),
		"a message":                request(nil, map[string]any{"role": "user", "type": "image_url", "content": "hi"}),
		"a tool":                   request(map[string]any{"tools": []any{picture}}, user("hi")),
		"a field of the request":   request(map[string]any{"attachment": picture}, user("hi")),
		"a part nested in a part":  request(nil, user([]any{map[string]any{"type": "text", "text": "hi", "more": []any{picture}}})),
		"a message's other fields": request(nil, map[string]any{"role": "user", "content": "hi", "attachments": []any{picture}}),
	} {
		if info := inspect(t, payload); info.Images != 0 {
			t.Errorf("%s marked image_url counted as %d images", name, info.Images)
		}
	}
}

func TestTheMeasureIsTheRequestWrittenCompactly(t *testing.T) {
	for _, source := range []string{
		`{"model":"lab/model","messages":[{"role":"user","content":"hi there"}],"temperature":0.25,"stop":["a","b"],"logprobs":false,"user":null}`,
		`{"a":{"b":{"c":[1,2,3,{"d":"e"}]}},"f":[],"g":{},"h":"ñandú 漢字 🙂"}`,
		`[]`, `{}`, `"text"`, `12345`, `true`, `false`, `null`,
	} {
		decoder := json.NewDecoder(strings.NewReader(source))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		encoder := json.NewEncoder(&compact)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
		written := int64(compact.Len() - 1) // the encoder ends with a line break
		got := jsonTextBytes(value)
		// one separator is counted after every member and every item, the last included
		if got < written || got > written+int64(strings.Count(source, "{")+strings.Count(source, "[")) {
			t.Errorf("%s measured at %d bytes, written compactly it is %d", source, got, written)
		}
	}
	// what the router writes into a request itself is measured as the encoder writes it
	if got := jsonTextBytes(map[string]any{"max_tokens": int64(32768), "n": 7}); got != int64(len(`{"max_tokens":32768,"n":7}`))+1 {
		t.Fatalf("router-set numbers measured at %d", got)
	}
}

// Text hidden at random depths, as names, values and numbers: the estimate never falls under a quarter of its bytes.
func TestNoShapeOfRequestHidesText(t *testing.T) {
	random := rand.New(rand.NewSource(20261127))
	var build func(depth int, budget *int) any
	word := func(budget *int) string {
		size := 1 + random.Intn(4000)
		*budget += size
		return strings.Repeat("x", size)
	}
	build = func(depth int, budget *int) any {
		switch choice := random.Intn(8); {
		case depth > 5 || choice < 2:
			return word(budget)
		case choice == 2:
			size := 1 + random.Intn(300)
			*budget += size
			return json.Number(strings.Repeat("9", size))
		case choice == 3:
			return random.Intn(2) == 0
		case choice < 6:
			items := make([]any, random.Intn(5))
			for i := range items {
				items[i] = build(depth+1, budget)
			}
			return items
		default:
			object := map[string]any{}
			for i, count := 0, random.Intn(5); i < count; i++ {
				object[word(budget)+fmt.Sprint(i)] = build(depth+1, budget)
			}
			if random.Intn(3) == 0 {
				object["type"] = "image_url"
			}
			return object
		}
	}
	for round := 0; round < 300; round++ {
		hidden := 0
		payload := request(map[string]any{"extra": build(0, &hidden), "tools": []any{build(0, &hidden)}}, user(build(0, &hidden)), build(0, &hidden))
		info := inspect(t, payload)
		if info.EstimatedPromptTokens*4 < int64(hidden) {
			t.Fatalf("round %d: %d bytes of text estimated at %d tokens", round, hidden, info.EstimatedPromptTokens)
		}
	}
}
