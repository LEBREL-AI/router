package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/model"
)

// The batch endpoint delivers many acceptances and settlements in one call. Its contract is in
// docs/platform-settlement-batch.md: each item is processed exactly as its single endpoint would
// process it, independently of the others, and answered with that endpoint's status and error.

const (
	BatchPath     = "/api/internal/gateway/reservations/batch"
	MaxBatchItems = 100

	BatchAccepted = "accepted"
	BatchSettle   = "settle"
)

var (
	// ErrBatchUnsupported means the control plane has no batch endpoint, or refused the batch's
	// shape: deliver the items one by one.
	ErrBatchUnsupported = errors.New("control plane has no batch endpoint")
	// ErrBatchEnvelope marks an answer to the batch call as a whole (a 409, a redirect, a 5xx): it
	// says nothing about any item, so every item it carries is retried, never refused.
	ErrBatchEnvelope = errors.New("the batch call failed as a whole")
)

// BatchItem is one acceptance or settlement inside a batch; one item per reservation.
type BatchItem struct {
	Kind          string
	ReservationID string
	Actual        int64
	Usage         model.Usage
	Outcome       model.SettlementOutcome
	Provider      string
}

// Key is the item's idempotency key, the same one its single endpoint requires.
func (i BatchItem) Key() string {
	return i.ReservationID + ":" + i.Kind
}

// validate is the check its single call makes before sending anything.
func (i BatchItem) validate() error {
	if i.ReservationID == "" {
		return fmt.Errorf("%w: a batch item without a reservation id", ErrInvalidOperation)
	}
	switch i.Kind {
	case BatchAccepted:
		return nil
	case BatchSettle:
		if i.Actual < 0 || !i.Outcome.Valid() || !i.Usage.Valid() {
			return fmt.Errorf("%w: settlement of %q for %d with outcome %q", ErrInvalidOperation, i.ReservationID, i.Actual, i.Outcome)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown batch item kind %q", ErrInvalidOperation, i.Kind)
	}
}

// Batcher delivers several reservations' operations in one call.
type Batcher interface {
	DeliverBatch(context.Context, []BatchItem) ([]error, error)
}

type wireBatchItem struct {
	Key            string                  `json:"key"`
	Kind           string                  `json:"kind"`
	ReservationID  string                  `json:"reservationId"`
	ActualMicrousd *int64                  `json:"actualMicrousd,omitempty"`
	Usage          *remoteUsage            `json:"usage,omitempty"`
	Outcome        model.SettlementOutcome `json:"outcome,omitempty"`
	Provider       string                  `json:"provider,omitempty"`
}

// DeliverBatch sends the items in one call and returns one error per item, in order (nil:
// delivered), each mapped like its single call's. An item this side refuses to send is refused
// alone; an answer to the call as a whole is every sent item's (ErrBatchEnvelope: retry). The error
// alone, ErrBatchUnsupported, means nothing was answered: deliver the items one by one.
func (l *Remote) DeliverBatch(ctx context.Context, items []BatchItem) ([]error, error) {
	if len(items) == 0 || len(items) > MaxBatchItems {
		return nil, fmt.Errorf("%w: a batch holds 1 to %d items, not %d", ErrBatchUnsupported, MaxBatchItems, len(items))
	}
	errs := make([]error, len(items))
	request := struct {
		Items []wireBatchItem `json:"items"`
	}{Items: make([]wireBatchItem, 0, len(items))}
	index := make(map[string]int, len(items))
	reservations := make(map[string]bool, len(items))
	for i, item := range items {
		if err := item.validate(); err != nil {
			errs[i] = err
			continue
		}
		if reservations[item.ReservationID] {
			// one operation per reservation per call: this one goes in a later call
			errs[i] = fmt.Errorf("%w: a second operation of %s in one batch", ErrUnavailable, item.ReservationID)
			continue
		}
		reservations[item.ReservationID] = true
		wire := wireBatchItem{Key: item.Key(), Kind: item.Kind, ReservationID: item.ReservationID}
		if item.Kind == BatchSettle {
			actual, usage := item.Actual, toRemoteUsage(item.Usage)
			wire.ActualMicrousd, wire.Usage, wire.Outcome, wire.Provider = &actual, &usage, item.Outcome, item.Provider
		}
		index[wire.Key] = i
		request.Items = append(request.Items, wire)
	}
	if len(request.Items) == 0 {
		return errs, nil
	}
	var response struct {
		Results []struct {
			Key    string          `json:"key"`
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error,omitempty"`
		} `json:"results"`
	}
	if err := l.client.DoJSON(ctx, http.MethodPost, BatchPath, request, &response, nil); err != nil {
		var httpError *controlplane.HTTPError
		if errors.As(err, &httpError) {
			switch httpError.StatusCode {
			case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge,
				http.StatusUnprocessableEntity, http.StatusNotImplemented:
				return nil, fmt.Errorf("%w: %w", ErrBatchUnsupported, err)
			}
		}
		envelope := fmt.Errorf("%w: %w", ErrBatchEnvelope, mapRemoteError(err))
		for _, i := range index {
			errs[i] = envelope
		}
		return errs, nil
	}
	answered := make(map[int]bool, len(index))
	for _, result := range response.Results {
		i, ok := index[result.Key]
		if !ok || answered[i] {
			continue
		}
		answered[i] = true
		switch {
		case result.Status >= 200 && result.Status < 300:
		case result.Status < 100 || result.Status > 599:
			errs[i] = fmt.Errorf("%w: batch result status %d for %s", ErrUnavailable, result.Status, result.Key)
		default:
			var body []byte
			if len(result.Error) > 0 {
				body = append(append([]byte(`{"error":`), result.Error...), '}')
			}
			errs[i] = mapRemoteError(controlplane.NewHTTPError(result.Status, body))
		}
	}
	for key, i := range index {
		if !answered[i] {
			errs[i] = fmt.Errorf("%w: the batch answer omitted %s", ErrUnavailable, key)
		}
	}
	return errs, nil
}
