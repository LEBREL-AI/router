package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"lebrel-router/internal/meter"
)

// A one-pixel PNG, as a client sends a picture.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// tinyPDF is a one-page PDF whose page says text, in Helvetica: a document with a text layer.
func tinyPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 18 Tf 20 100 Td (%s) Tj ET", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func visionRoute() *Route {
	route := routeFor()
	route.ImageTokens, route.MaxImages = 1024, 4
	return &route
}

// mediaServer is a server whose fetches are answered from a table of addresses.
func mediaServer(files map[string][]byte) *Server {
	return &Server{mediaFetch: func(_ context.Context, address string) ([]byte, string, error) {
		data, ok := files[address]
		if !ok {
			return nil, "", formatErrorf("the file at %s answered HTTP 404", address)
		}
		return data, "", nil
	}}
}

func decodeChat(t *testing.T, raw string) map[string]any {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	return request
}

func contentOf(t *testing.T, message map[string]any) []map[string]any {
	t.Helper()
	parts, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("content is not a list of parts: %#v", message["content"])
	}
	out := make([]map[string]any, len(parts))
	for i, part := range parts {
		out[i] = part.(map[string]any)
	}
	return out
}

// Claude Code sends pictures and documents as Messages blocks, and the pictures its Read tool returns inside tool
// results: every one reaches the model in the chat format, images inline, documents as their text.
func TestClaudeCodeImagesAndDocumentsReachTheModel(t *testing.T) {
	pdfData := base64.StdEncoding.EncodeToString(tinyPDF("Hello Lebrel"))
	request := decodeChat(t, `{"model":"m","max_tokens":100000,"messages":[
		{"role":"user","content":[{"type":"text","text":"Look at these."},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+onePixelPNG+`"}},
			{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}},
			{"type":"document","title":"notes.txt","source":{"type":"text","media_type":"text/plain","data":"line one"}},
			{"type":"document","title":"report.pdf","source":{"type":"base64","media_type":"application/pdf","data":"`+pdfData+`"}}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"shot.png"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+onePixelPNG+`"}}]},
			{"type":"text","text":"What does the screenshot show?"}]}]}`)
	chat, err := anthropicToChat(request, 393_216)
	if err != nil {
		t.Fatal(err)
	}
	if chat["max_tokens"] != int64(100_000) {
		t.Fatalf("max_tokens = %v, the edition's limit is 393,216", chat["max_tokens"])
	}
	server := mediaServer(map[string][]byte{"https://example.com/cat.png": mustBase64(t, onePixelPNG)})
	if err := server.resolveMedia(context.Background(), chat, visionRoute()); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]map[string]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want user, assistant, tool, user", len(messages))
	}
	first := contentOf(t, messages[0])
	if first[0]["text"] != "Look at these." || first[1]["type"] != "image_url" || first[2]["type"] != "image_url" {
		t.Fatalf("first turn: %#v", first)
	}
	for _, index := range []int{1, 2} {
		url := first[index]["image_url"].(map[string]any)["url"].(string)
		if !strings.HasPrefix(url, "data:image/png;base64,") {
			t.Fatalf("image %d is not inline: %.60s", index, url)
		}
	}
	if text, _ := first[3]["text"].(string); text != "<document title=\"notes.txt\">\nline one\n</document>" {
		t.Fatalf("text document: %q", text)
	}
	if text, _ := first[4]["text"].(string); !strings.Contains(text, "Hello Lebrel") || !strings.HasPrefix(text, "<document title=\"report.pdf\">") {
		t.Fatalf("the PDF's text: %q", text)
	}
	if messages[2]["role"] != "tool" || messages[2]["content"] != "The tool returned an image; it follows in the next message." {
		t.Fatalf("tool message: %#v", messages[2])
	}
	last := contentOf(t, messages[3])
	if last[0]["text"] != "The images returned by the tool calls above:" || last[1]["type"] != "image_url" || last[2]["text"] != "What does the screenshot show?" {
		t.Fatalf("the user turn after the tool: %#v", last)
	}

	// A conversation without media is as it was: text alone, byte for byte (its prompt cache depends on it).
	plain, err := anthropicToChat(decodeChat(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.resolveMedia(context.Background(), plain, visionRoute()); err != nil {
		t.Fatal(err)
	}
	if got := plain["messages"].([]map[string]any)[0]["content"]; got != "a\nb" {
		t.Fatalf("text-only content = %#v", got)
	}

	// What cannot be read is refused with its reason.
	for name, block := range map[string]string{
		"files api":     `{"type":"image","source":{"type":"file","file_id":"file_1"}}`,
		"not a picture": `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8gd29ybGQ="}}`,
		"svg":           `{"type":"image","source":{"type":"base64","media_type":"image/svg+xml","data":"PHN2Zy8+"}}`,
		"word file":     `{"type":"document","source":{"type":"url","url":"ftp://example.com/a.docx"}}`,
	} {
		_, err := anthropicToChat(decodeChat(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[`+block+`]}]}`), 0)
		var invalid *formatError
		if err == nil || !errorsAs(err, &invalid) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
	}
	// A scanned PDF (no text layer) and an unreadable one are refused once read.
	for name, data := range map[string][]byte{"scan": tinyPDF(""), "damaged": []byte("%PDF-1.4 not really")} {
		chat, err := anthropicToChat(decodeChat(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"`+base64.StdEncoding.EncodeToString(data)+`"}}]}]}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.resolveMedia(context.Background(), chat, visionRoute()); err == nil {
			t.Fatalf("%s PDF: read", name)
		}
	}
}

// Codex sends pictures as input_image, files as input_file, and what a function returned as its output's parts.
func TestCodexImagesFilesAndFunctionImagesReachTheModel(t *testing.T) {
	pdf := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(tinyPDF("Quarterly figures"))
	request := decodeChat(t, `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Check this."},{"type":"input_image","image_url":"data:image/png;base64,`+onePixelPNG+`"},{"type":"input_file","filename":"q3.pdf","file_data":"`+pdf+`"}]},
		{"type":"function_call","call_id":"c1","name":"view","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"view","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"data:image/png;base64,`+onePixelPNG+`"}]},
		{"type":"function_call_output","call_id":"c2","output":"done"},
		{"type":"message","role":"user","content":"And now?"}]}`)
	chat, err := responsesToChat(request, 393_216, 32_768)
	if err != nil {
		t.Fatal(err)
	}
	if chat["max_tokens"] != int64(32_768) {
		t.Fatalf("Codex without max_output_tokens gets %v, want the edition's default 32,768", chat["max_tokens"])
	}
	if err := mediaServer(nil).resolveMedia(context.Background(), chat, visionRoute()); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]map[string]any)
	roles := []string{}
	for _, message := range messages {
		roles = append(roles, message["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,tool,user,user" {
		t.Fatalf("roles = %v: the function's image goes after the run of tool messages", roles)
	}
	first := contentOf(t, messages[0])
	if first[1]["type"] != "image_url" || !strings.Contains(first[2]["text"].(string), "Quarterly figures") {
		t.Fatalf("first turn: %#v", first)
	}
	if messages[2]["content"] != "The function returned an image; it follows in the next message." || messages[3]["content"] != "done" {
		t.Fatalf("tool messages: %#v %#v", messages[2], messages[3])
	}
	images := contentOf(t, messages[4])
	if images[0]["text"] != "The images returned by the function calls above:" || images[1]["type"] != "image_url" {
		t.Fatalf("the function's image: %#v", images)
	}
	if _, err := responsesToChat(decodeChat(t, `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file-1"}]}]}`), 0, 0); err == nil {
		t.Fatal("an image kept in OpenAI's Files API was accepted")
	}
}

