package chat

import (
	"context"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const inputPromptStyled = "\033[36m你 >\033[0m "

// LineInputModel 是按 rune 编辑的单行终端输入框。终端规范模式会按字节删除
// UTF-8 中文字符；Bubble Tea 的 KeyRunes 可以保证一次退格删除一个完整字符。
type LineInputModel struct {
	value    []rune
	cursor   int
	maxBytes int
	prompt   string
	width    int
	done     bool
	err      error
}

func NewLineInputModel(maxBytes int) LineInputModel {
	return LineInputModel{maxBytes: maxBytes, prompt: inputPromptStyled}
}

func (m LineInputModel) Init() tea.Cmd { return nil }

func (m LineInputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch key.Type {
	case tea.KeyEnter, tea.KeyCtrlJ:
		m.done = true
		return m, nil
	case tea.KeyCtrlC:
		m.err = context.Canceled
		m.done = true
		return m, nil
	case tea.KeyCtrlD:
		if len(m.value) == 0 {
			m.err = context.Canceled
			m.done = true
			return m, nil
		}
		m.deleteAtCursor()
	case tea.KeyBackspace, tea.KeyCtrlH:
		m.deleteBeforeCursor()
	case tea.KeyDelete:
		m.deleteAtCursor()
	case tea.KeyLeft, tea.KeyCtrlB:
		if m.cursor > 0 {
			m.cursor--
		}
	case tea.KeyRight, tea.KeyCtrlF:
		if m.cursor < len(m.value) {
			m.cursor++
		}
	case tea.KeyHome, tea.KeyCtrlA:
		m.cursor = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		m.cursor = len(m.value)
	case tea.KeyCtrlLeft:
		m.cursor = previousWord(m.value, m.cursor)
	case tea.KeyCtrlRight:
		m.cursor = nextWord(m.value, m.cursor)
	case tea.KeyCtrlW:
		start := previousWord(m.value, m.cursor)
		m.value = deleteRunes(m.value, start, m.cursor)
		m.cursor = start
	case tea.KeyCtrlU:
		m.value = deleteRunes(m.value, 0, m.cursor)
		m.cursor = 0
	case tea.KeyCtrlK:
		m.value = deleteRunes(m.value, m.cursor, len(m.value))
	case tea.KeyRunes, tea.KeySpace:
		m.insert(key.Runes)
	}
	return m, nil
}

func (m LineInputModel) View() string {
	if m.done {
		if m.err != nil {
			return ""
		}
		return m.prompt + string(m.value) + "\n"
	}

	start, end := m.visibleRange()
	before := string(m.value[start:m.cursor])
	after := ""
	cursor := " "
	if m.cursor < len(m.value) {
		cursor = string(m.value[m.cursor])
		after = string(m.value[m.cursor+1 : end])
	}
	return m.prompt + before + "\033[7m" + cursor + "\033[0m" + after
}

func (m LineInputModel) Value() string { return string(m.value) }
func (m LineInputModel) Cursor() int   { return m.cursor }
func (m LineInputModel) Done() bool    { return m.done }
func (m LineInputModel) Err() error    { return m.err }

// WithPrompt 返回使用指定提示词的输入框副本。
func (m LineInputModel) WithPrompt(prompt string) LineInputModel {
	m.prompt = prompt
	return m
}

func (m LineInputModel) visibleRange() (int, int) {
	if m.width <= 0 {
		return 0, len(m.value)
	}
	available := m.width - lipgloss.Width(m.prompt)
	if available < 1 {
		available = 1
	}
	cursorWidth := 1
	if m.cursor < len(m.value) {
		cursorWidth = lipgloss.Width(string(m.value[m.cursor]))
	}
	start := 0
	for start < m.cursor && lipgloss.Width(string(m.value[start:m.cursor]))+cursorWidth > available {
		start++
	}
	used := lipgloss.Width(string(m.value[start:m.cursor])) + cursorWidth
	end := m.cursor
	if m.cursor < len(m.value) {
		end++
	}
	for end < len(m.value) {
		width := lipgloss.Width(string(m.value[end]))
		if used+width > available {
			break
		}
		used += width
		end++
	}
	return start, end
}

func (m *LineInputModel) insert(runes []rune) {
	accepted := make([]rune, 0, len(runes))
	remaining := m.maxBytes
	if m.maxBytes > 0 {
		remaining -= len(string(m.value))
	}
	for _, r := range runes {
		// 粘贴多行文本时保持单行语义，避免一段粘贴意外提交多轮请求。
		if r == '\r' || r == '\n' {
			r = ' '
		}
		if m.maxBytes > 0 {
			size := utf8.RuneLen(r)
			if size > remaining {
				break
			}
			remaining -= size
		}
		accepted = append(accepted, r)
	}
	if len(accepted) == 0 {
		return
	}
	candidate := make([]rune, 0, len(m.value)+len(accepted))
	candidate = append(candidate, m.value[:m.cursor]...)
	candidate = append(candidate, accepted...)
	candidate = append(candidate, m.value[m.cursor:]...)
	m.value = candidate
	m.cursor += len(accepted)
}

func (m *LineInputModel) deleteBeforeCursor() {
	if m.cursor == 0 {
		return
	}
	m.value = deleteRunes(m.value, m.cursor-1, m.cursor)
	m.cursor--
}

func (m *LineInputModel) deleteAtCursor() {
	if m.cursor >= len(m.value) {
		return
	}
	m.value = deleteRunes(m.value, m.cursor, m.cursor+1)
}

func deleteRunes(value []rune, start, end int) []rune {
	result := make([]rune, 0, len(value)-(end-start))
	result = append(result, value[:start]...)
	return append(result, value[end:]...)
}

func previousWord(value []rune, cursor int) int {
	for cursor > 0 && unicode.IsSpace(value[cursor-1]) {
		cursor--
	}
	for cursor > 0 && !unicode.IsSpace(value[cursor-1]) {
		cursor--
	}
	return cursor
}

func nextWord(value []rune, cursor int) int {
	for cursor < len(value) && !unicode.IsSpace(value[cursor]) {
		cursor++
	}
	for cursor < len(value) && unicode.IsSpace(value[cursor]) {
		cursor++
	}
	return cursor
}
