// Package chat 实现基于 Bubble Tea 的终端诊断助手。
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/agent"
	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/config"
	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/llm"
	"diagnostic-system/internal/logging"
	"diagnostic-system/internal/session"
	"diagnostic-system/internal/tools"
)

// ConversationAgent 是聊天 UI 依赖的流式 Agent 能力。
type ConversationAgent interface {
	Stream(context.Context, []*schema.Message, func(intent.Result), func(string)) (string, error)
}

// App 保存与 UI 无关的对话、会话和图片业务状态。
type App struct {
	agent         ConversationAgent
	sessions      *session.Store
	sess          *session.Session
	inputMaxBytes int
	imageMaxBytes int
	imageDetail   string
}

// NewApp 创建聊天应用实例。终端输入输出全部由外层 Model 管理。
func NewApp(ag ConversationAgent, sessions *session.Store, sess *session.Session,
	imageMaxBytes int, imageDetail string) *App {
	return &App{
		agent:         ag,
		sessions:      sessions,
		sess:          sess,
		inputMaxBytes: InputMaxBytes(1_000_000),
		imageMaxBytes: imageMaxBytes,
		imageDetail:   imageDetail,
	}
}

// CurrentSession 返回应用当前选中的会话。
func (a *App) CurrentSession() *session.Session { return a.sess }

// Run 初始化依赖，并启动唯一一个常驻 Bubble Tea 程序。
func Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	events := make(chan tea.Msg, 256)
	closeLog, err := logging.SetupWithConsole(cfg, uiLogWriter{ctx: ctx, events: events})
	if err != nil {
		return err
	}
	defer closeLog()
	logging.InstallEinoCallbacks()
	slog.Info("启动", "config", cfg.Redacted())

	cm, err := llm.NewChatModel(ctx, cfg)
	if err != nil {
		return err
	}

	approver := NewUIApprover(ctx, events, cfg.Operator)
	tools.Tunnel.SetOperator(cfg.Operator)

	var storeOpts []approval.Option
	if cfg.AuditLog != "" {
		storeOpts = append(storeOpts, approval.WithAuditLog(cfg.AuditLog))
	}
	gate := tools.NewGate(
		approval.NewStore(storeOpts...),
		tools.WithApprover(approver),
		tools.WithTimeout(cfg.ToolTimeout),
	)
	reg, err := registerTools(ctx, gate)
	if err != nil {
		return err
	}

	ag, err := agent.New(ctx, cm, reg, cfg)
	if err != nil {
		return err
	}
	sessionStore, err := session.OpenStore(cfg.HistoryTurns, cfg.HistoryTokens, cfg.HistoryFile)
	if err != nil {
		return fmt.Errorf("加载对话历史失败: %w", err)
	}
	sess, activeSession := sessionStore.Current()
	if sess == nil {
		return errors.New("会话存储没有当前会话")
	}

	app := NewApp(ag, sessionStore, sess, cfg.ImageMaxBytes, cfg.ImageDetail)
	app.inputMaxBytes = InputMaxBytes(cfg.ContextTokens)
	model := NewModel(ctx, app, events, bannerText(cfg, reg, ag.SkillNames(), activeSession, sess))
	_, err = runTeaProgram(ctx, model)
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("运行终端界面: %w", err)
	}
	return nil
}

func runTeaProgram(ctx context.Context, model tea.Model) (tea.Model, error) {
	return tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithoutSignalHandler(),
	).Run()
}

