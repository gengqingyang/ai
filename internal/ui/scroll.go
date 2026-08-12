package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	scrollbarWidth   = 2
	mouseScrollLines = 3
)

type scrollViewport struct {
	width        int
	height       int
	contentWidth int
	lines        []string
	offset       int
	maxOffset    int
	thumbStart   int
	thumbSize    int
	showBar      bool
}

func (m Model) newScrollViewport(width, height int) scrollViewport {
	viewport := scrollViewport{
		width:        max(1, width),
		height:       max(0, height),
		contentWidth: max(1, width),
	}
	viewport.lines = renderedLines(m.renderBody(viewport.contentWidth))
	viewport.showBar = viewport.height > 0 && len(viewport.lines) > viewport.height && width > scrollbarWidth
	if viewport.showBar {
		viewport.contentWidth = width - scrollbarWidth
		viewport.lines = renderedLines(m.renderBody(viewport.contentWidth))
	}

	viewport.maxOffset = max(0, len(viewport.lines)-viewport.height)
	viewport.offset = min(max(0, m.scrollOffset), viewport.maxOffset)
	if viewport.showBar {
		viewport.thumbSize = max(1, viewport.height*viewport.height/len(viewport.lines))
		viewport.thumbSize = min(viewport.height, viewport.thumbSize)
		travel := viewport.height - viewport.thumbSize
		topLine := viewport.maxOffset - viewport.offset
		if viewport.maxOffset > 0 {
			viewport.thumbStart = (topLine*travel + viewport.maxOffset/2) / viewport.maxOffset
		}
	}
	return viewport
}

func (v scrollViewport) View() string {
	if v.height <= 0 {
		return ""
	}

	end := len(v.lines) - v.offset
	start := max(0, end-v.height)
	visible := v.lines[start:end]
	var out strings.Builder
	for row := 0; row < v.height; row++ {
		line := ""
		if row < len(visible) {
			line = visible[row]
		}
		if !v.showBar {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}

		line = lipgloss.NewStyle().MaxWidth(v.contentWidth).Render(line)
		out.WriteString(line)
		out.WriteString(strings.Repeat(" ", max(0, v.contentWidth-lipgloss.Width(line))+1))
		if row >= v.thumbStart && row < v.thumbStart+v.thumbSize {
			out.WriteString(scrollThumbStyle.Render("█"))
		} else {
			out.WriteString(scrollTrackStyle.Render("│"))
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func renderedLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func (m Model) currentScrollViewport() (scrollViewport, int) {
	width := max(1, m.viewWidth())
	headerHeight := lineCount(m.renderHeader(width))
	footerHeight := lineCount(m.renderFooter(width))
	bodyHeight := max(0, m.height-headerHeight-footerHeight)
	return m.newScrollViewport(width, bodyHeight), headerHeight
}

func (m *Model) clampScrollOffset() {
	viewport, _ := m.currentScrollViewport()
	m.scrollOffset = min(max(0, m.scrollOffset), viewport.maxOffset)
	if !viewport.showBar {
		m.scrollDragging = false
	}
}

func (m *Model) scrollBy(lines int) {
	viewport, _ := m.currentScrollViewport()
	m.scrollOffset = min(max(0, m.scrollOffset+lines), viewport.maxOffset)
}

func (m Model) scrollPageSize() int {
	viewport, _ := m.currentScrollViewport()
	return max(1, viewport.height/2)
}

func (m *Model) updateScrollMouse(msg tea.MouseMsg) bool {
	viewport, bodyTop := m.currentScrollViewport()
	if !viewport.showBar {
		m.scrollDragging = false
		return false
	}

	row := msg.Y - bodyTop
	inBody := row >= 0 && row < viewport.height
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if inBody {
			m.scrollBy(mouseScrollLines)
			return true
		}
	case tea.MouseButtonWheelDown:
		if inBody {
			m.scrollBy(-mouseScrollLines)
			return true
		}
	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress {
			m.scrollDragging = inBody && msg.X == viewport.width-1
			if m.scrollDragging {
				m.scrollToTrackRow(row, viewport)
				return true
			}
		}
		if msg.Action == tea.MouseActionMotion && m.scrollDragging {
			m.scrollToTrackRow(row, viewport)
			return true
		}
	case tea.MouseButtonNone:
		if msg.Action == tea.MouseActionRelease && m.scrollDragging {
			m.scrollDragging = false
			return true
		}
	}
	return false
}

func (m *Model) scrollToTrackRow(row int, viewport scrollViewport) {
	row = min(max(0, row), viewport.height-1)
	topLine := 0
	if viewport.height > 1 {
		topLine = (row*viewport.maxOffset + (viewport.height-1)/2) / (viewport.height - 1)
	}
	m.scrollOffset = viewport.maxOffset - topLine
}
