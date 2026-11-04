// Package alert hands the router's alerts to an operator: always a log line, and a POST to a
// webhook when one is configured. The body follows the webhook's host: a Telegram bot's
// sendMessage URL gets {"chat_id", "text"}, a Discord webhook {"content"}, anything else (Slack's
// incoming webhooks among them) {"text"}. Sending never blocks the caller.
//
// The webhook's URL carries its secret (a bot token, a webhook key), so it is checked once at
// start and never written to a log: a failed send is logged by its kind only.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	queued      = 32
	sendTimeout = 10 * time.Second
)

type Notifier struct {
	prefix string
	target string
	chatID string
	client *http.Client
	logger *log.Logger
	queue  chan string
	// sending is held while an alert is on its way, so Drain waits for it
	sending sync.Mutex
}

// New returns a notifier whose alerts start with prefix (the instance's name). An empty target
// only logs; chatID is the Telegram chat for a Telegram target. A target that is not a usable
// webhook URL is refused, and the error never repeats it.
func New(prefix, target, chatID string, logger *log.Logger) (*Notifier, error) {
	if logger == nil {
		logger = log.Default()
	}
	if target != "" {
		if err := check(target, chatID); err != nil {
			return nil, err
		}
	}
	return &Notifier{prefix: prefix, target: target, chatID: chatID, logger: logger, queue: make(chan string, queued),
		client: &http.Client{Timeout: sendTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// check refuses what could not be a webhook: control characters or spaces anywhere, a URL that
// does not parse, anything but https (http only to this machine, for tests), a host missing, a
// Telegram bot without its chat.
func check(target, chatID string) error {
	for _, r := range target {
		if r <= ' ' || r == 0x7f {
			return errors.New("ALERT_WEBHOOK_URL holds a space or a control character")
		}
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("ALERT_WEBHOOK_URL is not a valid webhook URL")
	}
	if _, err := strconv.Atoi(parsed.Port()); parsed.Port() != "" && err != nil {
		return errors.New("ALERT_WEBHOOK_URL has an invalid port")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if host := parsed.Hostname(); host != "localhost" && !loopback(host) {
			return errors.New("ALERT_WEBHOOK_URL must use https")
		}
	default:
		return errors.New("ALERT_WEBHOOK_URL must use https")
	}
	if strings.EqualFold(parsed.Hostname(), "api.telegram.org") && strings.TrimSpace(chatID) == "" {
		return errors.New("a Telegram ALERT_WEBHOOK_URL needs ALERT_TELEGRAM_CHAT_ID")
	}
	return nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Notify logs the alert and queues it for the webhook; when 32 are already waiting it is only logged.
func (n *Notifier) Notify(text string) {
	if n.prefix != "" {
		text = n.prefix + ": " + text
	}
	n.logger.Printf("ALERT %s", text)
	if n.target == "" {
		return
	}
	select {
	case n.queue <- text:
	default:
		n.logger.Printf("alert webhook: %d alerts already waiting, this one is only logged", queued)
	}
}

// Run sends queued alerts until ctx ends; an alert already on its way is finished, not dropped.
func (n *Notifier) Run(ctx context.Context) {
	if n.target == "" {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case text := <-n.queue:
			n.deliver(context.Background(), text)
		}
	}
}

// Drain sends what is still queued, for a process about to end, until ctx ends.
func (n *Notifier) Drain(ctx context.Context) {
	if n.target == "" {
		return
	}
	for ctx.Err() == nil {
		select {
		case text := <-n.queue:
			n.deliver(ctx, text)
		default:
			// wait for an alert Run may still be sending
			n.sending.Lock()
			n.sending.Unlock()
			return
		}
	}
}

func (n *Notifier) deliver(ctx context.Context, text string) {
	n.sending.Lock()
	defer n.sending.Unlock()
	if err := n.send(ctx, text); err != nil {
		n.logger.Printf("alert webhook: not delivered (%v)", err)
	}
}

func (n *Notifier) body(text string) map[string]string {
	host := ""
	if parsed, err := url.Parse(n.target); err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	switch {
	case host == "api.telegram.org":
		return map[string]string{"chat_id": n.chatID, "text": text}
	case host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com"):
		return map[string]string{"content": text}
	default:
		return map[string]string{"text": text}
	}
}

// send posts one alert. Its errors name what went wrong, never the URL.
func (n *Notifier) send(ctx context.Context, text string) error {
	encoded, err := json.Marshal(n.body(text))
	if err != nil {
		return errors.New("the alert could not be encoded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, n.target, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("the webhook request could not be built")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := n.client.Do(request)
	if err != nil {
		return kind(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return errors.New("the webhook answered HTTP " + strconv.Itoa(response.StatusCode))
	}
	return nil
}

// kind says what kind of failure a send met, from the error's type alone: its text quotes the URL.
func kind(err error) error {
	var urlError *url.Error
	var netError net.Error
	var dnsError *net.DNSError
	var opError *net.OpError
	switch {
	case errors.As(err, &netError) && netError.Timeout():
		return errors.New("timeout")
	case errors.As(err, &dnsError):
		return errors.New("the webhook's host does not resolve")
	case errors.As(err, &opError):
		return errors.New("could not connect to the webhook")
	case errors.As(err, &urlError):
		return errors.New("the request failed")
	default:
		return errors.New("the request failed")
	}
}
