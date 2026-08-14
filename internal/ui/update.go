package ui

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	uiinput "diagnostic-system/internal/ui/input"
)

// Update 路由后台事件和当前交互模式；只有根模型能退出程序。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch event := msg.(type) {
	case intentMsg:
		suffix := ""
		if event.result.NeedsClarification {
			suffix = " · 需澄清"
		}
		m.appendEntry(roleIntent, fmt.Sprintf("%s · %d%%%s",
			event.result.Intent.Label(), int(event.result.Confidence*100+0.5), suffix))
		return m, m.waitForEvent()
	case agentProgressMsg:
		if strings.TrimSpace(event.text) != "" {
			m.flushStreaming()
			m.appendEntry(roleProgress, strings.TrimSpace(event.text))
		}
		return m, m.waitForEvent()
	case reasoningChunkMsg:
		m.appendStreaming(roleReasoning, event.chunk)
		return m, m.waitForEvent()
	case assistantChunkMsg:
		m.assistantStreamed = true
		m.appendStreaming(roleAssistant, event.chunk)
		return m, m.waitForEvent()
	case logMsg:
		if strings.TrimSpace(event.line) != "" {
			m.appendEntry(roleLog, strings.TrimSpace(event.line))
		}
		return m, m.waitForEvent()
	case approvalRequestMsg:
		if m.approval != nil {
			event.result <- approvalResult{err: errors.New("已有待处理的人工审核")}
			return m, m.waitForEvent()
		}
		m.openApproval(event)
		return m, m.waitForEvent()
	case executionNoticeMsg:
		m.handleExecutionNotice(event.proposal)
		return m, m.waitForEvent()
	case askDoneMsg:
		m.finishAsk(event)
		return m, m.waitForEvent()
	case contextDoneMsg:
		m.cancelAll()
		return m, tea.Quit
	}

	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		m.height = size.Height
		updated, _ := m.input.Update(size)
		m.input = updated.(uiinput.Model)
		m.resizeMenu()
		m.clampScrollOffset()
		return m, nil
	}

	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyCtrlC:
			m.cancelAll()
			return m, tea.Quit
		case tea.KeyF2:
			m.copyMode = !m.copyMode
			m.scrollDragging = false
			if m.copyMode {
				return m, tea.DisableMouse
			}
			return m, tea.EnableMouseCellMotion
		case tea.KeyEsc:
			if m.copyMode {
				m.copyMode = false
				return m, tea.EnableMouseCellMotion
			}
		case tea.KeyPgUp:
			m.scrollBy(m.scrollPageSize())
			return m, nil
		case tea.KeyPgDown:
			m.scrollBy(-m.scrollPageSize())
			return m, nil
		}
	}
	if mouse, ok := msg.(tea.MouseMsg); ok {
		if m.copyMode || m.updateScrollMouse(mouse) {
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
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyUp:
			m.previousInput()
			return m, nil
		case tea.KeyDown:
			m.nextInput()
			return m, nil
		default:
			if mutatesInput(key) {
				m.resetHistoryNavigation()
			}
		}
	}
	updated, _ := m.input.Update(msg)
	m.input = updated.(uiinput.Model)
	if !m.input.Done() {
		return m, nil
	}
	if m.input.Err() != nil {
		m.cancelAll()
		return m, tea.Quit
	}
	line := strings.TrimSpace(m.input.Value())
	m.input = uiinput.New(m.inputMaxBytes)
	m.resizeInput()
	if line == "" {
		return m, nil
	}
	m.rememberInput(line)
	return m.submit(line)
}
