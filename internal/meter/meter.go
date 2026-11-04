package meter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strings"
	"unicode/utf8"

	"lebrel-router/internal/model"
)

type RequestInfo struct {
	Model                 string
	Stream                bool
	EstimatedPromptTokens int64
	MaxOutputTokens       int64
}

func (r RequestInfo) EstimatedTotalTokens() int64 {
	if r.EstimatedPromptTokens < 0 || r.MaxOutputTokens < 0 ||
		r.EstimatedPromptTokens > math.MaxInt64-r.MaxOutputTokens {
		return math.MaxInt64
	}
	return r.EstimatedPromptTokens + r.MaxOutputTokens
}

// Pricing stores rates as integer micro-USD per TokenRateDenominator tokens.
// Charges aggregate every applicable token class and round up once per request.
type Pricing struct {
	InputMicrousdPerMillionTokens           int64
	CachedInputMicrousdPerMillionTokens     int64
	LongInputMicrousdPerMillionTokens       int64
	LongCachedInputMicrousdPerMillionTokens int64
	OutputMicrousdPerMillionTokens          int64
	LongOutputMicrousdPerMillionTokens      int64
	ReservationBPS                          int64
}

const TokenRateDenominator int64 = 1_000_000
const deepSeekV41ModelID = "lebrel/deepseek-v4.1-flash-uncensored"

func (p Pricing) Validate() error {
	for _, rate := range []int64{
		p.InputMicrousdPerMillionTokens,
		p.CachedInputMicrousdPerMillionTokens,
		p.LongInputMicrousdPerMillionTokens,
		p.LongCachedInputMicrousdPerMillionTokens,
		p.OutputMicrousdPerMillionTokens,
		p.LongOutputMicrousdPerMillionTokens,
	} {
		if rate < 0 {
			return errors.New("pricing rates cannot be negative")
		}
	}
	if p.ReservationBPS < 10_000 {
		return errors.New("reservation safety BPS must be at least 10000")
	}
	return nil
}

func InspectAndRequireUsage(completionPath string, body []byte, defaultMaxOutput, maxOutputTokens int64) ([]byte, RequestInfo, error) {
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return nil, RequestInfo{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, RequestInfo{}, fmt.Errorf("invalid JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, RequestInfo{}, errors.New("request contains trailing JSON")
	}

	stream, _ := payload["stream"].(bool)
	modelID, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(modelID) == "" || len(modelID) > 256 || strings.ContainsAny(modelID, "\r\n") {
		return nil, RequestInfo{}, errors.New("model must be a non-empty string")
	}
	if modelID == deepSeekV41ModelID {
		if completionPath != "/v1/chat/completions" {
			return nil, RequestInfo{}, errors.New("DeepSeek V4.1 supports only /v1/chat/completions")
		}
		if err := normalizeDeepSeekV41Routing(payload); err != nil {
			return nil, RequestInfo{}, err
		}
	}
	for _, field := range []string{"n", "best_of"} {
		if value, present := payload[field]; present {
			parsed, err := jsonNumberToInt64(value)
			if err != nil || parsed != 1 {
				return nil, RequestInfo{}, fmt.Errorf("%s must be 1", field)
			}
		}
	}
	if prompt, present := payload["prompt"]; present {
		if _, batched := prompt.([]any); batched {
			return nil, RequestInfo{}, errors.New("batched completion prompts are not supported")
		}
	}
	maxOutput, err := effectiveMaxOutputTokens(completionPath, payload, defaultMaxOutput, maxOutputTokens)
	if err != nil {
		return nil, RequestInfo{}, err
	}
	if stream {
		options, _ := payload["stream_options"].(map[string]any)
		if options == nil {
			options = make(map[string]any)
		}
		options["include_usage"] = true
		payload["stream_options"] = options
	}

	estimate := estimateValueTokens(payload) + 8
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, RequestInfo{}, fmt.Errorf("encode request: %w", err)
	}
	return encoded, RequestInfo{
		Model:                 modelID,
		Stream:                stream,
		EstimatedPromptTokens: estimate,
		MaxOutputTokens:       maxOutput,
	}, nil
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("invalid JSON object key: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid JSON object key")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("request contains duplicate JSON key: %s", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid JSON object termination")
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid JSON array termination")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("request contains trailing JSON")
	}
	return nil
}

