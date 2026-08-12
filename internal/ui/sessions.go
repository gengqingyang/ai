package ui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/session"
	"diagnostic-system/internal/ui/menu"
)

func (m *Model) openSessions() {
	m.sessions = m.app.Sessions()
	options := make([]string, 0, len(m.sessions)+1)
	defaultIndex := 0
	for index, item := range m.sessions {
		options = append(options, sessionOption(item))
		if item.Active {
			defaultIndex = index
		}
	}
	options = append(options, "＋ 新建会话")
	m.menu = menu.New("选择会话", options, defaultIndex, nil)
	m.resizeMenu()
	m.mode = modeSessions
}

func (m Model) updateSessions(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, _ := m.menu.Update(msg)
	m.menu = updated.(menu.Model)
	if !m.menu.Done() {
		return m, nil
	}
	if m.menu.Err() != nil {
		m.mode = modeInput
		m.appendEntry(roleStatus, "未切换会话。")
		return m, nil
	}
	selected := m.menu.Selected()
	if selected == len(m.sessions) {
		info, err := m.app.CreateSession("")
		if err != nil {
			m.appendEntry(roleError, err.Error())
		} else {
			m.transcript = nil
			m.appendEntry(roleStatus, fmt.Sprintf("已新建并切换到会话：%s [%s]", info.Name, info.ShortID()))
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
		m.appendEntry(roleError, err.Error())
		return
	}
	messages, tokens := m.app.CurrentSessionStats()
	m.transcript = nil
	m.appendEntry(roleStatus, fmt.Sprintf("已切换到会话：%s [%s]，%d 条消息，约 %d tokens",
		info.Name, info.ShortID(), messages, tokens))
}

func (m *Model) showHistory() {
	messages := m.app.History()
	if len(messages) == 0 {
		m.appendEntry(roleStatus, "历史为空。")
		return
	}
	m.appendEntry(roleStatus, fmt.Sprintf("当前会话历史（%d 条）", len(messages)))
	for _, message := range messages {
		role := roleAssistant
		if message.Role == schema.User {
			role = roleUser
		}
		m.appendEntry(role, historyMessageContent(message))
	}
}

func historyMessageContent(message *schema.Message) string {
	return messageImagePrefix(message) + message.Content
}

func sessionOption(info session.Info) string {
	marker := "  "
	if info.Active {
		marker = "● "
	}
	return fmt.Sprintf("%s%s  ·  %s  ·  %s",
		marker, info.Name, info.UpdatedAt.Local().Format("01-02 15:04"), info.ShortID())
}
