package app

import "strings"

type ModelInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Credits         string `json:"credits,omitempty"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	SupportsImages  bool   `json:"supports_images,omitempty"`
}

type ModelSettings struct {
	Models         []string           `json:"models"`
	DefaultModel   string             `json:"default_model"`
	PoolStrategy   string             `json:"pool_strategy"`
	ModelCatalog   []ModelInfo        `json:"model_catalog,omitempty"`
	PoolStrategies []PoolStrategyInfo `json:"pool_strategies,omitempty"`
}

type PoolStrategyInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

const (
	PoolStrategyRoundRobin = "round-robin"
	PoolStrategyFillFirst  = "fill-first"
)

var DefaultCodeBuddyModelIDs = []string{
	"glm-5.1",
	"minimax-m2.7",
	"kimi-k2.6",
}

var CodeBuddyModelCatalog = []ModelInfo{
	{ID: "glm-5.1", Name: "GLM-5.1", Credits: "x1.06 credits", MaxInputTokens: 200000, MaxOutputTokens: 48000},
	{ID: "minimax-m2.7", Name: "MiniMax-M2.7", Credits: "x0.26 credits", MaxInputTokens: 200000, MaxOutputTokens: 48000, SupportsImages: true},
	{ID: "kimi-k2.6", Name: "Kimi-K2.6", Credits: "x0.59 credits", MaxInputTokens: 256000, MaxOutputTokens: 32000, SupportsImages: true},
	{ID: "deepseek-v3-2-volc", Name: "DeepSeek-V3.2", Credits: "x0.29 credits", MaxInputTokens: 96000, MaxOutputTokens: 32000},
	{ID: "minimax-m2.5", Name: "MiniMax-M2.5", Credits: "x0.18 credits", MaxInputTokens: 200000, MaxOutputTokens: 48000},
	{ID: "glm-5.0", Name: "GLM-5.0", Credits: "x0.80 credits", MaxInputTokens: 200000, MaxOutputTokens: 48000},
	{ID: "glm-5.0-turbo", Name: "GLM-5.0-Turbo", Credits: "x0.95 credits", MaxInputTokens: 200000, MaxOutputTokens: 48000},
	{ID: "glm-5v-turbo", Name: "GLM-5v-Turbo", Credits: "x0.95 credits", MaxInputTokens: 200000, MaxOutputTokens: 38000, SupportsImages: true},
	{ID: "glm-4.7", Name: "GLM-4.7", Credits: "x0.21 credits", MaxInputTokens: 200000, MaxOutputTokens: 48000},
	{ID: "glm-4.6", Name: "GLM-4.6", Credits: "x0.23 credits", MaxInputTokens: 168000, MaxOutputTokens: 32000},
	{ID: "glm-4.6v", Name: "GLM-4.6V", Credits: "x0.11 credits", MaxInputTokens: 128000, MaxOutputTokens: 32000, SupportsImages: true},
	{ID: "kimi-k2.5", Name: "Kimi-K2.5", Credits: "x0.45 credits", MaxInputTokens: 164000, MaxOutputTokens: 32000, SupportsImages: true},
	{ID: "kimi-k2-thinking", Name: "Kimi-K2-Thinking", Credits: "x0.54 credits", MaxInputTokens: 164000, MaxOutputTokens: 32000},
	{ID: "hunyuan-2.0-thinking", Name: "Hunyuan-2.0-Thinking", Credits: "x0.04 credits", MaxInputTokens: 128000, MaxOutputTokens: 24000},
	{ID: "hy3-preview", Name: "Hy3 preview", Credits: "x0.00 credits", MaxInputTokens: 192000, MaxOutputTokens: 64000},
	{ID: "hunyuan-chat", Name: "Hunyuan-Turbos", Credits: "x0.10 credits", MaxInputTokens: 200000, MaxOutputTokens: 8192},
}

var CodeBuddyPoolStrategies = []PoolStrategyInfo{
	{ID: PoolStrategyRoundRobin, Name: "轮询", Description: "按优先级分层；同优先级内按权重轮询，适合均摊账号消耗。"},
	{ID: PoolStrategyFillFirst, Name: "填充", Description: "按优先级、权重、ID 顺序填满前面的账号；适合先吃完一个账号额度再切下一个。"},
}

func ModelSeed(envModels []string) []string {
	return normalizeModelIDs(append(append([]string{}, envModels...), DefaultCodeBuddyModelIDs...))
}

func NormalizeModelSettings(settings ModelSettings, fallback []string, fallbackPoolStrategy string) (ModelSettings, error) {
	models := normalizeModelIDs(settings.Models)
	if len(models) == 0 {
		models = ModelSeed(fallback)
	}
	if len(models) == 0 {
		models = append([]string{}, DefaultCodeBuddyModelIDs...)
	}
	for _, model := range models {
		if !validModelID(model) {
			return ModelSettings{}, &ValidationError{Message: "invalid model id: " + model}
		}
	}
	defaultModel := strings.TrimSpace(settings.DefaultModel)
	if defaultModel == "" || !containsString(models, defaultModel) {
		defaultModel = models[0]
	}
	poolStrategy := NormalizePoolStrategy(settings.PoolStrategy, fallbackPoolStrategy)
	return ModelSettings{
		Models:         models,
		DefaultModel:   defaultModel,
		PoolStrategy:   poolStrategy,
		ModelCatalog:   CodeBuddyModelCatalog,
		PoolStrategies: CodeBuddyPoolStrategies,
	}, nil
}

func NormalizePoolStrategy(value string, fallback string) string {
	normalizedFallback := normalizePoolStrategyID(fallback)
	switch normalizedFallback {
	case PoolStrategyRoundRobin, PoolStrategyFillFirst:
	default:
		normalizedFallback = PoolStrategyRoundRobin
	}
	switch normalizePoolStrategyID(value) {
	case PoolStrategyRoundRobin:
		return PoolStrategyRoundRobin
	case PoolStrategyFillFirst:
		return PoolStrategyFillFirst
	default:
		return normalizedFallback
	}
}

func normalizePoolStrategyID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.ReplaceAll(value, " ", "-")
	switch value {
	case PoolStrategyRoundRobin, "roundrobin", "rr", "轮询":
		return PoolStrategyRoundRobin
	case PoolStrategyFillFirst, "fillfirst", "fill", "first", "填充":
		return PoolStrategyFillFirst
	default:
		return value
	}
}

func normalizeModelIDs(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		for _, item := range strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == '\n' || r == '\r' || r == '\t'
		}) {
			model := strings.TrimSpace(item)
			if model == "" {
				continue
			}
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			result = append(result, model)
		}
	}
	return result
}

func validModelID(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}
