package openai

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func applyUsagePostProcessing(info *relaycommon.RelayInfo, usage *dto.Usage, responseBody []byte) {
	if info == nil || usage == nil {
		return
	}

	switch info.ChannelType {
	case constant.ChannelTypeDeepSeek:
		if usage.PromptTokensDetails.CachedTokens == 0 && usage.PromptCacheHitTokens != 0 {
			usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
		}
		// 兼容将阿里百炼渠道误配置为 DeepSeek 类型的情况：
		// 阿里在 prompt_tokens_details 中返回 cache_creation_input_tokens 与
		// cache_creation.ephemeral_5m_input_tokens，需要回填到统一计费字段。
		applyAliCacheCreationTokens(usage)
	case constant.ChannelTypeAli:
		// 阿里百炼 OpenAI 兼容协议返回非标准字段：
		//   - prompt_tokens_details.cache_creation_input_tokens：缓存写入 token 总数
		//   - prompt_tokens_details.cache_creation.ephemeral_5m_input_tokens：5 分钟 TTL 缓存写入
		// 标准协议字段 cached_creation_tokens 阿里不会返回，需要在此回填以触发缓存写入计费。
		applyAliCacheCreationTokens(usage)
	case constant.ChannelTypeZhipu_v4:
		// 智普的cached_tokens在标准位置: usage.prompt_tokens_details.cached_tokens
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.InputTokensDetails.CachedTokens
			} else if cachedTokens, ok := extractCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if usage.PromptCacheHitTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
			}
		}
	case constant.ChannelTypeMoonshot:
		// Moonshot的cached_tokens在非标准位置: choices[].usage.cached_tokens
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.InputTokensDetails.CachedTokens
			} else if cachedTokens, ok := extractMoonshotCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if cachedTokens, ok := extractCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if usage.PromptCacheHitTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
			}
		}
	case constant.ChannelTypeOpenAI:
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if cachedTokens, ok := extractLlamaCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			}
		}
	}
}

func extractCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Usage struct {
			PromptTokensDetails struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CachedTokens         *int `json:"cached_tokens"`
			PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	if payload.Usage.PromptTokensDetails.CachedTokens != nil {
		return *payload.Usage.PromptTokensDetails.CachedTokens, true
	}
	if payload.Usage.CachedTokens != nil {
		return *payload.Usage.CachedTokens, true
	}
	if payload.Usage.PromptCacheHitTokens != nil {
		return *payload.Usage.PromptCacheHitTokens, true
	}
	return 0, false
}

// extractMoonshotCachedTokensFromBody 从Moonshot的非标准位置提取cached_tokens
// Moonshot的流式响应格式: {"choices":[{"usage":{"cached_tokens":111}}]}
func extractMoonshotCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Choices []struct {
			Usage struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"usage"`
		} `json:"choices"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	// 遍历choices查找cached_tokens
	for _, choice := range payload.Choices {
		if choice.Usage.CachedTokens != nil && *choice.Usage.CachedTokens > 0 {
			return *choice.Usage.CachedTokens, true
		}
	}

	return 0, false
}

// extractLlamaCachedTokensFromBody 从llama.cpp的非标准位置提取cache_n
func extractLlamaCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Timings struct {
			CachedTokens *int `json:"cache_n"`
		} `json:"timings"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	if payload.Timings.CachedTokens == nil {
		return 0, false
	}
	return *payload.Timings.CachedTokens, true
}

// applyAliCacheCreationTokens 将阿里百炼返回的非标准缓存写入字段回填到统一计费字段。
//
// 阿里百炼 OpenAI 兼容协议在 prompt_tokens_details 中返回：
//   - cache_creation_input_tokens: 缓存写入 token 总数（顶层字段）
//   - cache_creation.ephemeral_5m_input_tokens: 5 分钟 TTL 缓存写入 token 数（嵌套对象）
//
// new-api 计费链路使用 cached_creation_tokens 与 claude_cache_creation_5_m_tokens 作为
// 统一字段。本函数把阿里非标准字段回填到统一字段，确保下游 text_quota.go 的缓存写入
// 计费逻辑能够正确触发，避免平台少收缓存写入费用。
//
// 回填规则：
//   1. 顶层 cache_creation_input_tokens -> CachedCreationTokens（仅在标准字段为 0 时回填，避免覆盖 OpenAI 标准协议返回值）
//   2. 嵌套 ephemeral_5m_input_tokens -> ClaudeCacheCreation5mTokens（仅在为 0 时回填）
func applyAliCacheCreationTokens(usage *dto.Usage) {
	if usage == nil {
		return
	}

	// 回填缓存写入总数（对应 CreateCacheRatio 计费）
	// 仅在标准字段 cached_creation_tokens 为 0 时回填，避免覆盖 OpenAI 标准协议返回值
	if usage.PromptTokensDetails.CachedCreationTokens == 0 &&
		usage.PromptTokensDetails.CacheCreationInputTokens > 0 {
		usage.PromptTokensDetails.CachedCreationTokens = usage.PromptTokensDetails.CacheCreationInputTokens
	}

	// 回填 5 分钟 TTL 缓存写入（对应 ClaudeCacheCreation5mTokens 计费）
	if usage.ClaudeCacheCreation5mTokens == 0 &&
		usage.PromptTokensDetails.CacheCreation != nil &&
		usage.PromptTokensDetails.CacheCreation.Ephemeral5mInputTokens > 0 {
		usage.ClaudeCacheCreation5mTokens = usage.PromptTokensDetails.CacheCreation.Ephemeral5mInputTokens
	}
}
