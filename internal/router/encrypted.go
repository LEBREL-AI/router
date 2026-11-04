package router

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/tinfoilsh/encrypted-http-body-protocol/identity"
	"github.com/tinfoilsh/encrypted-http-body-protocol/protocol"
)

// End-to-end encryption of routed requests (EHBP, the same protocol the editions' runtime speaks):
// the client encrypts the request body to the router's HPKE key, which is born inside the
// confidential VM, and decrypts the response with keys derived from that exchange. Everything
// between the client and the router (lebrel.ai's edge, Cloudflare, Phala's gateway) carries
// ciphertext; the router forwards the plaintext to the lab over the lab's own TLS.

// EncryptionMetadataPath is where a route publishes its signed encryption key.
const EncryptionMetadataPath = "/.well-known/lebrel-encryption"

// encryptionMetadataLifetime bounds how long a published key document is valid; clients refetch.
const encryptionMetadataLifetime = 5 * time.Minute

// encryptedReplayEntries bounds the encapsulations remembered against replay.
const encryptedReplayEntries = 200_000

// Encryption is the router's HPKE identity and what goes with it.
type Encryption struct {
	identity *identity.Identity
	config   []byte
	keyID    string
	// instanceID is the 32-hex instance identifier the metadata names.
	instanceID string
	mu         sync.Mutex
	replays    map[[32]byte]struct{}
}

// NewEncryption builds the router's HPKE identity from a 32-byte X25519 secret (inside a VM, the
// KMS-derived key for this compose; nowhere else is it accepted in production) and the instance
// id the metadata will name.
func NewEncryption(secret []byte, instanceID string) (*Encryption, error) {
	if len(secret) != 32 {
		return nil, errors.New("the encryption secret must be 32 bytes")
	}
	private, err := ecdh.X25519().NewPrivateKey(secret)
	if err != nil {
		return nil, err
	}
	store, err := json.Marshal(identity.IdentityStore{PublicKey: private.PublicKey().Bytes(), SecretKey: private.Bytes(), KEM: 0x0020, KDF: 0x0001, AEAD: 0x0002})
	if err != nil {
		return nil, err
	}
	id, err := identity.Import(store)
	if err != nil {
		return nil, err
	}
	config, err := id.MarshalConfig()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(config)
	instance := sha256.Sum256([]byte("lebrel-router-instance:" + instanceID))
	return &Encryption{identity: id, config: config, keyID: hex.EncodeToString(digest[:]), instanceID: hex.EncodeToString(instance[:16]), replays: map[[32]byte]struct{}{}}, nil
}

// PublicKey is the raw X25519 public key (32 bytes), what the attestation quote commits to.
func (e *Encryption) PublicKey() []byte { return e.identity.MarshalPublicKey() }

// KeyID is SHA-256 of the RFC 9458 key configuration, the id clients pin per request.
func (e *Encryption) KeyID() string { return e.keyID }

// encryptionMetadata is the signed document a client verifies before encrypting: the same shape the
// editions' runtime publishes, signed here by the receipt-signing key, which the attestation quote
// binds to this machine.
type encryptionMetadata struct {
	ExpiresAt        int64  `json:"expiresAt"`
	HPKEConfig       string `json:"hpkeConfig"`
	IssuedAt         int64  `json:"issuedAt"`
	KeyID            string `json:"keyId"`
	ModelID          string `json:"modelId"`
	ServerInstanceID string `json:"serverInstanceId"`
	SigningKeyID     string `json:"signingKeyId"`
	Version          int    `json:"version"`
}

