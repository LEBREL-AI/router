package router

// Mirrored from the gateway's billing contract (lebrel.billing.v2) so the control
// plane computes the same hash for a routed model as for a self-hosted one.
const (
	billingSchemaVersion = "lebrel.billing.v2"
	billingCurrencyUnit  = "microusd"
	billingRoundingMode  = "ceil_per_request"
	missingUsagePolicy   = "reserved"
)
