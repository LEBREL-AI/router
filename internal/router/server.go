package router

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"lebrel-router/internal/auth"
	"lebrel-router/internal/dstack"
	"lebrel-router/internal/ledger"
	"lebrel-router/internal/limit"
	"lebrel-router/internal/meter"
	"lebrel-router/internal/model"
	"lebrel-router/internal/settle"
)

const (
	completionPath          = "/v1/chat/completions"
	settlementHealthPath    = "/healthz/settlements"
	defaultMaxRequestBytes  = 32 << 20
	maxUpstreamBodyBytes    = 64 << 20
	maxErrorBodyBytes       = 1 << 20
	maxReceiptBytes         = 1 << 20
	streamBufferBytes       = 32 << 10
	defaultManifestWindow   = time.Hour
	defaultAcceptTimeout    = 3 * time.Second
	defaultAlertAge         = 5 * time.Second
	peerReceiptTimeout      = 2 * time.Second
	userAgent               = "lebrel-router/0.1"
	receiptHeader           = "Proof-Of-Edition-Receipt"
	verificationHeader      = "X-Lebrel-Verification"
	upstreamHeader          = "X-Lebrel-Upstream"
	contentRetentionHeader  = "X-Lebrel-Content-Retention"
	edgeAuthorizationHeader = "X-Lebrel-Edge-Authorization"
	// a receipt lookup between instances: answered from the instance's own store only
	receiptPeerHeader = "X-Lebrel-Receipt-Peer"
	// once a shutdown cuts an answer, its writes have this long: a customer who stopped reading
	// cannot hold the handler, and with it the answer's settlement, past the cut; a whole answer
	// being written (already settled exactly) gets longer
	cutWriteGrace         = time.Second
	cutBufferedWriteGrace = 5 * time.Second
)

// errShutdownCut is the cause a shutdown's cut gives the contexts of the answers it cuts.
var errShutdownCut = errors.New("the router is shutting down")

