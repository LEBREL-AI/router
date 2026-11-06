package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"lebrel-router/internal/controlplane"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrUnavailable  = errors.New("authentication service unavailable")
)

type Principal struct {
	AccountID          string
	APIKeyID           string
	Concurrency        int
	AccountConcurrency int
	DigestPrefix       string
}

type Record struct {
	AccountID          string
	Digest             [sha256.Size]byte
	Concurrency        int
	AccountConcurrency int
}

type Authenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}

// ClientActorHeader carries, to the control plane, the address a direct client's key came from.
const ClientActorHeader = "X-Lebrel-Client-Actor"

// clientActorKey marks a direct client's request with the address the control plane counts wrong
// keys for; a request the platform's edge admitted carries none (the edge counted it already).
type clientActorKey struct{}

// WithClientActor returns ctx marked with the client's address as ActorForAddress writes it; an
// empty actor marks nothing.
func WithClientActor(ctx context.Context, actor string) context.Context {
	if actor == "" {
		return ctx
	}
	return context.WithValue(ctx, clientActorKey{}, actor)
}

// ClientActor is the address WithClientActor marked ctx with, or "".
func ClientActor(ctx context.Context) string {
	actor, _ := ctx.Value(clientActorKey{}).(string)
	return actor
}

// ActorForAddress writes an address the way the platform's edge counts wrong keys: an IPv4 address
// whole, an IPv6 address by its /64, so one customer's block counts as one and an attacker cannot
// spread over the 2^64 addresses of theirs. A port or a zone is dropped; what is not an address
// gives "".
func ActorForAddress(address string) string {
	address = strings.TrimSpace(address)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	addr, err := netip.ParseAddr(strings.Trim(address, "[]"))
	if err != nil {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return "ipv4:" + addr.String()
	}
	b := addr.As16()
	return fmt.Sprintf("ipv6:%02x%02x:%02x%02x:%02x%02x:%02x%02x::/64", b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7])
}

type HMACAuthenticator struct {
	secret  []byte
	records []Record
}

func NewHMACAuthenticator(secret []byte, records []Record) *HMACAuthenticator {
	secretCopy := append([]byte(nil), secret...)
	recordsCopy := append([]Record(nil), records...)
	return &HMACAuthenticator{secret: secretCopy, records: recordsCopy}
}

func Digest(secret []byte, rawKey string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(rawKey))
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func (a *HMACAuthenticator) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	if err := ctx.Err(); err != nil {
		return Principal{}, err
	}
	rawKey, err := bearerToken(authorization)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	digest := Digest(a.secret, rawKey)
	var matched *Record
	for _, record := range a.records {
		if subtle.ConstantTimeCompare(digest[:], record.Digest[:]) == 1 {
			recordCopy := record
			matched = &recordCopy
		}
	}
	if matched != nil {
		return Principal{
			AccountID:          matched.AccountID,
			APIKeyID:           hex.EncodeToString(digest[:8]),
			Concurrency:        matched.Concurrency,
			AccountConcurrency: matched.AccountConcurrency,
			DigestPrefix:       hex.EncodeToString(digest[:4]),
		}, nil
	}
	return Principal{}, ErrUnauthorized
}

// refusedFor is how long a key hash the control plane refused is refused here without asking
// again; refusedMax bounds what a flood of distinct wrong keys can take in memory.
const (
	refusedFor = 10 * time.Second
	refusedMax = 8192
)

type RemoteAuthenticator struct {
	client controlplane.JSONClient
	now    func() time.Time
	// refused remembers the key hashes the control plane refused, until refusedFor passes: a wrong
	// key repeated at line rate (nothing slows it by address on a direct endpoint) costs the control
	// plane one call per refusedFor, not one per request. A right key is never remembered, so a key
	// created a moment ago works at once, and a revoked one is refused by the control plane on the
	// next look.
	mu      sync.Mutex
	refused map[string]time.Time
}

func NewRemoteAuthenticator(client controlplane.JSONClient) *RemoteAuthenticator {
	return &RemoteAuthenticator{client: client, now: time.Now, refused: map[string]time.Time{}}
}

func (a *RemoteAuthenticator) recentlyRefused(hash string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	until, ok := a.refused[hash]
	if !ok {
		return false
	}
	if a.now().After(until) {
		delete(a.refused, hash)
		return false
	}
	return true
}

func (a *RemoteAuthenticator) remember(hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if len(a.refused) >= refusedMax {
		for key, until := range a.refused {
			if now.After(until) {
				delete(a.refused, key)
			}
		}
		if len(a.refused) >= refusedMax {
			return
		}
	}
	a.refused[hash] = now.Add(refusedFor)
}

func (a *RemoteAuthenticator) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	rawKey, err := bearerToken(authorization)
	if err != nil || (!strings.HasPrefix(rawKey, "leb_live_") && !strings.HasPrefix(rawKey, "raw_live_")) {
		return Principal{}, ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(rawKey))
	hash := hex.EncodeToString(digest[:])
	if a.recentlyRefused(hash) {
		return Principal{}, ErrUnauthorized
	}
	request := struct {
		TokenHash string `json:"tokenHash"`
	}{TokenHash: hash}
	var response struct {
		AccountID          string `json:"accountId"`
		APIKeyID           string `json:"apiKeyId"`
		Concurrency        int    `json:"concurrency"`
		AccountConcurrency int    `json:"accountConcurrency"`
	}
	// A direct client's address goes along: the control plane counts wrong keys per address, so
	// guessing stays slow; for the requests the platform's edge admits, the edge sent it itself.
	var headers http.Header
	if actor := ClientActor(ctx); actor != "" {
		headers = http.Header{ClientActorHeader: {actor}}
	}
	if err := a.client.DoJSON(
		ctx,
		http.MethodPost,
		"/api/internal/gateway/authenticate",
		request,
		&response,
		headers,
	); err != nil {
		var httpError *controlplane.HTTPError
		if errors.As(err, &httpError) &&
			(httpError.StatusCode == http.StatusUnauthorized ||
				httpError.StatusCode == http.StatusForbidden ||
				httpError.StatusCode == http.StatusNotFound) {
			a.remember(hash)
			return Principal{}, ErrUnauthorized
		}
		return Principal{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if response.AccountID == "" || response.APIKeyID == "" || response.Concurrency < 1 || response.AccountConcurrency < 1 {
		return Principal{}, fmt.Errorf("%w: invalid control plane auth response", ErrUnavailable)
	}
	return Principal{
		AccountID:          response.AccountID,
		APIKeyID:           response.APIKeyID,
		Concurrency:        response.Concurrency,
		AccountConcurrency: response.AccountConcurrency,
		DigestPrefix:       hex.EncodeToString(digest[:4]),
	}, nil
}

func bearerToken(authorization string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return "", ErrUnauthorized
	}
	rawKey := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	if rawKey == "" || len(rawKey) > 4096 || strings.ContainsAny(rawKey, "\r\n") {
		return "", ErrUnauthorized
	}
	return rawKey, nil
}
