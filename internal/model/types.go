package model

import "time"

type QueueClass string

const (
	QueueShort QueueClass = "short"
	QueueLong  QueueClass = "long"
)

type SettlementOutcome string

const (
	SettlementSucceeded        SettlementOutcome = "succeeded"
	SettlementUpstreamRejected SettlementOutcome = "upstream_rejected"
	SettlementUsageMissing     SettlementOutcome = "usage_missing"
)

// Valid is true for the three outcomes the control plane knows.
func (o SettlementOutcome) Valid() bool {
	return o == SettlementSucceeded || o == SettlementUpstreamRejected || o == SettlementUsageMissing
}

// Valid is true when no counter is negative.
func (u Usage) Valid() bool {
	return u.PromptTokens >= 0 && u.CompletionTokens >= 0 && u.TotalTokens >= 0 && u.CachedTokens >= 0
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
}

type Reservation struct {
	ID                    string
	AccountID             string
	APIKeyID              string
	ModelID               string
	BillingContractHash   string
	EstimatedPromptTokens int64
	MaxOutputTokens       int64
	RequestID             string
	Amount                int64
	CreatedAt             time.Time
	Settled               bool
	Actual                int64
	Usage                 Usage
	UsageExact            bool
	SettlementOutcome     SettlementOutcome
	UpstreamAccepted      bool
	Queue                 QueueClass
	SettledAt             time.Time
}

type Settlement struct {
	ReservationID string
	Reserved      int64
	Actual        int64
	Refunded      int64
	Balance       int64
	Usage         Usage
	UsageExact    bool
	Outcome       SettlementOutcome
}
