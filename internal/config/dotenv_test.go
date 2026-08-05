package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
	}{
		{name: "普通键值", line: "LLM_MODEL=claude-sonnet-5", wantKey: "LLM_MODEL", wantValue: "claude-sonnet-5", wantOK: true},
		{name: "带 export 前缀", line: "export LLM_MODEL=abc", wantKey: "LLM_MODEL", wantValue: "abc", wantOK: true},
		{name: "键值两侧空格", line: "  LLM_MODEL = abc  ", wantKey: "LLM_MODEL", wantValue: "abc", wantOK: true},
		{name: "行尾注释被去掉", line: "LLM_MAX_TOKENS=4096 # 上限", wantKey: "LLM_MAX_TOKENS", wantValue: "4096", wantOK: true},
		{name: "双引号内原文保留", line: `PROMPT="a # b"`, wantKey: "PROMPT", wantValue: "a # b", wantOK: true},
		{name: "单引号内原文保留", line: `PROMPT='a # b'`, wantKey: "PROMPT", wantValue: "a # b", wantOK: true},
		{name: "URL 不受影响", line: "LLM_BASE_URL=https://api.example.com/v1", wantKey: "LLM_BASE_URL", wantValue: "https://api.example.com/v1", wantOK: true},
		{name: "空值合法", line: "LLM_API_KEY=", wantKey: "LLM_API_KEY", wantValue: "", wantOK: true},
		{name: "注释行跳过", line: "# LLM_MODEL=abc", wantOK: false},
		{name: "空行跳过", line: "   ", wantOK: false},
		{name: "没有等号跳过", line: "LLM_MODEL", wantOK: false},
		{name: "空键跳过", line: "=abc", wantOK: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, value, ok := parseEnvLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, 期望 %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if key != c.wantKey {
				t.Errorf("key = %q, 期望 %q", key, c.wantKey)
			}
			if value != c.wantValue {
				t.Errorf("value = %q, 期望 %q", value, c.wantValue)
			}
		})
	}
}

func TestLoadDotEnv(t *testing.T) {
	const (
		overriddenKey = "DOTENV_TEST_OVERRIDDEN"
		freshKey      = "DOTENV_TEST_FRESH"
	)

	path := filepath.Join(t.TempDir(), ".env")
	content := overriddenKey + "=from-file\n" +
		"# 注释行\n" +
		freshKey + "=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ENV_FILE", path)
	// 真实环境变量已存在的键不该被文件覆盖。
	t.Setenv(overriddenKey, "from-real-env")
	// freshKey 先注册进 t.Setenv 以便测试结束自动清理，再删掉模拟"未设置"。
	t.Setenv(freshKey, "")
	if err := os.Unsetenv(freshKey); err != nil {
		t.Fatal(err)
	}

	if err := loadDotEnv(); err != nil {
		t.Fatalf("loadDotEnv 失败: %v", err)
	}

	if got := os.Getenv(overriddenKey); got != "from-real-env" {
		t.Errorf("%s = %q, 期望真实环境变量优先", overriddenKey, got)
	}
	if got := os.Getenv(freshKey); got != "from-file" {
		t.Errorf("%s = %q, 期望从文件读到 from-file", freshKey, got)
	}
}

func TestLoadDotEnvMissingFileIsOK(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := loadDotEnv(); err != nil {
		t.Errorf("文件不存在时应当无错误，实际: %v", err)
	}
}
