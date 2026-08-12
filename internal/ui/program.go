package ui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// Run 启动唯一的全屏 Bubble Tea 程序。
func Run(ctx context.Context, model tea.Model) (tea.Model, error) {
	return tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithoutSignalHandler(),
	).Run()
}
