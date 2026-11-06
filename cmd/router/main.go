// Command router serves OpenAI-compatible completions routed to third-party APIs
// with Lebrel billing, verification status and signed route receipts.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"crypto/ed25519"

	"golang.org/x/crypto/acme/autocert"

	"lebrel-router/internal/alert"
	"lebrel-router/internal/auth"
	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/dstack"
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

	// Inside a dstack confidential VM the receipt-signing seed is born inside: the KMS derives it
	// for this app's measurement and hands it only to the measured VM. Every instance of the same
	// compose signs with the same key, and nothing outside can obtain it. An explicit
	// RECEIPT_SIGNING_SEED still wins (the non-attested deployment).
	var attester dstack.Attester
	keySource := "env"
	encryptInside := false
	seedText := env("RECEIPT_SIGNING_SEED", "")
	var seed []byte
	if socket := env("DSTACK_SOCKET", dstack.DefaultSocket); dstack.Available(socket) {
		client := dstack.New(socket)
		attester = client
		if seedText != "" && production {
			// A handed-in key inside the VM would let a human sign "attested" receipts: refused.
			return errors.New("RECEIPT_SIGNING_SEED must not be set inside an attested VM in production: the key is derived by the VM's KMS")
		}
		if seedText == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			// The compose hash goes into the derivation path, so instances of one compose sign
			// alike; it is an app key all the same (any compose registered under the app could
			// derive it), and it is the attestation that names the compose that served.
			info, err := client.Info(ctx)
			if err != nil {
				cancel()
				return fmt.Errorf("app identity from the VM: %w", err)
			}
			if len(info.ComposeHash) < 32 {
				cancel()
				return errors.New("the VM reports no compose hash")
			}
			derived, err := client.Key(ctx, "lebrel-router/receipt-signing/"+info.ComposeHash, "ed25519 seed of the receipt-signing key", "ed25519")
			cancel()
			if err != nil {
				return fmt.Errorf("receipt-signing seed from the VM's KMS: %w", err)
			}
			if len(derived) < 32 {
				return errors.New("receipt-signing seed from the VM's KMS is shorter than 32 bytes")
			}
			seed = derived[:32]
			keySource = "vm-kms"
			logger.Printf("receipt signing seed derived inside the attested VM for compose %s", info.ComposeHash)
			// The HPKE key clients encrypt to is born at random in this VM's memory, per boot and per
			// UTC day, never derived from the key service: that service derives per app, so a key it
			// handed out could be derived again by any compose registered under the app.
			encryptInside = true
		}
	}
	if seedText == "" && seed == nil {
		if production {
			return errors.New("RECEIPT_SIGNING_SEED is required in production")
		}
		seed = make([]byte, 32)
		if _, err := rand.Read(seed); err != nil {
			return err
		}
		logger.Printf("warning: ephemeral receipt signing key; receipts will not verify after restart")
	} else if seedText != "" {
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
	// Encrypted requests are accepted only where the key is born inside the attested VM; a
	// deployment outside one serves plaintext routed requests as before.
	var encryption *router.Encryption
	if encryptInside {
		encryption, err = router.NewEphemeralEncryption(instance, time.Now())
		if err != nil {
			return fmt.Errorf("encryption identity: %w", err)
		}
		encryption.RotateDaily(context.Background(), time.Now, func(err error) { logger.Printf("encryption key rotation: %v", err) })
		logger.Printf("encryption key id=%s public=%s (born at random inside the attested VM for today)", encryption.KeyID(), hex.EncodeToString(encryption.PublicKey()))
	}

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
	// Direct clients: the machine terminates TLS itself (TLS_CERT_FILE/TLS_KEY_FILE, or an ingress in
	// the same VM) and serves ordinary API clients without the platform's edge in front, under the
	// names DIRECT_HOSTS lists. Limits stay by key and by account; nothing is capped by address.
	// CLIENT_ADDRESS states where a client's address is known from, for the control plane's count of
	// wrong keys: "connection" (TLS ends here, nothing proxies in front), "header" (the last entry of
	// CLIENT_IP_HEADER when the connection comes from CLIENT_IP_HEADER_FROM, the ingress), or "none"
	// (a TCP ingress or a provider's gateway hides it: no address is told, wrong keys are counted for
	// nobody rather than for every client at once).
	directClients := strings.EqualFold(strings.TrimSpace(env("DIRECT_CLIENTS", "false")), "true")
	directHosts := listed(env("DIRECT_HOSTS", ""), strings.ToLower)
	clientAddress := strings.ToLower(strings.TrimSpace(env("CLIENT_ADDRESS", "")))
	clientIPHeader := strings.TrimSpace(env("CLIENT_IP_HEADER", ""))
	trustedIngress, err := prefixes(env("CLIENT_IP_HEADER_FROM", ""))
	if err != nil {
		return err
	}
	if directClients {
		if production && len(directHosts) == 0 {
			return errors.New("DIRECT_CLIENTS in production needs DIRECT_HOSTS: the names clients call this machine by")
		}
		if production && clientAddress == "" {
			return errors.New("DIRECT_CLIENTS in production needs CLIENT_ADDRESS: none, connection or header (where a client's address is known from)")
		}
		logger.Printf("direct clients: on hosts=%v client_address=%q header=%q from=%v", directHosts, clientAddress, clientIPHeader, trustedIngress)
	}
	server, err := router.New(router.Settings{
		Routes: routes, Auth: authenticator, Ledger: ledgerImpl, Signer: signer, Settlements: settlements,
		AcceptTimeout: time.Duration(acceptMillis) * time.Millisecond, AlertAge: alertAge, ReceiptPeers: peers,
		HealthToken: strings.TrimSpace(env("HEALTH_TOKEN", "")),
		Receipts:    router.NewReceiptStore(24*time.Hour, 100_000), Verification: verification,
		BoardURL: env("BOARD_URL", ""), EdgeProxySecret: env("EDGE_PROXY_SECRET", ""), EdgeProxyToken: env("EDGE_PROXY_TOKEN", ""), MaxRequestBytes: maxBytes, Logger: logger,
		DirectClients: directClients, DirectHosts: directHosts, ClientAddress: clientAddress, ClientIPHeader: clientIPHeader, TrustedIngress: trustedIngress,
		Alert:    notifier.Notify,
		Attester: attester, KeySource: keySource, Encryption: encryption,
	})
	if err != nil {
		return err
	}
	// Listeners: LISTEN_ADDR serves plaintext (the platform's edge and a provider's gateway, which
	// terminate TLS themselves; "off" for none); LISTEN_TLS_ADDR serves TLS ending in this process
	// when a certificate source is configured: files (TLS_CERT_FILE/TLS_KEY_FILE, re-read when they
	// change) or Let's Encrypt in-process (TLS_ACME_HOSTS, by the TLS-ALPN-01 challenge answered on
	// the TLS listener itself: no DNS credential, and no private key outside this process).
	tlsConfig, tlsSource, err := tlsSetup(strings.TrimSpace(env("TLS_CERT_FILE", "")), strings.TrimSpace(env("TLS_KEY_FILE", "")),
		listed(env("TLS_ACME_HOSTS", ""), strings.ToLower), strings.TrimSpace(env("TLS_ACME_CACHE", "")), strings.TrimSpace(env("TLS_ACME_EMAIL", "")), logger)
	if err != nil {
		return err
	}
	var servers []*http.Server
	if addr := env("LISTEN_ADDR", ":8090"); !strings.EqualFold(strings.TrimSpace(addr), "off") {
		servers = append(servers, &http.Server{Addr: addr, Handler: server, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second})
	}
	if tlsConfig != nil {
		servers = append(servers, &http.Server{Addr: env("LISTEN_TLS_ADDR", ":8443"), Handler: server, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second})
	}
	if len(servers) == 0 {
		return errors.New("nothing to listen on: LISTEN_ADDR is off and no TLS source is configured")
	}
	errs := make(chan error, len(servers))
	for _, httpServer := range servers {
		go func(httpServer *http.Server) {
			if httpServer.TLSConfig != nil {
				// The TLS connection ends in this process: nothing between the client and the measured machine reads it.
				logger.Printf("listening on %s with TLS (%s) routes=%d", httpServer.Addr, tlsSource, len(routes.Routes))
				errs <- httpServer.ListenAndServeTLS("", "")
				return
			}
			logger.Printf("listening on %s routes=%d", httpServer.Addr, len(routes.Routes))
			errs <- httpServer.ListenAndServe()
		}(httpServer)
	}
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
	err = shutdownAll(waiting, servers)
	// the budget ran out, or the second signal came
	cutNow := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	stopWaiting()
	if cutNow {
		// the rest are cut, each settled for what it relayed, and reported: those whose handler ends
		// only once their connection is closed too
		cutting, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		server.CutStreams(cutting)
		cancel()
		for _, httpServer := range servers {
			_ = httpServer.Close()
		}
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

// listed splits a comma-separated setting into its entries, trimmed and normalised, empties dropped.
func listed(spec string, normalise func(string) string) []string {
	var entries []string
	for _, entry := range strings.Split(spec, ",") {
		if entry = normalise(strings.TrimSpace(entry)); entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// prefixes reads a comma-separated list of addresses or CIDRs; a bare address is its own prefix.
func prefixes(spec string) ([]netip.Prefix, error) {
	var result []netip.Prefix
	for _, entry := range listed(spec, func(s string) string { return s }) {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("CLIENT_IP_HEADER_FROM: %q is neither an address nor a CIDR", entry)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			// the IPv4-mapped form of an IPv4 prefix, compared as the IPv4 prefix it is
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		result = append(result, prefix.Masked())
	}
	return result, nil
}

// shutdownAll stops every listener at once and waits for their answers in flight, until ctx ends;
// the first error (ctx's, when the wait ran out) is returned.
func shutdownAll(ctx context.Context, servers []*http.Server) error {
	errs := make(chan error, len(servers))
	for _, httpServer := range servers {
		go func(httpServer *http.Server) { errs <- httpServer.Shutdown(ctx) }(httpServer)
	}
	var first error
	for range servers {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// tlsSetup is the TLS listener's configuration from one certificate source, or nil for none:
// certificate files, or Let's Encrypt in-process for the names given. The source is described
// for the log.
func tlsSetup(certFile, keyFile string, acmeHosts []string, acmeCache, acmeEmail string, logger *log.Logger) (*tls.Config, string, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, "", errors.New("TLS_CERT_FILE and TLS_KEY_FILE go together")
	}
	if certFile != "" && len(acmeHosts) > 0 {
		return nil, "", errors.New("TLS_CERT_FILE and TLS_ACME_HOSTS exclude each other")
	}
	switch {
	case certFile != "":
		certs := &certificates{certFile: certFile, keyFile: keyFile, logger: logger}
		if _, err := certs.get(nil); err != nil {
			return nil, "", fmt.Errorf("TLS_CERT_FILE/TLS_KEY_FILE: %w", err)
		}
		return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get}, "certificate files", nil
	case len(acmeHosts) > 0:
		config, err := acmeTLS(acmeHosts, acmeCache, acmeEmail)
		if err != nil {
			return nil, "", err
		}
		return config, "Let's Encrypt in-process for " + strings.Join(acmeHosts, ", "), nil
	}
	return nil, "", nil
}

// acmeTLS obtains and renews the machine's own certificates from Let's Encrypt inside the process,
// by the TLS-ALPN-01 challenge answered on the TLS listener itself: no DNS credential, no
// certificate file, and no private key outside this process. hosts are the names served (any other
// name is refused at the handshake); the cache directory keeps the account key and the certificates
// across restarts (Let's Encrypt limits how often a name may be issued); email is where Let's
// Encrypt writes about expiry.
func acmeTLS(hosts []string, cacheDir, email string) (*tls.Config, error) {
	if len(hosts) == 0 {
		return nil, errors.New("TLS_ACME_HOSTS is empty")
	}
	for _, host := range hosts {
		if strings.ContainsAny(host, " /:@") || strings.HasPrefix(host, "*") {
			return nil, fmt.Errorf("TLS_ACME_HOSTS: %q is not a host name Let's Encrypt can issue for by TLS-ALPN-01", host)
		}
	}
	if cacheDir == "" {
		return nil, errors.New("TLS_ACME_CACHE is required with TLS_ACME_HOSTS: the directory the certificates persist in")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("TLS_ACME_CACHE: %w", err)
	}
	manager := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(hosts...), Cache: autocert.DirCache(cacheDir), Email: email}
	config := manager.TLSConfig()
	config.MinVersion = tls.VersionTLS12
	return config, nil
}

// certificates serves TLS_CERT_FILE/TLS_KEY_FILE and reads them again when the certificate file
// changes (a renewal), looking at most once a minute, so a renewed certificate needs no restart.
// A pair that fails to load after a change is logged and the previous one kept.
type certificates struct {
	certFile, keyFile string
	logger            *log.Logger
	mu                sync.Mutex
	loaded            *tls.Certificate
	modTime           time.Time
	checked           time.Time
}

func (c *certificates) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.loaded == nil || now.Sub(c.checked) >= time.Minute {
		c.checked = now
		info, err := os.Stat(c.certFile)
		if err != nil && c.loaded == nil {
			return nil, err
		}
		if err == nil && (c.loaded == nil || !info.ModTime().Equal(c.modTime)) {
			pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
			switch {
			case err != nil && c.loaded == nil:
				return nil, err
			case err != nil:
				c.logger.Printf("tls: the changed certificate could not be loaded, keeping the previous one: %v", err)
			default:
				if c.loaded != nil {
					c.logger.Printf("tls: certificate reloaded from %s", c.certFile)
				}
				c.loaded, c.modTime = &pair, info.ModTime()
			}
		}
	}
	return c.loaded, nil
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