// An upstream that refuses Lebrel's own credential or credit (401, 402, 403), or that has no endpoint
// for the model Lebrel named (404: the router sets the model, never the customer), is, for the
// customer, an upstream that is down: the next one goes, exactly as after a timeout or a 5xx.
var retryStatuses = map[int]bool{401: true, 402: true, 403: true, 404: true, 408: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// credentialStatuses are the refusals that are Lebrel's own at an upstream (its credential, its
// credit, the model it named), never the customer's: with no upstream left they are told as Lebrel's
// outage, in Lebrel's words.
var credentialStatuses = map[int]bool{401: true, 402: true, 403: true, 404: true}

type Settings struct {
	Routes       Config
	Auth         auth.Authenticator
	Ledger       ledger.Ledger
	Limiter      limit.ConcurrencyLimiter
	Signer       *Signer
	Receipts     *ReceiptStore
	Verification *Verification
	// Settlements owes the control plane every acceptance and settlement until acknowledged;
	// New creates a memory-only queue when none is given.
	Settlements *settle.Queue
	// AcceptTimeout bounds the acceptance call made before the first byte (default 3 s); past it
	// the acceptance is owed through Settlements and the answer goes on.
	AcceptTimeout time.Duration
	// AlertAge is the backlog age /healthz/settlements calls lagging (default 5 s).
	AlertAge time.Duration
	// HealthToken opens /healthz/settlements to monitors (Authorization: Bearer); without it the
	// endpoint does not exist. Its own secret, never the edge's, which admits completions.
	HealthToken string
	// ReceiptPeers are the base URLs of the other instances behind the same address: a receipt this
	// instance does not hold is asked of them, and served only if it carries this router's signature.
	ReceiptPeers    []string
	PeerClient      *http.Client
	BoardURL        string
	EdgeProxySecret string
	// DirectClients admits completions without the edge header: the machine terminates TLS itself and
	// serves ordinary API clients, and the key is checked here as always. No ceiling by address:
	// limits are by key and by account, as published; the only thing counted per address is a wrong
	// key, by the control plane, so guessing stays slow. DirectHosts are the names (lowercase, no
	// port) direct clients may call the machine by; under any other name a direct call is refused as
	// misdirected, so the service exists only at its published address (empty: any name).
	// ClientIPHeader names the header an ingress sets with the client's address, honoured only when
	// the connection comes from TrustedIngress (the ingress in the same VM) and read from its last
	// entry, the one that ingress wrote; otherwise the connection's own address counts.
	DirectClients  bool
	DirectHosts    []string
	ClientIPHeader string
	TrustedIngress []netip.Prefix
	// Alert hands the operator an alert (the notifier's Notify); nil only logs.
	Alert           func(string)
	MaxRequestBytes int64
	ManifestWindow  time.Duration
	HTTPClient      *http.Client
	Now             func() time.Time
	Logger          *log.Logger
	// Attester is the confidential VM's guest agent when the router runs in one: it serves the
	// quote at /.well-known/attestation bound to the receipt-signing key. Nil outside a VM.
	Attester dstack.Attester
	// KeySource names where the receipt-signing seed came from: "vm-kms" or "env".
	KeySource string
	// Encryption is the router's HPKE identity when it accepts end-to-end encrypted requests
	// (inside the attested VM); nil means plaintext only.
	Encryption *Encryption
}

type Server struct {
	settings  Settings
	routes    map[string]*Route
	slugs     map[string]*Route
	hashes    map[string]string
	edgeToken string
	mu        sync.Mutex
	manifests map[string]cachedManifest
	// attestations caches the last quote asked without a nonce.
	attestations attestationCache

	// shutdown: the completions being answered, the switch that cuts them once the budget is spent,
	// and the reservations it cut
	active atomic.Int64
	cutCtx context.Context
	cutAll context.CancelFunc
	cutMu  sync.Mutex
	cutIDs []string
	// alerts throttles the operator's alerts per subject (a route's fallbacks, a route's outage)
	alertMu sync.Mutex
	alerts  map[string]alertState
}

type cachedManifest struct {
	document SignedDocument
	identity string
	expires  time.Time
}

func DeriveEdgeProxyToken(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("lebrel-edge-proxy-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func New(settings Settings) (*Server, error) {
	if settings.Auth == nil || settings.Ledger == nil || settings.Signer == nil || settings.Receipts == nil {
		return nil, errors.New("auth, ledger, signer and receipts are required")
	}
	if settings.Limiter == nil {
		settings.Limiter = limit.NewInMemory()
	}
	if settings.Verification == nil {
		settings.Verification = NewVerification("")
	}
	if settings.MaxRequestBytes <= 0 {
		settings.MaxRequestBytes = defaultMaxRequestBytes
	}
	if settings.ManifestWindow <= 0 {
		settings.ManifestWindow = defaultManifestWindow
	}
	if settings.AcceptTimeout <= 0 {
		settings.AcceptTimeout = defaultAcceptTimeout
	}
	if settings.AlertAge <= 0 {
		settings.AlertAge = defaultAlertAge
	}
	if settings.PeerClient == nil {
		settings.PeerClient = &http.Client{Timeout: peerReceiptTimeout, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if settings.HTTPClient == nil {
		// A lab's API speaks HTTP/2 and carries every stream on a few connections; one that speaks
		// HTTP/1.1 needs a connection per stream, and the idle pool keeps them for the next request
		// instead of opening (and leaving in TIME_WAIT) one per request.
		settings.HTTPClient = &http.Client{Transport: &http.Transport{
			Proxy: nil, ResponseHeaderTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second,
			MaxIdleConnsPerHost: 1024, IdleConnTimeout: 90 * time.Second, DisableCompression: true,
		}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if settings.Now == nil {
		settings.Now = time.Now
	}
	if settings.Logger == nil {
		settings.Logger = log.Default()
	}
	if settings.Settlements == nil {
		queue, err := settle.New("", settings.Ledger, settle.Options{Logger: settings.Logger, Now: settings.Now})
		if err != nil {
			return nil, err
		}
		settings.Settlements = queue
	}
	if err := settings.Routes.Validate(); err != nil {
		return nil, err
	}
	server := &Server{settings: settings, routes: map[string]*Route{}, slugs: map[string]*Route{}, hashes: map[string]string{}, alerts: map[string]alertState{}, manifests: map[string]cachedManifest{}}
	server.cutCtx, server.cutAll = context.WithCancel(context.Background())
	for i := range settings.Routes.Routes {
		route := &settings.Routes.Routes[i]
		hash, err := route.BillingContractHash()
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", route.ModelID, err)
		}
		sort.SliceStable(route.Upstreams, func(a, b int) bool { return route.Upstreams[a].Priority < route.Upstreams[b].Priority })
		server.routes[route.ModelID] = route
		server.slugs[route.Slug()] = route
		server.hashes[route.ModelID] = hash
		for _, u := range route.Upstreams {
			if u.APIKeyEnv != "" && u.APIKey() == "" {
				settings.Logger.Printf("route=%s upstream=%s warning: %s is not set, upstream will fail", route.ModelID, u.Name, u.APIKeyEnv)
			}
		}
	}
	if settings.EdgeProxySecret != "" {
		server.edgeToken = DeriveEdgeProxyToken(settings.EdgeProxySecret)
	}
	if settings.Encryption != nil {
		settings.Encryption.SetRotationHook(server.attestations.invalidate)
	}
	return server, nil
}

// ServeHTTP accepts the OpenAI paths at the root and under /m/{slug}/ so the platform
// edge can give every model its own base URL and readiness probe.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	var scoped *Route
	if strings.HasPrefix(path, "/m/") {
		rest := strings.TrimPrefix(path, "/m/")
		slug, tail, _ := strings.Cut(rest, "/")
		route, ok := s.slugs[slug]
		if !ok {
			s.writeError(w, http.StatusNotFound, "model_not_found", "unknown model path")
			return
		}
		scoped = route
		path = "/" + tail
	}
	if publicDocumentPath(path) {
		// Manifests, receipts and the listing are public documents: browsers verify them at lebrel.ai/verify.
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Accept, Content-Type")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if path == completionPath && s.settings.DirectClients {
		// Browsers and tools call the machine directly: the same CORS the platform's edge offers.
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", allowedRequestHeaders(r))
		h.Set("Access-Control-Expose-Headers", "X-Request-ID, Proof-Of-Edition-Receipt, X-Lebrel-Upstream, X-Lebrel-Verification, Retry-After")
		h.Set("Access-Control-Max-Age", "86400")
		h.Add("Vary", "Access-Control-Request-Headers")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	switch {
	case path == completionPath && r.Header.Get("Ehbp-Encapsulated-Key") != "":
		s.handleEncryptedCompletion(w, r, scoped)
	case path == completionPath:
		s.handleCompletion(w, r, scoped)
	case path == "/readyz":
		s.handleReady(w, r, scoped)
	case path == settlementHealthPath && scoped == nil:
		s.handleSettlementHealth(w, r)
	case path == "/.well-known/proof-of-edition":
		s.handleManifest(w, r, scoped)
	case path == AttestationPath:
		// The machine's attestation is the same for every route it serves; a route path answers it too.
		s.handleAttestation(w, r)
	case path == EncryptionMetadataPath:
		s.handleEncryptionMetadata(w, r, scoped)
	case strings.HasPrefix(path, "/v1/receipts/"):
		s.handleReceipt(w, r, strings.TrimPrefix(path, "/v1/receipts/"))
	case path == "/v1/models":
		s.handleModels(w, r, scoped)
	default:
		s.writeError(w, http.StatusNotFound, "not_found", "unknown path")
	}
}

func (s *Server) privacyHeaders(w http.ResponseWriter, requestID string) {
	h := w.Header()
	h.Set("X-Request-ID", requestID)
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set(contentRetentionHeader, "none")
}

func (s *Server) validEdge(r *http.Request) bool {
	if s.edgeToken == "" {
		return true
	}
	received := strings.TrimPrefix(r.Header.Get(edgeAuthorizationHeader), "v1=")
	return len(received) == len(s.edgeToken) && hmac.Equal([]byte(received), []byte(s.edgeToken))
}

// corsAllowedHeaders is the preflight's answer when the browser names no headers of its own.
const corsAllowedHeaders = "Authorization, Content-Type, Accept, X-Lebrel-Model, Ehbp-Encapsulated-Key"

// allowedRequestHeaders answers a preflight with the headers the browser asks for, when they are
// plain header names (the OpenAI SDK in a browser sends its own x-stainless-* set, which a fixed
// list would refuse), else the known list. Authentication is a bearer token, never a cookie, so
// allowing a page's headers gives it nothing it could not send from a server.
func allowedRequestHeaders(r *http.Request) string {
	asked := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers"))
	if asked == "" || len(asked) > 1024 {
		return corsAllowedHeaders
	}
	for _, c := range asked {
		if !(c == ',' || c == ' ' || c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return corsAllowedHeaders
		}
	}
	return asked
}

// admit lets a completion through on the edge's word or, for direct clients, as it is when it came
// by one of the machine's published names: the key is checked by the handler, and limits are by key
// and account. It writes the refusal itself.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) bool {
	if s.validEdge(r) {
		return true
	}
	if !s.settings.DirectClients {
		s.writeError(w, http.StatusUnauthorized, "edge_unauthorized", "missing edge authorization")
		return false
	}
	if !s.directHost(r.Host) {
		// the machine's bare address, a provider's gateway hostname: not where the service is published
		s.writeError(w, http.StatusMisdirectedRequest, "misdirected_request", "this name does not serve API clients directly")
		return false
	}
	return true
}

// directHost says whether direct clients are served under the name the request came by.
func (s *Server) directHost(host string) bool {
	if len(s.settings.DirectHosts) == 0 {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	for _, allowed := range s.settings.DirectHosts {
		if host == allowed {
			return true
		}
	}
	return false
}

// clientIP is the address the control plane's wrong-key throttle counts for a direct client: the
// connection's own address or, when the connection is a trusted ingress's, the last address that
// ingress wrote in ClientIPHeader (a client may write the header too: its entries sit before).
func (s *Server) clientIP(r *http.Request) string {
	connection := r.RemoteAddr
	if host, _, err := net.SplitHostPort(connection); err == nil {
		connection = host
	}
	if s.settings.ClientIPHeader == "" || !s.fromTrustedIngress(connection) {
		return connection
	}
	values := r.Header.Values(s.settings.ClientIPHeader)
	if len(values) == 0 {
		return connection
	}
	entries := strings.Split(values[len(values)-1], ",")
	if last := strings.TrimSpace(entries[len(entries)-1]); last != "" {
		return last
	}
	return connection
}

func (s *Server) fromTrustedIngress(address string) bool {
	addr, err := netip.ParseAddr(strings.Trim(address, "[]"))
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range s.settings.TrustedIngress {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// authContext marks a direct client's request with its address, for the control plane's count of
// wrong keys; a request the edge admitted was counted there already and carries nothing.
func (s *Server) authContext(ctx context.Context, r *http.Request) context.Context {
	if !s.settings.DirectClients || s.validEdge(r) {
		return ctx
	}
	return auth.WithClientActor(ctx, auth.ActorForAddress(s.clientIP(r)))
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return "req_" + hex.EncodeToString(raw[:])
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}})
}

// ---- readiness, manifests, receipts, models ----

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request, scoped *Route) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	if !s.validEdge(r) {
		s.writeError(w, http.StatusUnauthorized, "edge_unauthorized", "missing edge authorization")
		return
	}
	response := map[string]any{"status": "ready"}
	if scoped != nil {
		response["models"] = []string{scoped.ModelID}
		response["billingContractHash"] = s.hashes[scoped.ModelID]
	} else {
		ids := make([]string, 0, len(s.routes))
		for id := range s.routes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		response["models"] = ids
		hashes := make(map[string]string, len(ids))
		for _, id := range ids {
			hashes[id] = s.hashes[id]
		}
		response["billingContractHashes"] = hashes
	}
	loadedAt, err := s.settings.Verification.Health()
	verification := map[string]any{"source": s.settings.Verification.Source(), "loaded_at": loadedAt.Unix()}
	if err != nil {
		verification["error"] = err.Error()
	}
	response["verification"] = verification
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

// handleSettlementHealth is this instance's backlog for monitors: 200 while the oldest owed
// settlement is under the alert age, 503 once it is older. It never touches /readyz, which the
// platform reads to list the models: a slow ledger must not take the routes offline. It answers only
// to HealthToken, and not at all when none is set.
func (s *Server) handleSettlementHealth(w http.ResponseWriter, r *http.Request) {
	if s.settings.HealthToken == "" {
		s.writeError(w, http.StatusNotFound, "not_found", "unknown path")
		return
	}
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	received := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(received) != len(s.settings.HealthToken) || !hmac.Equal([]byte(received), []byte(s.settings.HealthToken)) {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "missing or wrong health token")
		return
	}
	health := s.settings.Settlements.Health(s.settings.AlertAge)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if health.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"instance_id": s.settings.Signer.InstanceID, "signing_key_id": s.settings.Signer.KeyID, "settlements": health})
}

