package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/session"
	"diagnostic-system/internal/tools"
)

type uiMode int

const (
	modeInput uiMode = iota
	modeBusy
	modeApproval
	modeRejectReason
	modeSessions
)

func (m uiMode) String() string {
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

type transcriptEntry struct {
	role    string
	content string
}

type intentMsg struct{ result intent.Result }
type assistantChunkMsg struct{ chunk string }
type uiLogMsg struct{ line string }
type askDoneMsg struct {
	reply string
	err   error
}
type uiContextDoneMsg struct{}

type errorModel struct {
	err    error
	width  int
	height int
}

func (m errorModel) Init() tea.Cmd { return nil }

func (m errorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		m.height = size.Height
		return m, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyEnter, tea.KeyEsc, tea.KeyCtrlC:
			return m, tea.Quit
		}
		if key.String() == "q" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m errorModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	message := "未知错误"
	if m.err != nil {
		message = m.err.Error()
	}
	var view strings.Builder
	view.WriteString(errorStyle.Render("CDN 诊断助手启动失败"))
	view.WriteString("\n\n")
	view.WriteString(strings.Join(wrapCells(message, width), "\n"))
	view.WriteString("\n\n")
	view.WriteString(metaStyle.Render("Enter / Esc / q 退出"))
	view.WriteByte('\n')
	return view.String()
}

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2DD4BF"))
	metaStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#A1A1AA"))
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#22D3EE"))
	agentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#4ADE80"))
	intentStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E879F9"))
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FACC15"))
	errorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F87171"))
)

// Model 是应用唯一的 Bubble Tea 根模型。所有终端输入和展示状态都由它管理。
type Model struct {
	ctx    context.Context
	app    *App
	events chan tea.Msg

	mode       uiMode
	input      LineInputModel
	menu       MenuModel
	transcript []transcriptEntry
	streaming  string
	approval   *approvalRequestMsg
	sessions   []session.Info

	requestCancel context.CancelFunc
	width         int
	height        int
	scrollOffset  int
}

// NewModel 创建常驻聊天界面。events 为 nil 时可在单元测试中手工投递消息。
func NewModel(ctx context.Context, app *App, events chan tea.Msg, banner string) Model {
	if ctx == nil {
		ctx = context.Background()
	}
	maxBytes := 1024 * 1024
	if app != nil && app.inputMaxBytes > 0 {
		maxBytes = app.inputMaxBytes
	}
	m := Model{
		ctx:    ctx,
		app:    app,
		events: events,
		mode:   modeInput,
		input:  NewLineInputModel(maxBytes),
	}
	if strings.TrimSpace(banner) != "" {
		m.appendEntry("system", banner)
	}
	return m
}

func (m Model) Init() tea.Cmd { return m.waitForEvent() }

