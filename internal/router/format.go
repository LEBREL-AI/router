package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// The two request formats coding agents speak beside OpenAI chat completions: the Anthropic Messages
// API (Claude Code) at /v1/messages and the OpenAI Responses API (Codex) at /v1/responses. Each
// request is translated into one chat completion, run through the router as any other (an edition's
// or a routed model's), and translated back, streamed as that format's events when asked. The local
// proxy did this on the customer's machine; it happens here now, so a tool points straight at the
// router with its own format.

const (
	anthropicMessagesPath = "/v1/messages"
	anthropicCountPath    = "/v1/messages/count_tokens"
	responsesPath         = "/v1/responses"
	// formatScanLine bounds one line of the inner stream (one chat completion chunk).
	formatScanLine = 4 << 20
)

type formatKind string

const (
	formatAnthropic formatKind = "anthropic"
	formatResponses formatKind = "responses"
)

// formatPath says which format a path asks for, and whether it is the token count.
func formatPath(path string) (format formatKind, count bool, ok bool) {
	switch path {
	case anthropicMessagesPath:
		return formatAnthropic, false, true
	case anthropicCountPath:
		return formatAnthropic, true, true
	case responsesPath:
		return formatResponses, false, true
	}
	return "", false, false
}

func (s *Server) writeFormatError(w http.ResponseWriter, format formatKind, status int, code, message string) {
	var body map[string]any
	if format == formatAnthropic {
		body = anthropicErrorBody(status, code, message)
	} else {
		body = responsesErrorBody(status, code, message)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// captureWriter receives the inner chat completion's answer: its status and headers once, its body
// through a pipe the translator reads as it is written.
type captureWriter struct {
	header  http.Header
	status  int
	once    sync.Once
	headers chan struct{}
	writer  *io.PipeWriter
}

func newCaptureWriter() (*captureWriter, *io.PipeReader) {
	reader, writer := io.Pipe()
	return &captureWriter{header: http.Header{}, headers: make(chan struct{}), writer: writer}, reader
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(status int) {
	c.once.Do(func() {
		c.status = status
		close(c.headers)
	})
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.writer.Write(p)
}

func (c *captureWriter) Flush() {}

// finish ends the inner answer: headers signalled even when nothing was written, the pipe closed.
func (c *captureWriter) finish() {
	c.WriteHeader(http.StatusOK)
	c.writer.Close()
}

// handleFormatted serves a Messages or Responses request: translated to a chat completion, run through
// the ordinary completion handler, translated back.
func (s *Server) handleFormatted(w http.ResponseWriter, r *http.Request, scoped *Route, format formatKind, count bool) {
	s.active.Add(1)
	defer s.active.Add(-1)
	outerID := newRequestID()
	s.privacyHeaders(w, outerID)
	if !s.admit(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		s.writeFormatError(w, format, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.settings.MaxRequestBytes))
	if err != nil {
		s.writeFormatError(w, format, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil || request == nil {
		s.writeFormatError(w, format, http.StatusBadRequest, "invalid_request_error", "the request body must be a JSON object")
		return
	}
	model, _ := request["model"].(string)
	route, ok := s.routes[strings.TrimSpace(model)]
	if !ok || (scoped != nil && scoped != route) {
		s.writeFormatError(w, format, http.StatusNotFound, "model_not_found", "unknown model")
		return
	}
	var maxOutput int64
	if route.Kind != routeKindEdition {
		maxOutput = route.MaxOutputTokens
	}
	if count {
		estimate, err := anthropicCountTokensEstimate(request)
		if err != nil {
			s.writeFormatError(w, format, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": estimate})
		return
	}
	var chat map[string]any
	if format == formatAnthropic {
		chat, err = anthropicToChat(request, maxOutput)
	} else {
		chat, err = responsesToChat(request, maxOutput)
	}
	if err != nil {
		var invalid *formatError
		if errors.As(err, &invalid) {
			s.writeFormatError(w, format, http.StatusBadRequest, "invalid_request_error", invalid.message)
			return
		}
		s.writeFormatError(w, format, http.StatusInternalServerError, "internal_error", "the request could not be translated")
		return
	}
	chat["model"] = route.ModelID
	stream := chat["stream"] == true
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(chat); err != nil {
		s.writeFormatError(w, format, http.StatusBadRequest, "invalid_request_error", "the request could not be encoded")
		return
	}
	inner, err := http.NewRequestWithContext(r.Context(), http.MethodPost, completionPath, bytes.NewReader(body.Bytes()))
	if err != nil {
		s.writeFormatError(w, format, http.StatusInternalServerError, "internal_error", "the request could not be built")
		return
	}
	inner.Host = r.Host
	inner.RemoteAddr = r.RemoteAddr
	inner.TLS = r.TLS
	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Set("Accept", "application/json")
	if stream {
		inner.Header.Set("Accept", "text/event-stream")
	}
	for _, name := range []string{"Authorization", edgeAuthorizationHeader, "X-Lebrel-Service-Tier"} {
		if value := r.Header.Get(name); value != "" {
			inner.Header.Set(name, value)
		}
	}
	if s.settings.ClientIPHeader != "" {
		for _, value := range r.Header.Values(s.settings.ClientIPHeader) {
			inner.Header.Add(s.settings.ClientIPHeader, value)
		}
	}
	capture, reader := newCaptureWriter()
	go func() {
		defer capture.finish()
		s.handleCompletion(capture, inner, nil)
	}()
	defer reader.Close()
	select {
	case <-capture.headers:
	case <-r.Context().Done():
		return
	}
	if capture.status != http.StatusOK {
		limited, _ := io.ReadAll(io.LimitReader(reader, 64<<10))
		code, message := plaintextEdgeError(limited)
		if code == "unencrypted_transport_error" {
			code, message = "upstream_error", "the request could not be served"
		}
		if retry := capture.header.Get("Retry-After"); retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		s.writeFormatError(w, format, capture.status, code, message)
		return
	}
	for _, name := range []string{"X-Request-ID", upstreamHeader, verificationHeader, receiptHeader, contentRetentionHeader, editionRequestIDHeader} {
		if value := capture.header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	requestID := capture.header.Get(editionRequestIDHeader)
	if requestID == "" {
		requestID = capture.header.Get("X-Request-ID")
	}
	if !stream {
		raw, err := io.ReadAll(io.LimitReader(reader, 32<<20))
		if err != nil {
			s.writeFormatError(w, format, http.StatusBadGateway, "upstream_error", "the answer could not be read")
			return
		}
		var completion map[string]any
		if err := json.Unmarshal(raw, &completion); err != nil || completion == nil {
			s.writeFormatError(w, format, http.StatusBadGateway, "upstream_error", "the answer is not a completion")
			return
		}
		var translated map[string]any
		if format == formatAnthropic {
			translated = anthropicFromChat(completion, route.ModelID, requestID)
		} else {
			translated = responsesFromChat(completion, request, route.ModelID, requestID, nowUnix(s.settings.Now))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(translated)
		return
	}
	var translator interface {
		Start() []formatEvent
		Push(map[string]any) []formatEvent
		Finish() []formatEvent
	}
	if format == formatAnthropic {
		translator = newAnthropicStream(route.ModelID, requestID)
	} else {
		translator = newResponsesStream(request, route.ModelID, requestID, nowUnix(s.settings.Now))
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(events []formatEvent) bool {
		for _, event := range events {
			data, err := json.Marshal(event.Data)
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Name, data); err != nil {
				return false
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if !write(translator.Start()) {
		return
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), formatScanLine)
	var data []string
	done := false
	flush := func() bool {
		if len(data) == 0 {
			return true
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if payload == "[DONE]" {
			done = true
			return true
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil || chunk == nil {
			return true
		}
		return write(translator.Push(chunk))
	}
	for !done && scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if !flush() {
				return
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if !done {
		if !flush() {
			return
		}
	}
	write(translator.Finish())
}
