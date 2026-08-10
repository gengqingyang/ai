package test

import (
	"testing"
	"time"

	. "diagnostic-system/internal/config"
)

func TestEnvDuration(t *testing.T) {
	const key = "TEST_TOOL_TIMEOUT"
	def := 60 * time.Second

	tests := []struct {
		name string
		set  string
		want time.Duration
	}{
		{"未设置走默认值", "", def},
		{"带单位", "90s", 90 * time.Second},
		{"分钟", "2m", 2 * time.Minute},
		// 只写数字按秒算：TOOL_TIMEOUT=60 是很自然的写法，不该解析失败。
		{"裸数字按秒", "45", 45 * time.Second},
		// 写错了退回默认值，而不是把超时保护关掉。
		{"写错了", "很久", def},
		{"零", "0", def},
		{"负数", "-5s", def},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.set)
			if got := EnvDuration(key, def); got != tc.want {
				t.Errorf("envDuration(%q) = %s, want %s", tc.set, got, tc.want)
			}
		})
	}
}

// 超时是保护措施，配成 0 就等于没有——校验必须拦住。
func TestValidateRejectsZeroToolTimeout(t *testing.T) {
	c := validTestConfig()
	c.ToolTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatal("ToolTimeout=0 应该报错")
	}

	c.ToolTimeout = time.Second
	if err := c.Validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestValidateProvider(t *testing.T) {
	c := validTestConfig()
	c.Provider = "unsupported"
	if err := c.Validate(); err == nil {
		t.Fatal("不支持的 LLM_PROVIDER 应该报错")
	}

	for _, provider := range []string{"openai", "claude"} {
		c.Provider = provider
		if err := c.Validate(); err != nil {
			t.Errorf("validate() provider=%q error = %v", provider, err)
		}
	}
}

func TestValidateContextBudget(t *testing.T) {
	c := validTestConfig()
	c.ContextTokens = 1_000_000
	c.HistoryTokens = 900_000
	c.MaxTokens = 32_000
	if err := c.Validate(); err != nil {
		t.Fatalf("1M 上下文配置应该有效: %v", err)
	}

	c.HistoryTokens = c.ContextTokens
	if err := c.Validate(); err == nil {
		t.Fatal("历史预算占满上下文时应该报错")
	}
}

func TestValidateImageDetail(t *testing.T) {
	for _, detail := range []string{"auto", "low", "high"} {
		c := validTestConfig()
		c.ImageDetail = detail
		if err := c.Validate(); err != nil {
			t.Errorf("detail=%q 应该有效: %v", detail, err)
		}
	}

	c := validTestConfig()
	c.ImageDetail = "ultra"
	if err := c.Validate(); err == nil {
		t.Fatal("未知图片 detail 应该报错")
	}
}

func TestValidateSkillsDir(t *testing.T) {
	c := validTestConfig()
	c.SkillsDir = ""
	if err := c.Validate(); err == nil {
		t.Fatal("空 AGENT_SKILLS_DIR 应该报错")
	}
}

func validTestConfig() *Config {
	return &Config{
		Provider:      "openai",
		AuthToken:     "t",
		MaxTokens:     1,
		ContextTokens: 100,
		Temperature:   -1,
		MaxStep:       1,
		SkillsDir:     "skills",
		ToolTimeout:   time.Second,
		HistoryTokens: 90,
		ImageMaxBytes: 1,
		ImageDetail:   "auto",
	}
}
