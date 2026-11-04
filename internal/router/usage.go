package router

import (
	"encoding/json"

	"lebrel-router/internal/model"
)

// ParseUsage reads a provider's usage block. Providers disagree on where cached
// tokens live: OpenAI-style APIs use prompt_tokens_details.cached_tokens, DeepSeek
// adds prompt_cache_hit_tokens. total_tokens is recomputed from prompt and
// completion so a provider that counts reasoning tokens separately still bills.
func ParseUsage(format string, data []byte) (model.Usage, bool) {
	var envelope struct {
		Usage *struct {
			PromptTokens         *int64 `json:"prompt_tokens"`
			CompletionTokens     *int64 `json:"completion_tokens"`
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
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 {
		return model.Usage{}, false
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
