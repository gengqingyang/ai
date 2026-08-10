// Package logging 配置全局结构化日志，并把 eino 的组件调用接进来。
//
// 分两条线，别混：
//   - 审计日志（internal/approval）只记变更提案的状态流转，是给「谁批准了什么」
//     这个问题留证据的，格式必须稳定，不能被日志级别过滤掉。
//   - 运行日志（本包）记模型调用、工具调用、耗时、报错，是给排障看的。
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"

	"diagnostic-system/internal/config"
)

// Setup 按配置初始化全局 logger，返回关闭函数。
//
// 日志写文件而不是 stderr：终端里正在跑对话，日志刷屏会把对话冲掉。
// 配置 LOG_FILE=stderr 可以强制打到终端，调试时用。
func Setup(cfg *config.Config) (func() error, error) {
	return SetupWithConsole(cfg, os.Stderr)
}

// SetupWithConsole 允许 TUI 把显式配置的终端日志接入自己的消息通道。
// console 为 nil 时丢弃终端日志，文件日志行为不变。
func SetupWithConsole(cfg *config.Config, console io.Writer) (func() error, error) {
	w, closeFn, err := openSink(cfg.LogFile, console)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}
	// 文件用 JSON（好 grep、好喂给日志系统），终端用文本（好读）。
	var h slog.Handler
	if isStderr(cfg.LogFile) {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))

	return closeFn, nil
}

// InstallEinoCallbacks 把 eino 的组件生命周期接进 slog。
//
// 必须在构造 agent 之前调用一次，AppendGlobalHandlers 本身不是并发安全的。
func InstallEinoCallbacks() {
	h := callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
			if info == nil {
				return ctx
			}
			slog.Debug("组件开始", "component", string(info.Component), "name", info.Name, "type", info.Type)
			return context.WithValue(ctx, startTimeKey{}, time.Now())
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, _ callbacks.CallbackOutput) context.Context {
			if info == nil {
				return ctx
			}
			args := []any{"component", string(info.Component), "name", info.Name}
			if start, ok := ctx.Value(startTimeKey{}).(time.Time); ok {
				args = append(args, "elapsed_ms", time.Since(start).Milliseconds())
			}
			// 工具调用是排障时最想看的一行，提到 info 级。
			if info.Component == components.ComponentOfTool {
				slog.Info("工具调用结束", args...)
			} else {
				slog.Debug("组件结束", args...)
			}
			return ctx
		}).
		OnErrorFn(func(ctx context.Context, info *callbacks.RunInfo, err error) context.Context {
			name, comp := "", ""
			if info != nil {
				name, comp = info.Name, string(info.Component)
			}
			slog.Error("组件报错", "component", comp, "name", name, "err", err)
			return ctx
		}).
		Build()

	callbacks.AppendGlobalHandlers(h)
}

type startTimeKey struct{}

// openSink 打开日志输出目标。空路径表示丢弃。
func openSink(path string, console io.Writer) (io.Writer, func() error, error) {
	noop := func() error { return nil }

	switch {
	case path == "":
		return io.Discard, noop, nil
	case isStderr(path):
		if console == nil {
			console = io.Discard
		}
		return console, noop, nil
	}

	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("创建日志目录 %s 失败: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("打开日志文件 %s 失败: %w", path, err)
	}
	return f, f.Close, nil
}

func isStderr(path string) bool {
	switch strings.ToLower(path) {
	case "stderr", "-", "console":
		return true
	}
	return false
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