func (s *Server) manifest(route *Route) (SignedDocument, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.settings.Now()
	if cached, ok := s.manifests[route.ModelID]; ok && now.Before(cached.expires.Add(-time.Minute)) {
		return cached.document, cached.identity, nil
	}
	document, identity, err := s.settings.Signer.RouteManifest(*route, s.settings.BoardURL, now, s.settings.ManifestWindow, s.settings.KeySource == "vm-kms")
	if err != nil {
		return SignedDocument{}, "", err
	}
	s.manifests[route.ModelID] = cachedManifest{document: document, identity: identity, expires: now.Add(s.settings.ManifestWindow)}
	return document, identity, nil
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request, scoped *Route) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	routes := []*Route{}
	if scoped != nil {
		routes = append(routes, scoped)
	} else {
		for _, route := range s.routes {
			routes = append(routes, route)
		}
		sort.Slice(routes, func(a, b int) bool { return routes[a].ModelID < routes[b].ModelID })
	}
	manifests := make([]SignedDocument, 0, len(routes))
	for _, route := range routes {
		document, _, err := s.manifest(route)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "manifest_unavailable", err.Error())
			return
		}
		manifests = append(manifests, document)
	}
	w.Header().Set("Content-Type", "application/json")
	if scoped != nil {
		// One route answers like a runtime: the bare signed manifest, so poe-verify-receipt
		// verifies against https://router.lebrel.ai/m/{slug} exactly as against a runtime.
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(manifests[0])
		return
	}
	response := map[string]any{
		"kind": DocumentKindRoute, "public_key": hex.EncodeToString(s.settings.Signer.Public), "signing_key_id": s.settings.Signer.KeyID,
		"instance_id": s.settings.Signer.InstanceID, "verification_board": s.settings.BoardURL, "manifests": manifests,
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	// Receipts are public documents: they carry digests, counts and the price, never the
	// prompt, the answer or the account, and request ids are unguessable. Anyone the customer
	// hands a request id to can verify the answer, the same way runtime receipts are served.
	document, ok := s.settings.Receipts.Get(id)
	if !ok && r.Header.Get(receiptPeerHeader) == "" {
		document, ok = s.receiptFromPeers(r.Context(), id)
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "receipt_not_found", "no receipt for that request id (receipts expire)")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(document)
}

// receiptFromPeers asks the other instances for a receipt this one does not hold: behind a load
// balancer the request may have been answered by any of them. Only a receipt for this request id
// signed with this router's key is served (every instance signs with the same key).
func (s *Server) receiptFromPeers(ctx context.Context, id string) (SignedDocument, bool) {
	if len(s.settings.ReceiptPeers) == 0 || !validRequestID(id) {
		return SignedDocument{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, peerReceiptTimeout)
	defer cancel()
	found := make(chan SignedDocument, len(s.settings.ReceiptPeers))
	var wg sync.WaitGroup
	for _, peer := range s.settings.ReceiptPeers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(peer, "/")+"/v1/receipts/"+id, nil)
			if err != nil {
				return
			}
			request.Header.Set(receiptPeerHeader, "1")
			request.Header.Set("User-Agent", userAgent)
			response, err := s.settings.PeerClient.Do(request)
			if err != nil {
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return
			}
			var document SignedDocument
			if err := json.NewDecoder(io.LimitReader(response.Body, maxReceiptBytes)).Decode(&document); err != nil {
				return
			}
			if document.Payload["request_id"] != id || !Verify(document, s.settings.Signer.Public) {
				s.settings.Logger.Printf("receipt %s from peer %s refused: not this router's signature for this request", id, peer)
				return
			}
			found <- document
		}(peer)
	}
	go func() {
		wg.Wait()
		close(found)
	}()
	for document := range found {
		s.settings.Receipts.Put(id, document)
		return document, true
	}
	return SignedDocument{}, false
}

