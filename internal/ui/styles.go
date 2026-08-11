package ui

import "github.com/charmbracelet/lipgloss"

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#2DD4BF"))
	metaStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#A1A1AA"))
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#22D3EE"))
	agentStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#4ADE80"))
	intentStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E879F9"))
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FACC15"))
	errorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F87171"))
)
