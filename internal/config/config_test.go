package config

import (
	"testing"
	"time"
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
			if got := envDuration(key, def); got != tc.want {
				t.Errorf("envDuration(%q) = %s, want %s", tc.set, got, tc.want)
			}
		})
	}
}

// 超时是保护措施，配成 0 就等于没有——校验必须拦住。
func TestValidateRejectsZeroToolTimeout(t *testing.T) {
	c := &Config{Provider: "openai", AuthToken: "t", MaxTokens: 1, MaxStep: 1, Temperature: -1}
	if err := c.validate(); err == nil {
		t.Fatal("ToolTimeout=0 应该报错")
	}

	c.ToolTimeout = time.Second
	if err := c.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestValidateProvider(t *testing.T) {
	c := &Config{
		Provider:    "unsupported",
		AuthToken:   "t",
		MaxTokens:   1,
		MaxStep:     1,
		Temperature: -1,
		ToolTimeout: time.Second,
	}
	if err := c.validate(); err == nil {
		t.Fatal("不支持的 LLM_PROVIDER 应该报错")
	}

	for _, provider := range []string{"openai", "claude"} {
		c.Provider = provider
		if err := c.validate(); err != nil {
			t.Errorf("validate() provider=%q error = %v", provider, err)
		}
	}
}
