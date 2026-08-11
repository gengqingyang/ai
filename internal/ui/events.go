package ui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/intent"
)

type intentMsg struct{ result intent.Result }
type assistantChunkMsg struct{ chunk string }
type logMsg struct{ line string }
type askDoneMsg struct {
	reply string
	err   error
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
