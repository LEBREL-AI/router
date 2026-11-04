package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/model"
)

type Remote struct {
	client controlplane.JSONClient
}

func (l *Remote) MarkAccepted(
	ctx context.Context,
	reservationID string,
) error {
	if reservationID == "" {
		return fmt.Errorf("%w: acceptance without a reservation id", ErrInvalidOperation)
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", reservationID+":accepted")
	if err := l.client.DoJSON(
		ctx,
		http.MethodPost,
		"/api/internal/gateway/reservations/"+url.PathEscape(reservationID)+"/accepted",
		map[string]any{},
		nil,
		headers,
	); err != nil {
		return mapRemoteError(err)
	}
	return nil
}

func NewRemote(client controlplane.JSONClient) *Remote {
	return &Remote{client: client}
}

func (l *Remote) Reserve(
	ctx context.Context,
	accountID string,
	apiKeyID string,
	modelID string,
	billingContractHash string,
	requestID string,
	amount int64,
	queue model.QueueClass,
	estimatedPromptTokens int64,
	maxOutputTokens int64,
) (model.Reservation, error) {
	if accountID == "" || apiKeyID == "" || modelID == "" || billingContractHash == "" || requestID == "" || amount < 0 || estimatedPromptTokens < 0 || maxOutputTokens < 1 {
		return model.Reservation{}, errors.New("invalid reservation")
	}
	request := struct {
		AccountID             string           `json:"accountId"`
		APIKeyID              string           `json:"apiKeyId"`
		ModelID               string           `json:"modelId"`
		BillingContractHash   string           `json:"billingContractHash"`
		RequestID             string           `json:"requestId"`
		AmountMicrousd        int64            `json:"amountMicrousd"`
		Queue                 model.QueueClass `json:"queue"`
		EstimatedPromptTokens int64            `json:"estimatedPromptTokens"`
		MaxOutputTokens       int64            `json:"maxOutputTokens"`
	}{
		AccountID:             accountID,
		APIKeyID:              apiKeyID,
		ModelID:               modelID,
		BillingContractHash:   billingContractHash,
		RequestID:             requestID,
		AmountMicrousd:        amount,
		Queue:                 queue,
		EstimatedPromptTokens: estimatedPromptTokens,
		MaxOutputTokens:       maxOutputTokens,
	}
	var response struct {
		ID             string    `json:"id"`
		ReservationID  string    `json:"reservationId"`
		CreatedAt      time.Time `json:"createdAt"`
		AmountMicrousd int64     `json:"amountMicrousd"`
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", requestID)
	err := l.client.DoJSON(
		ctx,
		http.MethodPost,
		"/api/internal/gateway/reservations",
		request,
		&response,
		headers,
	)
	if err != nil {
		return model.Reservation{}, mapRemoteError(err)
	}
	id := response.ID
	if id == "" {
		id = response.ReservationID
	}
	if id == "" {
		return model.Reservation{}, errors.New("control plane reservation response omitted id")
	}
	createdAt := response.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return model.Reservation{
		ID:                    id,
		AccountID:             accountID,
		APIKeyID:              apiKeyID,
		ModelID:               modelID,
		BillingContractHash:   billingContractHash,
		EstimatedPromptTokens: estimatedPromptTokens,
		MaxOutputTokens:       maxOutputTokens,
		RequestID:             requestID,
		Amount:                amount,
		CreatedAt:             createdAt,
		Queue:                 queue,
	}, nil
}

func (l *Remote) Settle(
	ctx context.Context,
	reservationID string,
	actual int64,
	usage model.Usage,
	outcome model.SettlementOutcome,
	provider string,
) (model.Settlement, error) {
	if reservationID == "" || actual < 0 || !outcome.Valid() || !usage.Valid() {
		return model.Settlement{}, fmt.Errorf("%w: settlement of %q for %d with outcome %q", ErrInvalidOperation, reservationID, actual, outcome)
	}
	// provider is the upstream that served the request; the control plane records it on
	// the usage event (defaulting to "lebrel" for self-hosted models when omitted).
	request := struct {
		ActualMicrousd int64                   `json:"actualMicrousd"`
		Usage          remoteUsage             `json:"usage"`
		Outcome        model.SettlementOutcome `json:"outcome"`
		Provider       string                  `json:"provider,omitempty"`
	}{
		ActualMicrousd: actual,
		Usage:          toRemoteUsage(usage),
		Outcome:        outcome,
		Provider:       provider,
	}
	var response struct {
		ReservationID string                  `json:"reservationId"`
		Reserved      int64                   `json:"reservedMicrousd"`
		Actual        int64                   `json:"actualMicrousd"`
		Refunded      int64                   `json:"refundedMicrousd"`
		Usage         remoteUsage             `json:"usage"`
		UsageExact    bool                    `json:"usageExact"`
		Outcome       model.SettlementOutcome `json:"outcome"`
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", reservationID+":settle")
	err := l.client.DoJSON(
		ctx,
		http.MethodPost,
		"/api/internal/gateway/reservations/"+url.PathEscape(reservationID)+"/settle",
		request,
		&response,
		headers,
	)
	if err != nil {
		return model.Settlement{}, mapRemoteError(err)
	}
	settlementUsage := usage
	if response.Usage.PromptTokens != 0 || response.Usage.CompletionTokens != 0 || response.Usage.TotalTokens != 0 {
		settlementUsage = response.Usage.modelUsage()
	}
	responseActual := response.Actual
	if responseActual == 0 && actual != 0 {
		responseActual = actual
	}
	responseUsageExact := outcome == model.SettlementSucceeded
	responseOutcome := response.Outcome
	if responseOutcome == "" {
		responseOutcome = outcome
	}
	if response.ReservationID == "" {
		response.ReservationID = reservationID
	}
	return model.Settlement{
		ReservationID: response.ReservationID,
		Reserved:      response.Reserved,
		Actual:        responseActual,
		Refunded:      response.Refunded,
		Usage:         settlementUsage,
		UsageExact:    responseUsageExact,
		Outcome:       responseOutcome,
	}, nil
}

func (l *Remote) Balance(ctx context.Context, accountID string) (int64, error) {
	if accountID == "" {
		return 0, errors.New("account id is required")
	}
	var response struct {
		BalanceMicrousd int64 `json:"balanceMicrousd"`
	}
	err := l.client.DoJSON(
		ctx,
		http.MethodGet,
		"/api/internal/gateway/accounts/"+url.PathEscape(accountID)+"/balance",
		nil,
		&response,
		nil,
	)
	if err != nil {
		return 0, mapRemoteError(err)
	}
	return response.BalanceMicrousd, nil
}

type remoteUsage struct {
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	CachedTokens     int64 `json:"cachedTokens"`
}

func toRemoteUsage(usage model.Usage) remoteUsage {
	return remoteUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CachedTokens:     usage.CachedTokens,
	}
}

func (u remoteUsage) modelUsage() model.Usage {
	return model.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.CachedTokens,
	}
}

// mapRemoteError names the control plane's answer with the ledger's errors and keeps the answer
// itself in the chain, so callers can still read its status, code and details.
func mapRemoteError(err error) error {
	var httpError *controlplane.HTTPError
	if !errors.As(err, &httpError) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	switch httpError.StatusCode {
	case http.StatusPaymentRequired:
		return fmt.Errorf("%w: %w", ErrInsufficientCredit, err)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", ErrConcurrencyLimit, err)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrReservationMissing, err)
	case http.StatusConflict:
		return fmt.Errorf("%w: %w", ErrReservationConflict, err)
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

// Settled is how the control plane says it already settled a reservation.
type Settled struct {
	Outcome model.SettlementOutcome `json:"outcome"`
	Actual  int64                   `json:"actualMicrousd"`
}

// StoredSettlement reads the settlement a refusal names (error.settled, see
// docs/platform-settlement-batch.md), when the control plane sent one.
func StoredSettlement(err error) (Settled, bool) {
	var httpError *controlplane.HTTPError
	if !errors.As(err, &httpError) || len(httpError.Body) == 0 {
		return Settled{}, false
	}
	var envelope struct {
		Error struct {
			Settled *Settled `json:"settled"`
		} `json:"error"`
	}
	if json.Unmarshal(httpError.Body, &envelope) != nil || envelope.Error.Settled == nil || envelope.Error.Settled.Outcome == "" {
		return Settled{}, false
	}
	return *envelope.Error.Settled, true
}

// ErrorCode is the control plane's error.code for a refusal ("" when it sent none).
func ErrorCode(err error) string {
	var httpError *controlplane.HTTPError
	if errors.As(err, &httpError) {
		return httpError.Code
	}
	return ""
}
