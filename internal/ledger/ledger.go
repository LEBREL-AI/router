package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"lebrel-router/internal/model"
)

var (
	ErrInsufficientCredit  = errors.New("insufficient credit")
	ErrConcurrencyLimit    = errors.New("distributed concurrency limit exceeded")
	ErrReservationMissing  = errors.New("reservation not found")
	ErrReservationConflict = errors.New("reservation conflicts with the control plane's view")
	ErrUnavailable         = errors.New("ledger unavailable")
	// ErrInvalidOperation is an acceptance or settlement this side refuses to send: it can never
	// become valid by waiting, so it is never retried.
	ErrInvalidOperation = errors.New("invalid ledger operation")
)

type Ledger interface {
	Reserve(context.Context, string, string, string, string, string, int64, model.QueueClass, int64, int64) (model.Reservation, error)
	MarkAccepted(context.Context, string) error
	Settle(context.Context, string, int64, model.Usage, model.SettlementOutcome, string) (model.Settlement, error)
	Balance(context.Context, string) (int64, error)
}

func (l *InMemory) MarkAccepted(ctx context.Context, reservationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	reservation, ok := l.reservations[reservationID]
	if !ok {
		return ErrReservationMissing
	}
	if reservation.Settled {
		return fmt.Errorf("%w: reservation is already settled", ErrReservationConflict)
	}
	reservation.UpstreamAccepted = true
	return nil
}

type InMemory struct {
	mu             sync.Mutex
	balances       map[string]int64
	reservations   map[string]*model.Reservation
	requestToResID map[string]string
	now            func() time.Time
}

func NewInMemory(initialBalances map[string]int64) *InMemory {
	balances := make(map[string]int64, len(initialBalances))
	for account, balance := range initialBalances {
		balances[account] = balance
	}
	return &InMemory{
		balances:       balances,
		reservations:   make(map[string]*model.Reservation),
		requestToResID: make(map[string]string),
		now:            time.Now,
	}
}

func (l *InMemory) Reserve(
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
	if err := ctx.Err(); err != nil {
		return model.Reservation{}, err
	}
	if accountID == "" || apiKeyID == "" || modelID == "" || billingContractHash == "" || requestID == "" || amount < 0 || estimatedPromptTokens < 0 || maxOutputTokens < 1 {
		return model.Reservation{}, errors.New("invalid reservation")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	requestKey := accountID + "\x00" + requestID
	if id, ok := l.requestToResID[requestKey]; ok {
		existing := *l.reservations[id]
		if existing.APIKeyID != apiKeyID || existing.ModelID != modelID || existing.BillingContractHash != billingContractHash || existing.Amount != amount || existing.Queue != queue || existing.EstimatedPromptTokens != estimatedPromptTokens || existing.MaxOutputTokens != maxOutputTokens {
			return model.Reservation{}, errors.New("request id reused with different reservation")
		}
		return existing, nil
	}
	if l.balances[accountID] < amount {
		return model.Reservation{}, ErrInsufficientCredit
	}
	id, err := randomID()
	if err != nil {
		return model.Reservation{}, fmt.Errorf("generate reservation id: %w", err)
	}
	reservation := &model.Reservation{
		ID:                    id,
		AccountID:             accountID,
		APIKeyID:              apiKeyID,
		ModelID:               modelID,
		BillingContractHash:   billingContractHash,
		EstimatedPromptTokens: estimatedPromptTokens,
		MaxOutputTokens:       maxOutputTokens,
		RequestID:             requestID,
		Amount:                amount,
		Queue:                 queue,
		CreatedAt:             l.now().UTC(),
	}
	l.balances[accountID] -= amount
	l.reservations[id] = reservation
	l.requestToResID[requestKey] = id
	return *reservation, nil
}

func (l *InMemory) Settle(
	ctx context.Context,
	reservationID string,
	actual int64,
	usage model.Usage,
	outcome model.SettlementOutcome,
	provider string,
) (model.Settlement, error) {
	_ = provider
	if err := ctx.Err(); err != nil {
		return model.Settlement{}, err
	}
	if actual < 0 || !outcome.Valid() || !usage.Valid() {
		return model.Settlement{}, fmt.Errorf("%w: settlement %d %q", ErrInvalidOperation, actual, outcome)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	reservation, ok := l.reservations[reservationID]
	if !ok {
		return model.Settlement{}, ErrReservationMissing
	}
	if !reservation.Settled {
		if outcome == model.SettlementUpstreamRejected && reservation.UpstreamAccepted {
			return model.Settlement{}, fmt.Errorf("%w: accepted upstream cannot be rejected", ErrReservationConflict)
		}
		if outcome != model.SettlementUpstreamRejected && !reservation.UpstreamAccepted {
			return model.Settlement{}, fmt.Errorf("%w: upstream acceptance is not durable", ErrReservationConflict)
		}
		adjustment := reservation.Amount - actual
		l.balances[reservation.AccountID] += adjustment
		reservation.Settled = true
		reservation.Actual = actual
		reservation.Usage = usage
		reservation.UsageExact = outcome == model.SettlementSucceeded
		reservation.SettlementOutcome = outcome
		reservation.SettledAt = l.now().UTC()
	}
	return model.Settlement{
		ReservationID: reservation.ID,
		Reserved:      reservation.Amount,
		Actual:        reservation.Actual,
		Refunded:      reservation.Amount - reservation.Actual,
		Balance:       l.balances[reservation.AccountID],
		Usage:         reservation.Usage,
		UsageExact:    reservation.UsageExact,
		Outcome:       reservation.SettlementOutcome,
	}, nil
}

func (l *InMemory) Balance(ctx context.Context, accountID string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balances[accountID], nil
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
