package router

import (
	"encoding/json"

	"lebrel-router/internal/model"
)

// maxCountedTokens is more than any request can be.
const maxCountedTokens = 1 << 40

// ParseUsage reads a provider's usage block. Providers disagree on where cached
// tokens live: OpenAI-style APIs use prompt_tokens_details.cached_tokens, DeepSeek
// adds prompt_cache_hit_tokens. A host that counts what it wrote beyond completion_tokens
// (reasoning it reports apart) says so in total_tokens: everything past the prompt is
// output and is billed as output, and total_tokens is then the sum of the two.
func ParseUsage(format string, data []byte) (model.Usage, bool) {
	var envelope struct {
		Usage *struct {
			PromptTokens         *int64 `json:"prompt_tokens"`
			CompletionTokens     *int64 `json:"completion_tokens"`
			TotalTokens          *int64 `json:"total_tokens"`
			PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
			PromptDetails        *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Usage == nil ||
		envelope.Usage.PromptTokens == nil || envelope.Usage.CompletionTokens == nil {
		return model.Usage{}, false
	}
	usage := model.Usage{PromptTokens: *envelope.Usage.PromptTokens, CompletionTokens: *envelope.Usage.CompletionTokens}
	// A count that cannot be one is no count: every request has a prompt, so a prompt of zero tokens says the host
	// counted nothing, and no request is a trillion tokens. Such an answer settles as one whose usage is missing.
	if usage.PromptTokens <= 0 || usage.CompletionTokens < 0 || usage.PromptTokens > maxCountedTokens || usage.CompletionTokens > maxCountedTokens {
		return model.Usage{}, false
	}
	if total := envelope.Usage.TotalTokens; total != nil && *total > usage.PromptTokens && *total-usage.PromptTokens > usage.CompletionTokens {
		usage.CompletionTokens = *total - usage.PromptTokens
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if envelope.Usage.PromptDetails != nil && envelope.Usage.PromptDetails.CachedTokens != nil {
		usage.CachedTokens = *envelope.Usage.PromptDetails.CachedTokens
	} else if format == "deepseek" && envelope.Usage.PromptCacheHitTokens != nil {
		usage.CachedTokens = *envelope.Usage.PromptCacheHitTokens
	}
	if usage.CachedTokens < 0 {
		usage.CachedTokens = 0
	}
	if usage.CachedTokens > usage.PromptTokens {
		usage.CachedTokens = usage.PromptTokens
	}
	return usage, true
}

// widerUsage is what two usage blocks of one answer say together. A stream carries one block, at its end; a host
// that sends several counts upward, so the larger count of each kind stands and a later block never lowers an
// earlier one. The cached share is the one of the block that counted the most prompt, the later of two equals.
func widerUsage(earlier, later model.Usage) model.Usage {
	wider := later
	if earlier.PromptTokens > later.PromptTokens {
		wider.PromptTokens, wider.CachedTokens = earlier.PromptTokens, earlier.CachedTokens
	}
	if earlier.CompletionTokens > later.CompletionTokens {
		wider.CompletionTokens = earlier.CompletionTokens
	}
	wider.TotalTokens = wider.PromptTokens + wider.CompletionTokens
	return wider
}
