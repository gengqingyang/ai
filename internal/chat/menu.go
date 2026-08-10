package chat

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	ErrMenuInterrupted = errors.New("确认被中断")
	ErrMenuEOF         = errors.New("确认输入已结束")

	menuTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F4F4F5"))
	menuNormalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A1A1AA"))
	menuRunStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ECFDF5")).
			Background(lipgloss.Color("#166534"))
	menuDenyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FEF2F2")).
			Background(lipgloss.Color("#991B1B"))
	menuHelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#71717A"))
)

// MenuModel 是 Bubble Tea 驱动的确认菜单。selected 只在用户明确确认后赋值。
type MenuModel struct {
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

func NewMenuModel(prompt string, options []string, def int, shortcuts map[byte]int) MenuModel {
	if def < 0 || def >= len(options) {
		def = 0
	}
	return MenuModel{
		prompt:    prompt,
		options:   append([]string(nil), options...),
		shortcuts: shortcuts,
		cursor:    def,
		selected:  -1,
		danger:    -1,
	}
}

func (m MenuModel) Init() tea.Cmd { return nil }

func (m MenuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		return m, nil
	case "ctrl+c", "esc", "q":
		m.err = ErrMenuInterrupted
		m.done = true
		return m, nil
	case "ctrl+d":
		m.err = ErrMenuEOF
		m.done = true
		return m, nil
	default:
		pressed := key.String()
		if len(pressed) == 1 {
			if selected, exists := m.shortcuts[pressed[0]]; exists && selected >= 0 && selected < len(m.options) {
				m.cursor = selected
				m.selected = selected
				m.done = true
				return m, nil
			}
		}
	}
	return m, nil
}

func (m MenuModel) View() string {
	if m.done || len(m.options) == 0 {
		return ""
	}

	rowWidth := 0
	for _, option := range m.options {
		if w := lipgloss.Width(option); w > rowWidth {
			rowWidth = w
		}
	}
	rowWidth += 4
	if m.width > 0 && rowWidth > m.width-4 {
		rowWidth = m.width - 4
	}
	if rowWidth < 8 {
		rowWidth = 8
	}

	limit := len(m.options)
	if limit > 12 {
		limit = 12
	}
	if m.height > 0 {
		available := m.height - 7
		if available < 2 {
			available = 2
		}
		if limit > available {
			limit = available
		}
	}
	start := m.cursor - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > len(m.options) {
		start = len(m.options) - limit
	}
	end := start + limit

	var view strings.Builder
	view.WriteString("  ")
	view.WriteString(menuTitleStyle.Render(m.prompt))
	view.WriteString("\n\n")
	for i := start; i < end; i++ {
		option := TruncateCells(m.options[i], rowWidth-2)
		row := "  " + option
		style := menuNormalStyle
		if i == m.cursor {
			row = "› " + option
			style = menuRunStyle
			if i == m.danger {
				style = menuDenyStyle
			}
		}
		view.WriteString("  ")
		view.WriteString(style.Width(rowWidth).Render(row))
		view.WriteByte('\n')
	}
	view.WriteByte('\n')
	view.WriteString("  ")
	help := "↑/↓ 移动 · Enter 确认 · Esc 取消"
	if len(m.shortcuts) > 0 {
		help = "↑/↓ 移动 · Enter 确认 · y/n 快捷选择 · Esc 取消"
	}
	if len(m.options) > limit {
		help = fmt.Sprintf("%d/%d · %s", m.cursor+1, len(m.options), help)
	}
	if m.width > 0 {
		help = TruncateCells(help, m.width-4)
	}
	view.WriteString(menuHelpStyle.Render(help))
	view.WriteByte('\n')
	return view.String()
}

// Cursor 返回当前光标位置。
func (m MenuModel) Cursor() int { return m.cursor }

// Selected 返回已确认项；尚未确认时返回 -1。
func (m MenuModel) Selected() int { return m.selected }

// Done 报告菜单是否已经结束。
func (m MenuModel) Done() bool { return m.done }

// Err 返回菜单结束原因。
func (m MenuModel) Err() error { return m.err }

// WithCursor 返回将光标移动到指定项后的菜单副本。
func (m MenuModel) WithCursor(cursor int) MenuModel {
	if cursor >= 0 && cursor < len(m.options) {
		m.cursor = cursor
	}
	return m
}

// WithDanger 返回标记危险项后的菜单副本。
func (m MenuModel) WithDanger(danger int) MenuModel {
	if danger >= 0 && danger < len(m.options) {
		m.danger = danger
	}
	return m
}

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
