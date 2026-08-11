package ui

import (
	"context"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

type logWriter struct {
	ctx    context.Context
	events chan tea.Msg
}

// NewLogWriter 把显式终端日志转换为根 UI 消息。
func NewLogWriter(ctx context.Context, events chan tea.Msg) io.Writer {
	return logWriter{ctx: ctx, events: events}
}

func (w logWriter) Write(p []byte) (int, error) {
	sendEvent(w.ctx, w.events, logMsg{line: string(p)})
	return len(p), nil
}
