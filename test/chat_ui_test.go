package test

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/chat"
	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/session"
	. "diagnostic-system/internal/ui"
)

type fakeConversationAgent struct{}

func (fakeConversationAgent) Stream(_ context.Context, _ []*schema.Message,
	onIntent func(intent.Result), onChunk func(string)) (string, error) {
	onIntent(intent.Result{Intent: intent.PluginFailure, Confidence: 0.91})
	onChunk("第一段")
	onChunk("，第二段")
	return "第一段，第二段", nil
}

func newRootModel(t *testing.T, events chan tea.Msg) Model {
	t.Helper()
	store, err := session.OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Current()
	app := chat.NewApp(fakeConversationAgent{}, store, current, 1024, "auto")
	return NewModel(context.Background(), app, events, ModelConfig{Banner: "已启动", InputMaxBytes: 1024})
}

func TestRootModelUnicodeInputAndStreaming(t *testing.T) {
	events := make(chan tea.Msg, 8)
	model := newRootModel(t, events)
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("插件坏坏")})
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyBackspace})
	if model.InputValue() != "插件坏" {
		t.Fatalf("退格后输入 = %q", model.InputValue())
	}

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if cmd == nil || model.Mode() != "busy" || model.InputValue() != "" {
		t.Fatalf("提交后 mode=%q input=%q cmd nil=%v", model.Mode(), model.InputValue(), cmd == nil)
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("后台命令应通过统一事件通道投递，直接返回了 %T", msg)
	}

	for len(events) > 0 {
		model = updateRoot(t, model, <-events)
	}
	if model.Mode() != "input" {
		t.Fatalf("完成后 mode=%q, want input", model.Mode())
	}
	text := model.TranscriptText()
	for _, want := range []string{"user: 插件坏", "intent: 插件异常 · 91%", "assistant: 第一段，第二段"} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript 没有 %q:\n%s", want, text)
		}
	}
}

func TestRootModelSessionMenuSwitchesSession(t *testing.T) {
	store, err := session.OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	first, firstInfo := store.Current()
	_, secondInfo, err := store.Create("第二会话")
	if err != nil {
		t.Fatal(err)
	}
	app := chat.NewApp(fakeConversationAgent{}, store, first, 1024, "auto")
	if _, err := app.SwitchSession(secondInfo.ID); err != nil {
		t.Fatal(err)
	}
	model := NewModel(context.Background(), app, nil, ModelConfig{InputMaxBytes: 1024})

	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/sessions")})
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	if model.Mode() != "sessions" {
		t.Fatalf("mode=%q, want sessions", model.Mode())
	}
	// 当前会话排在首位，向下选择另一个会话。
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyDown})
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	_, selected := store.Current()
	if selected.ID != firstInfo.ID || model.Mode() != "input" {
		t.Fatalf("selected=%s mode=%q, want %s/input", selected.ID, model.Mode(), firstInfo.ID)
	}
}

func TestRootModelViewFitsConfiguredHeight(t *testing.T) {
	model := newRootModel(t, nil)
	model = updateRoot(t, model, tea.WindowSizeMsg{Width: 32, Height: 12})
	for _, r := range []rune(strings.Repeat("很长的终端内容", 20)) {
		model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	view := model.View()
	if lines := strings.Count(view, "\n"); lines != 12 {
		t.Fatalf("View 高度=%d, want 12:\n%s", lines, view)
	}
}

func TestRootModelViewAdaptsToTerminalResize(t *testing.T) {
	model := newRootModel(t, nil)
	sizes := []tea.WindowSizeMsg{
		{Width: 72, Height: 24},
		{Width: 21, Height: 11},
		{Width: 48, Height: 17},
	}

	for _, size := range sizes {
		model = updateRoot(t, model, size)
		view := model.View()
		if got := strings.Count(view, "\n"); got != size.Height {
			t.Errorf("窗口 %dx%d 下 View 高度=%d:\n%s", size.Width, size.Height, got, view)
		}
		lines := strings.Split(strings.TrimSuffix(view, "\n"), "\n")
		if len(lines) < 3 || !strings.Contains(lines[len(lines)-3], "›") {
			t.Errorf("窗口 %dx%d 下输入行没有固定在倒数第三行:\n%s", size.Width, size.Height, view)
		}
		for _, line := range lines {
			if got := lipgloss.Width(line); got > size.Width {
				t.Errorf("窗口 %dx%d 下行宽=%d: %q", size.Width, size.Height, got, line)
			}
		}
	}
}
