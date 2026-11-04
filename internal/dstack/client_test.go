package dstack

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A fake guest agent on a unix socket: it answers the three v0 calls the router makes and records
// what it was asked.
func fakeAgent(t *testing.T) (socket string, asked *[]map[string]any) {
	t.Helper()
	// A short path: unix socket paths are limited to ~100 bytes and t.TempDir() is longer on macOS.
	dir, err := os.MkdirTemp("/tmp", "ds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket = filepath.Join(dir, "dstack.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	calls := []map[string]any{}
	asked = &calls
	mux := http.NewServeMux()
	mux.HandleFunc("/GetKey", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		in["_path"] = r.URL.Path
		calls = append(calls, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"key": hex.EncodeToString(make([]byte, 32)), "signature_chain": []string{"aa", "bb"}})
	})
	mux.HandleFunc("/GetQuote", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		in["_path"] = r.URL.Path
		calls = append(calls, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"quote": "04000200", "event_log": "[]", "report_data": in["report_data"]})
	})
	mux.HandleFunc("/Info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"app_id": "4d72", "instance_id": "9fed", "compose_hash": "50d6", "os_image_hash": "ec43", "tcb_info": "{}"})
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket, asked
}

func TestTheClientSpeaksTheGuestAgentsV0CallsOverTheSocket(t *testing.T) {
	socket, asked := fakeAgent(t)
	client := New(socket)
	ctx := context.Background()

	key, err := client.Key(ctx, "lebrel-router/receipt-signing", "receipt signing seed", "ed25519")
	if err != nil || len(key) != 32 {
		t.Fatalf("key: %v (%d bytes)", err, len(key))
	}
	quote, err := client.Quote(ctx, []byte{1, 2, 3})
	if err != nil || hex.EncodeToString(quote.Quote) != "04000200" || quote.EventLog != "[]" {
		t.Fatalf("quote: %v %+v", err, quote)
	}
	info, err := client.Info(ctx)
	if err != nil || info.AppID != "4d72" || info.ComposeHash != "50d6" {
		t.Fatalf("info: %v %+v", err, info)
	}

	calls := *asked
	if len(calls) != 2 || calls[0]["_path"] != "/GetKey" || calls[0]["algorithm"] != "ed25519" || calls[0]["path"] != "lebrel-router/receipt-signing" {
		t.Fatalf("GetKey asked wrongly: %v", calls)
	}
	if calls[1]["_path"] != "/GetQuote" || calls[1]["report_data"] != "010203" {
		t.Fatalf("GetQuote asked wrongly: %v", calls[1])
	}
}

func TestReportDataLongerThanTheFieldIsRefusedBeforeAsking(t *testing.T) {
	socket, asked := fakeAgent(t)
	_, err := New(socket).Quote(context.Background(), make([]byte, 65))
	if err == nil || len(*asked) != 0 {
		t.Fatalf("65 bytes accepted: err=%v calls=%d", err, len(*asked))
	}
}

func TestAvailableMeansTheSocketExists(t *testing.T) {
	socket, _ := fakeAgent(t)
	if !Available(socket) {
		t.Fatal("a listening socket reported unavailable")
	}
	plain := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Available(plain) || Available(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a plain file or a missing path reported available")
	}
}
