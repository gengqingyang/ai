package ui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/intent"
)

type intentMsg struct{ result intent.Result }
type agentProgressMsg struct{ text string }
type reasoningChunkMsg struct{ chunk string }
type assistantChunkMsg struct{ chunk string }
type logMsg struct{ line string }
type askDoneMsg struct {
	reply string
	err   error
	// note 是一句结果说明，用在本轮除了模型回复之外还有事要交代的时候
	// （例如审批命令：提案最终落到了哪个状态）。
	note string
}
type contextDoneMsg struct{}

func (m Model) waitForEvent() tea.Cmd {
	if m.events == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case event := <-m.events:
			return event
		case <-m.ctx.Done():
			return contextDoneMsg{}
		}
	}
}

func sendEvent(ctx context.Context, events chan tea.Msg, msg tea.Msg) bool {
	if events == nil {
		return false
	}
	select {
	case events <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}