// validRequestID is the shape newRequestID gives: "req_" and 32 lowercase hex digits.
func validRequestID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "req_") {
		return false
	}
	for _, r := range id[4:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request, scoped *Route) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	routes := []*Route{}
	if scoped != nil {
		routes = append(routes, scoped)
	} else {
		for _, route := range s.routes {
			routes = append(routes, route)
		}
		sort.Slice(routes, func(a, b int) bool { return routes[a].ModelID < routes[b].ModelID })
	}
	data := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		upstreams := make([]map[string]any, 0, len(route.Upstreams))
		for _, u := range route.Upstreams {
			status := s.settings.Verification.ForRoute(u.Name, route.ModelID)
			upstreams = append(upstreams, map[string]any{"name": u.Name, "model": u.Model, "verification": status, "data_note": u.DataNote})
		}
		data = append(data, map[string]any{
			"id": route.ModelID, "object": "model", "owned_by": "lebrel-router",
			"lebrel": map[string]any{"level": route.Level, "family": route.Family, "claimed_revision": route.ClaimedRevision,
				"data_note": route.DataNote, "pricing": route.Pricing, "upstreams": upstreams},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// ---- completions ----

type upstreamResult struct {
	upstream *Upstream
	response *http.Response
	// release ends the call's context once the answer is done with
	release func()
}

func (s *Server) handleCompletion(w http.ResponseWriter, r *http.Request, scoped *Route) {
	s.active.Add(1)
	defer s.active.Add(-1)
	requestID := newRequestID()
	s.privacyHeaders(w, requestID)
	if !s.admit(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if r.URL.RawQuery != "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request_error", "query strings are not accepted")
		return
	}
	// the request's context ends with the customer, or when a shutdown cuts the answers in flight
	ctx, stop := context.WithCancelCause(r.Context())
	defer stop(nil)
	defer context.AfterFunc(s.cutCtx, func() { stop(errShutdownCut) })()
	// The encrypted path authenticates the key before decrypting and hands the principal over.
	principal, already := ctx.Value(principalKey{}).(auth.Principal)
	var err error
	if !already {
		principal, err = s.settings.Auth.Authenticate(s.authContext(ctx, r), r.Header.Get("Authorization"))
	}
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			s.writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
		} else {
			s.writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
		}
		return
	}
	if held, _ := ctx.Value(limitsKey{}).(bool); !held {
		releaseKey, err := s.settings.Limiter.Acquire(ctx, "key:"+principal.APIKeyID, principal.Concurrency)
		if err != nil {
			w.Header().Set("Retry-After", "1")
			s.writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many concurrent requests for this key")
			return
		}
		defer releaseKey()
		releaseAccount, err := s.settings.Limiter.Acquire(ctx, "account:"+principal.AccountID, principal.AccountConcurrency)
		if err != nil {
			w.Header().Set("Retry-After", "1")
			s.writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many concurrent requests for this account")
			return
		}
		defer releaseAccount()
	}

	original, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.settings.MaxRequestBytes))
	if err != nil {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(original, &peek); err != nil || strings.TrimSpace(peek.Model) == "" {
		s.writeError(w, http.StatusBadRequest, "invalid_request_error", "model must be a non-empty string")
		return
	}
	route, ok := s.routes[peek.Model]
	if !ok || (scoped != nil && scoped != route) {
		s.writeError(w, http.StatusNotFound, "model_not_found", "unknown model")
		return
	}
	normalized, info, err := meter.InspectAndRequireUsage(completionPath, original, route.DefaultMaxOutputTokens, route.MaxOutputTokens)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if info.EstimatedTotalTokens() > route.MaxContextTokens {
		s.writeError(w, http.StatusBadRequest, "context_length_exceeded", "prompt plus max output exceeds the model's context")
		return
	}
	grace := cutWriteGrace
	if !info.Stream {
		grace = cutBufferedWriteGrace
	}
	defer s.boundWritesOnCut(w, grace)()
	promptEstimate := info.EstimatedPromptTokens
	info.EstimatedPromptTokens = generousEstimate(info.EstimatedPromptTokens)
	if info.EstimatedTotalTokens() > route.MaxContextTokens {
		info.EstimatedPromptTokens = route.MaxContextTokens - info.MaxOutputTokens
	}
	pricing := route.MeterPricing()
	queue := meter.QueueFor(info, route.LongContextThreshold())
	percent := route.MultiplierPercent(s.settings.Now())
	amount, err := pricing.ReservationScaled(info, queue, percent)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request_error", "request is too large to price")
		return
	}
	reservation, err := s.settings.Ledger.Reserve(ctx, principal.AccountID, principal.APIKeyID, route.ModelID, s.hashes[route.ModelID], requestID, amount, queue, info.EstimatedPromptTokens, info.MaxOutputTokens)
	if errors.Is(err, ledger.ErrReservationConflict) {
		// The control plane's clock sits on the other side of a peak boundary: price as it does.
		if other, ok := route.OtherMultiplierPercent(percent); ok {
			percent = other
			if amount, err = pricing.ReservationScaled(info, queue, percent); err == nil {
				reservation, err = s.settings.Ledger.Reserve(ctx, principal.AccountID, principal.APIKeyID, route.ModelID, s.hashes[route.ModelID], requestID, amount, queue, info.EstimatedPromptTokens, info.MaxOutputTokens)
			}
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrInsufficientCredit):
			s.writeError(w, http.StatusPaymentRequired, "insufficient_credit", "not enough prepaid credit for this request")
		case errors.Is(err, ledger.ErrConcurrencyLimit):
			w.Header().Set("Retry-After", "1")
			s.writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "account concurrency ceiling reached")
		default:
			s.writeError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing is temporarily unavailable")
		}
		return
	}
	if a, ok := ctx.Value(admissionKey{}).(*admission); ok {
		a.reserved = true
	}
	settled := false
	servedBy := ""
	// The money is decided here; telling the control plane is owed, never waited for: the
	// queue delivers it now if it can, and keeps it on disk until it can.
	settle := func(actual int64, usage model.Usage, outcome model.SettlementOutcome) {
		if settled {
			return
		}
		settled = true
		s.settings.Settlements.Settle(reservation.ID, requestID, actual, usage, outcome, servedBy)
	}
	defer settle(0, model.Usage{}, model.SettlementUpstreamRejected)

	result, lastErr := s.callUpstreams(ctx, route, normalized, requestID, r.Header.Get("Accept"))
	if result == nil {
		settle(0, model.Usage{}, model.SettlementUpstreamRejected)
		if cutCaused(ctx, lastErr) {
			// a shutdown cut it while the lab had not answered yet: never accepted, nothing charged
			s.noteCut(reservation.ID)
			s.settings.Logger.Printf("request=%s model=%s cut at shutdown before the upstream answered, charged 0", requestID, route.ModelID)
			w.Header().Set("Retry-After", "1")
			s.writeError(w, http.StatusServiceUnavailable, "router_restarting", "the router is restarting; retry the request")
			return
		}
		s.settings.Logger.Printf("request=%s model=%s all upstreams failed: %v", requestID, route.ModelID, lastErr)
		s.noteOutage(route, lastErr)
		s.writeError(w, http.StatusBadGateway, "upstream_unavailable", "no upstream could serve the request")
		return
	}
	upstream, response := result.upstream, result.response
	servedBy = upstream.Name
	defer result.release()
	defer response.Body.Close()
	w.Header().Set(upstreamHeader, upstream.Name)
	status := s.settings.Verification.ForRoute(upstream.Name, route.ModelID)
	w.Header().Set(verificationHeader, status.Status)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
		settle(0, model.Usage{}, model.SettlementUpstreamRejected)
		w.Header().Set("Content-Type", contentTypeOr(response, "application/json"))
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
		s.settings.Logger.Printf("request=%s model=%s upstream=%s status=%d rejected", requestID, route.ModelID, upstream.Name, response.StatusCode)
		return
	}
	// The upstream accepted the request. The control plane learns it before the customer sees a
	// byte, so the hold can never be closed as unanswered while its answer is on the way.
	if err := s.acceptUpstream(ctx, reservation.ID, requestID); err != nil {
		s.settings.Logger.Printf("request=%s model=%s upstream=%s the control plane refused the acceptance, answer withheld: %v", requestID, route.ModelID, upstream.Name, err)
		s.writeError(w, http.StatusServiceUnavailable, "billing_unavailable", "billing could not record this request")
		return
	}
	_, manifestIdentity, err := s.manifest(route)
	if err != nil {
		s.settings.Logger.Printf("request=%s manifest: %v", requestID, err)
	}
	receipt := ReceiptInput{
		RequestID: requestID, ModelID: route.ModelID, ManifestIdentity: manifestIdentity, Upstream: upstream.Name, UpstreamModel: upstream.Model,
		Verification: status, FeeBPS: route.Pricing.FeeBPS, Streamed: info.Stream,
		PricingTier: PricingTier(percent), MultiplierPercent: percent, HashSalt: receiptSalt(ctx),
	}
	if len(receipt.HashSalt) > 0 {
		receipt.HashSaltScheme = ReceiptSaltScheme
	}
	receipt.PromptSHA256 = receipt.Digest(original)
	started := s.settings.Now()
	var cut bool
	if info.Stream {
		cut = s.finishStream(w, response, upstream, route, pricing, queue, info, percent, promptEstimate, receipt, settle, requestID, started)
	} else {
		cut = s.finishBuffered(w, response, upstream, route, pricing, queue, info, percent, receipt, settle, requestID, started)
	}
	if cut {
		s.noteCut(reservation.ID)
	}
}

