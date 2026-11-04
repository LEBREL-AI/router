package router

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"lebrel-router/internal/dstack"
)

// AttestationPath is where the router serves its hardware attestation: the TDX quote of the
// confidential VM it runs in, bound to its receipt-signing key.
const AttestationPath = "/.well-known/attestation"

// attestationDomain separates this binding from any other use of the quote's report data.
const attestationDomain = "lebrel-router-attestation-v1"

// attestationNonceMax bounds the nonce a verifier may ask into the report data.
const attestationNonceMax = 32

// attestationCacheFor is how long a quote without a nonce is reused.
const attestationCacheFor = 5 * time.Minute

// AttestationReportData is what the quote's 64-byte report data commits to: the receipt-signing
// public key, with the verifier's nonce when one is given. report_data = SHA-512(domain || public
// key || nonce), so a verifier recomputes it from the document and compares it with the quote.
func AttestationReportData(publicKey []byte, nonce []byte) []byte {
	h := sha512.New()
	h.Write([]byte(attestationDomain))
	h.Write(publicKey)
	h.Write(nonce)
	return h.Sum(nil)
}

// AttestationDocument is the public document a verifier reads.
type AttestationDocument struct {
	Kind string `json:"kind"`
	// Scheme names how report_data is derived, for verifiers.
	Scheme string `json:"scheme"`
	// PublicKey and SigningKeyID are the receipt-signing key the quote is bound to.
	PublicKey    string `json:"public_key"`
	SigningKeyID string `json:"signing_key_id"`
	InstanceID   string `json:"instance_id"`
	Nonce        string `json:"nonce"`
	ReportData   string `json:"report_data"`
	// Quote is the TDX quote, hex; EventLog explains RTMR3 (JSON, as the agent gives it).
	Quote    string          `json:"quote"`
	EventLog json.RawMessage `json:"event_log"`
	// Info is the app's identity as the guest agent states it.
	Info     dstack.Info `json:"info"`
	IssuedAt int64       `json:"issued_at"`
}

type attestationCache struct {
	mu       sync.Mutex
	document *AttestationDocument
	expires  time.Time
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
	reportData := AttestationReportData(s.settings.Signer.Public, nonce)
	quote, err := s.settings.Attester.Quote(ctx, reportData)
	if err != nil {
		return nil, err
	}
	info, err := s.settings.Attester.Info(ctx)
	if err != nil {
		return nil, err
	}
	eventLog := json.RawMessage(quote.EventLog)
	if !json.Valid(eventLog) {
		eventLog, _ = json.Marshal(quote.EventLog)
	}
	document := &AttestationDocument{
		Kind: "lebrel-router-attestation", Scheme: "report_data = sha512(\"" + attestationDomain + "\" || public_key || nonce)",
		PublicKey: hex.EncodeToString(s.settings.Signer.Public), SigningKeyID: s.settings.Signer.KeyID, InstanceID: s.settings.Signer.InstanceID,
		Nonce: hex.EncodeToString(nonce), ReportData: hex.EncodeToString(reportData),
		Quote: hex.EncodeToString(quote.Quote), EventLog: eventLog, Info: info, IssuedAt: now.Unix(),
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
	document, err := s.attestation(r.Context(), nonce)
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, "attestation_unavailable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(document)
}