// Update 路由后台事件和当前交互模式。子输入框和菜单从不自行退出程序。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch event := msg.(type) {
	case intentMsg:
		suffix := ""
		if event.result.NeedsClarification {
			suffix = " · 需澄清"
		}
		m.appendEntry("intent", fmt.Sprintf("%s · %d%%%s",
			event.result.Intent.Label(), int(event.result.Confidence*100+0.5), suffix))
		return m, m.waitForEvent()
	case assistantChunkMsg:
		m.streaming += event.chunk
		m.scrollOffset = 0
		return m, m.waitForEvent()
	case uiLogMsg:
		if strings.TrimSpace(event.line) != "" {
			m.appendEntry("log", strings.TrimSpace(event.line))
		}
		return m, m.waitForEvent()
	case approvalRequestMsg:
		if m.approval != nil {
			event.result <- approvalResult{err: errors.New("已有待处理的人工审核")}
			return m, m.waitForEvent()
		}
		m.approval = &event
		m.mode = modeApproval
		m.scrollOffset = 0
		m.menu = NewMenuModel("执行这项操作？", []string{"执行", "拒绝"}, OptRun,
			map[byte]int{'y': OptRun, 'Y': OptRun, 'n': OptDeny, 'N': OptDeny}).WithDanger(OptDeny)
		m.resizeMenu()
		return m, m.waitForEvent()
	case executionNoticeMsg:
		m.handleExecutionNotice(event.proposal)
		return m, m.waitForEvent()
	case askDoneMsg:
		m.finishAsk(event)
		return m, m.waitForEvent()
	case uiContextDoneMsg:
		m.cancelAll()
		return m, tea.Quit
	}

	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		m.height = size.Height
		updated, _ := m.input.Update(size)
		m.input = updated.(LineInputModel)
		m.resizeMenu()
		return m, nil
	}

	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyCtrlC {
		m.cancelAll()
		return m, tea.Quit
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyPgUp:
			m.scrollOffset += max(1, (m.height-7)/2)
			return m, nil
		case tea.KeyPgDown:
			m.scrollOffset -= max(1, (m.height-7)/2)
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
			return m, nil
		}
	}

	switch m.mode {
	case modeInput:
		return m.updateInput(msg)
	case modeBusy:
		return m, nil
	case modeApproval:
		return m.updateApproval(msg)
	case modeRejectReason:
		return m.updateRejectReason(msg)
	case modeSessions:
		return m.updateSessions(msg)
	default:
		return m, nil
	}
}

func (m Model) updateInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, _ := m.input.Update(msg)
	m.input = updated.(LineInputModel)
	if !m.input.Done() {
		return m, nil
	}
	if m.input.Err() != nil {
		m.cancelAll()
		return m, tea.Quit
	}
	line := strings.TrimSpace(m.input.Value())
	m.input = NewLineInputModel(m.maxInputBytes())
	m.resizeInput()
	if line == "" {
		return m, nil
	}
	return m.submit(line)
}

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
			m.appendEntry("error", err.Error())
			return m, nil
		}
		m.transcript = nil
		m.appendEntry("status", "已清空当前会话的对话历史。")
		return m, nil
	case "/history":
		m.showHistory()
		return m, nil
	case "/sessions", "/session":
		m.openSessions()
		return m, nil
	case "/new":
		info, err := m.app.CreateSession(CommandArgs(line))
		if err != nil {
			m.appendEntry("error", err.Error())
		} else {
			m.transcript = nil
			m.appendEntry("status", fmt.Sprintf("已新建并切换到会话：%s [%s]", info.Name, info.ShortID()))
		}
		return m, nil
	case "/switch":
		query := CommandArgs(line)
		if query == "" {
			m.appendEntry("error", "用法: /switch <会话 ID 或名称>")
			return m, nil
		}
		m.switchSession(query)
		return m, nil
	case "/image":
		msg, summary, err := m.app.ImageMessage(line)
		if err != nil {
			m.appendEntry("error", err.Error())
			return m, nil
		}
		m.appendEntry("status", summary)
		display := messageSummary(msg)
		return m.beginAsk(msg, display)
	case "/help":
		m.appendEntry("system", helpText())
		return m, nil
	default:
		m.appendEntry("error", fmt.Sprintf("未知命令 %s，输入 /help 查看可用命令", fields[0]))
		return m, nil
	}
}