type encryptionEnvelope struct {
	Version   int    `json:"version"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// SignedMetadata is the envelope for one route at time now.
func (e *Encryption) SignedMetadata(signer *Signer, modelID string, now time.Time) ([]byte, error) {
	payload, err := json.Marshal(encryptionMetadata{
		ExpiresAt: now.Add(encryptionMetadataLifetime).Unix(), HPKEConfig: base64.StdEncoding.EncodeToString(e.config), IssuedAt: now.Unix(),
		KeyID: e.keyID, ModelID: modelID, ServerInstanceID: e.instanceID, SigningKeyID: signer.KeyID, Version: 1,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(encryptionEnvelope{Version: 1, Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signer.private, payload))})
}

// remember records an encapsulation; false when it was seen before (a replay) or the table is full.
func (e *Encryption) remember(encapsulation []byte) (fresh bool, full bool) {
	digest := sha256.Sum256(encapsulation)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, seen := e.replays[digest]; seen {
		return false, false
	}
	if len(e.replays) >= encryptedReplayEntries {
		return false, true
	}
	e.replays[digest] = struct{}{}
	return true, false
}

// validFrames checks EHBP request framing within an already bounded buffer: 4-byte big-endian
// lengths, each frame at least an AEAD tag, nothing dangling.
func validFrames(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	for len(body) > 0 {
		if len(body) < 4 {
			return false
		}
		n := uint64(binary.BigEndian.Uint32(body[:4]))
		if n < 16 || n > uint64(len(body)-4) {
			return false
		}
		body = body[4+int(n):]
	}
	return true
}

// handleEncryptionMetadata serves GET /m/{slug}/.well-known/lebrel-encryption: the route's signed
// key document. Public, like the manifests.
func (s *Server) handleEncryptionMetadata(w http.ResponseWriter, r *http.Request, scoped *Route) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	if s.settings.Encryption == nil {
		s.writeError(w, http.StatusNotFound, "encryption_unavailable", "this instance does not accept encrypted requests")
		return
	}
	if scoped == nil {
		s.writeError(w, http.StatusNotFound, "not_found", "the encryption key is published per route: /m/{model}/.well-known/lebrel-encryption")
		return
	}
	document, err := s.settings.Encryption.SignedMetadata(s.settings.Signer, scoped.ModelID, s.settings.Now())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "encryption_unavailable", "the key document could not be signed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(document)
}

// handleEncryptedCompletion decrypts an EHBP request and runs it through the ordinary completion
// path with an encrypting response writer: every byte of the answer, errors included, leaves
// encrypted to the client's key for this request. The edge header is checked by the completion
// handler as for any request; the API key travels in clear (it is not content).
func (s *Server) handleEncryptedCompletion(w http.ResponseWriter, r *http.Request, scoped *Route) {
	encryption := s.settings.Encryption
	if encryption == nil {
		s.writeError(w, http.StatusBadRequest, "encryption_unavailable", "this instance does not accept encrypted requests")
		return
	}
	if len(r.Header.Values(protocol.EncapsulatedKeyHeader)) != 1 || len(r.Header.Get(protocol.EncapsulatedKeyHeader)) != 64 {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "one EHBP encapsulated key is required")
		return
	}
	if expected := r.Header.Get("X-Lebrel-Encryption-Key-Id"); expected != "" && expected != encryption.keyID {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": protocol.KeyConfigProblemType, "title": "encryption key rotated", "status": http.StatusUnprocessableEntity, "detail": "fetch the key document again"})
		return
	}
	ciphertext, err := io.ReadAll(io.LimitReader(r.Body, s.settings.MaxRequestBytes+1))
	_ = r.Body.Close()
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "could not read the encrypted request")
		return
	}
	if int64(len(ciphertext)) > s.settings.MaxRequestBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	if !validFrames(ciphertext) {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "invalid encrypted request framing")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(ciphertext))
	responseContext, err := encryption.identity.DecryptRequestWithContext(r)
	if err != nil || responseContext == nil {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "invalid encrypted request")
		return
	}
	plaintext, err := io.ReadAll(io.LimitReader(r.Body, s.settings.MaxRequestBytes+1))
	_ = r.Body.Close()
	if err != nil || int64(len(plaintext)) > s.settings.MaxRequestBytes {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "invalid encrypted request")
		return
	}
	encapsulation, _ := hex.DecodeString(r.Header.Get(protocol.EncapsulatedKeyHeader))
	fresh, full := encryption.remember(encapsulation)
	ew, err := encryption.identity.SetupDerivedResponseEncryption(w, responseContext)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "encryption_failed", "response encryption unavailable")
		return
	}
	ew.Header().Set("X-Lebrel-Encryption-Key-Id", encryption.keyID)
	// From here on every answer is encrypted, errors included.
	if !fresh {
		if full {
			s.writeError(ew, http.StatusServiceUnavailable, "key_capacity_reached", "the router must rotate its encryption key before accepting more requests")
		} else {
			s.writeError(ew, http.StatusConflict, "replayed_request", "this encrypted request has already been submitted")
		}
		return
	}
	inner := r.Clone(r.Context())
	inner.Header.Del(protocol.EncapsulatedKeyHeader)
	inner.Header.Del("X-Lebrel-Encryption-Key-Id")
	inner.Header.Set("Content-Type", "application/json")
	inner.ContentLength = int64(len(plaintext))
	inner.Body = io.NopCloser(bytes.NewReader(plaintext))
	s.handleCompletion(ew, inner, scoped)
}
