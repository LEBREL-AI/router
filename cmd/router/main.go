// Command router serves OpenAI-compatible completions routed to third-party APIs
// with Lebrel billing, verification status and signed route receipts.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"crypto/ed25519"
	"lebrel-router/internal/alert"
	"lebrel-router/internal/auth"
	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/ledger"
	"lebrel-router/internal/router"
	"lebrel-router/internal/settle"
)

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	if err := run(logger); err != nil {
		logger.Fatalf("router: %v", err)
	}
}

func run(logger *log.Logger) error {
	routesFile := env("ROUTES_FILE", "")
	if routesFile == "" {
		return errors.New("ROUTES_FILE is required")
	}
	routes, err := router.LoadConfig(routesFile)
	if err != nil {
		return err
	}
	production := strings.ToLower(env("ENVIRONMENT", "development")) == "production"

	var authenticator auth.Authenticator
	var ledgerImpl ledger.Ledger
	// the settlement queue's calls (batches among them) get more time than a request's own
	var settlementLedger settle.Ledger
	controlURL, controlSecret := env("CONTROL_PLANE_URL", ""), env("CONTROL_PLANE_SECRET", "")
	switch {
	case controlURL != "" && controlSecret != "":
		client, err := controlplane.New(controlURL, controlSecret, 5*time.Second, 1<<20)
		if err != nil {
			return fmt.Errorf("control plane: %w", err)
		}
		settlementClient, err := controlplane.New(controlURL, controlSecret, 10*time.Second, 1<<20)
		if err != nil {
			return fmt.Errorf("control plane: %w", err)
		}
		authenticator = auth.NewRemoteAuthenticator(client)
		ledgerImpl = ledger.NewRemote(client)
		settlementLedger = ledger.NewRemote(settlementClient)
		logger.Printf("mode=remote control_plane=%s", controlURL)
	case production:
		return errors.New("production requires CONTROL_PLANE_URL and CONTROL_PLANE_SECRET")
	default:
		records, balances, err := localKeys(env("LOCAL_API_KEYS", ""))
		if err != nil {
			return err
		}
		local := ledger.NewInMemory(balances)
		authenticator, ledgerImpl, settlementLedger = records, local, local
		logger.Printf("mode=local keys=%d (development only)", len(balances))
	}

	seedText := env("RECEIPT_SIGNING_SEED", "")
	var seed []byte
	if seedText == "" {
		if production {
			return errors.New("RECEIPT_SIGNING_SEED is required in production")
		}
		seed = make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			return err
		}
		logger.Printf("warning: ephemeral receipt signing key; receipts will not verify after restart")
	} else {
		seed, err = base64.StdEncoding.DecodeString(seedText)
		if err != nil || len(seed) != 32 {
			return errors.New("RECEIPT_SIGNING_SEED must be standard base64 of exactly 32 bytes")
		}
	}
	instance := env("INSTANCE_ID", "")
	if instance == "" {
		host, _ := os.Hostname()
		var suffix [4]byte
		_, _ = rand.Read(suffix[:])
		instance = host + "-" + hex.EncodeToString(suffix[:])
	}
	signer, err := router.NewSigner(seed, instance)
	if err != nil {
		return err
	}
	logger.Printf("receipt signing key id=%s public=%s", signer.KeyID, hex.EncodeToString(signer.Public))

	verification := router.NewVerification(env("BOARD_SOURCE", ""))
	if keyHex := env("BOARD_PUBLIC_KEY_HEX", ""); keyHex != "" {
		key, err := hex.DecodeString(keyHex)
		if err != nil || len(key) != ed25519.PublicKeySize {
			logger.Fatalf("BOARD_PUBLIC_KEY_HEX must be 64 hex characters")
		}
		verification.RequireSignature(ed25519.PublicKey(key))
	} else if verification.Source() != "" {
		logger.Printf("warning: BOARD_PUBLIC_KEY_HEX is not set; the watch board is applied without checking its signature")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if verification.Source() != "" {
		refresh, _ := strconv.Atoi(env("BOARD_REFRESH_SECONDS", "300"))
		if refresh < 30 {
			refresh = 30
		}
		go verification.Run(ctx, time.Duration(refresh)*time.Second)
	}
	maxBytes, _ := strconv.ParseInt(env("MAX_REQUEST_BYTES", "33554432"), 10, 64)
	settlementDir := env("SETTLEMENT_DIR", "")
	if settlementDir == "" && env("STATE_DIRECTORY", "") != "" {
		settlementDir = filepath.Join(env("STATE_DIRECTORY", ""), "settlements") // systemd's StateDirectory
	}
	if production && settlementDir == "" {
		return errors.New("SETTLEMENT_DIR (or systemd StateDirectory) is required in production: settlements must survive a restart")
	}
	concurrency, err := positive("SETTLEMENT_CONCURRENCY", 64)
	if err != nil {
		return err
	}
	batchSize, err := positive("SETTLEMENT_BATCH_SIZE", 1)
	if err != nil {
		return err
	}
	alertSeconds, err := positive("SETTLEMENT_ALERT_SECONDS", 5)
	if err != nil {
		return err
	}
	acceptMillis, err := positive("ACCEPT_TIMEOUT_MS", 3000)
	if err != nil {
		return err
	}
	// nginx gives an answer 600 s between reads (proxy_read_timeout): a restart lets the answers in
	// flight finish within the same time (the units' TimeoutStopSec covers it)
	shutdownSeconds, err := positive("SHUTDOWN_SECONDS", 600)
	if err != nil {
		return err
	}
	alertAge := time.Duration(alertSeconds) * time.Second
	settlements, err := settle.New(settlementDir, settlementLedger, settle.Options{Logger: logger, Concurrency: concurrency, BatchSize: batchSize})
	if err != nil {
		return err
	}
	defer settlements.Close()
	logger.Printf("settlements: dir=%q concurrency=%d batch=%d alert_after=%s accept_timeout=%dms", settlementDir, concurrency, batchSize, alertAge, acceptMillis)
	// The queue, its watch and the alerts outlive the signal: settlements of the answers finishing
	// during a shutdown are delivered, and its alerts are sent, before the process ends.
	background, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	go settlements.Run(background)
	// a trailing CR from a file written on another system is not part of the URL
	notifier, err := alert.New("router "+instance, strings.TrimSpace(env("ALERT_WEBHOOK_URL", "")), strings.TrimSpace(env("ALERT_TELEGRAM_CHAT_ID", "")), logger)
	if err != nil {
		return err
	}
	go notifier.Run(background)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		settlements.Watch(background, 5*time.Second, alertAge, notifier.Notify)
	}()
	var peers []string
	for _, peer := range strings.Split(env("RECEIPT_PEERS", ""), ",") {
		if peer = strings.TrimSpace(peer); peer != "" {
			peers = append(peers, peer)
		}
	}
	server, err := router.New(router.Settings{
		Routes: routes, Auth: authenticator, Ledger: ledgerImpl, Signer: signer, Settlements: settlements,
		AcceptTimeout: time.Duration(acceptMillis) * time.Millisecond, AlertAge: alertAge, ReceiptPeers: peers,
		HealthToken: strings.TrimSpace(env("HEALTH_TOKEN", "")),
		Receipts:    router.NewReceiptStore(24*time.Hour, 100_000), Verification: verification,
		BoardURL: env("BOARD_URL", ""), EdgeProxySecret: env("EDGE_PROXY_SECRET", ""), MaxRequestBytes: maxBytes, Logger: logger,
	})
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: env("LISTEN_ADDR", ":8090"), Handler: server, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errs := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s routes=%d", httpServer.Addr, len(routes.Routes))
		errs <- httpServer.ListenAndServe()
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	// No new request from here; the answers in flight have the budget to finish. A second stop
	// signal cuts them at once instead of leaving SIGKILL to strand their holds.
	again := make(chan os.Signal, 1)
	signal.Notify(again, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(again)
	budget := time.Duration(shutdownSeconds) * time.Second
	logger.Printf("stopping: no new requests; %d answers in flight have up to %s to finish (a second stop signal cuts them now)", server.InFlight(), budget)
	stopping := time.Now()
	waiting, stopWaiting := context.WithTimeout(context.Background(), budget)
	var early atomic.Bool
	go func() {
		select {
		case <-again:
			early.Store(true)
			logger.Printf("stopping: a second stop signal, cutting the answers in flight now")
			stopWaiting()
		case <-waiting.Done():
		}
	}()
	err = httpServer.Shutdown(waiting)
	// the budget ran out, or the second signal came
	cutNow := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	stopWaiting()
	if cutNow {
		// the rest are cut, each settled for what it relayed, and reported: those whose handler ends
		// only once their connection is closed too
		cutting, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		server.CutStreams(cutting)
		cancel()
		_ = httpServer.Close()
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cut := server.CutStreams(closing)
		cancel()
		if len(cut) > 0 {
			notifier.Notify(cutText(cut, time.Since(stopping).Round(time.Second), early.Load()))
		}
	}
	// what is owed goes out for a few more seconds; the journal keeps the rest for the next start
	flushing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = settlements.Flush(flushing)
	cancel()
	stopBackground()
	// the watch's last pass (the disk, its alerts, the last refusals) before the alerts go out
	select {
	case <-watched:
	case <-time.After(5 * time.Second):
	}
	sending, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	notifier.Drain(sending)
	cancel()
	if health := settlements.Health(alertAge); health.MemoryOnly > 0 {
		logger.Printf("stopped: %d settlements still owed are kept on disk for the next start, except %d held only in memory: the disk refused them, they are lost", health.Pending, health.MemoryOnly)
	} else {
		logger.Printf("stopped: %d settlements still owed are kept on disk for the next start", health.Pending)
	}
	return nil
}

