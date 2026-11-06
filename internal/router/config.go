// Package router forwards OpenAI-compatible completions to third-party APIs with
// Lebrel's billing, a verification status per upstream and a signed route receipt
// per response. It never logs prompt or completion content.
package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"lebrel-router/internal/meter"
)

// PricingConfig is what a route charges, in microUSD per million tokens. The fee is
// already inside these rates: the control plane recomputes every charge from the
// published contract, so there is no separate fee field.
type PricingConfig struct {
	InputMicrousdPerMillionTokens       int64        `json:"input_microusd_per_million_tokens"`
	CachedInputMicrousdPerMillionTokens int64        `json:"cached_input_microusd_per_million_tokens"`
	OutputMicrousdPerMillionTokens      int64        `json:"output_microusd_per_million_tokens"`
	ReservationBPS                      int64        `json:"reservation_bps"`
	Version                             string       `json:"version"`
	EffectiveAt                         string       `json:"effective_at"`
	FeeBPS                              int64        `json:"fee_bps"`
	Peak                                *PeakPricing `json:"peak,omitempty"`
}

// PeakPricing passes a lab's own peak-hour surcharge through to the customer: inside the
// windows (UTC, half-open, on the listed weekdays, never on the listed holidays) every rate
// is multiplied by MultiplierPercent/100. DeepSeek: 200% on weekdays 01:00-04:00 and
// 06:00-10:00 UTC, except Chinese public holidays. The schedule is part of the billing
// contract, so the control plane prices the same request identically.
type PeakPricing struct {
	MultiplierPercent int64       `json:"multiplier_percent"`
	Weekdays          []int       `json:"weekdays"`
	WindowsUTC        [][2]string `json:"windows_utc"`
	HolidaysUTC       []string    `json:"holidays_utc,omitempty"`
}

func parseClock(value string) (int, error) {
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil || len(value) != 5 || hour < 0 || hour > 24 || minute < 0 || minute > 59 || (hour == 24 && minute != 0) {
		return 0, fmt.Errorf("bad clock %q, want HH:MM", value)
	}
	return hour*60 + minute, nil
}

func (p *PeakPricing) validate() error {
	if p == nil {
		return nil
	}
	if p.MultiplierPercent < 101 || p.MultiplierPercent > 1000 {
		return errors.New("peak.multiplier_percent must be between 101 and 1000")
	}
	if len(p.WindowsUTC) == 0 || len(p.WindowsUTC) > 8 {
		return errors.New("peak.windows_utc needs 1 to 8 windows")
	}
	if len(p.Weekdays) > 7 {
		return errors.New("peak.weekdays has more than 7 entries")
	}
	for _, day := range p.Weekdays {
		if day < 0 || day > 6 {
			return errors.New("peak.weekdays must be 0 (Sunday) to 6 (Saturday)")
		}
	}
	for _, window := range p.WindowsUTC {
		start, err := parseClock(window[0])
		if err != nil {
			return err
		}
		end, err := parseClock(window[1])
		if err != nil {
			return err
		}
		if end <= start {
			return fmt.Errorf("peak window %s-%s must end after it starts", window[0], window[1])
		}
	}
	if len(p.HolidaysUTC) > 64 {
		return errors.New("peak.holidays_utc has more than 64 entries")
	}
	for _, day := range p.HolidaysUTC {
		if _, err := time.Parse("2006-01-02", day); err != nil || len(day) != 10 {
			return fmt.Errorf("peak.holidays_utc entry %q must be YYYY-MM-DD", day)
		}
	}
	return nil
}

