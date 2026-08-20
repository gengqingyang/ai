package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/chat"
	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/repository"
	uiinput "diagnostic-system/internal/ui/input"
)

func (m Model) submit(line string) (tea.Model, tea.Cmd) {
	if !strings.HasPrefix(line, "/") {
		return m.beginAsk(schema.UserMessage(line), line)
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return m, nil
	}

	switch fields[0] {
	case "/exit", "/quit":
		m.cancelAll()
		return m, tea.Quit
	case "/reset":
		if err := m.app.ResetSession(); err != nil {
			m.appendEntry(roleError, err.Error())
			return m, nil
		}
		m.transcript = nil
		m.appendEntry(roleStatus, "已清空当前会话的对话历史。")
		return m, nil
	case "/history":
		m.showHistory()
		return m, nil
	case "/sessions", "/session":
		m.openSessions()
		return m, nil
	case "/repos":
		m.appendEntry(roleSystem, repositoriesText(m.app.Repositories()))
		return m, nil
	case "/repo":
		info, err := m.app.RunRepositoryCommand(m.ctx, line)
		if err != nil {
			m.appendEntry(roleError, err.Error())
		} else {
			m.appendEntry(roleStatus, fmt.Sprintf("当前代码仓库：%s（%d 文件，%d 符号，commit %s）",
				info.Name, info.FileCount, info.SymbolCount, shortCommit(info.GitCommit)))
		}
		return m, nil
	case "/new":
		info, err := m.app.CreateSession(chat.CommandArgs(line))
		if err != nil {
			m.appendEntry(roleError, err.Error())
		} else {
			m.resetHistoryNavigation()
			m.transcript = nil
			m.appendEntry(roleStatus, fmt.Sprintf("已新建并切换到会话：%s [%s]", info.Name, info.ShortID()))
		}
		return m, nil
	case "/switch":
		query := chat.CommandArgs(line)
		if query == "" {
			m.appendEntry(roleError, "用法: /switch <会话 ID 或名称>")
			return m, nil
		}
		m.switchSession(query)
		return m, nil
	case "/image":
		msg, meta, err := m.app.ImageMessage(line)
		if err != nil {
			m.appendEntry(roleError, err.Error())
			return m, nil
		}
		m.appendEntry(roleStatus, imageSummary(meta))
		return m.beginAsk(msg, messageSummary(msg))
	case "/help":
		m.appendEntry(roleSystem, helpText())
		return m, nil
	case "/approvals":
		return m.runApprovals(line)
	default:
		m.appendEntry(roleError, fmt.Sprintf("未知命令 %s，输入 /help 查看可用命令", fields[0]))
		return m, nil
	}
}

func (m Model) beginAsk(msg *schema.Message, display string) (tea.Model, tea.Cmd) {
	m.appendEntry(roleUser, display)
	m.streaming = ""
	m.streamRole = roleAssistant
	m.assistantStreamed = false
	m.mode = modeBusy
	requestCtx, cancel := context.WithCancel(m.ctx)
	m.requestCancel = cancel

	cmd := func() tea.Msg {
		reply, err := m.app.AskMessageWithProgress(
			requestCtx,
			msg,
			func(result intent.Result) {
				sendEvent(requestCtx, m.events, intentMsg{result: result})
			},
			func(progress string) {
				sendEvent(requestCtx, m.events, agentProgressMsg{text: progress})
			},
			func(chunk string) {
				sendEvent(requestCtx, m.events, reasoningChunkMsg{chunk: chunk})
			},
			func(chunk string) {
				sendEvent(requestCtx, m.events, assistantChunkMsg{chunk: chunk})
			},
		)
		done := askDoneMsg{reply: reply, err: err}
		if m.events == nil {
			return done
		}
		sendEvent(requestCtx, m.events, done)
		return nil
	}
	return m, cmd
}

func (m *Model) finishAsk(done askDoneMsg) {
	if m.requestCancel != nil {
		m.requestCancel()
		m.requestCancel = nil
	}
	if m.approval != nil {
		m.approval.result <- approvalResult{err: ErrNoApproval}
		m.approval = nil
	}
	m.flushStreaming()
	if !m.assistantStreamed && done.err == nil && done.reply != "" {
		m.appendEntry(roleAssistant, done.reply)
	} else if !m.assistantStreamed && done.err == nil && done.note == "" {
		m.appendEntry(roleStatus, "模型没有再补充说明。")
	}
	if done.note != "" {
		m.appendEntry(roleStatus, done.note)
	}
	if done.err != nil {
		m.appendEntry(roleError, "本轮失败: "+humanErr(done.err))
	}
	m.assistantStreamed = false
	m.mode = modeInput
	m.input = uiinput.New(m.inputMaxBytes)
	m.resetHistoryNavigation()
	m.resizeInput()
}

func (m *Model) cancelAll() {
	if m.requestCancel != nil {
		m.requestCancel()
		m.requestCancel = nil
	}
	if m.approval != nil {
		m.approval.result <- approvalResult{err: ErrNoApproval}
		m.approval = nil
	}
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

func imageSummary(meta chat.ImageMeta) string {
	if meta.Remote {
		return fmt.Sprintf("已附加远程图片: %s", meta.Source)
	}
	return fmt.Sprintf("已读取图片: %s（%s，%.1fKB）",
		meta.Source, meta.MIMEType, float64(meta.Bytes)/1024)
}

func messageSummary(msg *schema.Message) string {
	return messageImagePrefix(msg) + oneLine(msg.Content)
}

func messageImagePrefix(msg *schema.Message) string {
	if images := messageImageCount(msg); images == 1 {
		return "[图片] "
	} else if images > 1 {
		return fmt.Sprintf("[图片 x%d] ", images)
	}
	return ""
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

func oneLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 90 {
		return string(runes[:90]) + "…"
	}
	return text
}

func helpText() string {
	return "直接输入问题即可。命令：/sessions 选择会话 | /new [名称] 新建 | " +
		"/switch ID 切换 | /image <路径或 URL> [问题] | /repos | " +
		"/repo add|use|reindex | /approvals [approve|reject <ID>] | " +
		"/history | /reset | /help | /exit"
}

func repositoriesText(items []repository.Info) string {
	if len(items) == 0 {
		return "尚未添加代码仓库。使用 /repo add <path> [name] 添加。"
	}
	var out strings.Builder
	for _, item := range items {
		marker := " "
		if item.Active {
			marker = "*"
		}
		fmt.Fprintf(&out, "%s %s · %d 文件 · %d 符号 · %s\n  %s\n",
			marker, item.Name, item.FileCount, item.SymbolCount, shortCommit(item.GitCommit), item.Root)
	}
	return strings.TrimSpace(out.String())
}

func shortCommit(commit string) string {
	if commit == "" {
		return "无 Git 版本"
	}
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