// noteCut lists a reservation a shutdown cut, for the cut alert.
func (s *Server) noteCut(reservationID string) {
	s.cutMu.Lock()
	s.cutIDs = append(s.cutIDs, reservationID)
	s.cutMu.Unlock()
}

// cutCaused is true when a shutdown's cut, and nothing else, ended the call to the labs: it came
// before the customer left, and the call failed because it was cancelled, not on its own.
func cutCaused(ctx context.Context, err error) bool {
	return errors.Is(context.Cause(ctx), errShutdownCut) && (errors.Is(err, context.Canceled) || errors.Is(err, errShutdownCut))
}

// boundWritesOnCut gives an answer's writes grace once a shutdown cuts it: a write blocked on a
// customer who stopped reading then fails, and the answer is settled for what it relayed. The func
// it returns runs as the handler returns; from then on the response is never touched.
func (s *Server) boundWritesOnCut(w http.ResponseWriter, grace time.Duration) func() {
	var mu sync.Mutex
	finished := false
	controller := http.NewResponseController(w)
	stop := context.AfterFunc(s.cutCtx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			_ = controller.SetWriteDeadline(time.Now().Add(grace))
		}
	})
	return func() {
		stop()
		mu.Lock()
		finished = true
		mu.Unlock()
	}
}

// InFlight is how many completions are being answered now.
func (s *Server) InFlight() int64 {
	return s.active.Load()
}

// CutStreams ends the answers still in flight once a shutdown's budget is spent: each one is
// settled for what it had relayed (never left as an accepted hold for the reconciler's maximum).
// It waits for their handlers until ctx ends and returns every reservation cut so far; called again
// once the server's connections are closed, it adds those whose handlers ended only then.
func (s *Server) CutStreams(ctx context.Context) []string {
	s.cutAll()
	for s.active.Load() > 0 && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.cutMu.Lock()
	defer s.cutMu.Unlock()
	return append([]string(nil), s.cutIDs...)
}

// relayedUsage is what an answer cut at shutdown is charged for: the prompt as estimated before
// the reservation's margin, and for the answer (its text and its reasoning, both sent to the
// customer and both billed by the lab) at least one token per event relayed and one per four
// characters, whichever is more, never above the request's output cap. Nothing relayed is nothing
// charged.
func relayedUsage(promptEstimate int64, events int, runes int, maxOutput int64) model.Usage {
	completion := int64(events)
	if byChars := int64(runes+3) / 4; byChars > completion {
		completion = byChars
	}
	if completion == 0 {
		return model.Usage{}
	}
	if completion > maxOutput {
		completion = maxOutput
	}
	if promptEstimate < 0 {
		promptEstimate = 0
	}
	return model.Usage{PromptTokens: promptEstimate, CompletionTokens: completion, TotalTokens: promptEstimate + completion}
}

// acceptUpstream marks the reservation accepted inside the request. A slow or unreachable control
// plane never costs the customer the answer: past AcceptTimeout, or on any failure that is not an
// answer, the acceptance is owed through the queue, which delivers it before this reservation's
// settlement. A refusal (the reservation is gone or already closed) is returned: nothing could be
// charged for the answer, so it is withheld and the hold is closed at zero.
func (s *Server) acceptUpstream(ctx context.Context, reservationID, requestID string) error {
	// a customer hanging up must not cut the call halfway: its outcome decides what is owed
	attempt, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.settings.AcceptTimeout)
	err := s.settings.Ledger.MarkAccepted(attempt, reservationID)
	cancel()
	switch {
	case err == nil:
		return nil
	case settle.Transient(err):
		s.settings.Settlements.Accepted(reservationID, requestID)
		s.settings.Logger.Printf("request=%s acceptance owed through the settlement queue: %v", requestID, err)
		return nil
	default:
		return err
	}
}

