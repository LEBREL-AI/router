package router

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"lebrel-router/internal/dstack"
)

// AttestationPath is where the router serves its hardware attestation: the TDX quote of the
// confidential VM it runs in, bound to its receipt-signing key.
const AttestationPath = "/.well-known/attestation"

// attestationDomain separates this binding from any other use of the quote's report data. v2 binds
// the receipt-signing key and the request-encryption key; v1 (no encryption key) bound the first.
const attestationDomain = "lebrel-router-attestation-v2"

// attestationNonceMax bounds the nonce a verifier may ask into the report data.
const attestationNonceMax = 32

// attestationCacheFor is how long a quote without a nonce is reused.
const attestationCacheFor = 5 * time.Minute

// attestationInFlight bounds the quotes asked of the VM at once: the guest agent makes them one at
// a time, so beyond this the answer is a 429, never a queue that drains the agent.
const attestationInFlight = 2

// attestationPerClient is how many fresh quotes (with a nonce) one client address may ask per
// minute; without a nonce the cached document costs nothing. Behind a gateway or proxy every
// request carries the gateway's address, so this is in practice a limit for all verifiers together:
// generous enough for honest use, small enough that the agent's serial quoting is never drained.
const attestationPerClient = 30

// AttestationReportData is what the quote's 64-byte report data commits to: the receipt-signing
// public key, the request-encryption public key (empty where the instance accepts no encrypted
// requests), and the verifier's nonce when one is given. report_data = SHA-512(domain ||
// public_key || encryption_public_key || nonce), so a verifier recomputes it from the document.
func AttestationReportData(publicKey []byte, encryptionPublicKey []byte, nonce []byte) []byte {
	h := sha512.New()
	h.Write([]byte(attestationDomain))
	h.Write(publicKey)
	h.Write(encryptionPublicKey)
	h.Write(nonce)
	return h.Sum(nil)
}

// AttestationDocument is the public document a verifier reads.
type AttestationDocument struct {
	Kind string `json:"kind"`
	// Scheme names how report_data is derived, for verifiers.
	Scheme string `json:"scheme"`
	// PublicKey and SigningKeyID are the receipt-signing key the quote is bound to; KeySource says
	// where the key came from: "vm-kms" (derived inside, by the VM's KMS for this app and compose)
	// or "env" (handed in; never inside a VM in production).
	PublicKey    string `json:"public_key"`
	SigningKeyID string `json:"signing_key_id"`
	KeySource    string `json:"key_source"`
	// EncryptionPublicKey and EncryptionKeyID are the HPKE key clients encrypt requests to (raw
	// X25519 public key, hex; key id = SHA-256 of the RFC 9458 configuration), bound by the quote
	// beside the receipt key; empty where the instance accepts no encrypted requests.
	EncryptionPublicKey string `json:"encryption_public_key,omitempty"`
	EncryptionKeyID     string `json:"encryption_key_id,omitempty"`
	InstanceID          string `json:"instance_id"`
	Nonce               string `json:"nonce"`
	ReportData          string `json:"report_data"`
	// Quote is the TDX quote, hex; EventLog explains RTMR3 (JSON, as the agent gives it); VMConfig
	// is the VM's launch configuration as the agent states it, which dstack-verifier and dstack-mr
	// need to recompute the OS measurements (MRTD, RTMR0-2) from the published image.
	Quote    string          `json:"quote"`
	EventLog json.RawMessage `json:"event_log"`
	VMConfig json.RawMessage `json:"vm_config,omitempty"`
	// Info is the app's identity as the guest agent states it.
	Info     dstack.Info `json:"info"`
	IssuedAt int64       `json:"issued_at"`
}

type attestationCache struct {
	mu       sync.Mutex
	document *AttestationDocument
	expires  time.Time
	// info is the app's identity, read once: it cannot change while the VM runs.
	info    *dstack.Info
	inUse   int
	clients map[string]*attestationClient
}

type attestationClient struct {
	count   int
	resetAt time.Time
}

// admit reserves one of the quote slots and one of the client's fresh quotes for this minute;
// it returns the code of the refusal when it cannot.
func (c *attestationCache) admit(client string, fresh bool, now time.Time) (release func(), refused string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fresh {
		if c.clients == nil {
			c.clients = map[string]*attestationClient{}
		}
		if len(c.clients) > 4096 {
			for key, entry := range c.clients {
				if now.After(entry.resetAt) {
					delete(c.clients, key)
				}
			}
		}
		entry := c.clients[client]
		if entry == nil || now.After(entry.resetAt) {
			entry = &attestationClient{resetAt: now.Add(time.Minute)}
			c.clients[client] = entry
		}
		if entry.count >= attestationPerClient {
			return nil, "too_many_quotes"
		}
		// A request refused for a full machine is not one of the client's quotes.
		if c.inUse >= attestationInFlight {
			return nil, "attestation_busy"
		}
		entry.count++
	}
	if c.inUse >= attestationInFlight {
		return nil, "attestation_busy"
	}
	c.inUse++
	return func() {
		c.mu.Lock()
		c.inUse--
		c.mu.Unlock()
	}, ""
}

