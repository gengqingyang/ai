// Package config 负责从环境变量加载运行配置。
//
// 应用专属的 LLM_* 变量优先，同时兼容 OPENAI_* 和 ANTHROPIC_* 变量。
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config 是整个应用的运行配置。
type Config struct {
	// Provider 是模型协议：openai 或 claude。
	Provider string
	// BaseURL 是模型服务地址，走代理/中转网关时必填。留空则用供应商官方地址。
	BaseURL string
	// APIKey 与 AuthToken 二选一即可。中转网关一般用 AuthToken。
	APIKey    string
	AuthToken string
	// Model 是模型 ID，例如 gpt-5.6-sol。
	Model string
	// MaxTokens 是单次回复的最大 token 数。
	MaxTokens int
	// Temperature 取值 [0,1]，负数表示不设置、用服务端默认。
	Temperature float32

	// MaxStep 限制 ReAct agent 的最大步数，防止工具调用死循环。
	MaxStep int
	// ToolTimeout 是单次工具执行的超时。只圈住「真正下发那一段」，
	// 等人审核的时间不算在内。
	ToolTimeout time.Duration
	// HistoryTurns 是对话历史保留的轮数（一轮 = 一问一答）。
	HistoryTurns int
	// AuditLog 是审计日志路径。变更提案的每次状态流转都会追加一行 JSON。
	// 留空表示不落盘（仅内存），生产环境不应留空。
	AuditLog string
	// Operator 是当前操作人标识，写进审计日志、也随命令上报给 tunnel 对端。
	Operator string
	// LogFile 是运行日志路径。写文件而不是终端，免得日志把对话刷掉。
	// 填 stderr 可强制打到终端（调试用），留空表示不记日志。
	LogFile string
	// LogLevel 是运行日志级别：debug / info / warn / error。
	LogLevel string
	// Debug 打开后会打印工具调用等内部信息。
	Debug bool
}

// Load 读取配置：先加载 .env（不覆盖已有的真实环境变量），再读环境变量、填默认值。
func Load() (*Config, error) {
	if err := loadDotEnv(); err != nil {
		return nil, fmt.Errorf("加载 .env 失败: %w", err)
	}

	cfg := &Config{
		Provider:     firstEnv("LLM_PROVIDER"),
		BaseURL:      firstEnv("LLM_BASE_URL", "OPENAI_BASE_URL", "ANTHROPIC_BASE_URL"),
		APIKey:       firstEnv("LLM_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"),
		AuthToken:    firstEnv("LLM_AUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"),
		Model:        firstEnv("LLM_MODEL", "OPENAI_MODEL", "ANTHROPIC_MODEL"),
		MaxTokens:    envInt("LLM_MAX_TOKENS", 4096),
		Temperature:  envFloat32("LLM_TEMPERATURE", -1),
		MaxStep:      envInt("AGENT_MAX_STEP", 12),
		ToolTimeout:  envDuration("TOOL_TIMEOUT", 60*time.Second),
		HistoryTurns: envInt("AGENT_HISTORY_TURNS", 10),
		AuditLog:     envOr("AUDIT_LOG", "audit.log"),
		Operator:     firstEnv("OPERATOR", "USER", "USERNAME"),
		LogFile:      envOr("LOG_FILE", "diagnostic.log"),
		LogLevel:     envOr("LOG_LEVEL", "info"),
		Debug:        envBool("DEBUG", false),
	}

	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-5.6-sol"
	}
	if cfg.Operator == "" {
		cfg.Operator = "unknown-operator"
	}
	// DEBUG=true 时把日志级别一并调到 debug，省得再配一个变量。
	if cfg.Debug {
		cfg.LogLevel = "debug"
	}
	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	switch c.Provider {
	case "openai", "claude":
	default:
		return fmt.Errorf("LLM_PROVIDER 仅支持 openai 或 claude，当前为 %q", c.Provider)
	}
	if c.APIKey == "" && c.AuthToken == "" {
		return errors.New("缺少凭证。请任选一种方式：\n" +
			"  1) 复制模板后填入: cp .env.example .env && vim .env\n" +
			"  2) 临时导出: export LLM_AUTH_TOKEN=<你的 token>\n" +
			"  变量名也可用 OPENAI_API_KEY / ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN")
	}
	if c.MaxTokens <= 0 {
		return fmt.Errorf("LLM_MAX_TOKENS 必须大于 0，当前为 %d", c.MaxTokens)
	}
	if c.Temperature >= 0 && c.Temperature > 1 {
		return fmt.Errorf("LLM_TEMPERATURE 需在 [0,1] 区间，当前为 %v", c.Temperature)
	}
	if c.MaxStep <= 0 {
		return fmt.Errorf("AGENT_MAX_STEP 必须大于 0，当前为 %d", c.MaxStep)
	}
	if c.ToolTimeout <= 0 {
		return fmt.Errorf("TOOL_TIMEOUT 必须大于 0，当前为 %s", c.ToolTimeout)
	}
	return nil
}

// Redacted 返回可安全打印的配置摘要。
func (c *Config) Redacted() string {
	base := c.BaseURL
	if base == "" {
		base = "(默认供应商官方端点)"
	}
	cred := "APIKey"
	if c.APIKey == "" {
		cred = "AuthToken"
	}
	return fmt.Sprintf("provider=%s model=%s endpoint=%s cred=%s maxTokens=%d maxStep=%d toolTimeout=%s operator=%s audit=%s log=%s(%s)",
		c.Provider, c.Model, base, cred, c.MaxTokens, c.MaxStep, c.ToolTimeout, c.Operator,
		pathDesc(c.AuditLog), pathDesc(c.LogFile), c.LogLevel)
}

func pathDesc(path string) string {
	if path == "" {
		return "(未开启)"
	}
	return path
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// envOr 读环境变量，未设置或为空时返回默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat32(key string, def float32) float32 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 32)
	if err != nil {
		return def
	}
	return float32(f)
}

// envDuration 读时长配置。
//
// 支持 "90s"、"2m" 这类写法；只写数字按秒算（TOOL_TIMEOUT=60 是很自然的手感）。
// 写错了退回默认值而不是报错——超时是保护措施，不该因为一个笔误让程序起不来。
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
