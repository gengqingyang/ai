package config

import (
	"bufio"
	"os"
	"strings"
)

// defaultEnvFile 是默认的配置文件名，可用 ENV_FILE 环境变量覆盖。
const defaultEnvFile = ".env"

// loadDotEnv 把 .env 文件里的键值对灌进进程环境。
//
// 已经存在于真实环境变量里的键不会被覆盖 —— 真实环境优先，方便临时用
// `LLM_MODEL=xxx go run ./cmd/chat` 覆盖单个配置。
// 文件不存在不算错误：CI 或容器里通常直接注入环境变量，没有 .env。
func loadDotEnv() error {
	path := os.Getenv("ENV_FILE")
	if path == "" {
		path = defaultEnvFile
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := parseEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// parseEnvLine 解析一行 KEY=VALUE，返回是否解析成功。
// 支持 `export KEY=VALUE`、行首缩进、`#` 注释行，以及用单/双引号包裹的值。
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")

	key, value, ok = strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false
	}

	value = strings.TrimSpace(value)
	// 带引号时保留引号内原文（含 # 和空格）；不带引号时去掉行尾注释。
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return key, value[1 : len(value)-1], true
	}
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return key, value, true
}