// attestation builds the document; without a nonce the last one is reused for a few minutes.
func (s *Server) attestation(ctx context.Context, nonce []byte) (*AttestationDocument, error) {
	if s.settings.Attester == nil {
		return nil, errors.New("not running in an attested VM")
	}
	now := s.settings.Now()
	if len(nonce) == 0 {
		s.attestations.mu.Lock()
		cached, expires := s.attestations.document, s.attestations.expires
		s.attestations.mu.Unlock()
		if cached != nil && now.Before(expires) {
			return cached, nil
		}
	}
	var encryptionPublic []byte
	encryptionKeyID := ""
	if s.settings.Encryption != nil {
		encryptionPublic = s.settings.Encryption.PublicKey()
		encryptionKeyID = s.settings.Encryption.KeyID()
	}
	reportData := AttestationReportData(s.settings.Signer.Public, encryptionPublic, nonce)
	quote, err := s.settings.Attester.Quote(ctx, reportData)
	if err != nil {
		return nil, err
	}
	s.attestations.mu.Lock()
	cachedInfo := s.attestations.info
	s.attestations.mu.Unlock()
	var info dstack.Info
	if cachedInfo != nil {
		info = *cachedInfo
	} else {
		info, err = s.settings.Attester.Info(ctx)
		if err != nil {
			return nil, err
		}
		s.attestations.mu.Lock()
		s.attestations.info = &info
		s.attestations.mu.Unlock()
	}
	eventLog := json.RawMessage(quote.EventLog)
	if !json.Valid(eventLog) {
		eventLog, _ = json.Marshal(quote.EventLog)
	}
	var vmConfig json.RawMessage
	if quote.VMConfig != "" {
		vmConfig = json.RawMessage(quote.VMConfig)
		if !json.Valid(vmConfig) {
			vmConfig, _ = json.Marshal(quote.VMConfig)
		}
	}
	document := &AttestationDocument{
		Kind: "lebrel-router-attestation", Scheme: "report_data = sha512(\"" + attestationDomain + "\" || public_key || encryption_public_key || nonce)",
		PublicKey: hex.EncodeToString(s.settings.Signer.Public), SigningKeyID: s.settings.Signer.KeyID, KeySource: s.settings.KeySource,
		EncryptionPublicKey: hex.EncodeToString(encryptionPublic), EncryptionKeyID: encryptionKeyID, InstanceID: s.settings.Signer.InstanceID,
		Nonce: hex.EncodeToString(nonce), ReportData: hex.EncodeToString(reportData),
		Quote: hex.EncodeToString(quote.Quote), EventLog: eventLog, VMConfig: vmConfig, Info: info, IssuedAt: now.Unix(),
	}
	if len(nonce) == 0 {
		s.attestations.mu.Lock()
		s.attestations.document, s.attestations.expires = document, now.Add(attestationCacheFor)
		s.attestations.mu.Unlock()
	}
	return document, nil
}

// handleAttestation serves GET /.well-known/attestation[?nonce=<hex, up to 32 bytes>]. Public, like
// the manifests: anyone may ask the machine to prove what it runs.
func (s *Server) handleAttestation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	var nonce []byte
	if text := r.URL.Query().Get("nonce"); text != "" {
		decoded, err := hex.DecodeString(text)
		if err != nil || len(decoded) == 0 || len(decoded) > attestationNonceMax {
			s.writeError(w, http.StatusBadRequest, "invalid_nonce", "nonce must be hex of 1 to 32 bytes")
			return
		}
		nonce = decoded
	}
	if s.settings.Attester == nil {
		s.writeError(w, http.StatusNotFound, "attestation_unavailable", "this instance does not run in an attested VM")
		return
	}
	client, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		client = r.RemoteAddr
	}
	// A fresh cached document costs the machine nothing: served before any limit applies.
	if len(nonce) == 0 {
		s.attestations.mu.Lock()
		cached, expires := s.attestations.document, s.attestations.expires
		s.attestations.mu.Unlock()
		if cached != nil && s.settings.Now().Before(expires) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(cached)
			return
		}
	}
	release, refused := s.attestations.admit(client, len(nonce) > 0, s.settings.Now())
	if refused != "" {
		w.Header().Set("Retry-After", "5")
		s.writeError(w, http.StatusTooManyRequests, refused, "the attestation is being asked faster than the machine can quote; try again in a few seconds")
		return
	}
	defer release()
	document, err := s.attestation(r.Context(), nonce)
	if err != nil {
		if s.settings.Logger != nil {
			s.settings.Logger.Printf("attestation: %v", err)
		}
		s.writeError(w, http.StatusServiceUnavailable, "attestation_unavailable", "the machine could not produce a quote right now")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(document)
}