func normalizeDeepSeekV41Routing(payload map[string]any) error {
	signals := make([]int, 0, 4)
	publicSignals := 0
	if value, present := payload["profile"]; present {
		profile, ok := value.(string)
		if !ok || (profile != "chat" && profile != "thinking_50") {
			return errors.New("profile must be exactly chat or thinking_50")
		}
		if profile == "thinking_50" {
			signals = append(signals, 1)
		} else {
			signals = append(signals, 0)
		}
		publicSignals++
	}
	if value, present := payload["thinking"]; present {
		thinking, ok := value.(bool)
		if !ok {
			return errors.New("thinking must be boolean")
		}
		if thinking {
			signals = append(signals, 1)
		} else {
			signals = append(signals, 0)
		}
		publicSignals++
	}
	template := map[string]any{}
	if value, present := payload["chat_template_kwargs"]; present {
		var ok bool
		template, ok = value.(map[string]any)
		if !ok || len(template) != 1 {
			return errors.New("chat_template_kwargs may contain only thinking")
		}
		thinking, ok := template["thinking"].(bool)
		if !ok {
			return errors.New("chat_template_kwargs.thinking must be boolean")
		}
		if thinking {
			signals = append(signals, 1)
		} else {
			signals = append(signals, 0)
		}
	}
	custom := map[string]any{}
	if value, present := payload["custom_params"]; present {
		var ok bool
		custom, ok = value.(map[string]any)
		if !ok || len(custom) != 1 {
			return errors.New("custom_params may contain only lebrel_profile_id")
		}
		profileID, ok := custom["lebrel_profile_id"].(json.Number)
		if !ok || (profileID.String() != "0" && profileID.String() != "1") {
			return errors.New("custom_params.lebrel_profile_id must be integer 0 or 1")
		}
		if profileID.String() == "1" {
			signals = append(signals, 1)
		} else {
			signals = append(signals, 0)
		}
	}
	if publicSignals == 0 {
		return errors.New("an explicit public thinking/profile selector is required")
	}
	selected := signals[0]
	for _, signal := range signals[1:] {
		if signal != selected {
			return errors.New("request routing selectors conflict")
		}
	}
	template["thinking"] = selected == 1
	custom["lebrel_profile_id"] = selected
	payload["chat_template_kwargs"] = template
	payload["custom_params"] = custom
	if selected == 0 {
		if _, present := payload["reasoning_effort"]; present {
			return errors.New("reasoning_effort is incompatible with chat profile")
		}
	} else {
		if value, present := payload["reasoning_effort"]; present && value != "high" {
			return errors.New("reasoning_effort must be exactly high for thinking_50")
		}
		payload["reasoning_effort"] = "high"
	}
	return nil
}

func effectiveMaxOutputTokens(completionPath string, payload map[string]any, fallback, maximum int64) (int64, error) {
	if maximum < 1 || fallback < 1 || fallback > maximum {
		return 0, errors.New("output token limits are invalid")
	}
	maxTokens, hasMaxTokens := payload["max_tokens"]
	maxCompletionTokens, hasMaxCompletionTokens := payload["max_completion_tokens"]
	if hasMaxTokens && hasMaxCompletionTokens {
		return 0, errors.New("max_tokens and max_completion_tokens cannot both be set")
	}

	field := ""
	var value any
	switch completionPath {
	case "/v1/chat/completions":
		if hasMaxCompletionTokens {
			field, value = "max_completion_tokens", maxCompletionTokens
		} else if hasMaxTokens {
			field, value = "max_tokens", maxTokens
		}
	case "/v1/completions":
		if hasMaxCompletionTokens {
			return 0, errors.New("max_completion_tokens is not supported on /v1/completions")
		}
		if hasMaxTokens {
			field, value = "max_tokens", maxTokens
		}
	default:
		return 0, errors.New("unsupported completion path")
	}
	if field == "" {
		// Make the reserved fallback explicit so the serving engine cannot apply a
		// different endpoint default after admission.
		payload["max_tokens"] = fallback
		return fallback, nil
	}
	parsed, err := jsonNumberToInt64(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", field)
	}
	if parsed > maximum {
		return 0, fmt.Errorf("%s exceeds maximum output token limit of %d", field, maximum)
	}
	return parsed, nil
}