// Open reports whether the surcharge applies at the given instant.
func (p *PeakPricing) Open(at time.Time) bool {
	if p == nil {
		return false
	}
	utc := at.UTC()
	day := utc.Format("2006-01-02")
	for _, holiday := range p.HolidaysUTC {
		if holiday == day {
			return false
		}
	}
	if len(p.Weekdays) > 0 {
		match := false
		for _, weekday := range p.Weekdays {
			if time.Weekday(weekday) == utc.Weekday() {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	minute := utc.Hour()*60 + utc.Minute()
	for _, window := range p.WindowsUTC {
		start, _ := parseClock(window[0])
		end, _ := parseClock(window[1])
		if minute >= start && minute < end {
			return true
		}
	}
	return false
}

// MultiplierPercent is the rate multiplier in force at the given instant: 100, or the peak one.
func (r Route) MultiplierPercent(at time.Time) int64 {
	if r.Pricing.Peak != nil && r.Pricing.Peak.Open(at) {
		return r.Pricing.Peak.MultiplierPercent
	}
	return 100
}

// OtherMultiplierPercent is the multiplier on the other side of a peak boundary, when the
// route has one: the control plane's clock may sit there when ours does not.
func (r Route) OtherMultiplierPercent(percent int64) (int64, bool) {
	if r.Pricing.Peak == nil {
		return 0, false
	}
	if percent == 100 {
		return r.Pricing.Peak.MultiplierPercent, true
	}
	return 100, true
}

// PricingTier names the multiplier for receipts and logs.
func PricingTier(percent int64) string {
	if percent == 100 {
		return "standard"
	}
	return "peak"
}

// Upstream is one place a route can send a request. Lower priority goes first; the
// order is fixed in the routes file and never changes with the clock. Body fields are
// forced on every request (a client never picks the provider behind a route); Defaults
// apply only when the client did not send the key (a lab's thinking mode, for instance).
// Dialect names the request format the upstream speaks: "" or "openai" forwards the
// client's fields as they are; "openrouter" maps the labs' own reasoning switches
// (DeepSeek's `thinking`, Alibaba's `enable_thinking`) onto OpenRouter's `reasoning`,
// so a client of the route keeps writing what the lab documents.
type Upstream struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	// BaseURLEnv names an environment variable holding the base URL instead, for an endpoint whose
	// address carries an account detail (Alibaba's Model Studio puts the workspace id in the host).
	BaseURLEnv  string            `json:"base_url_env,omitempty"`
	Model       string            `json:"model"`
	APIKeyEnv   string            `json:"api_key_env"`
	Priority    int               `json:"priority"`
	Body        map[string]any    `json:"body,omitempty"`
	Defaults    map[string]any    `json:"defaults,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	UsageFormat string            `json:"usage_format,omitempty"`
	Dialect     string            `json:"dialect,omitempty"`
	// ReasoningFixed marks an endpoint that cannot reason less than always (Alibaba's qwen3.8-max
	// through OpenRouter refuses `reasoning.enabled: false`): a client asking for no reasoning gets
	// the lowest effort instead, which is the closest the endpoint offers, and the data note says so.
	ReasoningFixed bool   `json:"reasoning_fixed,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	DataNote       string `json:"data_note,omitempty"`
}

// Endpoint is the base URL the requests go to: the environment's when BaseURLEnv names one, else BaseURL.
func (u Upstream) Endpoint() string {
	if u.BaseURLEnv != "" {
		if value := strings.TrimSpace(os.Getenv(u.BaseURLEnv)); value != "" {
			return value
		}
	}
	return u.BaseURL
}

func (u Upstream) APIKey() string {
	if u.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(u.APIKeyEnv)
}

func (u Upstream) Timeout() time.Duration {
	if u.TimeoutSeconds <= 0 {
		return 90 * time.Second
	}
	return time.Duration(u.TimeoutSeconds) * time.Second
}

// Route is one public model id and everything the router needs to serve it.
type Route struct {
	ModelID                string        `json:"model_id"`
	DisplayName            string        `json:"display_name,omitempty"`
	BaseModel              string        `json:"base_model,omitempty"`
	Level                  int           `json:"level"`
	Family                 string        `json:"family"`
	ClaimedRevision        string        `json:"claimed_revision,omitempty"`
	Upstreams              []Upstream    `json:"upstreams"`
	Pricing                PricingConfig `json:"pricing"`
	MaxContextTokens       int64         `json:"max_context_tokens"`
	MaxOutputTokens        int64         `json:"max_output_tokens"`
	DefaultMaxOutputTokens int64         `json:"default_max_output_tokens"`
	DataNote               string        `json:"data_note,omitempty"`
}

type Config struct {
	Routes []Route `json:"routes"`
}

func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("routes: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if len(c.Routes) == 0 {
		return errors.New("routes: at least one route is required")
	}
	seen := make(map[string]bool, len(c.Routes))
	for i := range c.Routes {
		route := &c.Routes[i]
		if err := route.validate(); err != nil {
			return fmt.Errorf("route %q: %w", route.ModelID, err)
		}
		if seen[route.ModelID] {
			return fmt.Errorf("route %q: duplicate model id", route.ModelID)
		}
		seen[route.ModelID] = true
	}
	return nil
}

func (r *Route) validate() error {
	if strings.TrimSpace(r.ModelID) == "" || len(r.ModelID) > 256 || strings.ContainsAny(r.ModelID, "\r\n\t ") || strings.Count(r.ModelID, "/") > 1 {
		return errors.New("model_id must be a non-empty org/name id without whitespace and with at most one slash")
	}
	if r.Level < 1 || r.Level > 3 {
		return errors.New("level must be 1, 2 or 3")
	}
	if len(r.Upstreams) == 0 {
		return errors.New("at least one upstream is required")
	}
	names := make(map[string]bool, len(r.Upstreams))
	for i := range r.Upstreams {
		u := &r.Upstreams[i]
		if u.Name == "" || names[u.Name] {
			return fmt.Errorf("upstream %d: name missing or duplicate", i)
		}
		names[u.Name] = true
		parsed, err := url.Parse(u.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("upstream %q: base_url must be an absolute URL", u.Name)
		}
		if parsed.Scheme != "https" && !isLoopback(parsed.Hostname()) {
			return fmt.Errorf("upstream %q: base_url must use https", u.Name)
		}
		if strings.TrimSpace(u.Model) == "" {
			return fmt.Errorf("upstream %q: model is required", u.Name)
		}
		switch u.UsageFormat {
		case "", "openai", "deepseek":
		default:
			return fmt.Errorf("upstream %q: unknown usage_format %q", u.Name, u.UsageFormat)
		}
		switch u.Dialect {
		case "", "openai", "openrouter", "alibaba":
		default:
			return fmt.Errorf("upstream %q: unknown dialect %q", u.Name, u.Dialect)
		}
	}
	if r.MaxContextTokens <= 0 || r.MaxOutputTokens <= 0 || r.DefaultMaxOutputTokens <= 0 || r.DefaultMaxOutputTokens > r.MaxOutputTokens {
		return errors.New("max_context_tokens, max_output_tokens and default_max_output_tokens must be positive, default <= max")
	}
	if r.Pricing.ReservationBPS == 0 {
		r.Pricing.ReservationBPS = 15_000
	}
	if strings.TrimSpace(r.Pricing.Version) == "" {
		return errors.New("pricing.version is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Pricing.EffectiveAt); err != nil || !strings.HasSuffix(r.Pricing.EffectiveAt, ".000Z") {
		return errors.New("pricing.effective_at must look like 2026-09-23T00:00:00.000Z (the control plane normalises with toISOString)")
	}
	if r.Pricing.FeeBPS < 0 || r.Pricing.FeeBPS > 10_000 {
		return errors.New("pricing.fee_bps must be between 0 and 10000")
	}
	if err := r.Pricing.Peak.validate(); err != nil {
		return err
	}
	return r.MeterPricing().Validate()
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// MeterPricing maps the route's three rates onto the gateway's six. Routed models
// have no long-context surcharge, so the long rates equal the short ones and the long
// threshold equals the context limit: every request is "short".
func (r Route) MeterPricing() meter.Pricing {
	return meter.Pricing{
		InputMicrousdPerMillionTokens:           r.Pricing.InputMicrousdPerMillionTokens,
		CachedInputMicrousdPerMillionTokens:     r.Pricing.CachedInputMicrousdPerMillionTokens,
		LongInputMicrousdPerMillionTokens:       r.Pricing.InputMicrousdPerMillionTokens,
		LongCachedInputMicrousdPerMillionTokens: r.Pricing.CachedInputMicrousdPerMillionTokens,
		OutputMicrousdPerMillionTokens:          r.Pricing.OutputMicrousdPerMillionTokens,
		LongOutputMicrousdPerMillionTokens:      r.Pricing.OutputMicrousdPerMillionTokens,
		ReservationBPS:                          r.Pricing.ReservationBPS,
	}
}

// BillingContractHash is the same canonical document the gateway publishes, so the
// control plane's PUBLIC_MODEL_RELEASES_JSON entry for this model hashes identically.
// Field order is part of the byte sequence; change it only with a schema bump.
func (r Route) BillingContractHash() (string, error) {
	pricing := r.MeterPricing()
	if err := pricing.Validate(); err != nil {
		return "", err
	}
	document := struct {
		SchemaVersion                           string `json:"schemaVersion"`
		Version                                 string `json:"version"`
		EffectiveAt                             string `json:"effectiveAt"`
		ModelID                                 string `json:"modelId"`
		CurrencyUnit                            string `json:"currencyUnit"`
		TokenDenominator                        int64  `json:"tokenDenominator"`
		RoundingMode                            string `json:"roundingMode"`
		InputMicrousdPerMillionTokens           int64  `json:"inputMicrousdPerMillionTokens"`
		CachedInputMicrousdPerMillionTokens     int64  `json:"cachedInputMicrousdPerMillionTokens"`
		LongInputMicrousdPerMillionTokens       int64  `json:"longInputMicrousdPerMillionTokens"`
		LongCachedInputMicrousdPerMillionTokens int64  `json:"longCachedInputMicrousdPerMillionTokens"`
		OutputMicrousdPerMillionTokens          int64  `json:"outputMicrousdPerMillionTokens"`
		LongOutputMicrousdPerMillionTokens      int64  `json:"longOutputMicrousdPerMillionTokens"`
		ReservationSafetyBPS                    int64  `json:"reservationSafetyBps"`
		MissingUsagePolicy                      string `json:"missingUsagePolicy"`
		LongContextThresholdTokens              int64  `json:"longContextThresholdTokens"`
		MaxContextTokens                        int64  `json:"maxContextTokens"`
		MaxOutputTokens                         int64  `json:"maxOutputTokens"`
		PeakSchedule                            *struct {
			MultiplierPercent int64       `json:"multiplierPercent"`
			Weekdays          []int       `json:"weekdays"`
			WindowsUtc        [][2]string `json:"windowsUtc"`
			HolidaysUtc       []string    `json:"holidaysUtc"`
		} `json:"peakSchedule,omitempty"`
	}{
		SchemaVersion:                           billingSchemaVersion,
		Version:                                 r.Pricing.Version,
		EffectiveAt:                             r.Pricing.EffectiveAt,
		ModelID:                                 r.ModelID,
		CurrencyUnit:                            billingCurrencyUnit,
		TokenDenominator:                        meter.TokenRateDenominator,
		RoundingMode:                            billingRoundingMode,
		InputMicrousdPerMillionTokens:           pricing.InputMicrousdPerMillionTokens,
		CachedInputMicrousdPerMillionTokens:     pricing.CachedInputMicrousdPerMillionTokens,
		LongInputMicrousdPerMillionTokens:       pricing.LongInputMicrousdPerMillionTokens,
		LongCachedInputMicrousdPerMillionTokens: pricing.LongCachedInputMicrousdPerMillionTokens,
		OutputMicrousdPerMillionTokens:          pricing.OutputMicrousdPerMillionTokens,
		LongOutputMicrousdPerMillionTokens:      pricing.LongOutputMicrousdPerMillionTokens,
		ReservationSafetyBPS:                    pricing.ReservationBPS,
		MissingUsagePolicy:                      missingUsagePolicy,
		LongContextThresholdTokens:              r.LongContextThreshold(),
		MaxContextTokens:                        r.MaxContextTokens,
		MaxOutputTokens:                         r.MaxOutputTokens,
	}
	if peak := r.Pricing.Peak; peak != nil {
		weekdays := append([]int{}, peak.Weekdays...)
		windows := append([][2]string{}, peak.WindowsUTC...)
		holidays := append([]string{}, peak.HolidaysUTC...)
		document.PeakSchedule = &struct {
			MultiplierPercent int64       `json:"multiplierPercent"`
			Weekdays          []int       `json:"weekdays"`
			WindowsUtc        [][2]string `json:"windowsUtc"`
			HolidaysUtc       []string    `json:"holidaysUtc"`
		}{MultiplierPercent: peak.MultiplierPercent, Weekdays: weekdays, WindowsUtc: windows, HolidaysUtc: holidays}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("marshal billing contract: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// LongContextThreshold is the published threshold. The control plane requires it below the
// context limit, so a request that fills the whole context is "long"; long rates equal short
// rates on a route, so only the queue name changes, and it must match the control plane's.
func (r Route) LongContextThreshold() int64 {
	return r.MaxContextTokens - 1
}

// Slug is the path segment a route lives under: "deepseek/deepseek-v4.1-flash" -> "deepseek--deepseek-v4.1-flash".
func (r Route) Slug() string {
	return strings.ReplaceAll(r.ModelID, "/", "--")
}
