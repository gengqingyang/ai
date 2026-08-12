// Package chat 提供与终端 UI 无关的对话、图片和会话业务能力。
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/repository"
	"diagnostic-system/internal/session"
)

// ConversationAgent 是聊天业务依赖的流式 Agent 能力。
type ConversationAgent interface {
	Stream(context.Context, []*schema.Message, func(intent.Result), func(string)) (string, error)
}

// App 保存对话、会话和图片业务状态，不持有任何终端对象。
type App struct {
	agent         ConversationAgent
	sessions      *session.Store
	sess          *session.Session
	imageMaxBytes int
	imageDetail   string
	repositories  *repository.Manager
}

// NewApp 创建聊天应用实例。
func NewApp(ag ConversationAgent, sessions *session.Store, sess *session.Session,
	imageMaxBytes int, imageDetail string, repositories ...*repository.Manager) *App {
	app := &App{
		agent:         ag,
		sessions:      sessions,
		sess:          sess,
		imageMaxBytes: imageMaxBytes,
		imageDetail:   imageDetail,
	}
	if len(repositories) > 0 {
		app.repositories = repositories[0]
	}
	return app
}

// CurrentSession 返回当前选中的会话。
func (a *App) CurrentSession() *session.Session { return a.sess }

// CurrentSessionInfo 返回当前会话元数据。
func (a *App) CurrentSessionInfo() session.Info {
	if a.sessions == nil {
		return session.Info{}
	}
	_, info := a.sessions.Current()
	return info
}

// CurrentSessionStats 返回当前会话消息数和估算 token 数。
func (a *App) CurrentSessionStats() (messages, tokens int) {
	if a.sess == nil {
		return 0, 0
	}
	return a.sess.Len(), a.sess.EstimatedTokens()
}

// History 返回当前会话历史快照。
func (a *App) History() []*schema.Message {
	if a.sess == nil {
		return nil
	}
	return a.sess.Messages()
}

// AskMessage 执行一轮模型调用。
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

// ImageMessage 解析图片命令并返回多模态消息和图片元数据。
func (a *App) ImageMessage(line string) (*schema.Message, ImageMeta, error) {
	source, prompt, err := ParseImageCommand(line)
	if err != nil {
		return nil, ImageMeta{}, err
	}
	return BuildImageMessage(source, prompt, a.imageMaxBytes, a.imageDetail)
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

// Repositories 返回已登记的本地代码仓库。
func (a *App) Repositories() []repository.Info {
	if a.repositories == nil {
		return nil
	}
	return a.repositories.List()
}

// AddRepository 添加、索引并切换到一个本地代码仓库。
func (a *App) AddRepository(ctx context.Context, path, name string) (repository.Info, error) {
	if a.repositories == nil {
		return repository.Info{}, errors.New("仓库管理器未配置")
	}
	return a.repositories.Add(ctx, path, name)
}

// UseRepository 切换当前代码仓库。
func (a *App) UseRepository(ctx context.Context, name string) (repository.Info, error) {
	if a.repositories == nil {
		return repository.Info{}, errors.New("仓库管理器未配置")
	}
	return a.repositories.Use(ctx, name)
}

// ReindexRepository 增量更新当前代码仓库索引。
func (a *App) ReindexRepository(ctx context.Context) (repository.Info, error) {
	if a.repositories == nil {
		return repository.Info{}, errors.New("仓库管理器未配置")
	}
	return a.repositories.Reindex(ctx)
}

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
	case "/history", "/sessions", "/session", "/repos", "/help":
		return true, nil
	case "/repo":
		_, err := a.RunRepositoryCommand(ctx, line)
		return true, err
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

// RunRepositoryCommand 执行仓库管理命令。路径或名称含空格时可以使用单/双引号。
func (a *App) RunRepositoryCommand(ctx context.Context, line string) (repository.Info, error) {
	fields, err := commandWords(line)
	if err != nil {
		return repository.Info{}, err
	}
	if len(fields) < 2 {
		return repository.Info{}, errors.New("用法: /repo add <path> [name] | /repo use <name> | /repo reindex")
	}
	switch fields[1] {
	case "add":
		if len(fields) < 3 {
			return repository.Info{}, errors.New("用法: /repo add <path> [name]")
		}
		return a.AddRepository(ctx, fields[2], strings.Join(fields[3:], " "))
	case "use":
		if len(fields) < 3 {
			return repository.Info{}, errors.New("用法: /repo use <name>")
		}
		return a.UseRepository(ctx, strings.Join(fields[2:], " "))
	case "reindex":
		if len(fields) != 2 {
			return repository.Info{}, errors.New("用法: /repo reindex")
		}
		return a.ReindexRepository(ctx)
	default:
		return repository.Info{}, fmt.Errorf("未知仓库命令 %q", fields[1])
	}
}

func commandWords(line string) ([]string, error) {
	var words []string
	var current strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if started {
			words = append(words, current.String())
			current.Reset()
			started = false
		}
	}
	for _, r := range line {
		if escaped {
			current.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			flush()
			continue
		}
		current.WriteRune(r)
		started = true
	}
	if escaped {
		return nil, errors.New("命令末尾不能是转义符")
	}
	if quote != 0 {
		return nil, errors.New("命令参数的引号没有闭合")
	}
	flush()
	return words, nil
}

// CommandArgs 返回命令名后的原始参数，并保留名称内部空格。
func CommandArgs(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
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