func (m Model) beginAsk(msg *schema.Message, display string) (tea.Model, tea.Cmd) {
	m.appendEntry("user", display)
	m.streaming = ""
	m.mode = modeBusy
	requestCtx, cancel := context.WithCancel(m.ctx)
	m.requestCancel = cancel

	cmd := func() tea.Msg {
		reply, err := m.app.AskMessage(
			requestCtx,
			msg,
			func(result intent.Result) {
				sendUIEvent(requestCtx, m.events, intentMsg{result: result})
			},
			func(chunk string) {
				sendUIEvent(requestCtx, m.events, assistantChunkMsg{chunk: chunk})
			},
		)
		done := askDoneMsg{reply: reply, err: err}
		if m.events == nil {
			return done
		}
		sendUIEvent(requestCtx, m.events, done)
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
	if strings.TrimSpace(m.streaming) != "" {
		m.appendEntry("assistant", m.streaming)
	} else if done.err == nil && done.reply != "" {
		m.appendEntry("assistant", done.reply)
	} else if done.err == nil {
		m.appendEntry("status", "模型没有再补充说明。")
	}
	if done.err != nil {
		m.appendEntry("error", "本轮失败: "+humanErr(done.err))
	}
	m.streaming = ""
	m.mode = modeInput
	m.input = NewLineInputModel(m.maxInputBytes())
	m.resizeInput()
}

func (m Model) updateApproval(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, _ := m.menu.Update(msg)
	m.menu = updated.(MenuModel)
	if !m.menu.Done() {
		return m, nil
	}
	if m.menu.Err() != nil {
		m.approval.result <- approvalResult{err: m.menu.Err()}
		m.approval = nil
		m.mode = modeBusy
		m.appendEntry("status", "人工审核已取消，操作不会执行。")
		return m, nil
	}
	if m.menu.Selected() == OptRun {
		m.approval.result <- approvalResult{decision: tools.Decision{Approved: true}}
		m.approval = nil
		m.mode = modeBusy
		m.appendEntry("status", "已批准，正在节点上执行…")
		return m, nil
	}

	m.mode = modeRejectReason
	m.input = NewLineInputModel(m.maxInputBytes()).WithPrompt("\033[31m拒绝理由 >\033[0m ")
	m.resizeInput()
	return m, nil
}

func (m Model) updateRejectReason(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyEsc {
		return m.reject("")
	}
	updated, _ := m.input.Update(msg)
	m.input = updated.(LineInputModel)
	if !m.input.Done() {
		return m, nil
	}
	return m.reject(strings.TrimSpace(m.input.Value()))
}

func (m Model) reject(reason string) (tea.Model, tea.Cmd) {
	if reason == "" {
		reason = "（未填写理由）"
	}
	m.approval.result <- approvalResult{decision: tools.Decision{Approved: false, Reason: reason}}
	m.approval = nil
	m.mode = modeBusy
	m.input = NewLineInputModel(m.maxInputBytes())
	m.resizeInput()
	m.appendEntry("status", "已驳回，理由会回传给模型："+reason)
	return m, nil
}

func (m *Model) handleExecutionNotice(p *approval.Proposal) {
	if p == nil {
		return
	}
	if p.Status == approval.StatusFailed {
		m.appendEntry("error", "执行失败："+p.Error)
		return
	}
	m.appendEntry("status", "执行完毕，输出已回传给模型。")
}

func (m *Model) openSessions() {
	m.sessions = m.app.Sessions()
	options := make([]string, 0, len(m.sessions)+1)
	def := 0
	for i, item := range m.sessions {
		options = append(options, sessionOption(item))
		if item.Active {
			def = i
		}
	}
	options = append(options, "＋ 新建会话")
	m.menu = NewMenuModel("选择会话", options, def, nil)
	m.resizeMenu()
	m.mode = modeSessions
}

func (m Model) updateSessions(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, _ := m.menu.Update(msg)
	m.menu = updated.(MenuModel)
	if !m.menu.Done() {
		return m, nil
	}
	if m.menu.Err() != nil {
		m.mode = modeInput
		m.appendEntry("status", "未切换会话。")
		return m, nil
	}
	selected := m.menu.Selected()
	if selected == len(m.sessions) {
		info, err := m.app.CreateSession("")
		if err != nil {
			m.appendEntry("error", err.Error())
		} else {
			m.transcript = nil
			m.appendEntry("status", fmt.Sprintf("已新建并切换到会话：%s [%s]", info.Name, info.ShortID()))
		}
	} else if selected >= 0 && selected < len(m.sessions) {
		m.switchSession(m.sessions[selected].ID)
	}
	m.mode = modeInput
	return m, nil
}

func (m *Model) switchSession(query string) {
	info, err := m.app.SwitchSession(query)
	if err != nil {
		m.appendEntry("error", err.Error())
		return
	}
	m.transcript = nil
	m.appendEntry("status", fmt.Sprintf("已切换到会话：%s [%s]，%d 条消息，约 %d tokens",
		info.Name, info.ShortID(), m.app.sess.Len(), m.app.sess.EstimatedTokens()))
}

func (m *Model) showHistory() {
	messages := m.app.sess.Messages()
	if len(messages) == 0 {
		m.appendEntry("status", "历史为空。")
		return
	}
	m.appendEntry("status", fmt.Sprintf("当前会话历史（%d 条）", len(messages)))
	for _, message := range messages {
		role := "assistant"
		if message.Role == schema.User {
			role = "user"
		}
		m.appendEntry(role, messageSummary(message))
	}
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

func (m *Model) appendEntry(role, content string) {
	m.transcript = append(m.transcript, transcriptEntry{role: role, content: content})
	m.scrollOffset = 0
}

func (m Model) waitForEvent() tea.Cmd {
	if m.events == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case event := <-m.events:
			return event
		case <-m.ctx.Done():
			return uiContextDoneMsg{}
		}
	}
}

