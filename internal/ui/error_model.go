package ui

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

type errorModel struct {
	err   error
	width int
}

func (m errorModel) Init() tea.Cmd { return nil }

func (m errorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = size.Width
		return m, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyEnter, tea.KeyEsc, tea.KeyCtrlC:
			return m, tea.Quit
		}
		if key.String() == "q" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m errorModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	message := "未知错误"
	if m.err != nil {
		message = m.err.Error()
	}
	var view strings.Builder
	view.WriteString(errorStyle.Render("CDN 诊断助手启动失败"))
	view.WriteString("\n\n")
	view.WriteString(strings.Join(wrapCells(message, width), "\n"))
	view.WriteString("\n\n")
	view.WriteString(metaStyle.Render("Enter / Esc / q 退出"))
	view.WriteByte('\n')
	return view.String()
}

// ShowError 用 Bubble Tea 展示启动或运行错误。
func ShowError(err error) error {
	if err == nil {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, runErr := Run(ctx, errorModel{err: err})
	if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return runErr
}
