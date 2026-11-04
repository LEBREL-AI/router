package controlplane

import (
	"net/http"
	"testing"
	"time"
)

// The connections to the platform are health-checked: one silent for 3 s is pinged and closed if the
// ping goes unanswered, so the calls behind a dead connection go out on a fresh one in seconds.
func TestTheConnectionsToThePlatformAreHealthChecked(t *testing.T) {
	c, err := New("https://lebrel.ai", "secret", 10*time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", c.httpClient.Transport)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 to the platform")
	}
	h2 := transport.HTTP2
	if h2 == nil || h2.SendPingTimeout != 3*time.Second || h2.PingTimeout != 3*time.Second || h2.WriteByteTimeout != 5*time.Second {
		t.Fatalf("HTTP/2 health check %+v", h2)
	}
}