func QueueFor(info RequestInfo, longThreshold int64) model.QueueClass {
	if info.EstimatedTotalTokens() > longThreshold {
		return model.QueueLong
	}
	return model.QueueShort
}

func (p Pricing) Reservation(info RequestInfo, queue model.QueueClass) (int64, error) {
	base, err := p.Maximum(info, queue)
	if err != nil {
		return 0, err
	}
	return applyBPS(base, p.ReservationBPS)
}

// Maximum is the largest billable charge for the requested token envelope.
// Reservation adds a temporary safety margin above this amount; that margin is
// always refunded and is never itself a settlement price.
func (p Pricing) Maximum(info RequestInfo, queue model.QueueClass) (int64, error) {
	inputRate := p.InputMicrousdPerMillionTokens
	outputRate := p.OutputMicrousdPerMillionTokens
	if queue == model.QueueLong {
		inputRate = p.LongInputMicrousdPerMillionTokens
		outputRate = p.LongOutputMicrousdPerMillionTokens
	}
	return sumProductsRoundedUp(TokenRateDenominator,
		product{info.EstimatedPromptTokens, inputRate},
		product{info.MaxOutputTokens, outputRate},
	)
}

func (p Pricing) Actual(usage model.Usage, queue model.QueueClass) (int64, error) {
	cached := usage.CachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > usage.PromptTokens {
		cached = usage.PromptTokens
	}
	uncached := usage.PromptTokens - cached
	inputRate := p.InputMicrousdPerMillionTokens
	cachedInputRate := p.CachedInputMicrousdPerMillionTokens
	outputRate := p.OutputMicrousdPerMillionTokens
	if queue == model.QueueLong {
		inputRate = p.LongInputMicrousdPerMillionTokens
		cachedInputRate = p.LongCachedInputMicrousdPerMillionTokens
		outputRate = p.LongOutputMicrousdPerMillionTokens
	}
	return sumProductsRoundedUp(TokenRateDenominator,
		product{uncached, inputRate},
		product{cached, cachedInputRate},
		product{usage.CompletionTokens, outputRate},
	)
}

// MaximumScaled, ReservationScaled and ActualScaled apply a percent multiplier to every rate
// (100 = the published rates), as the control plane does for a peak-hour schedule: rates are
// scaled, the denominator becomes TokenRateDenominator*100, and the sum rounds up once.
func (p Pricing) MaximumScaled(info RequestInfo, queue model.QueueClass, percent int64) (int64, error) {
	if percent < 1 || percent > 1_000_000 {
		return 0, errors.New("multiplier percent out of range")
	}
	inputRate := p.InputMicrousdPerMillionTokens
	outputRate := p.OutputMicrousdPerMillionTokens
	if queue == model.QueueLong {
		inputRate = p.LongInputMicrousdPerMillionTokens
		outputRate = p.LongOutputMicrousdPerMillionTokens
	}
	return sumProductsRoundedUp(TokenRateDenominator*100,
		product{info.EstimatedPromptTokens, inputRate * percent},
		product{info.MaxOutputTokens, outputRate * percent},
	)
}

func (p Pricing) ReservationScaled(info RequestInfo, queue model.QueueClass, percent int64) (int64, error) {
	base, err := p.MaximumScaled(info, queue, percent)
	if err != nil {
		return 0, err
	}
	return applyBPS(base, p.ReservationBPS)
}

