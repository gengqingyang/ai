package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"diagnostic-system/internal/ui/menu"
)

func (m Model) View() string {
	width := max(1, m.viewWidth())
	header := m.renderHeader(width)
	footer := m.renderFooter(width)
	body := m.renderBody(width)

	if m.height > 0 {
		available := max(0, m.height-lineCount(header)-lineCount(footer))
		body = visibleTailLines(body, available, m.scrollOffset)
	}
	return header + body + footer
}

func (m Model) renderHeader(width int) string {
	title := headerStyle.Render(menu.TruncateCells("CDN 诊断助手", width))
	sessionText := ""
	if m.app != nil {
		info := m.app.CurrentSessionInfo()
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
	meta := metaStyle.Render(menu.TruncateCells(strings.TrimSpace(sessionText+"  ·  "+state), width))
	rule := metaStyle.Render(strings.Repeat("─", max(1, width)))
	return title + "\n" + meta + "\n" + rule + "\n"
}

func (m Model) renderBody(width int) string {
	var body strings.Builder
	for _, entry := range m.transcript {
		body.WriteString(renderEntry(entry, width))
	}
	if m.streaming != "" {
		body.WriteString(renderEntry(transcriptEntry{role: roleAssistant, content: m.streaming}, width))
	}
	switch m.mode {
	case modeApproval:
		body.WriteString(renderEntry(transcriptEntry{role: roleApproval, content: ApprovalCard(
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
		return rule + m.input.View() + "\n" + metaStyle.Render(menu.TruncateCells(
			"Enter 发送 · PgUp/PgDn 滚动 · /help 命令 · Ctrl-C 退出", width)) + "\n"
	case modeBusy:
		return rule + statusStyle.Render(menu.TruncateCells(
			"正在处理，请稍候 · Ctrl-C 取消并退出", width)) + "\n"
	case modeApproval:
		return rule + statusStyle.Render(menu.TruncateCells("请核对命令原文后明确选择", width)) + "\n"
	case modeRejectReason:
		return rule + m.input.View() + "\n" + metaStyle.Render(menu.TruncateCells(
			"Enter 提交理由 · Esc 不填写理由", width)) + "\n"
	case modeSessions:
		return rule + metaStyle.Render(menu.TruncateCells("Esc 返回", width)) + "\n"
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
	for index, line := range lines {
		if index == 0 && label != "" {
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

func entryLabel(role transcriptRole) (string, lipgloss.Style) {
	switch role {
	case roleUser:
		return "你 >", userStyle
	case roleAssistant:
		return "助手 >", agentStyle
	case roleIntent:
		return "意图 >", intentStyle
	case roleError:
		return "错误 >", errorStyle
	case roleStatus:
		return "状态 >", statusStyle
	case roleApproval:
		return "审批 >", statusStyle.Bold(true)
	case roleLog:
		return "日志 >", metaStyle
	case roleSystem:
		return "系统 >", metaStyle
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
	if limit <= 0 {
		return ""
	}
	if text == "" {
		return strings.Repeat("\n", limit)
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	end := max(0, len(lines)-max(0, offset))
	if end < limit {
		end = min(len(lines), limit)
	}
	end = min(end, len(lines))
	start := max(0, end-limit)
	visible := lines[start:end]
	return strings.Join(visible, "\n") + "\n" + strings.Repeat("\n", limit-len(visible))
}

func lineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n")
}
