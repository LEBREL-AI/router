package router

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tinfoilsh/encrypted-http-body-protocol/identity"
	"github.com/tinfoilsh/encrypted-http-body-protocol/protocol"
)

// An edition route is a Lebrel edition (the uncensored model on its own GPUs) served through this
// router to ordinary API clients: the router does here, inside the attested machine, what the
// local proxy did on the customer's machine. The request arrives in plaintext over the router's own
// TLS, is encrypted to the runtime's published key (checked against the pinned signing key, as the
// encrypted client checks it), and goes to Lebrel's edge exactly as the proxy's did: the edge admits
// and bills it without reading it, the runtime decrypts it, and the answer comes back encrypted to
// this process, which hands it to the client in plaintext over the same TLS connection. The edge and
// everything between see ciphertext; the plaintext exists only here and on the GPU that runs the
// model. Nothing changes for the runtime or the edge.

const (
	routeKindEdition = "edition"
	// editionMetadataMax bounds the runtime's signed encryption document.
	editionMetadataMax = 64 << 10
	// editionRequestMax is what the edge accepts as ciphertext.
	editionRequestMax = 8 << 20
	// editionHeaderTime is how long the edge may hold a request in line before answering.
	editionHeaderTime = 180 * time.Second
	// editionKeyRefresh is how long before its expiry a cached key is fetched again.
	editionKeyRefresh      = 30 * time.Second
	editionRequestIDHeader = "X-Lebrel-Request-Id"
	editionKeyIDHeader     = "X-Lebrel-Encryption-Key-Id"
	editionTierHeader      = "X-Lebrel-Service-Tier"
)

