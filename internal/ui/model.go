// Package ui 提供诊断助手唯一的常驻 Bubble Tea 界面。
package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/chat"
	"diagnostic-system/internal/session"
	uiinput "diagnostic-system/internal/ui/input"
	"diagnostic-system/internal/ui/menu"
)

type mode int

const (
	modeInput mode = iota
	modeBusy
	modeApproval
	modeRejectReason
	modeSessions
)

func (m mode) String() string {
	switch m {
	case modeInput:
		return "input"
	case modeBusy:
		return "busy"
	case modeApproval:
		return "approval"
	case modeRejectReason:
		return "rejection_reason"
	case modeSessions:
		return "sessions"
	default:
		return "unknown"
	}
}

type transcriptRole string

const (
	roleSystem    transcriptRole = "system"
	roleUser      transcriptRole = "user"
	roleAssistant transcriptRole = "assistant"
	roleIntent    transcriptRole = "intent"
	roleError     transcriptRole = "error"
	roleStatus    transcriptRole = "status"
	roleApproval  transcriptRole = "approval"
	roleLog       transcriptRole = "log"
)

type transcriptEntry struct {
	role    transcriptRole
	content string
}

// ModelConfig 配置根 UI，不把 UI 参数塞进聊天业务对象。
type ModelConfig struct {
	Banner        string
	InputMaxBytes int
}

// Model 是应用唯一的 Bubble Tea 根模型。
type Model struct {
	ctx    context.Context
	app    *chat.App
	events chan tea.Msg

	mode       mode
	input      uiinput.Model
	menu       menu.Model
	transcript []transcriptEntry
	streaming  string
	approval   *approvalRequestMsg
	sessions   []session.Info

	inputMaxBytes int
	requestCancel context.CancelFunc
	width         int
	height        int
	scrollOffset  int
}

// NewModel 创建根 UI。events 为 nil 时可在测试中手工投递消息。
func NewModel(ctx context.Context, app *chat.App, events chan tea.Msg, cfg ModelConfig) Model {
	if ctx == nil {
		ctx = context.Background()
	}
	maxBytes := cfg.InputMaxBytes
	if maxBytes <= 0 {
		maxBytes = 1024 * 1024
	}
	m := Model{
		ctx:           ctx,
		app:           app,
		events:        events,
		mode:          modeInput,
		input:         uiinput.New(maxBytes),
		inputMaxBytes: maxBytes,
	}
	if strings.TrimSpace(cfg.Banner) != "" {
		m.appendEntry(roleSystem, cfg.Banner)
	}
	return m
}

func (m Model) Init() tea.Cmd { return m.waitForEvent() }

func (m *Model) appendEntry(role transcriptRole, content string) {
	m.transcript = append(m.transcript, transcriptEntry{role: role, content: content})
	m.scrollOffset = 0
}

func (m *Model) resizeMenu() {
	height := 0
	if m.height > 0 {
		height = max(1, m.height-5)
	}
	updated, _ := m.menu.Update(tea.WindowSizeMsg{Width: m.viewWidth(), Height: height})
	m.menu = updated.(menu.Model)
}

func (m *Model) resizeInput() {
	updated, _ := m.input.Update(tea.WindowSizeMsg{Width: m.viewWidth(), Height: m.height})
	m.input = updated.(uiinput.Model)
}

func (m Model) viewWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 100
}

// Mode 返回当前 UI 模式，便于黑盒测试和状态诊断。
func (m Model) Mode() string { return m.mode.String() }

// InputValue 返回当前输入框内容。
func (m Model) InputValue() string { return m.input.Value() }

// TranscriptText 返回不带样式的当前展示记录。
func (m Model) TranscriptText() string {
	var out strings.Builder
	for _, entry := range m.transcript {
		fmt.Fprintf(&out, "%s: %s\n", entry.role, entry.content)
	}
	if m.streaming != "" {
		fmt.Fprintf(&out, "%s: %s\n", roleAssistant, m.streaming)
	}
	return out.String()
}

// InputMaxBytes 根据上下文窗口计算输入框的 UTF-8 字节上限。
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
