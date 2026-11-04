package router

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tinfoilsh/encrypted-http-body-protocol/identity"
	"github.com/tinfoilsh/encrypted-http-body-protocol/protocol"
	"lebrel-router/internal/auth"
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

// An encrypted request carries the moment it was sent (SentAtField, inside the ciphertext); one
// older or newer than encryptedClockSkew is refused, so an encapsulation needs remembering only for
// encryptedReplayFor, minutes, and the table holds minutes of traffic rather than days.
// encryptedReplayEntries bounds it and encryptedReplayPerAccount one account's share, far above any
// customer's rate (it only keeps a runaway from filling the table for everyone); a request refused
// before credit was reserved is forgotten at once. The table lives in memory with the keys: a
// restart empties both, so nothing captured before it can be decrypted or replayed after.
const (
	encryptedClockSkew        = 5 * time.Minute
	encryptedReplayFor        = 2*encryptedClockSkew + time.Minute
	encryptedReplayEntries    = 1_000_000
	encryptedReplayPerAccount = 65_536
	encryptedSweepEvery       = time.Second
)

// EncryptedRequestMax bounds an encrypted request body: the edge forwards at most 8 MiB.
const EncryptedRequestMax = 8 << 20

// ReceiptSaltField is the optional field a client puts inside the encrypted body, as its first
// field in compact form: `{"lebrel_receipt_salt":"<hex of 16 to 32 random bytes>",...}`. Only the
// two ends know the salt. The router cuts the field out textually (the rest of the body is
// forwarded byte for byte) and salts the receipt's digests with it: prompt_sha256 is SHA-256 of
// salt || the body without the field, response_sha256 SHA-256 of salt || the answer's text, so a
// receipt's hashes cannot be checked against guesses by anyone who merely read the receipt, while
// the client, who knows both, can.
const ReceiptSaltField = "lebrel_receipt_salt"

// ReceiptSaltScheme names the salting in the receipt.
const ReceiptSaltScheme = "client-salt-v1"

// SentAtField is the moment the client sent the request, unix seconds, inside the encrypted body
// right after the salt (or first): `"lebrel_sent_at":1793831729,`. Required: a request older or
// newer than encryptedClockSkew is refused, which is what lets the replay table forget an
// encapsulation after minutes rather than days.
const SentAtField = "lebrel_sent_at"

// EpochOf names the daily key epoch of a moment: the encryption key is born per UTC day, and the
// previous day's key is kept while a client may still hold its key document.
func EpochOf(now time.Time) string { return now.UTC().Format("2006-01-02") }

type encryptionKey struct {
	identity *identity.Identity
	config   []byte
	keyID    string
	epoch    string
}

func newEncryptionKey(secret []byte, epoch string) (*encryptionKey, error) {
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
	return &encryptionKey{identity: id, config: config, keyID: hex.EncodeToString(digest[:]), epoch: epoch}, nil
}

// Encryption is the router's HPKE identity, its daily rotation and what goes with it.
type Encryption struct {
	// derive hands the 32-byte secret of an epoch's key (random bytes inside a VM); nil means the
	// identity given at construction never rotates (tests, development).
	derive func(epoch string) ([]byte, error)
	// instanceID is the 32-hex instance identifier the metadata names.
	instanceID string
	mu         sync.Mutex
	current    *encryptionKey
	previous   *encryptionKey
	// onRotate runs after a rotation: the cached attestation names the key before it.
	onRotate  func()
	replays   map[[32]byte]replayEntry
	accounts  map[string]int
	inserts   int
	lastSweep time.Time
}

type replayEntry struct {
	expiry  time.Time
	account string
}

// NewEncryption builds a router identity from a fixed 32-byte X25519 secret that never rotates.
func NewEncryption(secret []byte, instanceID string) (*Encryption, error) {
	key, err := newEncryptionKey(secret, "")
	if err != nil {
		return nil, err
	}
	return &Encryption{instanceID: instanceIdentifier(instanceID), current: key, replays: map[[32]byte]replayEntry{}, accounts: map[string]int{}}, nil
}

// NewRotatingEncryption derives the key of the current epoch (and keeps the previous epoch's)
// through derive, and rotates when Rotate is called with a new epoch.
func NewRotatingEncryption(derive func(epoch string) ([]byte, error), instanceID string, now time.Time) (*Encryption, error) {
	e := &Encryption{derive: derive, instanceID: instanceIdentifier(instanceID), replays: map[[32]byte]replayEntry{}, accounts: map[string]int{}}
	if err := e.Rotate(now); err != nil {
		return nil, err
	}
	return e, nil
}

