package test

import (
	"strings"
	"testing"

	uiinput "diagnostic-system/internal/ui/input"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func updateLineInput(t *testing.T, model uiinput.Model, key tea.KeyMsg) uiinput.Model {
	t.Helper()
	updated, _ := model.Update(key)
	input, ok := updated.(uiinput.Model)
	if !ok {
		t.Fatalf("Update 返回了 %T, want input.Model", updated)
	}
	return input
}

func TestLineInputBackspaceDeletesWholeChineseRune(t *testing.T) {
	input := uiinput.New(1024)
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("等你你")})
	if input.Value() != "等你你" || input.Cursor() != 3 {
		t.Fatalf("输入后 value=%q cursor=%d", input.Value(), input.Cursor())
	}

	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyBackspace})
	if input.Value() != "等你" || input.Cursor() != 2 {
		t.Fatalf("第一次退格后 value=%q cursor=%d", input.Value(), input.Cursor())
	}
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyBackspace})
	if input.Value() != "等" || input.Cursor() != 1 {
		t.Fatalf("第二次退格后 value=%q cursor=%d", input.Value(), input.Cursor())
	}
}

func TestLineInputEditsAtCursor(t *testing.T) {
	input := uiinput.New(1024)
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("你好世界")})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyLeft})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyLeft})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyBackspace})
	if input.Value() != "你世界" || input.Cursor() != 1 {
		t.Fatalf("中间退格后 value=%q cursor=%d", input.Value(), input.Cursor())
	}
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyDelete})
	if input.Value() != "你界" || input.Cursor() != 1 {
		t.Fatalf("Delete 后 value=%q cursor=%d", input.Value(), input.Cursor())
	}
}

func TestLineInputWordAndLineShortcuts(t *testing.T) {
	input := uiinput.New(1024)
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("alpha")})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文")})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("test")})
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyCtrlW})
	if input.Value() != "alpha 中文 " {
		t.Fatalf("Ctrl-W 后 value=%q", input.Value())
	}
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyCtrlU})
	if input.Value() != "" || input.Cursor() != 0 {
		t.Fatalf("Ctrl-U 后 value=%q cursor=%d", input.Value(), input.Cursor())
	}
}

func TestLineInputHonorsUTF8ByteLimit(t *testing.T) {
	input := uiinput.New(4)
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("你ab")})
	if input.Value() != "你a" {
		t.Fatalf("value=%q, want %q", input.Value(), "你a")
	}
}

func TestLineInputRendersThreeLineBarAtTerminalWidth(t *testing.T) {
	const width = 32
	input := uiinput.New(1024)
	updated, _ := input.Update(tea.WindowSizeMsg{Width: width})
	input = updated.(uiinput.Model)

	view := input.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 3 {
		t.Fatalf("输入区域高度=%d 行, want 3: %q", len(lines), view)
	}
	if !strings.Contains(lines[1], "› ") {
		t.Errorf("输入栏提示符没有位于中间行:\n%s", view)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got != width {
			t.Errorf("输入栏宽度=%d, want %d: %q", got, width, line)
		}
	}
}

func TestLineInputUsesSameBarForRejectionReason(t *testing.T) {
	const width = 24
	input := uiinput.New(1024).WithPrompt("› 拒绝理由 ")
	updated, _ := input.Update(tea.WindowSizeMsg{Width: width})
	input = updated.(uiinput.Model)

	view := input.View()
	lines := strings.Split(view, "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "› 拒绝理由 ") {
		t.Fatalf("拒绝理由输入框格式不完整:\n%s", view)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got != width {
			t.Errorf("拒绝理由输入栏宽度=%d, want %d: %q", got, width, line)
		}
	}
}

func TestLineInputFitsNarrowTerminalWidths(t *testing.T) {
	input := uiinput.New(1024)
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("很长的输入内容")})

	for width := 1; width <= 12; width++ {
		updated, _ := input.Update(tea.WindowSizeMsg{Width: width})
		resized := updated.(uiinput.Model)
		view := resized.View()
		lines := strings.Split(view, "\n")
		if len(lines) != 3 {
			t.Errorf("终端宽度=%d 时输入区域高度=%d: %q", width, len(lines), view)
		}
		for _, line := range lines {
			if got := lipgloss.Width(line); got != width {
				t.Errorf("终端宽度=%d 时输入栏宽度=%d: %q", width, got, line)
			}
		}
	}
}