// generousEstimate widens the gateway's chars/4 estimate so the provider's own count fits the
// reserved envelope; the surplus is refunded at settlement.
func generousEstimate(estimate int64) int64 {
	if estimate < 0 {
		return 0
	}
	if estimate > (1<<62)/2 {
		return estimate
	}
	return estimate + estimate/2 + 64
}

func contentTypeOr(response *http.Response, fallback string) string {
	if value := response.Header.Get("Content-Type"); value != "" {
		return value
	}
	return fallback
}

func (s *Server) callUpstreams(ctx context.Context, route *Route, normalized []byte, requestID, accept string) (*upstreamResult, error) {
	var lastErr error
	// the upstreams tried before the one that answers, each with why it did not
	var skipped []string
	for i := range route.Upstreams {
		upstream := &route.Upstreams[i]
		body, err := upstreamBody(normalized, upstream)
		if err != nil {
			return nil, err
		}
		// The upstream has its own time to answer with headers; the body then streams under ctx, and
		// the call's context ends with the answer (release) or here, when the next upstream is tried.
		callCtx, endCall := context.WithCancelCause(ctx)
		silence := time.AfterFunc(upstream.Timeout(), func() { endCall(errUpstreamSilent) })
		request, err := http.NewRequestWithContext(callCtx, http.MethodPost, strings.TrimRight(upstream.Endpoint(), "/")+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			silence.Stop()
			endCall(nil)
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("X-Request-ID", requestID)
		for name, value := range upstream.Headers {
			request.Header.Set(name, value)
		}
		if key := upstream.APIKey(); key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := s.settings.HTTPClient.Do(request)
		silence.Stop()
		if err != nil {
			endCall(nil)
			reason := "transport error"
			if errors.Is(context.Cause(callCtx), errUpstreamSilent) {
				reason = fmt.Sprintf("no answer within %s", upstream.Timeout())
			}
			lastErr = fmt.Errorf("%s: %s: %w", upstream.Name, reason, err)
			s.settings.Logger.Printf("request=%s upstream=%s %s, trying next", requestID, upstream.Name, reason)
			if ctx.Err() != nil {
				return nil, lastErr
			}
			skipped = append(skipped, upstream.Name+" ("+reason+")")
			continue
		}
		if retryStatuses[response.StatusCode] && i < len(route.Upstreams)-1 {
			lastErr = fmt.Errorf("%s: HTTP %d", upstream.Name, response.StatusCode)
			s.settings.Logger.Printf("request=%s upstream=%s status=%d, trying next", requestID, upstream.Name, response.StatusCode)
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBodyBytes))
			response.Body.Close()
			endCall(nil)
			skipped = append(skipped, fmt.Sprintf("%s (HTTP %d)", upstream.Name, response.StatusCode))
			continue
		}
		if credentialStatuses[response.StatusCode] {
			// The last upstream refused Lebrel's own credential or credit: that is Lebrel's outage,
			// never the customer's, so its words ("insufficient credits", "invalid key") are not
			// relayed as if they were about the customer's account.
			lastErr = fmt.Errorf("%s: HTTP %d", upstream.Name, response.StatusCode)
			s.settings.Logger.Printf("request=%s upstream=%s status=%d refused Lebrel's credential, no upstream left", requestID, upstream.Name, response.StatusCode)
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBodyBytes))
			response.Body.Close()
			endCall(nil)
			return nil, lastErr
		}
		if len(skipped) > 0 {
			s.noteFallback(route, upstream, skipped, requestID)
		}
		return &upstreamResult{upstream: upstream, response: response, release: func() { endCall(nil) }}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no upstreams")
	}
	return nil, lastErr
}

// errUpstreamSilent ends a call whose upstream has not answered within its time.
var errUpstreamSilent = errors.New("the upstream did not answer in time")

// alertEvery is how often the operator hears about one subject again (a route's fallbacks, a
// route's outage): the first time at once, then with the count since.
const alertEvery = 10 * time.Minute

type alertState struct {
	count     int
	alertedAt time.Time
}

// alertThrottled counts an occurrence of subject and, the first time and then at most every
// alertEvery, hands the operator text(count) with the occurrences since the last alert. Without
// an Alert sink nothing is sent; the count still keeps its place.
func (s *Server) alertThrottled(subject string, text func(count int) string) {
	s.alertMu.Lock()
	state := s.alerts[subject]
	state.count++
	now := s.settings.Now()
	due := state.alertedAt.IsZero() || now.Sub(state.alertedAt) >= alertEvery
	count := state.count
	if due {
		state.alertedAt, state.count = now, 0
	}
	s.alerts[subject] = state
	s.alertMu.Unlock()
	if due && s.settings.Alert != nil {
		s.settings.Alert(text(count))
	}
}

// noteFallback records an answer served by a later upstream because the ones before it failed:
// Lebrel's cost (a fallback is dearer than the first choice) and worth the operator's look, told
// at most every alertEvery per route, in Spanish for the founder.
func (s *Server) noteFallback(route *Route, upstream *Upstream, skipped []string, requestID string) {
	s.settings.Logger.Printf("request=%s model=%s served by fallback %s after %s", requestID, route.ModelID, upstream.Name, strings.Join(skipped, ", "))
	s.alertThrottled("fallback:"+route.ModelID, func(count int) string {
		return fmt.Sprintf("%s: falló %s y respondió el respaldo %s. %d respuestas por respaldo en esta ruta desde el último aviso (avisa como mucho cada 10 min): cuesta más que el camino principal. 👉 Mira el proveedor principal si sigue.",
			route.Name(), strings.Join(skipped, " y "), upstream.Name, count)
	})
}

// noteOutage records a request no upstream could serve: the customer got a 502.
func (s *Server) noteOutage(route *Route, cause error) {
	s.alertThrottled("outage:"+route.ModelID, func(count int) string {
		return fmt.Sprintf("%s: ningún proveedor pudo responder (%v). %d peticiones devueltas con 502 en esta ruta desde el último aviso (avisa como mucho cada 10 min). 👉 Revisa los proveedores de la ruta.",
			route.Name(), cause, count)
	})
}

// upstreamBody rewrites the model id and applies the upstream's fixed fields; a client
// can never choose the provider behind a route, only the route.
func upstreamBody(normalized []byte, upstream *Upstream) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	payload["model"] = upstream.Model
	translateDialect(payload, upstream.Dialect, upstream.ReasoningFixed, upstream.ReasoningEfforts)
	for key, value := range upstream.Defaults {
		if _, present := payload[key]; !present {
			payload[key] = value
		}
	}
	for key, value := range upstream.Body {
		payload[key] = value
	}
	return json.Marshal(payload)
}