// ShowError 用 Bubble Tea 展示启动或运行错误，避免入口直接写系统 stderr。
func ShowError(err error) error {
	if err == nil {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, runErr := runTeaProgram(ctx, errorModel{err: err})
	if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return runErr
}

// registerTools 集中注册工具并声明风险等级。变更类工具必须先经过 Gate。
func registerTools(ctx context.Context, gate *tools.Gate) (*tools.Registry, error) {
	reg := tools.NewRegistry()
	tunnelTool, err := tools.NewTunnelTool()
	if err != nil {
		return nil, fmt.Errorf("构造 run_tunnel_cmd 工具失败: %w", err)
	}
	gatedTunnel, err := gate.Wrap(ctx, tunnelTool)
	if err != nil {
		return nil, fmt.Errorf("包装 run_tunnel_cmd 失败: %w", err)
	}
	if err := reg.Register(ctx, gatedTunnel, tools.RiskMutating); err != nil {
		return nil, err
	}
	return reg, nil
}

// AskMessage 执行一轮模型调用，并把意图和文本片段交给 Bubble Tea 消息桥接层。
func (a *App) AskMessage(ctx context.Context, userMessage *schema.Message,
	onIntent func(intent.Result), onChunk func(string)) (string, error) {
	if a.agent == nil {
		return "", errors.New("聊天 Agent 未配置")
	}
	if err := a.sess.AppendUserMessage(userMessage); err != nil {
		return "", fmt.Errorf("保存用户消息失败: %w", err)
	}
	slog.Info("用户提问", "chars", len([]rune(userMessage.Content)), "images", messageImageCount(userMessage))

	reply, err := a.agent.Stream(ctx, a.sess.Messages(), onIntent, onChunk)
	if err != nil {
		return reply, err
	}
	if reply == "" {
		slog.Warn("模型返回了空回复")
		return "", nil
	}
	if err := a.sess.AppendAssistant(reply); err != nil {
		return reply, fmt.Errorf("保存助手回复失败: %w", err)
	}
	slog.Info("模型回复", "chars", len([]rune(reply)))
	return reply, nil
}

// ImageMessage 解析图片命令并返回可交给 AskMessage 的多模态消息及展示摘要。
func (a *App) ImageMessage(line string) (*schema.Message, string, error) {
	source, prompt, err := ParseImageCommand(line)
	if err != nil {
		return nil, "", err
	}
	msg, meta, err := BuildImageMessage(source, prompt, a.imageMaxBytes, a.imageDetail)
	if err != nil {
		return nil, "", err
	}
	if meta.Remote {
		return msg, fmt.Sprintf("已附加远程图片: %s", meta.Source), nil
	}
	return msg, fmt.Sprintf("已读取图片: %s（%s，%.1fKB）",
		meta.Source, meta.MIMEType, float64(meta.Bytes)/1024), nil
}

// ResetSession 清空当前会话。
func (a *App) ResetSession() error {
	if err := a.sess.Reset(); err != nil {
		return fmt.Errorf("清空对话历史失败: %w", err)
	}
	return nil
}

// CreateSession 新建并切换会话。
func (a *App) CreateSession(name string) (session.Info, error) {
	created, info, err := a.sessions.Create(name)
	if err != nil {
		return session.Info{}, fmt.Errorf("新建会话失败: %w", err)
	}
	a.sess = created
	return info, nil
}

// SwitchSession 按 ID 或名称切换会话。
func (a *App) SwitchSession(query string) (session.Info, error) {
	selected, info, err := a.sessions.Select(query)
	if err != nil {
		return session.Info{}, fmt.Errorf("切换会话失败: %w", err)
	}
	a.sess = selected
	return info, nil
}

// Sessions 返回会话选择器的数据。
func (a *App) Sessions() []session.Info { return a.sessions.List() }

var errQuit = errors.New("quit")

// RunCommand 保留纯业务命令入口，供非 UI 调用和测试使用；不会读写终端。
func (a *App) RunCommand(ctx context.Context, line string) (bool, error) {
	if !strings.HasPrefix(line, "/") {
		return false, nil
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false, nil
	}
	switch fields[0] {
	case "/exit", "/quit":
		return true, errQuit
	case "/reset":
		return true, a.ResetSession()
	case "/history", "/sessions", "/session", "/help":
		return true, nil
	case "/new":
		_, err := a.CreateSession(CommandArgs(line))
		return true, err
	case "/switch":
		query := CommandArgs(line)
		if query == "" {
			return true, errors.New("用法: /switch <会话 ID 或名称>")
		}
		_, err := a.SwitchSession(query)
		return true, err
	case "/image":
		msg, _, err := a.ImageMessage(line)
		if err != nil {
			return true, err
		}
		_, err = a.AskMessage(ctx, msg, nil, nil)
		return true, err
	default:
		return true, fmt.Errorf("未知命令 %s，输入 /help 查看可用命令", fields[0])
	}
}

// CommandArgs 返回命令名后的原始参数，并保留名称内部空格。
func CommandArgs(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
}

func sessionOption(info session.Info) string {
	marker := "  "
	if info.Active {
		marker = "● "
	}
	return fmt.Sprintf("%s%s  ·  %s  ·  %s",
		marker, info.Name, info.UpdatedAt.Local().Format("01-02 15:04"), info.ShortID())
}

func messageSummary(msg *schema.Message) string {
	prefix := ""
	if images := messageImageCount(msg); images == 1 {
		prefix = "[图片] "
	} else if images > 1 {
		prefix = fmt.Sprintf("[图片 x%d] ", images)
	}
	return prefix + oneLine(msg.Content)
}

func messageImageCount(msg *schema.Message) int {
	count := 0
	for _, part := range msg.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeImageURL && part.Image != nil {
			count++
		}
	}
	return count
}

// InputMaxBytes 根据上下文窗口计算 Bubble Tea 输入框的 UTF-8 字节上限。
func InputMaxBytes(contextTokens int) int {
	const (
		minimum = 1024 * 1024
		maximum = 64 * 1024 * 1024
	)
	if contextTokens > maximum/5 {
		return maximum
	}
	size := contextTokens * 5
	if size < minimum {
		return minimum
	}
	return size
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 90 {
		return string(r[:90]) + "…"
	}
	return s
}

func bannerText(cfg *config.Config, reg *tools.Registry, skillNames []string,
	active session.Info, sess *session.Session) string {
	toolsText := make([]string, 0, len(reg.Entries()))
	for _, entry := range reg.Entries() {
		toolsText = append(toolsText, fmt.Sprintf("%s [%s]", entry.Name, entry.Risk))
	}
	loadedSkills := "无"
	if len(skillNames) > 0 {
		loadedSkills = strings.Join(skillNames, ", ")
	}
	return fmt.Sprintf("配置: %s\n工具: %s\nSkill: %s\n当前会话: %s [%s]，已加载 %d 条消息，约 %d tokens",
		cfg.Redacted(), strings.Join(toolsText, ", "), loadedSkills,
		active.Name, active.ShortID(), sess.Len(), sess.EstimatedTokens())
}

func humanErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，模型没有在规定时间内返回。可以再问一次，或把问题拆小一点。"
	case errors.Is(err, context.Canceled):
		return "请求已取消。"
	default:
		return err.Error()
	}
}