func (p Pricing) ActualScaled(usage model.Usage, queue model.QueueClass, percent int64) (int64, error) {
	if percent < 1 || percent > 1_000_000 {
		return 0, errors.New("multiplier percent out of range")
	}
	cached := usage.CachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > usage.PromptTokens {
		cached = usage.PromptTokens
	}
	uncached := usage.PromptTokens - cached
	inputRate := p.InputMicrousdPerMillionTokens
	cachedInputRate := p.CachedInputMicrousdPerMillionTokens
	outputRate := p.OutputMicrousdPerMillionTokens
	if queue == model.QueueLong {
		inputRate = p.LongInputMicrousdPerMillionTokens
		cachedInputRate = p.LongCachedInputMicrousdPerMillionTokens
		outputRate = p.LongOutputMicrousdPerMillionTokens
	}
	return sumProductsRoundedUp(TokenRateDenominator*100,
		product{uncached, inputRate * percent},
		product{cached, cachedInputRate * percent},
		product{usage.CompletionTokens, outputRate * percent},
	)
}

func UsageFromJSON(data []byte) (model.Usage, bool) {
	var envelope struct {
		Usage *struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			TotalTokens      *int64 `json:"total_tokens"`
			PromptDetails    *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Usage == nil ||
		envelope.Usage.PromptTokens == nil || envelope.Usage.CompletionTokens == nil ||
		envelope.Usage.TotalTokens == nil {
		return model.Usage{}, false
	}
	usage := model.Usage{
		PromptTokens:     *envelope.Usage.PromptTokens,
		CompletionTokens: *envelope.Usage.CompletionTokens,
		TotalTokens:      *envelope.Usage.TotalTokens,
	}
	if envelope.Usage.PromptDetails != nil {
		usage.CachedTokens = envelope.Usage.PromptDetails.CachedTokens
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return model.Usage{}, false
	}
	if usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		return model.Usage{}, false
	}
	if usage.CachedTokens < 0 || usage.CachedTokens > usage.PromptTokens {
		return model.Usage{}, false
	}
	return usage, true
}

func estimateValueTokens(value any) int64 {
	switch typed := value.(type) {
	case string:
		runes := int64(utf8.RuneCountInString(typed))
		if runes == 0 {
			return 0
		}
		return (runes + 3) / 4
	case []any:
		var total int64
		for _, item := range typed {
			total += estimateValueTokens(item) + 1
		}
		return total
	case map[string]any:
		var total int64
		for key, item := range typed {
			if key == "max_tokens" || key == "max_completion_tokens" || key == "stream_options" {
				continue
			}
			total += estimateValueTokens(item)
		}
		return total
	default:
		return 0
	}
}

func jsonNumberToInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case json.Number:
		return typed.Int64()
	case float64:
		if typed != math.Trunc(typed) || typed > math.MaxInt64 || typed < math.MinInt64 {
			return 0, errors.New("not an integer")
		}
		return int64(typed), nil
	default:
		return 0, errors.New("not a number")
	}
}

func applyBPS(value, bps int64) (int64, error) {
	return sumProductsRoundedUp(10_000, product{value, bps})
}

type product struct {
	left  int64
	right int64
}

// sumProductsRoundedUp calculates ceil(sum(left*right)/denominator) without
// floating point. The 128-bit accumulator avoids rejecting valid charges merely
// because an intermediate int64 multiplication overflows. Its callers use at
// most three signed-int64 products, whose non-negative sum fits in uint128.
func sumProductsRoundedUp(denominator int64, products ...product) (int64, error) {
	if denominator < 1 {
		return 0, errors.New("metering denominator must be positive")
	}
	var high, low uint64
	for _, item := range products {
		if item.left < 0 || item.right < 0 {
			return 0, errors.New("negative metering value")
		}
		productHigh, productLow := bits.Mul64(uint64(item.left), uint64(item.right))
		var carry uint64
		low, carry = bits.Add64(low, productLow, 0)
		high, carry = bits.Add64(high, productHigh, carry)
		if carry != 0 {
			return 0, errors.New("metering overflow")
		}
	}

	unsignedDenominator := uint64(denominator)
	if high >= unsignedDenominator {
		return 0, errors.New("metering overflow")
	}
	quotient, remainder := bits.Div64(high, low, unsignedDenominator)
	if remainder != 0 {
		if quotient == math.MaxUint64 {
			return 0, errors.New("metering overflow")
		}
		quotient++
	}
	if quotient > math.MaxInt64 {
		return 0, errors.New("metering overflow")
	}
	return int64(quotient), nil
}