// translateDialect rewrites the labs' own reasoning switches for a relay that speaks another
// dialect. OpenRouter takes `reasoning: {"enabled": bool}`; DeepSeek documents `thinking:
// {"type": "disabled" | "enabled"}` and Alibaba `enable_thinking: bool`, which the clients of these
// routes send. Both are mapped onto `reasoning` unless the client sent `reasoning` itself, and the
// lab-native keys are dropped: the relay would ignore them and reason by its own default. Alibaba's
// Model Studio has its own translation (translateAlibaba). Any other dialect leaves the payload as it is.
func translateDialect(payload map[string]any, dialect string, reasoningFixed bool, efforts []string) {
	if dialect == "alibaba" {
		translateAlibaba(payload, reasoningFixed, efforts)
		return
	}
	if dialect != "openrouter" {
		return
	}
	var enabled *bool
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		switch thinking["type"] {
		case "disabled":
			value := false
			enabled = &value
		case "enabled":
			value := true
			enabled = &value
		}
	}
	if value, ok := payload["enable_thinking"].(bool); ok && enabled == nil {
		enabled = &value
	}
	delete(payload, "thinking")
	delete(payload, "enable_thinking")
	if enabled == nil {
		return
	}
	if _, present := payload["reasoning"]; !present {
		if !*enabled && reasoningFixed {
			payload["reasoning"] = map[string]any{"effort": "low"}
			return
		}
		payload["reasoning"] = map[string]any{"enabled": *enabled}
	}
}

// translateAlibaba speaks Model Studio's OpenAI-compatible mode: reasoning is `enable_thinking: bool`
// (Alibaba's own switch, which Qwen clients already send); DeepSeek's `thinking: {"type"}` and
// OpenRouter's `reasoning: {"enabled"}` are mapped onto it, and the keys the host does not take
// (`thinking`, `reasoning`) are dropped so the request is not refused. `reasoning_effort` goes
// through: Model Studio takes it for DeepSeek, GLM, Kimi and Qwen, each with its own values and an
// error for any other, so a value outside the endpoint's list (efforts, lowest first) is dropped and
// the host's default effort applies rather than the request failing. A host that always reasons
// (`reasoning_fixed`) gets no switch; a client asking it for no reasoning gets the lowest effort
// the endpoint takes, unless the client named one.
func translateAlibaba(payload map[string]any, reasoningFixed bool, efforts []string) {
	var enabled *bool
	if value, ok := payload["enable_thinking"].(bool); ok {
		enabled = &value
	}
	if thinking, ok := payload["thinking"].(map[string]any); ok && enabled == nil {
		switch thinking["type"] {
		case "disabled":
			value := false
			enabled = &value
		case "enabled":
			value := true
			enabled = &value
		}
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok && enabled == nil {
		if value, ok := reasoning["enabled"].(bool); ok {
			enabled = &value
		}
	}
	delete(payload, "thinking")
	delete(payload, "reasoning")
	delete(payload, "enable_thinking")
	if effort, ok := payload["reasoning_effort"].(string); !ok || !effortAllowed(effort, efforts) {
		delete(payload, "reasoning_effort")
	}
	if enabled == nil {
		return
	}
	if !reasoningFixed {
		payload["enable_thinking"] = *enabled
		return
	}
	if _, named := payload["reasoning_effort"]; !*enabled && !named && len(efforts) > 0 {
		payload["reasoning_effort"] = efforts[0]
	}
}

// effortAllowed says whether the endpoint takes the effort: any, when it lists none.
func effortAllowed(effort string, efforts []string) bool {
	if len(efforts) == 0 {
		return true
	}
	for _, allowed := range efforts {
		if allowed == effort {
			return true
		}
	}
	return false
}

type assistantContent struct {
	Model             string
	Provider          string
	Text              string
	ResponseID        string
	SystemFingerprint string
}

func extractAssistant(body []byte) assistantContent {
	var document struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Provider          string `json:"provider"`
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	content := assistantContent{}
	if err := json.Unmarshal(body, &document); err != nil {
		return content
	}
	content.Model, content.Provider = document.Model, document.Provider
	content.ResponseID, content.SystemFingerprint = boundedID(document.ID), boundedID(document.SystemFingerprint)
	if len(document.Choices) == 0 {
		return content
	}
	raw := document.Choices[0].Message.Content
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		content.Text = text
		return content
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var builder strings.Builder
		for _, part := range parts {
			builder.WriteString(part.Text)
		}
		content.Text = builder.String()
	}
	return content
}

