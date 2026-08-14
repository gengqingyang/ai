// Package input 提供可嵌入 Bubble Tea 根模型的 Unicode 单行输入框。
package input

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const defaultPrompt = "› "

var (
	textStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#303030"))
	cursorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#303030")).
			Background(lipgloss.Color("#A1A1AA"))
)

type layout struct {
	contentWidth int
	padding      int
}

// Model 按 rune 编辑单行输入，避免 UTF-8 中文被按字节删除。
type Model struct {
	value      []rune
	cursor     int
	maxBytes   int
	prompt     string
	width      int
	done       bool
	err        error
	hideCursor bool
}

// New 创建输入框。
func New(maxBytes int) Model {
	return Model{maxBytes: maxBytes, prompt: defaultPrompt}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m Model) View() string {
	if m.hideCursor {
		return m.renderCompleted()
	}
	if m.done {
		if m.err != nil {
			return ""
		}
		return m.renderCompleted()
	}

	layout := m.layout()
	cursor := " "
	if m.cursor < len(m.value) {
		cursor = string(m.value[m.cursor])
	}
	cursorWidth := lipgloss.Width(cursor)
	if cursorWidth < 1 || cursorWidth > layout.contentWidth {
		cursor = " "
		cursorWidth = 1
	}
	prompt := truncateCells(m.prompt, max(0, layout.contentWidth-cursorWidth))
	available := layout.contentWidth - lipgloss.Width(prompt)
	start, end := m.visibleRange(available, cursorWidth)
	before := string(m.value[start:m.cursor])
	after := ""
	if m.cursor < len(m.value) {
		after = string(m.value[m.cursor+1 : end])
	}

	used := lipgloss.Width(prompt+before) + cursorWidth + lipgloss.Width(after)
	left := strings.Repeat(" ", layout.padding) + prompt + before
	right := after + strings.Repeat(" ", max(0, layout.contentWidth-used)+layout.padding)
	line := textStyle.Render(left) + cursorStyle.Render(cursor) + textStyle.Render(right)
	return renderBar(line, layout)
}

func (m Model) Value() string { return string(m.value) }
func (m Model) Cursor() int   { return m.cursor }
func (m Model) Done() bool    { return m.done }
func (m Model) Err() error    { return m.err }

// WithPrompt 返回使用指定提示词的输入框副本。
func (m Model) WithPrompt(prompt string) Model {
	m.prompt = prompt
	return m
}

// WithValue 返回内容替换为 value 的输入框副本，并将光标移到末尾。
func (m Model) WithValue(value string) Model {
	m.value = nil
	m.cursor = 0
	m.done = false
	m.err = nil
	m.insert([]rune(value))
	return m
}

// WithoutCursor 返回隐藏光标的输入框副本，用于展示不可编辑的忙碌状态。
func (m Model) WithoutCursor() Model {
	m.hideCursor = true
	return m
}

func (m Model) visibleRange(available, cursorWidth int) (int, int) {
	available = max(available, cursorWidth)
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

func (m Model) renderCompleted() string {
	layout := m.layout()
	prompt := truncateCells(m.prompt, layout.contentWidth)
	available := layout.contentWidth - lipgloss.Width(prompt)
	start := len(m.value)
	for start > 0 && lipgloss.Width(string(m.value[start-1:])) <= available {
		start--
	}
	value := string(m.value[start:])
	used := lipgloss.Width(prompt + value)
	line := strings.Repeat(" ", layout.padding) + prompt + value +
		strings.Repeat(" ", max(0, layout.contentWidth-used)+layout.padding)
	return renderBar(textStyle.Render(line), layout)
}

func (m Model) layout() layout {
	if m.width <= 0 {
		contentWidth := lipgloss.Width(m.prompt) + lipgloss.Width(string(m.value)) + 1
		return layout{contentWidth: max(1, contentWidth), padding: 1}
	}
	if m.width >= 3 {
		return layout{contentWidth: m.width - 2, padding: 1}
	}
	return layout{contentWidth: max(1, m.width)}
}

func renderBar(line string, layout layout) string {
	width := layout.contentWidth + 2*layout.padding
	spacer := textStyle.Render(strings.Repeat(" ", width))
	return spacer + "\n" + line + "\n" + spacer
}

func truncateCells(text string, width int) string {
	if width <= 0 {
		return ""
	}
	var result strings.Builder
	used := 0
	for _, r := range text {
		cellWidth := lipgloss.Width(string(r))
		if used+cellWidth > width {
			break
		}
		result.WriteRune(r)
		used += cellWidth
	}
	return result.String()
}

func (m *Model) insert(runes []rune) {
	accepted := make([]rune, 0, len(runes))
	remaining := m.maxBytes
	if m.maxBytes > 0 {
		remaining -= len(string(m.value))
	}
	for _, r := range runes {
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

func (m *Model) deleteBeforeCursor() {
	if m.cursor == 0 {
		return
	}
	m.value = deleteRunes(m.value, m.cursor-1, m.cursor)
	m.cursor--
}

func (m *Model) deleteAtCursor() {
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
