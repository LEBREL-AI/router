// Package dstack talks to the guest agent of a dstack confidential VM over its unix socket: keys
// derived by the KMS for this measured app, the TDX quote, and the app's identity. Everything the
// router needs to be verifiable lives behind these three calls; nothing here is reachable from
// outside the VM.
package dstack

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// DefaultSocket is where dstack mounts its guest agent inside the app's containers.
const DefaultSocket = "/var/run/dstack.sock"

// ReportDataSize is the size of the TDX report data field a quote carries.
const ReportDataSize = 64

// Info is the app's identity as the guest agent states it (the v0 Info call).
type Info struct {
	AppID        string `json:"app_id"`
	InstanceID   string `json:"instance_id"`
	ComposeHash  string `json:"compose_hash"`
	OSImageHash  string `json:"os_image_hash"`
	MrAggregated string `json:"mr_aggregated"`
	AppName      string `json:"app_name"`
	DeviceID     string `json:"device_id"`
	// TcbInfo is the agent's JSON document (measurements, event log) as a string, kept verbatim.
	TcbInfo string `json:"tcb_info"`
	// KeyProviderInfo names the key provider (the KMS) as a JSON string, kept verbatim.
	KeyProviderInfo string `json:"key_provider_info"`
}

// Quote is a TDX quote with the event log that explains its runtime measurements.
type Quote struct {
	Quote      []byte
	EventLog   string
	ReportData []byte
	VMConfig   string
}

// Attester is what the router needs from the VM; Client implements it, tests fake it.
type Attester interface {
	Quote(ctx context.Context, reportData []byte) (Quote, error)
	Info(ctx context.Context) (Info, error)
}

// Client speaks the guest agent's v0 HTTP API over the unix socket.
type Client struct {
	socket string
	http   *http.Client
}

// New returns a client for the guest agent at socket (DefaultSocket when empty).
func New(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Client{
		socket: socket,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// Available reports whether the guest agent's socket exists: outside a dstack VM it does not.
func Available(socket string) bool {
	if socket == "" {
		socket = DefaultSocket
	}
	info, err := os.Stat(socket)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// Key asks the KMS-derived key for this app under path and purpose; algorithm is "ed25519" or
// "secp256k1". The 32 bytes returned are deterministic for this app's measurement: every instance
// of the same compose gets the same key, and nothing outside the measured VM can obtain it.
func (c *Client) Key(ctx context.Context, path, purpose, algorithm string) ([]byte, error) {
	var out struct {
		Key            string   `json:"key"`
		SignatureChain []string `json:"signature_chain"`
		Error          string   `json:"error"`
	}
	if err := c.call(ctx, "/GetKey", map[string]any{"path": path, "purpose": purpose, "algorithm": algorithm}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, fmt.Errorf("dstack GetKey: %s", out.Error)
	}
	key, err := hex.DecodeString(out.Key)
	if err != nil || len(key) == 0 {
		return nil, errors.New("dstack GetKey: the key is not hex")
	}
	return key, nil
}

// Quote asks a TDX quote whose report data is reportData (at most 64 bytes; shorter is zero-padded
// on the right by the agent).
func (c *Client) Quote(ctx context.Context, reportData []byte) (Quote, error) {
	if len(reportData) > ReportDataSize {
		return Quote{}, fmt.Errorf("report data is %d bytes, at most %d", len(reportData), ReportDataSize)
	}
	var out struct {
		Quote      string `json:"quote"`
		EventLog   string `json:"event_log"`
		ReportData string `json:"report_data"`
		VMConfig   string `json:"vm_config"`
		Error      string `json:"error"`
	}
	if err := c.call(ctx, "/GetQuote", map[string]any{"report_data": hex.EncodeToString(reportData)}, &out); err != nil {
		return Quote{}, err
	}
	if out.Error != "" {
		return Quote{}, fmt.Errorf("dstack GetQuote: %s", out.Error)
	}
	quote, err := hex.DecodeString(out.Quote)
	if err != nil || len(quote) == 0 {
		return Quote{}, errors.New("dstack GetQuote: the quote is not hex")
	}
	reported, _ := hex.DecodeString(out.ReportData)
	return Quote{Quote: quote, EventLog: out.EventLog, ReportData: reported, VMConfig: out.VMConfig}, nil
}

// Info returns the app's identity.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out struct {
		Info
		Error string `json:"error"`
	}
	if err := c.call(ctx, "/Info", map[string]any{}, &out); err != nil {
		return Info{}, err
	}
	if out.Error != "" {
		return Info{}, fmt.Errorf("dstack Info: %s", out.Error)
	}
	return out.Info, nil
}

func (c *Client) call(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://dstack"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("dstack %s: %w", path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("dstack %s: %w", path, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("dstack %s: HTTP %d: %s", path, response.StatusCode, truncate(raw, 200))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("dstack %s: %w", path, err)
	}
	return nil
}

func truncate(raw []byte, n int) string {
	if len(raw) > n {
		return string(raw[:n]) + "…"
	}
	return string(raw)
}