// finishBuffered relays a whole answer at once; true when a shutdown cut it before it was read.
func (s *Server) finishBuffered(w http.ResponseWriter, response *http.Response, upstream *Upstream, route *Route, pricing meter.Pricing,
	queue model.QueueClass, info meter.RequestInfo, percent int64, receipt ReceiptInput, settle func(int64, model.Usage, model.SettlementOutcome), requestID string, started time.Time) bool {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamBodyBytes))
	if err != nil {
		if s.cutCtx.Err() != nil {
			// cut at shutdown before anything reached the customer: nothing is charged
			settle(0, model.Usage{}, model.SettlementSucceeded)
			s.writeError(w, http.StatusServiceUnavailable, "router_restarting", "the router restarted before this answer was complete; nothing was charged")
			s.settings.Logger.Printf("request=%s model=%s upstream=%s cut at shutdown before the answer was read, charged 0", requestID, route.ModelID, upstream.Name)
			return true
		}
		maximum, _ := pricing.MaximumScaled(info, queue, percent)
		settle(maximum, model.Usage{}, model.SettlementUsageMissing)
		s.writeError(w, http.StatusBadGateway, "upstream_read_failed", "the upstream response could not be read")
		return false
	}
	usage, exact := ParseUsage(upstream.UsageFormat, body)
	actual, billed, outcome := s.charge(pricing, queue, info, percent, usage, exact)
	exact = outcome == model.SettlementSucceeded
	settle(actual, billed, outcome)
	assistant := extractAssistant(body)
	receipt.ServedModel, receipt.Provider = assistant.Model, assistant.Provider
	receipt.UpstreamResponseID, receipt.UpstreamSystemFingerprint = assistant.ResponseID, assistant.SystemFingerprint
	receipt.ResponseSHA256 = receipt.Digest([]byte(assistant.Text))
	receipt.PromptTokens, receipt.CompletionTokens, receipt.CachedTokens = usage.PromptTokens, usage.CompletionTokens, usage.CachedTokens
	receipt.UsageExact, receipt.PriceMicrousd = exact, actual
	document, err := s.settings.Signer.RouteReceipt(receipt, s.settings.Now())
	if err == nil {
		s.settings.Receipts.Put(requestID, document)
		if encoded, err := json.Marshal(document); err == nil {
			w.Header().Set(receiptHeader, base64.StdEncoding.EncodeToString(encoded))
		}
	}
	w.Header().Set("Content-Type", contentTypeOr(response, "application/json"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	s.settings.Logger.Printf("request=%s model=%s upstream=%s status=200 tier=%s prompt=%d completion=%d cached=%d exact=%t charge=%d ms=%d",
		requestID, route.ModelID, upstream.Name, PricingTier(percent), usage.PromptTokens, usage.CompletionTokens, usage.CachedTokens, exact, actual, s.settings.Now().Sub(started).Milliseconds())
	return false
}

// charge mirrors the control plane's envelope rule: inside the reserved envelope the exact
// usage is billed, never more than the reserved maximum; usage that does not fit the envelope
// (a provider counting far more prompt tokens than our estimate) is settled as usage_missing,
// which charges the maximum under the "reserved" policy. Either way the client never pays
// more than the envelope it was quoted.
func (s *Server) charge(pricing meter.Pricing, queue model.QueueClass, info meter.RequestInfo, percent int64, usage model.Usage, exact bool) (int64, model.Usage, model.SettlementOutcome) {
	maximum, _ := pricing.MaximumScaled(info, queue, percent)
	if exact && usage.CompletionTokens <= info.MaxOutputTokens && usage.PromptTokens+usage.CompletionTokens <= info.EstimatedPromptTokens+info.MaxOutputTokens {
		if actual, err := pricing.ActualScaled(usage, queue, percent); err == nil {
			if actual > maximum {
				actual = maximum
			}
			return actual, usage, model.SettlementSucceeded
		}
	}
	return maximum, model.Usage{}, model.SettlementUsageMissing
}

// finishStream relays an answer event by event; true when a shutdown cut it before its end.
func (s *Server) finishStream(w http.ResponseWriter, response *http.Response, upstream *Upstream, route *Route, pricing meter.Pricing,
	queue model.QueueClass, info meter.RequestInfo, percent int64, promptEstimate int64, receipt ReceiptInput, settle func(int64, model.Usage, model.SettlementOutcome),
	requestID string, started time.Time) bool {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Every open stream holds this buffer, so it stays small: ReadBytes gathers a longer line
	// (a large tool call in one event) across refills, it never cuts one.
	reader := bufio.NewReaderSize(response.Body, streamBufferBytes)
	var usage model.Usage
	var sawUsage, sawDone, clientGone bool
	var text strings.Builder
	events, reasoningRunes := 0, 0
	var servedModel, provider, responseID, systemFingerprint string
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				// the customer is gone, or stopped reading past a cut's grace: this line never
				// reached them
				clientGone = true
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
				if bytes.Equal(payload, []byte("[DONE]")) {
					sawDone = true
				} else if len(payload) > 0 {
					if parsed, ok := ParseUsage(upstream.UsageFormat, payload); ok {
						usage, sawUsage = parsed, true
					}
					var event struct {
						ID                string `json:"id"`
						Model             string `json:"model"`
						Provider          string `json:"provider"`
						SystemFingerprint string `json:"system_fingerprint"`
						Choices           []struct {
							Delta struct {
								Content          string          `json:"content"`
								ReasoningContent json.RawMessage `json:"reasoning_content"`
								Reasoning        json.RawMessage `json:"reasoning"`
							} `json:"delta"`
						} `json:"choices"`
					}
					if json.Unmarshal(payload, &event) == nil {
						if event.Model != "" {
							servedModel = event.Model
						}
						if event.Provider != "" {
							provider = event.Provider
						}
						if event.ID != "" && responseID == "" {
							responseID = boundedID(event.ID)
						}
						if event.SystemFingerprint != "" {
							systemFingerprint = boundedID(event.SystemFingerprint)
						}
						if len(event.Choices) > 0 {
							delta := event.Choices[0].Delta
							// labs name it one way or the other, some both for compatibility: once
							reasoning := textOf(delta.ReasoningContent)
							if reasoning == "" {
								reasoning = textOf(delta.Reasoning)
							}
							text.WriteString(delta.Content)
							if delta.Content != "" || reasoning != "" {
								events++
								reasoningRunes += utf8.RuneCountInString(reasoning)
							}
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	// A shutdown cut it, whatever the customer did meanwhile (one who stopped reading fails a write
	// on the cut's grace): what reached the customer is what is charged, an estimate (the receipt
	// says so), never the reserved maximum.
	cut := !sawDone && s.cutCtx.Err() != nil
	var actual int64
	var exact bool
	if cut {
		usage = relayedUsage(promptEstimate, events, utf8.RuneCountInString(text.String())+reasoningRunes, info.MaxOutputTokens)
		outcome := model.SettlementSucceeded
		if usage.TotalTokens > 0 {
			maximum, _ := pricing.MaximumScaled(info, queue, percent)
			if charged, err := pricing.ActualScaled(usage, queue, percent); err == nil {
				actual = min(charged, maximum)
			} else {
				// no price for the estimate: the contract's rule for missing usage applies
				actual, usage, outcome = maximum, model.Usage{}, model.SettlementUsageMissing
			}
		}
		settle(actual, usage, outcome)
		s.settings.Logger.Printf("request=%s model=%s upstream=%s cut at shutdown after %d events, charged %d for an estimated %d+%d tokens",
			requestID, route.ModelID, upstream.Name, events, actual, usage.PromptTokens, usage.CompletionTokens)
	} else {
		var billed model.Usage
		var outcome model.SettlementOutcome
		exact = sawUsage && sawDone && !clientGone
		actual, billed, outcome = s.charge(pricing, queue, info, percent, usage, exact)
		exact = outcome == model.SettlementSucceeded
		settle(actual, billed, outcome)
	}
	receipt.ServedModel, receipt.Provider = servedModel, provider
	receipt.UpstreamResponseID, receipt.UpstreamSystemFingerprint = responseID, systemFingerprint
	receipt.ResponseSHA256 = receipt.Digest([]byte(text.String()))
	receipt.PromptTokens, receipt.CompletionTokens, receipt.CachedTokens = usage.PromptTokens, usage.CompletionTokens, usage.CachedTokens
	receipt.UsageExact, receipt.PriceMicrousd = exact, actual
	if document, err := s.settings.Signer.RouteReceipt(receipt, s.settings.Now()); err == nil {
		s.settings.Receipts.Put(requestID, document)
	}
	s.settings.Logger.Printf("request=%s model=%s upstream=%s status=200 stream=true prompt=%d completion=%d exact=%t charge=%d ms=%d",
		requestID, route.ModelID, upstream.Name, usage.PromptTokens, usage.CompletionTokens, exact, actual, s.settings.Now().Sub(started).Milliseconds())
	return cut
}

// textOf reads a JSON string; anything else (absent, null, an object) holds no text.
func textOf(raw json.RawMessage) string {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

// publicDocumentPath is true for the signed documents anyone may read from any origin.
func publicDocumentPath(path string) bool {
	return path == "/.well-known/proof-of-edition" || path == AttestationPath || path == EncryptionMetadataPath || path == "/v1/models" || strings.HasPrefix(path, "/v1/receipts/")
}

// boundedID keeps an upstream identifier printable and short enough for a receipt.
func boundedID(value string) string {
	if len(value) > 128 {
		value = value[:128]
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}
