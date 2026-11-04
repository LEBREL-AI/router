package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxErrorBody bounds what an HTTPError keeps of the control plane's answer.
const maxErrorBody = 4 << 10

type HTTPError struct {
	StatusCode int
	// Code is the control plane's error.code ("reservation_not_open", ...) when it sent one.
	Code    string
	Message string
	// Body is the error answer itself (bounded), for callers that read its details.
	Body []byte
}

func (e *HTTPError) Error() string {
	status := fmt.Sprintf("control plane returned HTTP %d", e.StatusCode)
	if e.Code != "" {
		status += " (" + e.Code + ")"
	}
	if e.Message == "" {
		return status
	}
	return status + ": " + e.Message
}

// NewHTTPError builds the error a call answered with this status and body would return.
func NewHTTPError(status int, body []byte) *HTTPError {
	code, message := errorDetails(body)
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody]
	}
	return &HTTPError{StatusCode: status, Code: code, Message: message, Body: append([]byte(nil), body...)}
}

type JSONClient interface {
	DoJSON(context.Context, string, string, any, any, http.Header) error
}

type Client struct {
	baseURL          *url.URL
	secret           string
	httpClient       *http.Client
	maxResponseBytes int64
}

func New(rawURL, secret string, timeout time.Duration, maxResponseBytes int64) (*Client, error) {
	if strings.TrimSpace(secret) == "" || len(secret) > 4096 || strings.ContainsAny(secret, "\r\n") {
		return nil, errors.New("control plane secret is required")
	}
	if timeout <= 0 || maxResponseBytes <= 0 {
		return nil, errors.New("control plane timeout and response limit must be positive")
	}
	baseURL, err := url.Parse(rawURL)
	if err != nil || baseURL.Host == "" {
		return nil, errors.New("CONTROL_PLANE_URL must be an absolute URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("CONTROL_PLANE_URL cannot contain userinfo, query, or fragment")
	}
	if baseURL.Scheme != "https" {
		if baseURL.Scheme != "http" || !isLoopbackHost(baseURL.Hostname()) {
			return nil, errors.New("CONTROL_PLANE_URL must use HTTPS unless it is loopback")
		}
	}

	// Hundreds of calls can be in flight (every request's hold and acceptance, plus the settlement
	// queue): over HTTP/2 they share a few connections; over HTTP/1.1 the idle pool keeps them
	// reused instead of opening one per call.
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: min(timeout, 5*time.Second), KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   512,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   min(timeout, 5*time.Second),
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		// Every call to the platform shares a few HTTP/2 connections, so one that dies without a word
		// (a moment of packet loss, a path that changed) would hold every call written on it until each
		// times out. A connection silent for 3 s is pinged and closed if the ping is not answered in 3 s
		// more, or if 5 s go by without a byte written: the calls behind it go out on a fresh one. On
		// 26 Oct 2026 one instance's settlement connection hung like that for 50 s.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 3 * time.Second, PingTimeout: 3 * time.Second, WriteByteTimeout: 5 * time.Second},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		baseURL:          baseURL,
		secret:           secret,
		httpClient:       httpClient,
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func (c *Client) DoJSON(
	ctx context.Context,
	method string,
	path string,
	input any,
	output any,
	headers http.Header,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode control plane request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return fmt.Errorf("create control plane request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.secret)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "inference-gateway/1")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, values := range headers {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Host") {
			continue
		}
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("control plane request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read control plane response: %w", err)
	}
	if int64(len(responseBody)) > c.maxResponseBytes {
		return errors.New("control plane response exceeded configured limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return NewHTTPError(response.StatusCode, responseBody)
	}
	if output == nil || response.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(responseBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("decode control plane response: %w", err)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// errorDetails reads the control plane's {"error": {"code", "message"}} (or a bare message).
func errorDetails(body []byte) (code, message string) {
	var envelope struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return "", ""
	}
	message = envelope.Message
	switch typed := envelope.Error.(type) {
	case string:
		if message == "" {
			message = typed
		}
	case map[string]any:
		code, _ = typed["code"].(string)
		if text, _ := typed["message"].(string); message == "" {
			message = text
		}
	}
	return code, message
}
