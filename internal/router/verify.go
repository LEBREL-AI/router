package router

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// BoardSignatureHeader carries the watch key's Ed25519 signature over the board's exact bytes.
const BoardSignatureHeader = "X-Lebrel-Watch-Signature"

// VerificationStatus is what the watch board last said about an upstream. "unknown"
// means the board has no entry for it or could not be loaded; the receipt says so.
// Fingerprint is the watch's last hourly check of Lebrel's own route for the model.
type VerificationStatus struct {
	Status      string             `json:"status"`
	RunID       string             `json:"run_id"`
	CheckedAt   int64              `json:"checked_at"`
	Fingerprint *FingerprintStatus `json:"fingerprint,omitempty"`
}

// FingerprintStatus: the answer through Lebrel's route against the lab's own API (first-word
// probabilities, similarity in basis points so signed documents stay integer-only) and the route
// receipts the watch verified in that check.
type FingerprintStatus struct {
	Verdict          string `json:"verdict"`
	SimilarityBPS    int64  `json:"similarity_bps"`
	RunID            string `json:"run_id"`
	CheckedAt        int64  `json:"checked_at"`
	ReceiptsVerified int64  `json:"receipts_verified"`
	ReceiptsTotal    int64  `json:"receipts_total"`
	// Compared is how many prompts the verdict rests on in that check; a percentage without it says little.
	Compared int64 `json:"compared"`
	// WeightsVerdict is what the watch found when it ran the weights the lab published and compared the lab's
	// own API with them (within_margin, at_margin, outside_margin, insufficient, or a sampled_* verdict when the
	// lab returns no probabilities). Empty when no such reference run exists for the model yet: the route is
	// then compared with the lab's own API only, and the pages say so.
	WeightsVerdict   string `json:"weights_verdict,omitempty"`
	WeightsCheckedAt int64  `json:"weights_checked_at,omitempty"`
}

var Unknown = VerificationStatus{Status: "unknown"}

type boardDocument struct {
	RunID       string  `json:"run_id"`
	GeneratedAt float64 `json:"generated_at"`
	Entries     []struct {
		Target          string `json:"target"`
		Status          string `json:"status"`
		RouteModelID    string `json:"route_model_id"`
		FingerprintOnly bool   `json:"fingerprint_only"`
		Fingerprint     *struct {
			Verdict           string   `json:"verdict"`
			SimilarityPercent *float64 `json:"similarity_percent"`
			RunID             string   `json:"run_id"`
			CheckedAt         float64  `json:"checked_at"`
			Stale             bool     `json:"stale"`
			Compared          *int64   `json:"compared"`
			Anchor            string   `json:"anchor"`
			Reference         *struct {
				Verdict    string  `json:"verdict"`
				MeasuredAt float64 `json:"measured_at"`
			} `json:"reference"`
			Receipts *struct {
				Verified int64 `json:"verified"`
				Failed   int64 `json:"failed"`
				Missing  int64 `json:"missing"`
			} `json:"receipts"`
		} `json:"fingerprint"`
	} `json:"entries"`
}

// Verification loads the watch board from a file or an https URL and answers
// per-upstream. Upstream names in the routes file must equal target names in the watch.
type Verification struct {
	mu           sync.RWMutex
	source       string
	client       *http.Client
	publicKey    ed25519.PublicKey
	statuses     map[string]VerificationStatus
	fingerprints map[string]FingerprintStatus
	// routeStatuses is the board's own row for Lebrel's route of a model (its agreement with the lab's API),
	// distinct from the lab's row: the lab's API can sit outside the margin of its published weights while the
	// route serving exactly that API stays consistent.
	routeStatuses map[string]string
	loadedAt      time.Time
	lastErr       error
}

func NewVerification(source string) *Verification {
	return &Verification{source: source, client: &http.Client{Timeout: 15 * time.Second}, statuses: map[string]VerificationStatus{}, fingerprints: map[string]FingerprintStatus{}, routeStatuses: map[string]string{}}
}

// RequireSignature pins the watch key: a board is applied only with a valid signature over its
// exact bytes (the X-Lebrel-Watch-Signature header, or a ".sig" file beside a file source).
func (v *Verification) RequireSignature(key ed25519.PublicKey) { v.publicKey = key }

func (v *Verification) Source() string { return v.source }

