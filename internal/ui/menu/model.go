// Package menu 提供可嵌入 Bubble Tea 根模型的选择菜单。
package menu

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	ErrInterrupted = errors.New("确认被中断")
	ErrEOF         = errors.New("确认输入已结束")

	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F4F4F5"))
	normalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#A1A1AA"))
	runStyle    = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("#ECFDF5")).Background(lipgloss.Color("#166534"))
	dangerStyle = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("#FEF2F2")).Background(lipgloss.Color("#991B1B"))
	helpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#71717A"))
)

// Model 是嵌入式选择菜单。selected 只在用户明确确认后赋值。
type Model struct {
	prompt    string
	options   []string
	shortcuts map[byte]int
	cursor    int
	selected  int
	danger    int
	width     int
	height    int
	done      bool
	err       error
}

// New 创建菜单。
func New(prompt string, options []string, defaultIndex int, shortcuts map[byte]int) Model {
	if defaultIndex < 0 || defaultIndex >= len(options) {
		defaultIndex = 0
	}
	return Model{
		prompt:    prompt,
		options:   append([]string(nil), options...),
		shortcuts: shortcuts,
		cursor:    defaultIndex,
		selected:  -1,
		danger:    -1,
	}
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.done {
		return m, nil
	}
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		m.height = size.Height
		return m, nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok || len(m.options) == 0 {
		return m, nil
	}
	switch key.String() {
	case "up", "left", "shift+tab", "k":
		m.cursor = (m.cursor - 1 + len(m.options)) % len(m.options)
	case "down", "right", "tab", "j":
		m.cursor = (m.cursor + 1) % len(m.options)
	case "enter":
		m.selected = m.cursor
		m.done = true
	case "ctrl+c", "esc", "q":
		m.err = ErrInterrupted
		m.done = true
	case "ctrl+d":
		m.err = ErrEOF
		m.done = true
	default:
		pressed := key.String()
		if len(pressed) == 1 {
			if selected, exists := m.shortcuts[pressed[0]]; exists && selected >= 0 && selected < len(m.options) {
				m.cursor = selected
				m.selected = selected
				m.done = true
			}
		}
	}
	return m, nil
}

func (m Model) View() string {
	if m.done || len(m.options) == 0 {
		return ""
	}

	indentWidth := 2
	contentWidth := 0
	if m.width > 0 {
		indentWidth = min(indentWidth, max(0, m.width-1))
		contentWidth = max(1, m.width-indentWidth)
	}
	indent := strings.Repeat(" ", indentWidth)

	rowWidth := 0
	for _, option := range m.options {
		if width := lipgloss.Width(option); width > rowWidth {
			rowWidth = width
		}
	}
	rowWidth += 4
	minimumRowWidth := 8
	if contentWidth > 0 {
		rowWidth = min(rowWidth, contentWidth)
		minimumRowWidth = min(minimumRowWidth, contentWidth)
	}
	rowWidth = max(rowWidth, minimumRowWidth)

	limit := min(len(m.options), 12)
	if m.height > 0 {
		available := max(1, m.height-4)
		limit = min(limit, available)
	}
	start := max(0, m.cursor-limit/2)
	if start+limit > len(m.options) {
		start = len(m.options) - limit
	}
	end := start + limit

	var view strings.Builder
	view.WriteString(indent)
	title := m.prompt
	if contentWidth > 0 {
		title = TruncateCells(title, contentWidth)
	}
	view.WriteString(titleStyle.Render(title))
	view.WriteString("\n\n")
	for index := start; index < end; index++ {
		prefix := "  "
		style := normalStyle
		if index == m.cursor {
			prefix = "› "
			style = runStyle
			if index == m.danger {
				style = dangerStyle
			}
		}
		if rowWidth == 1 {
			prefix = " "
			if index == m.cursor {
				prefix = "›"
			}
		}
		option := TruncateCells(m.options[index], max(0, rowWidth-lipgloss.Width(prefix)))
		row := prefix + option
		view.WriteString(indent)
		view.WriteString(style.Width(rowWidth).Render(row))
		view.WriteByte('\n')
	}
	view.WriteByte('\n')
	view.WriteString(indent)
	help := "↑/↓ 移动 · Enter 确认 · Esc 取消"
	if len(m.shortcuts) > 0 {
		help = "↑/↓ 移动 · Enter 确认 · y/n 快捷选择 · Esc 取消"
	}
	if len(m.options) > limit {
		help = fmt.Sprintf("%d/%d · %s", m.cursor+1, len(m.options), help)
	}
	if contentWidth > 0 {
		help = TruncateCells(help, contentWidth)
	}
	view.WriteString(helpStyle.Render(help))
	view.WriteByte('\n')
	return view.String()
}

func (m Model) Cursor() int   { return m.cursor }
func (m Model) Selected() int { return m.selected }
func (m Model) Done() bool    { return m.done }
func (m Model) Err() error    { return m.err }

func (m Model) WithCursor(cursor int) Model {
	if cursor >= 0 && cursor < len(m.options) {
		m.cursor = cursor
	}
	return m
}

func (m Model) WithDanger(danger int) Model {
	if danger >= 0 && danger < len(m.options) {
		m.danger = danger
	}
	return m
}

// TruncateCells 按终端单元格宽度截断字符串。
func TruncateCells(text string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= maxWidth {
		return text
	}
	if maxWidth == 1 {
		return "…"
	}

	var out strings.Builder
	used := 0
	for _, r := range text {
		width := lipgloss.Width(string(r))
		if used+width > maxWidth-1 {
			break
		}
		out.WriteRune(r)
		used += width
	}
	out.WriteRune('…')
	return out.String()
}
