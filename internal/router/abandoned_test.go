package router

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A call to a host outlives the customer who asked (27 November 2026): the host reads the prompt and writes its
// answer whether or not anybody waits, and bills both.

func leave(t *testing.T, h *harness, body string, after time.Duration) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testKey)
	impatient := &http.Client{Timeout: after}
	if response, err := impatient.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("the customer must have given up")
	}
}

func TestAWholeAnswerTheCustomerDidNotWaitForIsChargedAsTheHostCountedIt(t *testing.T) {
	var finished atomic.Bool
	host := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// a host that answers a whole request when it is done, half a second after the customer has gone
		select {
		case <-time.After(700 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		finished.Store(true)
		okJSON(`{"prompt_tokens":4000,"completion_tokens":300,"total_tokens":4300}`)(w, r)
	})
	h := newHarness(t, routeFor(upstream("deepseek/api", host.server.URL, 1)), "")
	before := h.balance(t)
	leave(t, h, `{"model":"deepseek/deepseek-v4.1-flash","max_tokens":400,"messages":[{"role":"user","content":"`+strings.Repeat("palabra ", 2000)+`"}]}`, 200*time.Millisecond)
	time.Sleep(900 * time.Millisecond)
	if !finished.Load() {
		t.Fatal("the call to the host was cancelled when the customer left")
	}
	// 4,000 prompt * 0.1575 + 300 output * 0.63 = 819 microUSD
	flushed(t, h.settlements)
	if charged := before - h.balance(t); charged != 819 {
		t.Fatalf("an answer the host wrote and nobody waited for was charged %d microUSD, want the 819 it cost", charged)
	}
}

func TestAStreamTheCustomerLeftBeforeItBeganIsClosedAndItsPromptCharged(t *testing.T) {
	var wrote atomic.Int64
	closed := make(chan struct{})
	host := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// a host that reads a long prompt for a while before its first word, then would write for a long time
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 200; i++ {
			if _, err := fmt.Fprint(w, "data: {\"id\":\"s\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"word \"}}]}\n\n"); err != nil {
				break
			}
			flusher.Flush()
			wrote.Add(1)
			select {
			case <-r.Context().Done():
				close(closed)
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	h := newHarness(t, routeFor(upstream("deepseek/api", host.server.URL, 1)), "")
	before := h.balance(t)
	prompt := strings.Repeat("palabra ", 2000) // 16,000 bytes: estimated at about 4,000 tokens
	leave(t, h, `{"model":"deepseek/deepseek-v4.1-flash","stream":true,"max_tokens":400,"messages":[{"role":"user","content":"`+prompt+`"}]}`, 150*time.Millisecond)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream nobody reads was left open")
	}
	if wrote.Load() > 20 {
		t.Fatalf("the host wrote %d events to nobody before the stream was closed", wrote.Load())
	}
	flushed(t, h.settlements)
	// the prompt as estimated, about 4,040 tokens at 0.1575 microUSD, and no output
	if charged := before - h.balance(t); charged < 600 || charged > 680 {
		t.Fatalf("a stream left before it began was charged %d microUSD, want the prompt's 640 or so", charged)
	}
}

func TestNoOtherUpstreamIsAskedForACustomerWhoHasLeft(t *testing.T) {
	var second atomic.Int64
	first := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		http.Error(w, `{"error":"overloaded"}`, http.StatusBadGateway)
	})
	backup := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		second.Add(1)
		okJSON(`{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}`)(w, r)
	})
	h := newHarness(t, routeFor(upstream("deepseek/api", first.server.URL, 1), upstream("backup", backup.server.URL, 2)), "")
	before := h.balance(t)
	leave(t, h, `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, 150*time.Millisecond)
	time.Sleep(900 * time.Millisecond)
	flushed(t, h.settlements)
	if second.Load() != 0 {
		t.Fatalf("the next upstream was asked %d times for a customer who had left", second.Load())
	}
	if charged := before - h.balance(t); charged != 0 {
		t.Fatalf("a request no host took was charged %d", charged)
	}
	// and with the customer waiting, the next upstream is asked as before
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+testKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get(upstreamHeader) != "backup" {
		t.Fatalf("a customer who waits: %v, status %d via %q", err, response.StatusCode, response.Header.Get(upstreamHeader))
	}
	response.Body.Close()
}
