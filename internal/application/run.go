// Package application 负责组装配置、模型、工具、业务层和终端 UI。
package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/agent"
	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/chat"
	"diagnostic-system/internal/config"
	"diagnostic-system/internal/llm"
	"diagnostic-system/internal/logging"
	"diagnostic-system/internal/session"
	"diagnostic-system/internal/tools"
	"diagnostic-system/internal/ui"
)

// Run 初始化全部依赖并启动终端应用。
func Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	events := make(chan tea.Msg, 256)
	closeLog, err := logging.SetupWithConsole(cfg, ui.NewLogWriter(ctx, events))
	if err != nil {
		return err
	}
	defer closeLog()
	logging.InstallEinoCallbacks()
	slog.Info("启动", "config", cfg.Redacted())

	chatModel, err := llm.NewChatModel(ctx, cfg)
	if err != nil {
		return err
	}
	approver := ui.NewApprover(ctx, events, cfg.Operator)
	tools.Tunnel.SetOperator(cfg.Operator)

	var storeOptions []approval.Option
	if cfg.AuditLog != "" {
		storeOptions = append(storeOptions, approval.WithAuditLog(cfg.AuditLog))
	}
	gate := tools.NewGate(
		approval.NewStore(storeOptions...),
		tools.WithApprover(approver),
		tools.WithTimeout(cfg.ToolTimeout),
	)
	registry, err := registerTools(ctx, gate, cfg.ToolTimeout)
	if err != nil {
		return err
	}

	diagnosticAgent, err := agent.New(ctx, chatModel, registry, cfg)
	if err != nil {
		return err
	}
	sessionStore, err := session.OpenStore(cfg.HistoryTurns, cfg.HistoryTokens, cfg.HistoryFile)
	if err != nil {
		return fmt.Errorf("加载对话历史失败: %w", err)
	}
	currentSession, activeSession := sessionStore.Current()
	if currentSession == nil {
		return errors.New("会话存储没有当前会话")
	}

	chatApp := chat.NewApp(diagnosticAgent, sessionStore, currentSession, cfg.ImageMaxBytes, cfg.ImageDetail)
	model := ui.NewModel(ctx, chatApp, events, ui.ModelConfig{
		Banner: ui.StartupBanner(startupInfo(
			cfg, registry, diagnosticAgent.SkillNames(), activeSession, currentSession,
		)),
		InputMaxBytes: ui.InputMaxBytes(cfg.ContextTokens),
	})
	_, err = ui.Run(ctx, model)
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("运行终端界面: %w", err)
	}
	return nil
}

func registerTools(ctx context.Context, gate *tools.Gate, timeout time.Duration) (*tools.Registry, error) {
	registry := tools.NewRegistry()
	evidenceTools, err := tools.NewTunnelEvidenceTools(timeout)
	if err != nil {
		return nil, err
	}
	for _, evidenceTool := range evidenceTools {
		if err := registry.Register(ctx, evidenceTool, tools.RiskReadOnly); err != nil {
			return nil, fmt.Errorf("注册只读采证工具失败: %w", err)
		}
	}

	tunnelTool, err := tools.NewTunnelTool()
	if err != nil {
		return nil, fmt.Errorf("构造 run_tunnel_cmd 工具失败: %w", err)
	}
	gatedTunnel, err := gate.Wrap(ctx, tunnelTool)
	if err != nil {
		return nil, fmt.Errorf("包装 run_tunnel_cmd 失败: %w", err)
	}
	if err := registry.Register(ctx, gatedTunnel, tools.RiskMutating); err != nil {
		return nil, err
	}
	return registry, nil
}

func startupInfo(cfg *config.Config, registry *tools.Registry, skillNames []string,
	active session.Info, current *session.Session) ui.StartupInfo {
	toolSummaries := make([]ui.ToolSummary, 0, len(registry.Entries()))
	for _, entry := range registry.Entries() {
		toolSummaries = append(toolSummaries, ui.ToolSummary{Name: entry.Name, Risk: entry.Risk.String()})
	}
	return ui.StartupInfo{
		Config:      cfg.Redacted(),
		Tools:       toolSummaries,
		Skills:      append([]string(nil), skillNames...),
		SessionName: active.Name,
		SessionID:   active.ShortID(),
		Messages:    current.Len(),
		Tokens:      current.EstimatedTokens(),
	}
}
