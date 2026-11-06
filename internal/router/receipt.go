package router

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Route receipts are Proof of Edition receipts of kind "route": they commit to where a
// request went and what the last verification of that upstream said. They never
// claim which weights answered; that is written in the spec and in the manifest.

const (
	RouteDocumentVersion = 2
	DocumentKindRoute    = "route"
)

type SignedDocument struct {
	Payload   map[string]any `json:"payload"`
	Signature string         `json:"signature"`
}

// CanonicalJSON matches proof-of-edition/receipts/schema.py: sorted keys, no
// whitespace, UTF-8 without HTML escaping. Payloads must be maps of JSON values.
func CanonicalJSON(payload map[string]any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

func SHA256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type Signer struct {
	private    ed25519.PrivateKey
	Public     ed25519.PublicKey
	KeyID      string
	InstanceID string
}

func NewSigner(seed []byte, instanceID string) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("signing seed must be 32 bytes")
	}
	if instanceID == "" {
		return nil, errors.New("instance id is required")
	}
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	return &Signer{private: private, Public: public, KeyID: SHA256Hex(public), InstanceID: instanceID}, nil
}

func (s *Signer) Sign(payload map[string]any) (SignedDocument, error) {
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		return SignedDocument{}, err
	}
	signature := ed25519.Sign(s.private, canonical)
	return SignedDocument{Payload: payload, Signature: base64.StdEncoding.EncodeToString(signature)}, nil
}

func Verify(document SignedDocument, public ed25519.PublicKey) bool {
	canonical, err := CanonicalJSON(document.Payload)
	if err != nil {
		return false
	}
	signature, err := base64.StdEncoding.DecodeString(document.Signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(public, canonical, signature)
}

// ManifestIdentity is the digest of a manifest without its validity window, as in schema.py.
func ManifestIdentity(payload map[string]any) (string, error) {
	identity := make(map[string]any, len(payload))
	for key, value := range payload {
		if key == "issued_at" || key == "expires_at" {
			continue
		}
		identity[key] = value
	}
	canonical, err := CanonicalJSON(identity)
	if err != nil {
		return "", err
	}
	return SHA256Hex(canonical), nil
}

// RouteManifest describes one route: the public model id, its level, the upstreams it
// may use (host and model id, never keys) and the pricing. Signed and republished
// every window like a serving manifest; receipts reference its identity. attested says
// whether the signer runs inside an attested VM (its quote at /.well-known/attestation
// binds the signing key): a manifest signed elsewhere says so too.
func (s *Signer) RouteManifest(route Route, boardURL string, now time.Time, window time.Duration, attested bool) (SignedDocument, string, error) {
	upstreams := make([]any, 0, len(route.Upstreams))
	for _, u := range route.Upstreams {
		// the service's public endpoint: an environment override stays within the same service
		// (config.go) and may carry an account detail, which a public document does not name
		upstreams = append(upstreams, map[string]any{"name": u.Name, "base_url": u.BaseURL, "model": u.Model, "data_note": u.DataNote})
	}
	hash, err := route.BillingContractHash()
	if err != nil {
		return SignedDocument{}, "", err
	}
	payload := map[string]any{
		"version":               RouteDocumentVersion,
		"kind":                  DocumentKindRoute,
		"model_id":              route.ModelID,
		"level":                 route.Level,
		"family":                route.Family,
		"claimed_revision":      route.ClaimedRevision,
		"upstreams":             upstreams,
		"pricing":               map[string]any{"input_microusd_per_million_tokens": route.Pricing.InputMicrousdPerMillionTokens, "cached_input_microusd_per_million_tokens": route.Pricing.CachedInputMicrousdPerMillionTokens, "output_microusd_per_million_tokens": route.Pricing.OutputMicrousdPerMillionTokens, "fee_bps": route.Pricing.FeeBPS, "version": route.Pricing.Version, "effective_at": route.Pricing.EffectiveAt, "peak": peakManifest(route.Pricing.Peak)},
		"billing_contract_hash": hash,
		"data_note":             route.DataNote,
		"verification_board":    boardURL,
		"claims":                "route only: the receipt proves where a request went and what the last probe of that upstream said, not which weights answered",
		"attested":              attested,
		"issued_at":             now.Unix(),
		"expires_at":            now.Add(window).Unix(),
		"signing_key_id":        s.KeyID,
	}
	identity, err := ManifestIdentity(payload)
	if err != nil {
		return SignedDocument{}, "", err
	}
	signed, err := s.Sign(payload)
	return signed, identity, err
}

func peakManifest(peak *PeakPricing) any {
	if peak == nil {
		return nil
	}
	windows := make([]any, 0, len(peak.WindowsUTC))
	for _, window := range peak.WindowsUTC {
		windows = append(windows, []any{window[0], window[1]})
	}
	weekdays := make([]any, 0, len(peak.Weekdays))
	for _, day := range peak.Weekdays {
		weekdays = append(weekdays, day)
	}
	holidays := make([]any, 0, len(peak.HolidaysUTC))
	for _, day := range peak.HolidaysUTC {
		holidays = append(holidays, day)
	}
	return map[string]any{"multiplier_percent": peak.MultiplierPercent, "weekdays": weekdays, "windows_utc": windows, "holidays_utc": holidays}
}

type ReceiptInput struct {
	RequestID         string
	ModelID           string
	ManifestIdentity  string
	Upstream          string
	UpstreamModel     string
	ServedModel       string
	Provider          string
	Verification      VerificationStatus
	PromptSHA256      string
	ResponseSHA256    string
	PromptTokens      int64
	CompletionTokens  int64
	CachedTokens      int64
	UsageExact        bool
	PriceMicrousd     int64
	FeeBPS            int64
	Streamed          bool
	PricingTier       string
	MultiplierPercent int64
	// the lab's own record of the answer: its response id and the backend configuration it reported
	UpstreamResponseID        string
	UpstreamSystemFingerprint string
	// HashSalt is the client's salt for the digests (sent inside an encrypted request); empty means
	// plain SHA-256. HashSaltScheme names it in the receipt (ReceiptSaltScheme).
	HashSalt       []byte
	HashSaltScheme string
}

// Digest is the receipt's digest of a body: plain SHA-256, or SHA-256 over the client's salt and
// the body when the request carried one.
func (input ReceiptInput) Digest(body []byte) string {
	if len(input.HashSalt) == 0 {
		return SHA256Hex(body)
	}
	h := sha256.New()
	h.Write(input.HashSalt)
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Signer) RouteReceipt(input ReceiptInput, now time.Time) (SignedDocument, error) {
	payload := map[string]any{
		"version":                     RouteDocumentVersion,
		"kind":                        DocumentKindRoute,
		"request_id":                  input.RequestID,
		"model_id":                    input.ModelID,
		"manifest_sha256":             input.ManifestIdentity,
		"upstream":                    input.Upstream,
		"upstream_model":              input.UpstreamModel,
		"served_model":                input.ServedModel,
		"provider":                    input.Provider,
		"verification":                verificationPayload(input.Verification),
		"upstream_response_id":        input.UpstreamResponseID,
		"upstream_system_fingerprint": input.UpstreamSystemFingerprint,
		"prompt_sha256":               input.PromptSHA256,
		"response_sha256":             input.ResponseSHA256,
		"prompt_tokens":               input.PromptTokens,
		"completion_tokens":           input.CompletionTokens,
		"cached_tokens":               input.CachedTokens,
		"usage_exact":                 input.UsageExact,
		"price_microusd":              input.PriceMicrousd,
		"fee_bps":                     input.FeeBPS,
		"pricing_tier":                input.PricingTier,
		"multiplier_percent":          input.MultiplierPercent,
		"streamed":                    input.Streamed,
		"issued_at":                   now.Unix(),
		"instance_id":                 s.InstanceID,
		"signing_key_id":              s.KeyID,
	}
	if input.HashSaltScheme != "" {
		payload["hash_salt_scheme"] = input.HashSaltScheme
	}
	return s.Sign(payload)
}

// ReceiptStore keeps signed receipts (hashes and metadata only) for a while so a
// client can fetch the receipt of a streamed response afterwards.
type ReceiptStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	limit   int
	entries map[string]storedReceipt
	order   []string
	now     func() time.Time
}