var (
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// EditionConfig says where an edition route sends its encrypted requests and whose key signs the
// runtime's encryption document.
type EditionConfig struct {
	// Origin is Lebrel's edge (https://api.lebrel.ai), which admits and bills the request as it does
	// the local proxy's; OriginEnv names an environment variable that overrides it (local development).
	Origin    string `json:"origin"`
	OriginEnv string `json:"origin_env,omitempty"`
	// SigningKeyB64 is the runtime's receipt-signing public key, base64 (the key the encrypted client pins).
	SigningKeyB64 string `json:"signing_key_b64"`
}

// Endpoint is the origin the requests go to: the environment's when OriginEnv names one, else Origin.
func (e EditionConfig) Endpoint() string {
	if e.OriginEnv != "" {
		if value := strings.TrimSpace(envValue(e.OriginEnv)); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return strings.TrimRight(e.Origin, "/")
}

func (e EditionConfig) SigningKey() (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(e.SigningKeyB64))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("signing_key_b64 must be the base64 of a 32-byte Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// editionKey is the runtime's encryption key as its signed document declares it, verified.
type editionKey struct {
	identity *identity.Identity
	keyID    string
	expires  time.Time
}

// verifyEditionMetadata checks the runtime's signed encryption document exactly as the encrypted
// client does (lebrel-encrypted, verify_metadata): the envelope, the signature by the pinned key,
// the exact set of fields, the model, a validity of at most ten minutes that includes now, the key
// id as the digest of the HPKE configuration, and a configuration that re-marshals to itself.
func verifyEditionMetadata(raw []byte, signingKey ed25519.PublicKey, now time.Time, modelID string) (*editionKey, error) {
	if len(raw) > editionMetadataMax || len(signingKey) != ed25519.PublicKeySize {
		return nil, errors.New("invalid configuration size")
	}
	var outer struct {
		Version   json.Number `json:"version"`
		Payload   string      `json:"payload"`
		Signature string      `json:"signature"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&outer); err != nil || outer.Version.String() != "1" {
		return nil, errors.New("unsupported envelope")
	}
	payload, err := base64.StdEncoding.DecodeString(outer.Payload)
	if err != nil || len(payload) > editionMetadataMax {
		return nil, errors.New("invalid signed payload")
	}
	signature, err := base64.StdEncoding.DecodeString(outer.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, errors.New("invalid signed payload")
	}
	if !ed25519.Verify(signingKey, payload, signature) {
		return nil, errors.New("the configuration is not signed by the runtime's key")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, errors.New("invalid configuration fields")
	}
	required := []string{"expiresAt", "hpkeConfig", "issuedAt", "keyId", "modelId", "serverInstanceId", "signingKeyId", "version"}
	if len(fields) != len(required) {
		return nil, errors.New("invalid configuration fields")
	}
	for _, name := range required {
		if _, present := fields[name]; !present {
			return nil, errors.New("invalid configuration fields")
		}
	}
	var version, issued, expires json.Number
	var model, keyID, signingKeyID, instance, configB64 string
	for name, into := range map[string]any{"version": &version, "issuedAt": &issued, "expiresAt": &expires, "modelId": &model, "keyId": &keyID, "signingKeyId": &signingKeyID, "serverInstanceId": &instance, "hpkeConfig": &configB64} {
		if err := json.Unmarshal(fields[name], into); err != nil {
			return nil, errors.New("invalid configuration fields")
		}
	}
	if version.String() != "1" || model != modelID {
		return nil, errors.New("wrong model or version")
	}
	issuedAt, err1 := issued.Int64()
	expiresAt, err2 := expires.Int64()
	if err1 != nil || err2 != nil || expiresAt-issuedAt <= 0 || expiresAt-issuedAt > 600 {
		return nil, errors.New("invalid validity interval")
	}
	if issuedAt > now.Unix()+30 || expiresAt <= now.Unix() {
		return nil, errors.New("expired or future configuration")
	}
	if !hex64.MatchString(keyID) {
		return nil, errors.New("invalid key identifier")
	}
	if digest := sha256.Sum256(signingKey); signingKeyID != hex.EncodeToString(digest[:]) {
		return nil, errors.New("wrong signing key identifier")
	}
	if !hex32.MatchString(instance) {
		return nil, errors.New("invalid server instance identifier")
	}
	config, err := base64.StdEncoding.DecodeString(configB64)
	if err != nil {
		return nil, errors.New("invalid configuration")
	}
	if digest := sha256.Sum256(config); hex.EncodeToString(digest[:]) != keyID {
		return nil, errors.New("configuration digest mismatch")
	}
	id, err := identity.UnmarshalPublicConfig(config)
	if err != nil {
		return nil, errors.New("invalid HPKE configuration")
	}
	canonical, err := id.MarshalConfig()
	if err != nil || !bytes.Equal(canonical, config) {
		return nil, errors.New("non-canonical HPKE configuration")
	}
	return &editionKey{identity: id, keyID: keyID, expires: time.Unix(expiresAt, 0)}, nil
}

// editionKeys caches each edition route's verified key until shortly before it expires.
type editionKeys struct {
	mu   sync.Mutex
	keys map[string]*editionKey
}

// editionKey is the edition's verified encryption key, fetched from the route's origin when none is
// cached or the cached one is about to expire.
func (s *Server) editionKey(ctx context.Context, route *Route, refresh bool) (*editionKey, error) {
	s.editions.mu.Lock()
	cached := s.editions.keys[route.ModelID]
	s.editions.mu.Unlock()
	now := s.settings.Now()
	if cached != nil && !refresh && now.Before(cached.expires.Add(-editionKeyRefresh)) {
		return cached, nil
	}
	signingKey, err := route.Edition.SigningKey()
	if err != nil {
		return nil, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, route.Edition.Endpoint()+EncryptionMetadataPath, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", userAgent)
	response, err := s.settings.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("the edition's encryption document could not be fetched: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the edition's encryption document answered HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, editionMetadataMax+1))
	if err != nil {
		return nil, err
	}
	key, err := verifyEditionMetadata(raw, signingKey, now, route.ModelID)
	if err != nil {
		return nil, fmt.Errorf("the edition's encryption document could not be verified: %w", err)
	}
	s.editions.mu.Lock()
	if s.editions.keys == nil {
		s.editions.keys = map[string]*editionKey{}
	}
	s.editions.keys[route.ModelID] = key
	s.editions.mu.Unlock()
	return key, nil
}

// editionResponseHeaders are the answer's headers handed to the client as they came: the receipt and
// what goes with it; nothing of the encryption itself, which ends here.
var editionResponseHeaders = []string{receiptHeader, contentRetentionHeader, "Retry-After"}

// handleEditionCompletion serves an edition route's completion: the plaintext request the client
// sent over the router's TLS is encrypted to the runtime's key and sent to the edge as the local
// proxy sent it; the encrypted answer is decrypted here and streamed back. The customer's key goes
// along as the edge expects it; the edge authenticates, admits and bills the request, so the router
// bills nothing for it and the receipt is the runtime's.
func (s *Server) handleEditionCompletion(ctx context.Context, w http.ResponseWriter, r *http.Request, route *Route, original []byte, requestID string) {
	decoder := json.NewDecoder(bytes.NewReader(original))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request_error", "the request body must be a JSON object")
		return
	}
	stream := payload["stream"] == true
	grace := cutWriteGrace
	if !stream {
		grace = cutBufferedWriteGrace
	}
	defer s.boundWritesOnCut(w, grace)()
	// The request reaches the runtime as the client sent it, so the receipt's prompt digest is the digest
	// of the client's own bytes. Only `service_tier` (the OpenAI parameter asking for the priority tier)
	// is taken out when present: it travels beside the ciphertext as the edge reads it, never inside it.
	priority := false
	body := bytes.TrimSpace(original)
	if tier, present := payload["service_tier"]; present {
		if value, ok := tier.(string); ok {
			priority = value == "priority"
		}
		delete(payload, "service_tier")
		var plaintext bytes.Buffer
		encoder := json.NewEncoder(&plaintext)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(payload); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid_request_error", "the request could not be encoded")
			return
		}
		body = bytes.TrimRight(plaintext.Bytes(), "\n")
	}
	if len(body)+20 > editionRequestMax {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	// One retry when the edge says the key the router encrypted to is no longer the runtime's.
	for attempt := 0; attempt < 2; attempt++ {
		key, err := s.editionKey(ctx, route, attempt > 0)
		if err != nil {
			s.settings.Logger.Printf("request=%s model=%s edition key: %v", requestID, route.ModelID, err)
			s.writeError(w, http.StatusServiceUnavailable, "encryption_unavailable", "the edition's encryption key is unavailable")
			return
		}
		callCtx, endCall := context.WithCancelCause(ctx)
		upstream, err := http.NewRequestWithContext(callCtx, http.MethodPost, route.Edition.Endpoint()+completionPath, io.NopCloser(bytes.NewReader(body)))
		if err != nil {
			endCall(nil)
			s.writeError(w, http.StatusInternalServerError, "internal_error", "the request could not be built")
			return
		}
		sealed, err := key.identity.EncryptRequestWithContext(upstream)
		if err != nil || sealed == nil {
			endCall(nil)
			s.writeError(w, http.StatusBadGateway, "encryption_failed", "the request could not be encrypted")
			return
		}
		// the edge takes a sized body: the ciphertext is buffered (it is bounded) and sent with its length
		ciphertext, err := io.ReadAll(io.LimitReader(upstream.Body, editionRequestMax+1))
		if err != nil || len(ciphertext) > editionRequestMax {
			endCall(nil)
			s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
			return
		}
		upstream.Body = io.NopCloser(bytes.NewReader(ciphertext))
		upstream.ContentLength = int64(len(ciphertext))
		upstream.TransferEncoding = nil
		upstream.Header.Del("Transfer-Encoding")
		upstream.Header.Set("Authorization", r.Header.Get("Authorization"))
		upstream.Header.Set("Content-Type", "application/octet-stream")
		upstream.Header.Set("Accept", accept)
		upstream.Header.Set("Accept-Encoding", "identity")
		upstream.Header.Set("User-Agent", userAgent)
		upstream.Header.Set(editionKeyIDHeader, key.keyID)
		editionRequestID := randomHex(16)
		upstream.Header.Set(editionRequestIDHeader, editionRequestID)
		if priority {
			upstream.Header.Set(editionTierHeader, "priority")
		}
		var callMu sync.Mutex
		answered := false
		silence := time.AfterFunc(editionHeaderTime, func() {
			callMu.Lock()
			defer callMu.Unlock()
			if !answered {
				endCall(errUpstreamSilent)
			}
		})
		response, err := s.settings.HTTPClient.Do(upstream)
		callMu.Lock()
		answered = true
		callMu.Unlock()
		silence.Stop()
		if err != nil {
			endCall(nil)
			if ctx.Err() != nil {
				return
			}
			s.settings.Logger.Printf("request=%s model=%s edition transport error", requestID, route.ModelID)
			s.writeError(w, http.StatusBadGateway, "upstream_unavailable", "the edition could not be reached")
			return
		}
		nonce := response.Header.Get(protocol.ResponseNonceHeader)
		if nonce == "" {
			// Before decryption the edge may answer in plaintext: its own refusals (a wrong key, no credit,
			// a full line, a key that rotated). Relayed by status and code, its body never trusted beyond that.
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			endCall(nil)
			code, message := plaintextEdgeError(raw)
			if response.StatusCode >= 200 && response.StatusCode <= 299 {
				s.writeError(w, http.StatusBadGateway, "encryption_response_invalid", "the edition answered in plaintext")
				return
			}
			// Once more, with the key fetched again, when the one the router encrypted to is no longer the
			// runtime's: the edge says so in its own words, or the runtime refuses the ciphertext itself (a
			// problem document, 422) after a restart gave it a new key.
			rotated := code == "encryption_key_rotated" || code == "encryption_required" ||
				response.StatusCode == http.StatusUnprocessableEntity || strings.HasPrefix(response.Header.Get("Content-Type"), "application/problem+json")
			if attempt == 0 && rotated {
				continue
			}
			if retry := response.Header.Get("Retry-After"); retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			s.writeError(w, response.StatusCode, code, message)
			return
		}
		if response.Header.Get(editionKeyIDHeader) != key.keyID {
			response.Body.Close()
			endCall(nil)
			s.writeError(w, http.StatusBadGateway, "encryption_response_invalid", "the answer is not encrypted to this request")
			return
		}
		if err := sealed.DecryptResponse(response); err != nil {
			response.Body.Close()
			endCall(nil)
			s.writeError(w, http.StatusBadGateway, "encryption_response_invalid", "the answer could not be decrypted")
			return
		}
		for _, name := range editionResponseHeaders {
			if value := response.Header.Get(name); value != "" {
				w.Header().Set(name, value)
			}
		}
		w.Header().Set(editionRequestIDHeader, editionRequestID)
		w.Header().Set("Content-Type", contentTypeOr(response, accept))
		w.Header().Set("X-Lebrel-Encryption", "router-to-runtime")
		w.WriteHeader(response.StatusCode)
		flusher, _ := w.(http.Flusher)
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := response.Body.Read(buffer)
			if n > 0 {
				if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					s.settings.Logger.Printf("request=%s model=%s edition answer cut: decryption or transport failed", requestID, route.ModelID)
				}
				break
			}
		}
		response.Body.Close()
		endCall(nil)
		return
	}
}

// plaintextEdgeError reads the code and message of an edge refusal ({"error": {"code", "message"}}),
// or names it as unencrypted when the body is anything else.
func plaintextEdgeError(raw []byte) (code, message string) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error.Code != "" && len(body.Error.Code) <= 64 && !strings.ContainsAny(body.Error.Code, " \r\n") {
		message = body.Error.Message
		if len(message) > 300 || strings.ContainsAny(message, "\r\n") {
			message = strings.ReplaceAll(body.Error.Code, "_", " ")
		}
		return body.Error.Code, message
	}
	return "unencrypted_transport_error", "the edge refused the request"
}

// editionRoute is the edition this router serves, when it serves one.
func (s *Server) editionRoute() *Route {
	for _, route := range s.routes {
		if route.Kind == routeKindEdition {
			return route
		}
	}
	return nil
}

// editionDocument relays one of the edition's public documents from the edge: the runtime's receipt
// of an answer (filed under the request id the router sent along and handed to the client as
// X-Lebrel-Request-Id) or its serving manifest. Public, bounded, never cached.
func (s *Server) editionDocument(w http.ResponseWriter, r *http.Request, route *Route, path, notFound string) {
	if strings.HasPrefix(path, "/v1/receipts/") && !regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`).MatchString(strings.TrimPrefix(path, "/v1/receipts/")) {
		s.writeError(w, http.StatusNotFound, notFound, "no such document")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, route.Edition.Endpoint()+path, nil)
	if err != nil {
		s.writeError(w, http.StatusNotFound, notFound, "no such document")
		return
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", userAgent)
	response, err := s.settings.HTTPClient.Do(request)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "document_unavailable", "the document could not be fetched")
		return
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, editionMetadataMax+1))
	if err != nil || len(raw) > editionMetadataMax {
		s.writeError(w, http.StatusBadGateway, "document_unavailable", "the document could not be fetched")
		return
	}
	if response.StatusCode != http.StatusOK {
		s.writeError(w, http.StatusNotFound, notFound, "no such document (receipts expire)")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}