// NewEphemeralEncryption is the identity of a router inside an attested VM: a key born at random in
// the VM's memory for today's UTC day, a new one at each midnight (the previous day's kept) and at
// each boot. The quote binds the public key; the secret exists nowhere else, and in particular not
// in the VM's key service, which derives keys per app rather than per measured compose.
func NewEphemeralEncryption(instanceID string, now time.Time) (*Encryption, error) {
	return NewRotatingEncryption(func(string) ([]byte, error) {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		return secret, nil
	}, instanceID, now)
}

func instanceIdentifier(instanceID string) string {
	instance := sha256.Sum256([]byte("lebrel-router-instance:" + instanceID))
	return hex.EncodeToString(instance[:16])
}

// Rotate makes the key of now's epoch current, keeping the one before it; a no-op within an epoch.
func (e *Encryption) Rotate(now time.Time) error {
	if e.derive == nil {
		return nil
	}
	epoch := EpochOf(now)
	e.mu.Lock()
	current := e.current
	e.mu.Unlock()
	if current != nil && current.epoch == epoch {
		return nil
	}
	secret, err := e.derive(epoch)
	if err != nil {
		return err
	}
	key, err := newEncryptionKey(secret, epoch)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.previous, e.current = e.current, key
	hook := e.onRotate
	e.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// RotateDaily rotates at every UTC midnight until ctx ends; a derivation that fails is retried a
// minute later, the old key serving meanwhile.
func (e *Encryption) RotateDaily(ctx context.Context, now func() time.Time, report func(error)) {
	go func() {
		for {
			t := now()
			next := time.Date(t.UTC().Year(), t.UTC().Month(), t.UTC().Day()+1, 0, 0, 5, 0, time.UTC)
			select {
			case <-ctx.Done():
				return
			case <-time.After(next.Sub(t)):
			}
			for attempt := 0; attempt < 60; attempt++ {
				if err := e.Rotate(now()); err == nil {
					break
				} else if report != nil {
					report(err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Minute):
					}
				}
			}
		}
	}()
}

func (e *Encryption) keys() (current, previous *encryptionKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current, e.previous
}

// PublicKey is the current raw X25519 public key (32 bytes), what the attestation quote commits to.
func (e *Encryption) PublicKey() []byte {
	current, _ := e.keys()
	return current.identity.MarshalPublicKey()
}

// KeyID is SHA-256 of the current RFC 9458 key configuration, the id clients pin per request.
func (e *Encryption) KeyID() string {
	current, _ := e.keys()
	return current.keyID
}

// Current is the current public key and its id, read together: an attestation never pairs one
// day's key with another day's id.
func (e *Encryption) Current() (publicKey []byte, keyID string) {
	current, _ := e.keys()
	return current.identity.MarshalPublicKey(), current.keyID
}

// SetRotationHook runs hook after every rotation (the server drops its cached attestation).
func (e *Encryption) SetRotationHook(hook func()) {
	e.mu.Lock()
	e.onRotate = hook
	e.mu.Unlock()
}

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

// SignedMetadata is the envelope for one route at time now, naming the current key.
func (e *Encryption) SignedMetadata(signer *Signer, modelID string, now time.Time) ([]byte, error) {
	current, _ := e.keys()
	payload, err := json.Marshal(encryptionMetadata{
		ExpiresAt: now.Add(encryptionMetadataLifetime).Unix(), HPKEConfig: base64.StdEncoding.EncodeToString(current.config), IssuedAt: now.Unix(),
		KeyID: current.keyID, ModelID: modelID, ServerInstanceID: e.instanceID, SigningKeyID: signer.KeyID, Version: 1,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(encryptionEnvelope{Version: 1, Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signer.private, payload))})
}

// remember records an encapsulation under the account that sent it; false when it was seen before
// (a replay), or when the table, or the account's share of it, is full of live entries. Expired
// entries are swept as the table grows.
func (e *Encryption) remember(encapsulation []byte, account string, now time.Time) (fresh bool, full bool) {
	digest := sha256.Sum256(encapsulation)
	e.mu.Lock()
	defer e.mu.Unlock()
	if entry, seen := e.replays[digest]; seen && now.Before(entry.expiry) {
		return false, false
	}
	e.inserts++
	// Expired entries are swept now and then, and when a cap is reached at most once a second: a
	// full table costs its holder a sweep per second, never one per request.
	capped := len(e.replays) >= encryptedReplayEntries || e.accounts[account] >= encryptedReplayPerAccount
	if (capped && now.Sub(e.lastSweep) >= encryptedSweepEvery) || e.inserts%4096 == 0 {
		e.lastSweep = now
		for key, entry := range e.replays {
			if !now.Before(entry.expiry) {
				e.drop(key, entry)
			}
		}
	}
	if len(e.replays) >= encryptedReplayEntries || e.accounts[account] >= encryptedReplayPerAccount {
		return false, true
	}
	if entry, seen := e.replays[digest]; seen {
		e.drop(digest, entry)
	}
	e.replays[digest] = replayEntry{expiry: now.Add(encryptedReplayFor), account: account}
	e.accounts[account]++
	return true, false
}

