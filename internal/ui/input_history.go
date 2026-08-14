package ui

import tea "github.com/charmbracelet/bubbletea"

const defaultInputHistoryKey = "__default__"

func (m Model) inputHistoryKey() string {
	if m.app == nil {
		return defaultInputHistoryKey
	}
	if id := m.app.CurrentSessionInfo().ID; id != "" {
		return id
	}
	return defaultInputHistoryKey
}

func (m *Model) rememberInput(line string) {
	key := m.inputHistoryKey()
	history := m.inputHistory[key]
	if len(history) == 0 || history[len(history)-1] != line {
		m.inputHistory[key] = append(history, line)
	}
	m.resetHistoryNavigation()
}

func (m *Model) previousInput() {
	history := m.inputHistory[m.inputHistoryKey()]
	if len(history) == 0 {
		return
	}
	if m.historyIndex < 0 {
		m.historyDraft = m.input.Value()
		m.historyIndex = len(history) - 1
	} else if m.historyIndex > 0 {
		m.historyIndex--
	}
	m.input = m.input.WithValue(history[m.historyIndex])
}

func (m *Model) nextInput() {
	if m.historyIndex < 0 {
		return
	}
	history := m.inputHistory[m.inputHistoryKey()]
	if m.historyIndex < len(history)-1 {
		m.historyIndex++
		m.input = m.input.WithValue(history[m.historyIndex])
		return
	}
	m.input = m.input.WithValue(m.historyDraft)
	m.resetHistoryNavigation()
}

func (m *Model) resetHistoryNavigation() {
	m.historyIndex = -1
	m.historyDraft = ""
}

func mutatesInput(key tea.KeyMsg) bool {
	switch key.Type {
	case tea.KeyRunes, tea.KeySpace, tea.KeyBackspace, tea.KeyCtrlH, tea.KeyDelete,
		tea.KeyCtrlD, tea.KeyCtrlW, tea.KeyCtrlU, tea.KeyCtrlK:
		return true
	default:
		return false
	}
}
