package test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"diagnostic-system/internal/ui"
	uimenu "diagnostic-system/internal/ui/menu"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func testMenu() uimenu.Model {
	return uimenu.New("执行这项操作？", []string{"执行", "拒绝"}, ui.OptionRun,
		map[byte]int{'y': ui.OptionRun, 'Y': ui.OptionRun, 'n': ui.OptionDeny, 'N': ui.OptionDeny}).WithDanger(ui.OptionDeny)
}

func updateMenu(t *testing.T, model uimenu.Model, key tea.KeyMsg) (uimenu.Model, tea.Cmd) {
	t.Helper()
	updated, cmd := model.Update(key)
	menu, ok := updated.(uimenu.Model)
	if !ok {
		t.Fatalf("Update 返回了 %T, want menu.Model", updated)
	}
	return menu, cmd
}

func TestMenuNavigationWraps(t *testing.T) {
	menu := testMenu()
	if menu.Cursor() != ui.OptionRun {
		t.Fatalf("默认光标 = %d, want OptionRun", menu.Cursor())
	}

	menu, _ = updateMenu(t, menu, tea.KeyMsg{Type: tea.KeyUp})
	if menu.Cursor() != ui.OptionDeny {
		t.Fatalf("向上循环后光标 = %d, want OptionDeny", menu.Cursor())
	}
	menu, _ = updateMenu(t, menu, tea.KeyMsg{Type: tea.KeyDown})
	if menu.Cursor() != ui.OptionRun {
		t.Fatalf("向下循环后光标 = %d, want OptionRun", menu.Cursor())
	}
}

func TestMenuEnterConfirmsCurrentOption(t *testing.T) {
	menu := testMenu()
	menu = menu.WithCursor(ui.OptionDeny)
	menu, cmd := updateMenu(t, menu, tea.KeyMsg{Type: tea.KeyEnter})
	if !menu.Done() || menu.Selected() != ui.OptionDeny || cmd != nil {
		t.Fatalf("回车后状态 = %#v, embedded cmd=%v", menu, cmd)
	}
}

func TestMenuShortcutsConfirmImmediately(t *testing.T) {
	tests := []struct {
		key  rune
		want int
	}{
		{'y', ui.OptionRun},
		{'Y', ui.OptionRun},
		{'n', ui.OptionDeny},
		{'N', ui.OptionDeny},
	}
	for _, tc := range tests {
		t.Run(string(tc.key), func(t *testing.T) {
			menu, cmd := updateMenu(t, testMenu(), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{tc.key}})
			if !menu.Done() || menu.Selected() != tc.want || cmd != nil {
				t.Fatalf("快捷键 %q 后状态 = %#v, embedded cmd=%v", tc.key, menu, cmd)
			}
		})
	}
}

func TestMenuKeepsFirstDecision(t *testing.T) {
	menu, _ := updateMenu(t, testMenu(), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	menu, cmd := updateMenu(t, menu, tea.KeyMsg{Type: tea.KeyCtrlD})
	if !menu.Done() || menu.Selected() != ui.OptionRun || menu.Err() != nil || cmd != nil {
		t.Fatalf("后续按键覆盖了第一次决定: %#v, embedded cmd=%v", menu, cmd)
	}
}

func TestMenuCancelAndEOF(t *testing.T) {
	tests := []struct {
		name string
		key  tea.KeyMsg
		want error
	}{
		{"Ctrl-C", tea.KeyMsg{Type: tea.KeyCtrlC}, uimenu.ErrInterrupted},
		{"Esc", tea.KeyMsg{Type: tea.KeyEsc}, uimenu.ErrInterrupted},
		{"q", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}, uimenu.ErrInterrupted},
		{"Ctrl-D", tea.KeyMsg{Type: tea.KeyCtrlD}, uimenu.ErrEOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			menu, cmd := updateMenu(t, testMenu(), tc.key)
			if !menu.Done() || !errors.Is(menu.Err(), tc.want) || cmd != nil {
				t.Fatalf("取消后状态 = %#v, embedded cmd=%v", menu, cmd)
			}
		})
	}
}

func TestMenuViewContainsStableControls(t *testing.T) {
	view := testMenu().View()
	for _, want := range []string{"执行这项操作？", "执行", "拒绝", "Enter", "Esc"} {
		if !strings.Contains(view, want) {
			t.Errorf("菜单没有 %q:\n%s", want, view)
		}
	}
}

func TestMenuViewScrollsLongOptionList(t *testing.T) {
	options := make([]string, 20)
	for i := range options {
		options[i] = fmt.Sprintf("会话 %02d", i+1)
	}
	menu := uimenu.New("选择会话", options, 15, nil)
	updated, _ := menu.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	menu = updated.(uimenu.Model)

	view := menu.View()
	if !strings.Contains(view, "会话 16") || !strings.Contains(view, "16/20") {
		t.Fatalf("滚动列表没有展示当前项和位置:\n%s", view)
	}
	if strings.Contains(view, "会话 01") {
		t.Fatalf("滚动列表仍展示了窗口外的第一项:\n%s", view)
	}
}

func TestMenuViewFitsNarrowTerminalWidths(t *testing.T) {
	for _, width := range []int{1, 4, 8} {
		menu := testMenu()
		updated, _ := menu.Update(tea.WindowSizeMsg{Width: width, Height: 8})
		menu = updated.(uimenu.Model)

		view := menu.View()
		if got := strings.Count(view, "\n"); got > 8 {
			t.Errorf("终端宽度=%d 时菜单高度=%d, want <=8:\n%s", width, got, view)
		}
		for _, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("终端宽度=%d 时菜单行宽=%d: %q", width, got, line)
			}
		}
	}
}

func TestTruncateCellsFitsCJKText(t *testing.T) {
	got := uimenu.TruncateCells("一个很长的会话名称", 8)
	if lipgloss.Width(got) > 8 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncateCells() = %q, width=%d", got, lipgloss.Width(got))
	}
}

// 光标默认停在「执行」——但这只是省一次按键，不是省一次确认：
// Bubble Tea 收到 Enter 前不会设置 selected。
func TestApproveIsTheDefaultOption(t *testing.T) {
	if ui.OptionRun != 0 {
		t.Fatalf("OptionRun = %d，光标默认位置必须是「执行」那一项", ui.OptionRun)
	}
	menu := testMenu()
	if menu.Cursor() != ui.OptionRun || menu.Selected() != -1 || menu.Done() {
		t.Fatalf("初始菜单状态不安全: %#v", menu)
	}
}
