package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

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

type RemoteAuthenticator struct {
	client controlplane.JSONClient
}

func NewRemoteAuthenticator(client controlplane.JSONClient) *RemoteAuthenticator {
	return &RemoteAuthenticator{client: client}
}

func (a *RemoteAuthenticator) Authenticate(ctx context.Context, authorization string) (Principal, error) {
	rawKey, err := bearerToken(authorization)
	if err != nil || (!strings.HasPrefix(rawKey, "leb_live_") && !strings.HasPrefix(rawKey, "raw_live_")) {
		return Principal{}, ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(rawKey))
	request := struct {
		TokenHash string `json:"tokenHash"`
	}{TokenHash: hex.EncodeToString(digest[:])}
	var response struct {
		AccountID          string `json:"accountId"`
		APIKeyID           string `json:"apiKeyId"`
		Concurrency        int    `json:"concurrency"`
		AccountConcurrency int    `json:"accountConcurrency"`
	}
	if err := a.client.DoJSON(
		ctx,
		http.MethodPost,
		"/api/internal/gateway/authenticate",
		request,
		&response,
		nil,
	); err != nil {
		var httpError *controlplane.HTTPError
		if errors.As(err, &httpError) &&
			(httpError.StatusCode == http.StatusUnauthorized ||
				httpError.StatusCode == http.StatusForbidden ||
				httpError.StatusCode == http.StatusNotFound) {
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