func sendUIEvent(ctx context.Context, events chan tea.Msg, msg tea.Msg) bool {
	if events == nil {
		return false
	}
	select {
	case events <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}

type uiLogWriter struct {
	ctx    context.Context
	events chan tea.Msg
}

func (w uiLogWriter) Write(p []byte) (int, error) {
	sendUIEvent(w.ctx, w.events, uiLogMsg{line: string(p)})
	return len(p), nil
}

func (m *Model) resizeMenu() {
	height := m.height - 5
	if height < 2 {
		height = 2
	}
	updated, _ := m.menu.Update(tea.WindowSizeMsg{Width: m.viewWidth(), Height: height})
	m.menu = updated.(MenuModel)
}

func (m *Model) resizeInput() {
	updated, _ := m.input.Update(tea.WindowSizeMsg{Width: m.viewWidth(), Height: m.height})
	m.input = updated.(LineInputModel)
}

func (m Model) maxInputBytes() int {
	if m.app != nil && m.app.inputMaxBytes > 0 {
		return m.app.inputMaxBytes
	}
	return 1024 * 1024
}

func (m Model) View() string {
	width := m.viewWidth()
	header := m.renderHeader(width)
	footer := m.renderFooter(width)
	body := m.renderBody(width)

	if m.height > 0 {
		available := m.height - lineCount(header) - lineCount(footer)
		if available < 1 {
			available = 1
		}
		body = visibleTailLines(body, available, m.scrollOffset)
	}
	return header + body + footer
}

func (m Model) renderHeader(width int) string {
	title := headerStyle.Render("CDN 诊断助手")
	sessionText := ""
	if m.app != nil && m.app.sessions != nil {
		_, info := m.app.sessions.Current()
		sessionText = fmt.Sprintf("会话 %s [%s]", info.Name, info.ShortID())
	}
	state := "就绪"
	if m.mode == modeBusy {
		state = "处理中"
	} else if m.mode == modeApproval || m.mode == modeRejectReason {
		state = "等待人工审核"
	} else if m.mode == modeSessions {
		state = "选择会话"
	}
	meta := metaStyle.Render(strings.TrimSpace(sessionText + "  ·  " + state))
	rule := metaStyle.Render(strings.Repeat("─", max(1, width)))
	return title + "\n" + meta + "\n" + rule + "\n"
}

func (m Model) renderBody(width int) string {
	var body strings.Builder
	for _, entry := range m.transcript {
		body.WriteString(renderEntry(entry, width))
	}
	if m.streaming != "" {
		body.WriteString(renderEntry(transcriptEntry{role: "assistant", content: m.streaming}, width))
	}
	switch m.mode {
	case modeApproval:
		body.WriteString(renderEntry(transcriptEntry{role: "approval", content: ApprovalCard(
			m.approval.proposal, m.approval.risk)}, width))
		body.WriteString(m.menu.View())
	case modeSessions:
		body.WriteString(m.menu.View())
	}
	return body.String()
}

func (m Model) renderFooter(width int) string {
	rule := metaStyle.Render(strings.Repeat("─", max(1, width))) + "\n"
	switch m.mode {
	case modeInput:
		return rule + m.input.View() + "\n" + metaStyle.Render(TruncateCells("Enter 发送 · PgUp/PgDn 滚动 · /help 命令 · Ctrl-C 退出", width)) + "\n"
	case modeBusy:
		return rule + statusStyle.Render("正在处理，请稍候 · Ctrl-C 取消并退出") + "\n"
	case modeApproval:
		return rule + statusStyle.Render("请核对命令原文后明确选择") + "\n"
	case modeRejectReason:
		return rule + m.input.View() + "\n" + metaStyle.Render(TruncateCells("Enter 提交理由 · Esc 不填写理由", width)) + "\n"
	case modeSessions:
		return rule + metaStyle.Render("Esc 返回") + "\n"
	default:
		return rule
	}
}

func renderEntry(entry transcriptEntry, width int) string {
	label, style := entryLabel(entry.role)
	labelWidth := lipgloss.Width(label)
	contentWidth := width - labelWidth - 1
	if contentWidth < 8 {
		contentWidth = max(1, width)
		label = ""
		labelWidth = 0
	}
	lines := wrapCells(entry.content, contentWidth)
	if len(lines) == 0 {
		lines = []string{""}
	}

	var out strings.Builder
	for i, line := range lines {
		if i == 0 && label != "" {
			out.WriteString(style.Render(label))
			out.WriteByte(' ')
		} else if labelWidth > 0 {
			out.WriteString(strings.Repeat(" ", labelWidth+1))
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	return out.String()
}

func entryLabel(role string) (string, lipgloss.Style) {
	switch role {
	case "user":
		return "你 >", userStyle
	case "assistant":
		return "助手 >", agentStyle
	case "intent":
		return "意图 >", intentStyle
	case "error":
		return "错误 >", errorStyle
	case "status":
		return "状态 >", statusStyle
	case "approval":
		return "审批 >", statusStyle.Bold(true)
	case "log":
		return "日志 >", metaStyle
	default:
		return "系统 >", metaStyle
	}
}

func wrapCells(text string, width int) []string {
	if width <= 0 {
		return []string{text}
	}
	var lines []string
	var line strings.Builder
	used := 0
	flush := func() {
		lines = append(lines, line.String())
		line.Reset()
		used = 0
	}
	for _, r := range text {
		if r == '\n' {
			flush()
			continue
		}
		cellWidth := lipgloss.Width(string(r))
		if used > 0 && used+cellWidth > width {
			flush()
		}
		line.WriteRune(r)
		used += cellWidth
	}
	if line.Len() > 0 || len(lines) == 0 {
		flush()
	}
	return lines
}

func visibleTailLines(text string, limit, offset int) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	end := len(lines) - offset
	if end < limit {
		end = min(len(lines), limit)
	}
	if end > len(lines) {
		end = len(lines)
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:end], "\n") + "\n"
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n")
}

func (m Model) viewWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 100
}

func helpText() string {
	return "直接输入问题即可。命令：/sessions 选择会话 | /new [名称] 新建 | " +
		"/switch ID 切换 | /image <路径或 URL> [问题] | /history | /reset | /help | /exit"
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
		fmt.Fprintf(&out, "assistant: %s\n", m.streaming)
	}
	return out.String()
}
