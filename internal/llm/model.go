// Package llm 负责构造 ChatModel 实例。
//
// 这里刻意只暴露一个工厂函数，后续要换模型供应商（Ark / OpenAI 兼容端点等）
// 只需在这里加分支，上层 agent 代码不用动。
package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"diagnostic-system/internal/config"
)

// NewChatModel 按配置创建一个支持 tool calling 的 ChatModel。
func NewChatModel(ctx context.Context, cfg *config.Config) (model.ToolCallingChatModel, error) {
	switch cfg.Provider {
	case "openai":
		return newOpenAIChatModel(ctx, cfg)
	case "claude":
		return newClaudeChatModel(ctx, cfg)
	default:
		return nil, fmt.Errorf("不支持的模型协议 %q", cfg.Provider)
	}
}

func newOpenAIChatModel(ctx context.Context, cfg *config.Config) (model.ToolCallingChatModel, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		// OpenAI 协议只有一个 Bearer token 字段；沿用已有 AuthToken 配置即可。
		apiKey = cfg.AuthToken
	}

	maxTokens := cfg.MaxTokens
	c := &openai.ChatModelConfig{
		APIKey:              apiKey,
		BaseURL:             openAIBaseURL(cfg.BaseURL),
		Model:               cfg.Model,
		MaxCompletionTokens: &maxTokens,
		HTTPClient:          newOpenAIHTTPClient(),
	}
	if cfg.Temperature >= 0 {
		t := cfg.Temperature
		c.Temperature = &t
	}

	cm, err := openai.NewChatModel(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("创建 openai chat model 失败: %w", err)
	}
	return cm, nil
}

// openAIBaseURL 把网关根地址规范化为 OpenAI 客户端需要的 API 根地址。
// 客户端会自行追加 /chat/completions，因此 https://host 应变成 https://host/v1。
func openAIBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" || strings.HasSuffix(baseURL, "/v1") {
		return baseURL
	}
	return baseURL + "/v1"
}

func newClaudeChatModel(ctx context.Context, cfg *config.Config) (model.ToolCallingChatModel, error) {
	c := &claude.Config{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		APIKey:    cfg.APIKey,
		AuthToken: cfg.AuthToken,
	}
	if cfg.BaseURL != "" {
		c.BaseURL = &cfg.BaseURL
	}
	if cfg.Temperature >= 0 {
		t := cfg.Temperature
		c.Temperature = &t
	}

	cm, err := claude.NewChatModel(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("创建 claude chat model 失败: %w", err)
	}
	return cm, nil
}
