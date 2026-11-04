package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuffer is a log destination a test can read while the notifier writes to it.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestTheBodyFollowsTheWebhook(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   map[string]string
	}{
		{"https://api.telegram.org/bot123:abc/sendMessage", map[string]string{"chat_id": "-100", "text": "router a: hola"}},
		{"https://discord.com/api/webhooks/1/xyz", map[string]string{"content": "router a: hola"}},
		{"https://hooks.slack.com/services/T/B/C", map[string]string{"text": "router a: hola"}},
	} {
		n, err := New("router a", tc.target, "-100", log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatalf("%s: %v", tc.target, err)
		}
		got := n.body("router a: hola")
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %v", tc.target, got)
		}
		for key, value := range tc.want {
			if got[key] != value {
				t.Fatalf("%s: %v", tc.target, got)
			}
		}
	}
}

func TestAlertsAreLoggedAndPosted(t *testing.T) {
	received := make(chan map[string]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		received <- body
	}))
	defer server.Close()
	var logs logBuffer
	n, err := New("router namecheap-1", server.URL+"/hook", "", log.New(&logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	n.Notify("las liquidaciones van con retraso")
	select {
	case body := <-received:
		if body["text"] != "router namecheap-1: las liquidaciones van con retraso" {
			t.Fatalf("posted %v", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook got nothing")
	}
	if !strings.Contains(logs.String(), "ALERT router namecheap-1: las liquidaciones van con retraso") {
		t.Fatalf("log %q", logs.String())
	}
}

// The token travels in the URL, so no path to the log may carry it: the refusal at start for a
// URL that cannot be used (a trailing CR from a CRLF env file, a bad port, a stray space, a bad
// escape, a missing chat), and every failure to send (no such host, nothing listening, a slow or
// failing webhook).
func TestTheWebhookSecretNeverReachesTheLogs(t *testing.T) {
	const secret = "SECRET-token-123456"
	for _, target := range []string{
		"https://api.telegram.org/bot" + secret + "/sendMessage\r",
		"https://api.telegram.org:443x/bot" + secret + "/sendMessage",
		"https://api.telegram.org/bot " + secret + "/sendMessage",
		"https://api.telegram.org/bot%zz" + secret + "/sendMessage",
		"https://user:" + secret + "@hooks.example.com/x",
		"ftp://hooks.example.com/" + secret,
		"http://hooks.example.com/" + secret,
	} {
		if _, err := New("router a", target, "-100", log.New(io.Discard, "", 0)); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("%q must be refused without quoting it: %v", target, err)
		}
	}
	if _, err := New("router a", "https://api.telegram.org/bot"+secret+"/sendMessage", "", log.New(io.Discard, "", 0)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("a Telegram URL without its chat must be refused without quoting it: %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope "+r.URL.Path, 500) }))
	defer failing.Close()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	for _, target := range []string{
		"https://no-such-host.invalid/bot" + secret + "/sendMessage",
		closedURL + "/bot" + secret + "/sendMessage",
		slow.URL + "/bot" + secret + "/sendMessage",
		failing.URL + "/bot" + secret + "/sendMessage",
	} {
		var logs logBuffer
		n, err := New("router a", target, "", log.New(&logs, "", 0))
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		n.client.Timeout = 100 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			n.Run(ctx)
			close(done)
		}()
		n.Notify("las liquidaciones van con retraso")
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(logs.String(), "not delivered") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done
		if !strings.Contains(logs.String(), "alert webhook: not delivered") {
			t.Fatalf("%s: the failure must be logged: %q", target, logs.String())
		}
		if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "/bot") {
			t.Fatalf("the webhook's URL reached the log: %q", logs.String())
		}
	}
}

func TestWithoutAWebhookAlertsAreOnlyLogged(t *testing.T) {
	var logs logBuffer
	n, err := New("router x", "", "", log.New(&logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		n.Notify("algo")
	}
	if strings.Count(logs.String(), "ALERT router x: algo") != 100 {
		t.Fatal("every alert is logged")
	}
}