func (v *Verification) Refresh(ctx context.Context) error {
	if v.source == "" {
		return errors.New("no verification board configured")
	}
	var raw []byte
	var signature string
	var err error
	if strings.HasPrefix(v.source, "https://") || strings.HasPrefix(v.source, "http://") {
		raw, signature, err = v.fetch(ctx)
	} else {
		raw, err = os.ReadFile(v.source)
		if err == nil && v.publicKey != nil {
			sig, sigErr := os.ReadFile(v.source + ".sig")
			if sigErr == nil {
				signature = strings.TrimSpace(string(sig))
			}
		}
	}
	if err == nil && v.publicKey != nil && !validBoardSignature(raw, signature, v.publicKey) {
		err = errors.New("board signature missing or invalid: the previous board stays in force")
	}
	if err == nil {
		err = v.apply(raw)
	}
	v.mu.Lock()
	v.lastErr = err
	v.mu.Unlock()
	return err
}

func validBoardSignature(raw []byte, signature string, key ed25519.PublicKey) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(decoded) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(key, raw, decoded)
}

func (v *Verification) fetch(ctx context.Context) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.source, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("User-Agent", "lebrel-router/1")
	response, err := v.client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("board returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	return raw, response.Header.Get(BoardSignatureHeader), err
}

func (v *Verification) apply(raw []byte) error {
	var board boardDocument
	if err := json.Unmarshal(raw, &board); err != nil {
		return fmt.Errorf("board: %w", err)
	}
	statuses := make(map[string]VerificationStatus, len(board.Entries))
	fingerprints := map[string]FingerprintStatus{}
	routeStatuses := map[string]string{}
	// The labs' own APIs are the anchors; when the watch has run a lab's published weights itself, the
	// anchor entry carries that comparison, and every route measured against that anchor inherits it.
	weights := map[string]struct {
		verdict string
		at      int64
	}{}
	for _, entry := range board.Entries {
		if fp := entry.Fingerprint; fp != nil && fp.Verdict == "anchor" && fp.Reference != nil && fp.Reference.Verdict != "" {
			weights[entry.Target] = struct {
				verdict string
				at      int64
			}{fp.Reference.Verdict, int64(fp.Reference.MeasuredAt)}
		}
	}
	for _, entry := range board.Entries {
		statuses[entry.Target] = VerificationStatus{Status: entry.Status, RunID: board.RunID, CheckedAt: int64(board.GeneratedAt)}
		if entry.RouteModelID != "" && entry.Status != "" {
			routeStatuses[entry.RouteModelID] = entry.Status
		}
		fp := entry.Fingerprint
		if entry.RouteModelID == "" || fp == nil || fp.Stale || fp.Verdict == "" {
			continue
		}
		status := FingerprintStatus{Verdict: fp.Verdict, RunID: fp.RunID, CheckedAt: int64(fp.CheckedAt)}
		if reference, ok := weights[fp.Anchor]; ok {
			status.WeightsVerdict = reference.verdict
			status.WeightsCheckedAt = reference.at
		}
		if fp.SimilarityPercent != nil {
			status.SimilarityBPS = int64(math.Round(*fp.SimilarityPercent * 100))
		}
		if fp.Compared != nil && *fp.Compared > 0 {
			status.Compared = *fp.Compared
		}
		if fp.Receipts != nil {
			status.ReceiptsVerified = fp.Receipts.Verified
			status.ReceiptsTotal = fp.Receipts.Verified + fp.Receipts.Failed + fp.Receipts.Missing
		}
		fingerprints[entry.RouteModelID] = status
	}
	v.mu.Lock()
	v.statuses = statuses
	v.fingerprints = fingerprints
	v.routeStatuses = routeStatuses
	v.loadedAt = time.Now()
	v.mu.Unlock()
	return nil
}

// ForRoute is Get plus the last fingerprint of Lebrel's own route for the model.
func (v *Verification) ForRoute(upstream, modelID string) VerificationStatus {
	status := v.Get(upstream)
	v.mu.RLock()
	defer v.mu.RUnlock()
	if own, ok := v.routeStatuses[modelID]; ok {
		status.Status = own // the route's own row: how Lebrel's route compares with the lab's API
	}
	if fp, ok := v.fingerprints[modelID]; ok {
		copied := fp
		status.Fingerprint = &copied
	}
	return status
}

// Get returns the last known status for an upstream name.
func (v *Verification) Get(upstream string) VerificationStatus {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if status, ok := v.statuses[upstream]; ok {
		return status
	}
	return Unknown
}

// Run refreshes on an interval until the context ends; errors are kept, not fatal.
func (v *Verification) Run(ctx context.Context, interval time.Duration) {
	_ = v.Refresh(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = v.Refresh(ctx)
		}
	}
}

func (v *Verification) Health() (loadedAt time.Time, err error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.loadedAt, v.lastErr
}
