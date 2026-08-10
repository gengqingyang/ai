package test

import (
	"testing"

	. "diagnostic-system/internal/chat"

	tea "github.com/charmbracelet/bubbletea"
)

func updateLineInput(t *testing.T, model LineInputModel, key tea.KeyMsg) LineInputModel {
	t.Helper()
	updated, _ := model.Update(key)
	input, ok := updated.(LineInputModel)
	if !ok {
		t.Fatalf("Update 返回了 %T, want LineInputModel", updated)
	}
	return input
}

func TestLineInputBackspaceDeletesWholeChineseRune(t *testing.T) {
	input := NewLineInputModel(1024)
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
	input := NewLineInputModel(1024)
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
	input := NewLineInputModel(1024)
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
	input := NewLineInputModel(4)
	input = updateLineInput(t, input, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("你ab")})
	if input.Value() != "你a" {
		t.Fatalf("value=%q, want %q", input.Value(), "你a")
	}
}
