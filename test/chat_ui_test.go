package test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/chat"
	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/repository"
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

func TestRootModelHistoryLoadsCompleteMessageContent(t *testing.T) {
	store, err := session.OpenStore(0, 10_000, "")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Current()
	userContent := "用户开头\n" + strings.Repeat("很长的用户内容", 40) + "\n用户结尾"
	assistantContent := "助手开头\n" + strings.Repeat("完整的助手回答", 40) + "\n助手结尾"
	if err := current.AppendUser(userContent); err != nil {
		t.Fatal(err)
	}
	if err := current.AppendAssistant(assistantContent); err != nil {
		t.Fatal(err)
	}
	app := chat.NewApp(fakeConversationAgent{}, store, current, 1024, "auto")
	model := NewModel(context.Background(), app, nil, ModelConfig{InputMaxBytes: 1024})

	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/history")})
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	transcript := model.TranscriptText()
	for _, want := range []string{userContent, assistantContent, "当前会话历史（2 条）"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("历史记录缺少完整内容 %q:\n%s", want, transcript)
		}
	}
	for _, truncated := range []string{
		string([]rune(userContent)[:90]) + "…",
		string([]rune(assistantContent)[:90]) + "…",
	} {
		if strings.Contains(transcript, truncated) {
			t.Errorf("历史记录仍包含截断摘要 %q", truncated)
		}
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

func TestRootModelConversationScrollbarSupportsKeyboardAndMouse(t *testing.T) {
	const (
		width  = 42
		height = 16
	)
	model := newRootModel(t, nil)
	model = updateRoot(t, model, tea.WindowSizeMsg{Width: width, Height: height})
	if strings.Contains(model.View(), "█") {
		t.Fatal("内容未溢出时不应显示滚动条滑块")
	}

	for range 8 {
		model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/help")})
		model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	}
	assertViewBounds(t, model.View(), width, height)
	trackTop, trackBottom := scrollbarTrackBounds(t, model.View())
	bottomThumb := scrollbarThumbRows(t, model.View())
	if bottomThumb[len(bottomThumb)-1] != trackBottom {
		t.Fatalf("位于最新内容时滑块=%v，轨道底部=%d", bottomThumb, trackBottom)
	}

	for range 6 {
		model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyPgUp})
	}
	keyboardThumb := scrollbarThumbRows(t, model.View())
	if keyboardThumb[0] >= bottomThumb[0] {
		t.Fatalf("PgUp 后滑块没有上移: before=%v after=%v", bottomThumb, keyboardThumb)
	}

	model = updateRoot(t, model, tea.MouseMsg{
		X: width - 1, Y: trackBottom, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	})
	for range 6 {
		model = updateRoot(t, model, tea.MouseMsg{
			X: width - 2, Y: trackTop + 1, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress,
		})
	}
	wheelThumb := scrollbarThumbRows(t, model.View())
	if wheelThumb[0] >= bottomThumb[0] {
		t.Fatalf("鼠标滚轮后滑块没有上移: before=%v after=%v", bottomThumb, wheelThumb)
	}

	model = updateRoot(t, model, tea.MouseMsg{
		X: width - 1, Y: trackBottom, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	})
	model = updateRoot(t, model, tea.MouseMsg{
		X: width - 1, Y: trackTop, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion,
	})
	model = updateRoot(t, model, tea.MouseMsg{
		X: width - 1, Y: trackTop, Button: tea.MouseButtonNone, Action: tea.MouseActionRelease,
	})
	draggedThumb := scrollbarThumbRows(t, model.View())
	if draggedThumb[0] != trackTop {
		t.Fatalf("拖到轨道顶部后滑块=%v，轨道顶部=%d", draggedThumb, trackTop)
	}
	if !strings.Contains(model.View(), "已启动") {
		t.Fatal("拖到顶部后没有显示最早的对话内容")
	}

	model = updateRoot(t, model, tea.WindowSizeMsg{Width: 25, Height: 12})
	assertViewBounds(t, model.View(), 25, 12)
	scrollbarThumbRows(t, model.View())
}

func assertViewBounds(t *testing.T, view string, width, height int) {
	t.Helper()
	if got := strings.Count(view, "\n"); got != height {
		t.Fatalf("View 高度=%d, want %d:\n%s", got, height, view)
	}
	for _, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("View 行宽=%d, want <=%d: %q", got, width, line)
		}
	}
}

func scrollbarTrackBounds(t *testing.T, view string) (int, int) {
	t.Helper()
	top, bottom := -1, -1
	for row, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
		if !strings.Contains(line, "│") && !strings.Contains(line, "█") {
			continue
		}
		if top == -1 {
			top = row
		}
		bottom = row
	}
	if top == -1 {
		t.Fatalf("View 中没有滚动条轨道:\n%s", view)
	}
	return top, bottom
}

func scrollbarThumbRows(t *testing.T, view string) []int {
	t.Helper()
	var rows []int
	for row, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
		if strings.Contains(line, "█") {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("View 中没有滚动条滑块:\n%s", view)
	}
	return rows
}

func TestRootModelHandlesRepositoryCommandsInBubbleTea(t *testing.T) {
	store, err := session.OpenStore(0, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := store.Current()
	manager, err := repository.Open(filepath.Join(t.TempDir(), "repositories.json"))
	if err != nil {
		t.Fatal(err)
	}
	app := chat.NewApp(fakeConversationAgent{}, store, current, 1024, "auto", manager)
	model := NewModel(context.Background(), app, nil, ModelConfig{InputMaxBytes: 1024})

	command := "/repo add " + makeCodeRepository(t) + " installer"
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(command)})
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	if model.Mode() != "input" || !strings.Contains(model.TranscriptText(), "当前代码仓库：installer") {
		t.Fatalf("after repo add mode=%q transcript=%s", model.Mode(), model.TranscriptText())
	}

	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/repos")})
	model = updateRoot(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	for _, want := range []string{"* installer", "文件", "符号"} {
		if !strings.Contains(model.TranscriptText(), want) {
			t.Errorf("repository transcript missing %q:\n%s", want, model.TranscriptText())
		}
	}
}