type storedReceipt struct {
	document SignedDocument
	storedAt time.Time
}

func NewReceiptStore(ttl time.Duration, limit int) *ReceiptStore {
	return &ReceiptStore{ttl: ttl, limit: limit, entries: make(map[string]storedReceipt), now: time.Now}
}

func (r *ReceiptStore) Put(requestID string, document SignedDocument) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked()
	if _, exists := r.entries[requestID]; !exists {
		r.order = append(r.order, requestID)
	}
	r.entries[requestID] = storedReceipt{document: document, storedAt: r.now()}
	for len(r.order) > r.limit {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.entries, oldest)
	}
}

func (r *ReceiptStore) Get(requestID string) (SignedDocument, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked()
	entry, ok := r.entries[requestID]
	return entry.document, ok
}

func (r *ReceiptStore) evictLocked() {
	cutoff := r.now().Add(-r.ttl)
	for len(r.order) > 0 {
		oldest := r.order[0]
		entry, ok := r.entries[oldest]
		if ok && entry.storedAt.After(cutoff) {
			break
		}
		r.order = r.order[1:]
		delete(r.entries, oldest)
	}
}

// verificationPayload is the board's verdict for the upstream and, when the watch has checked it,
// the fingerprint of Lebrel's own route: integers only, like every signed document.
func verificationPayload(status VerificationStatus) map[string]any {
	payload := map[string]any{"status": status.Status, "run_id": status.RunID, "checked_at": status.CheckedAt}
	if fp := status.Fingerprint; fp != nil {
		payload["fingerprint"] = map[string]any{"verdict": fp.Verdict, "similarity_bps": fp.SimilarityBPS, "run_id": fp.RunID,
			"checked_at": fp.CheckedAt, "receipts_verified": fp.ReceiptsVerified, "receipts_total": fp.ReceiptsTotal, "compared": fp.Compared}
		if fp.WeightsVerdict != "" {
			payload["fingerprint"].(map[string]any)["weights_verdict"] = fp.WeightsVerdict
			payload["fingerprint"].(map[string]any)["weights_checked_at"] = fp.WeightsCheckedAt
		}
	}
	return payload
}