// A model that reads no images is told so, never sent one it would drop unseen; one that reads them takes at most
// its number; an image by address is fetched once and goes on inline.
func TestImagesAreRefusedWhereTheModelReadsNoneAndCountedWhereItDoes(t *testing.T) {
	chat := func() map[string]any {
		return map[string]any{"messages": []map[string]any{{"role": "user", "content": []any{imagePart("data:image/png;base64," + onePixelPNG), imagePart("https://example.com/a.png")}}}}
	}
	blind := routeFor()
	err := mediaServer(nil).resolveMedia(context.Background(), chat(), &blind)
	if err == nil || !strings.Contains(err.Error(), "does not read images") {
		t.Fatalf("a model without images: %v", err)
	}
	few := visionRoute()
	few.MaxImages = 1
	if err := mediaServer(map[string][]byte{"https://example.com/a.png": mustBase64(t, onePixelPNG)}).resolveMedia(context.Background(), chat(), few); err == nil || !strings.Contains(err.Error(), "at most 1 images") {
		t.Fatalf("too many images: %v", err)
	}
	if err := mediaServer(nil).resolveMedia(context.Background(), chat(), visionRoute()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a missing file: %v", err)
	}
}

// A picture's base64 is no text: the estimate leaves it out and counts the image apart.
func TestTheMeterCountsImagesApartFromText(t *testing.T) {
	big := strings.Repeat("A", 4<<20)
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}]}]}`
	_, info, err := meter.InspectAndRequireUsage("/v1/chat/completions", []byte(body), 512, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if info.Images != 1 || info.EstimatedPromptTokens > 100 {
		t.Fatalf("images %d, estimate %d: the image's data was read as text", info.Images, info.EstimatedPromptTokens)
	}
}

// Through the router: a request with an image to a model that reads none is refused before anyone is asked; one
// to a model that reads them goes only to the upstreams marked to read them, held at the images' cost.
func TestImageRequestsGoOnlyToUpstreamsThatReadThem(t *testing.T) {
	blindUpstream := newFakeUpstream(t, okJSON(`{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`))
	seeing := newFakeUpstream(t, okJSON(`{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`))
	first, second := upstream("blind", blindUpstream.server.URL, 1), upstream("seeing", seeing.server.URL, 2)
	second.Images = true
	route := routeFor(first, second)
	route.ImageTokens, route.MaxImages = 1024, 4
	h := newHarness(t, route, "")
	image := `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"what is it"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + onePixelPNG + `"}}]}]}`
	response := h.post(t, image, testKey)
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("image request: %d", response.StatusCode)
	}
	if len(blindUpstream.requests) != 0 || len(seeing.requests) != 1 {
		t.Fatalf("asked blind %d, seeing %d: an image goes only where it is read", len(blindUpstream.requests), len(seeing.requests))
	}
	text := h.post(t, `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, testKey)
	io.Copy(io.Discard, text.Body)
	text.Body.Close()
	if len(blindUpstream.requests) != 1 {
		t.Fatal("a text request still goes to the first upstream")
	}

	blindRoute := routeFor(upstream("blind", blindUpstream.server.URL, 1))
	refused := newHarness(t, blindRoute, "").post(t, image, testKey)
	var body map[string]any
	_ = json.NewDecoder(refused.Body).Decode(&body)
	refused.Body.Close()
	message, _ := body["error"].(map[string]any)["message"].(string)
	if refused.StatusCode != http.StatusBadRequest || !strings.Contains(message, "does not read images") {
		t.Fatalf("a model without images: %d %q", refused.StatusCode, message)
	}
}

// An address is fetched only on the public internet: never the machine itself, a private network, the cloud's
// metadata service, or shared and reserved space.
func TestFetchesReachOnlyPublicAddresses(t *testing.T) {
	for address, public := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true, "127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false,
		"198.18.0.1": false, "224.0.0.1": false,
	} {
		if got := publicAddress(netip.MustParseAddr(address)); got != public {
			t.Fatalf("%s public = %v, want %v", address, got, public)
		}
	}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(mustBase64(t, onePixelPNG)) }))
	defer local.Close()
	server := &Server{mediaClient: newMediaClient()}
	_, _, err := server.fetchMedia(context.Background(), local.URL+"/a.png")
	if err == nil || !strings.Contains(err.Error(), "not on the public internet") {
		t.Fatalf("a fetch of the machine itself: %v", err)
	}
	if _, _, err := server.fetchMedia(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("a file: address was fetched")
	}
}

func mustBase64(t *testing.T, value string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func errorsAs(err error, target **formatError) bool {
	invalid, ok := err.(*formatError)
	if ok {
		*target = invalid
	}
	return ok
}