// forget releases an encapsulation whose request was refused before credit was reserved: nothing
// reached the lab, so there is nothing to replay.
func (e *Encryption) forget(encapsulation []byte) {
	digest := sha256.Sum256(encapsulation)
	e.mu.Lock()
	defer e.mu.Unlock()
	if entry, seen := e.replays[digest]; seen {
		e.drop(digest, entry)
	}
}

func (e *Encryption) drop(digest [32]byte, entry replayEntry) {
	delete(e.replays, digest)
	if e.accounts[entry.account] <= 1 {
		delete(e.accounts, entry.account)
	} else {
		e.accounts[entry.account]--
	}
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

type receiptSaltKey struct{}

// receiptSalt is the client's salt for this request's receipt digests, if it sent one.
func receiptSalt(ctx context.Context) []byte {
	salt, _ := ctx.Value(receiptSaltKey{}).([]byte)
	return salt
}

// stripClientFields cuts the client's fields out of the plaintext, textually, so the body the
// digest covers is exactly what the client wrote after them: an optional salt first
// (`{"lebrel_receipt_salt":"<hex>",`), then the moment the request was sent
// (`"lebrel_sent_at":<unix seconds>,`), required. A field that is malformed, missing or anywhere
// else is an error of the client's.
func stripClientFields(plaintext []byte) (body []byte, salt []byte, sentAt int64, err error) {
	rest := plaintext
	if saltPrefix := []byte(`{"` + ReceiptSaltField + `":"`); bytes.HasPrefix(rest, saltPrefix) {
		rest = rest[len(saltPrefix):]
		end := bytes.IndexByte(rest, '"')
		if end < 0 {
			return nil, nil, 0, errors.New(ReceiptSaltField + " must be a hex string")
		}
		salt, err = hex.DecodeString(string(rest[:end]))
		if err != nil || len(salt) < 16 || len(salt) > 32 {
			return nil, nil, 0, errors.New(ReceiptSaltField + " must be hex of 16 to 32 bytes")
		}
		rest = rest[end+1:]
		if !bytes.HasPrefix(rest, []byte(",")) {
			return nil, nil, 0, errors.New(ReceiptSaltField + " must be followed by " + SentAtField)
		}
		rest = append([]byte("{"), rest[1:]...)
	}
	sentPrefix := []byte(`{"` + SentAtField + `":`)
	if !bytes.HasPrefix(rest, sentPrefix) {
		return nil, nil, 0, errors.New(SentAtField + " (unix seconds) must come first inside an encrypted request, after " + ReceiptSaltField + " if any")
	}
	rest = rest[len(sentPrefix):]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits > 12 {
		return nil, nil, 0, errors.New(SentAtField + " must be unix seconds")
	}
	sentAt, _ = strconv.ParseInt(string(rest[:digits]), 10, 64)
	rest = rest[digits:]
	switch {
	case bytes.HasPrefix(rest, []byte(",")):
		body = append([]byte("{"), rest[1:]...)
	case bytes.HasPrefix(rest, []byte("}")) && len(bytes.TrimSpace(rest[1:])) == 0:
		body = []byte("{}")
	default:
		return nil, nil, 0, errors.New(SentAtField + " must be followed by the request's fields")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) == nil {
		if _, present := fields[ReceiptSaltField]; present {
			return nil, nil, 0, errors.New(ReceiptSaltField + " must be the first field of a compact JSON object")
		}
		if _, present := fields[SentAtField]; present {
			return nil, nil, 0, errors.New(SentAtField + " must appear once, first")
		}
	}
	return body, salt, sentAt, nil
}

// handleEncryptedCompletion decrypts an EHBP request and runs it through the ordinary completion
// path with an encrypting response writer: every byte of the answer, errors included, leaves
// encrypted to the client's key for this request. The edge and the API key (in clear: it is not
// content) are checked before anything is read, so nobody outside the edge, and no key that is not
// a customer's, can make the machine decrypt or remember anything; an encapsulation is remembered
// under its account, and forgotten again if the request is refused before credit is reserved.
func (s *Server) handleEncryptedCompletion(w http.ResponseWriter, r *http.Request, scoped *Route) {
	encryption := s.settings.Encryption
	if encryption == nil {
		s.writeError(w, http.StatusBadRequest, "encryption_unavailable", "this instance does not accept encrypted requests")
		return
	}
	if !s.validEdge(r) {
		s.writeError(w, http.StatusUnauthorized, "edge_unauthorized", "missing edge authorization")
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if len(r.Header.Values(protocol.EncapsulatedKeyHeader)) != 1 || len(r.Header.Get(protocol.EncapsulatedKeyHeader)) != 64 {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "one EHBP encapsulated key is required")
		return
	}
	principal, err := s.settings.Auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			s.writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
		} else {
			s.writeError(w, http.StatusServiceUnavailable, "authentication_unavailable", "authentication is temporarily unavailable")
		}
		return
	}
	// The key's and the account's concurrency slots are taken before the body is read, as on the
	// plaintext path: a key cannot hold more uploads or decryptions in flight than its slots.
	releaseKey, err := s.settings.Limiter.Acquire(r.Context(), "key:"+principal.APIKeyID, principal.Concurrency)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		s.writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many concurrent requests for this key")
		return
	}
	defer releaseKey()
	releaseAccount, err := s.settings.Limiter.Acquire(r.Context(), "account:"+principal.AccountID, principal.AccountConcurrency)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		s.writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many concurrent requests for this account")
		return
	}
	defer releaseAccount()
	current, previous := encryption.keys()
	key := current
	if expected := r.Header.Get("X-Lebrel-Encryption-Key-Id"); expected != "" && expected != current.keyID {
		if previous != nil && expected == previous.keyID {
			key = previous
		} else {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": protocol.KeyConfigProblemType, "title": "encryption key rotated", "status": http.StatusUnprocessableEntity, "detail": "fetch the key document again"})
			return
		}
	}
	limit := s.settings.MaxRequestBytes
	if limit <= 0 || limit > EncryptedRequestMax {
		limit = EncryptedRequestMax
	}
	ciphertext, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	_ = r.Body.Close()
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "could not read the encrypted request")
		return
	}
	if int64(len(ciphertext)) > limit {
		s.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
		return
	}
	if !validFrames(ciphertext) {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "invalid encrypted request framing")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(ciphertext))
	responseContext, err := key.identity.DecryptRequestWithContext(r)
	if err != nil || responseContext == nil {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "invalid encrypted request")
		return
	}
	plaintext, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	_ = r.Body.Close()
	if err != nil || int64(len(plaintext)) > limit {
		s.writeError(w, http.StatusBadRequest, "invalid_ciphertext", "invalid encrypted request")
		return
	}
	encapsulation, _ := hex.DecodeString(r.Header.Get(protocol.EncapsulatedKeyHeader))
	fresh, full := encryption.remember(encapsulation, principal.AccountID, s.settings.Now())
	ew, err := key.identity.SetupDerivedResponseEncryption(w, responseContext)
	if err != nil {
		encryption.forget(encapsulation)
		s.writeError(w, http.StatusInternalServerError, "encryption_failed", "response encryption unavailable")
		return
	}
	ew.Header().Set("X-Lebrel-Encryption-Key-Id", key.keyID)
	// From here on every answer is encrypted, errors included.
	if !fresh {
		if full {
			s.writeError(ew, http.StatusServiceUnavailable, "key_capacity_reached", "the router is remembering too many recent requests; try again in a moment")
		} else {
			s.writeError(ew, http.StatusConflict, "replayed_request", "this encrypted request has already been submitted")
		}
		return
	}
	body, salt, sentAt, err := stripClientFields(plaintext)
	if err != nil {
		encryption.forget(encapsulation)
		s.writeError(ew, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if age := s.settings.Now().Sub(time.Unix(sentAt, 0)); age > encryptedClockSkew || age < -encryptedClockSkew {
		encryption.forget(encapsulation)
		s.writeError(ew, http.StatusBadRequest, "request_expired", SentAtField+" is outside the accepted window (five minutes either way): check the clock and send the request again")
		return
	}
	admitted := &admission{}
	ctx := context.WithValue(context.WithValue(context.WithValue(r.Context(), principalKey{}, principal), admissionKey{}, admitted), limitsKey{}, true)
	if len(salt) > 0 {
		ctx = context.WithValue(ctx, receiptSaltKey{}, salt)
	}
	inner := r.Clone(ctx)
	inner.Header.Del(protocol.EncapsulatedKeyHeader)
	inner.Header.Del("X-Lebrel-Encryption-Key-Id")
	inner.Header.Set("Content-Type", "application/json")
	inner.ContentLength = int64(len(body))
	inner.Body = io.NopCloser(bytes.NewReader(body))
	s.handleCompletion(ew, inner, scoped)
	if !admitted.reserved {
		// Refused before credit was reserved (no slot, no credit, unknown model...): nothing reached
		// the lab, so there is nothing to replay and nothing to remember.
		encryption.forget(encapsulation)
	}
}

// principalKey carries a principal the encrypted path authenticated before decrypting; limitsKey
// says the key's and the account's concurrency slots are already held; admissionKey carries the
// record the completion handler marks once the request's credit is reserved.
type (
	principalKey struct{}
	limitsKey    struct{}
	admissionKey struct{}
	admission    struct{ reserved bool }
)