// cutText is the alert for answers a shutdown cut, in Spanish for the founder.
func cutText(reservations []string, waited time.Duration, early bool) string {
	listed := reservations
	more := ""
	if len(listed) > 10 {
		listed, more = listed[:10], fmt.Sprintf(" y %d más", len(reservations)-10)
	}
	when := fmt.Sprintf("tras esperar %s", waited)
	if early {
		when = fmt.Sprintf("al llegar una segunda orden de parada a los %s", waited)
	}
	return fmt.Sprintf("al apagar quedaban %d respuestas en curso %s: las corté y cada una se cobra por lo que ya se había enviado (estimado), nunca por el máximo reservado. Reservas: %s%s.",
		len(reservations), when, strings.Join(listed, ", "), more)
}

// positive reads a whole number setting above zero, or its default when unset.
func positive(name string, fallback int) (int, error) {
	value, err := strconv.Atoi(env(name, strconv.Itoa(fallback)))
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a whole number above zero", name)
	}
	return value, nil
}

// localKeys parses LOCAL_API_KEYS="account:rawKey:balanceMicrousd[:concurrency];..." for development.
func localKeys(spec string) (*auth.HMACAuthenticator, map[string]int64, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil, errors.New("LOCAL_API_KEYS is required in local mode (account:rawKey:balanceMicrousd[:concurrency];...)")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, nil, err
	}
	var records []auth.Record
	balances := map[string]int64{}
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 3 {
			return nil, nil, fmt.Errorf("LOCAL_API_KEYS entry %q: expected account:rawKey:balance", entry)
		}
		balance, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || balance < 0 {
			return nil, nil, fmt.Errorf("LOCAL_API_KEYS entry %q: bad balance", entry)
		}
		concurrency := 8
		if len(parts) > 3 {
			if concurrency, err = strconv.Atoi(parts[3]); err != nil || concurrency < 1 {
				return nil, nil, fmt.Errorf("LOCAL_API_KEYS entry %q: bad concurrency", entry)
			}
		}
		records = append(records, auth.Record{AccountID: parts[0], Digest: auth.Digest(secret, parts[1]), Concurrency: concurrency, AccountConcurrency: concurrency})
		balances[parts[0]] += balance
	}
	_ = sha256.Size
	return auth.NewHMACAuthenticator(secret, records), balances, nil
}
