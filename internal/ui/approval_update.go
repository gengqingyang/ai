package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/tools"
	uiinput "diagnostic-system/internal/ui/input"
	"diagnostic-system/internal/ui/menu"
)

const (
	OptionRun = iota
	OptionDeny
)

func (m *Model) openApproval(request approvalRequestMsg) {
	m.approval = &request
	m.mode = modeApproval
	m.scrollOffset = 0
	m.menu = menu.New("执行这项操作？", []string{"执行", "拒绝"}, OptionRun,
		map[byte]int{'y': OptionRun, 'Y': OptionRun, 'n': OptionDeny, 'N': OptionDeny}).WithDanger(OptionDeny)
	m.resizeMenu()
}

func (m Model) updateApproval(msg tea.Msg) (tea.Model, tea.Cmd) {
	updated, _ := m.menu.Update(msg)
	m.menu = updated.(menu.Model)
	if !m.menu.Done() {
		return m, nil
	}
	if m.menu.Err() != nil {
		m.approval.result <- approvalResult{err: m.menu.Err()}
		m.approval = nil
		m.mode = modeBusy
		m.appendEntry(roleStatus, "人工审核已取消，操作不会执行。")
		return m, nil
	}
	if m.menu.Selected() == OptionRun {
		m.approval.result <- approvalResult{decision: tools.Decision{Approved: true}}
		m.approval = nil
		m.mode = modeBusy
		m.appendEntry(roleStatus, "已批准，正在节点上执行…")
		return m, nil
	}

	m.mode = modeRejectReason
	m.input = uiinput.New(m.inputMaxBytes).WithPrompt("› 拒绝理由 ")
	m.resizeInput()
	return m, nil
}

func (m Model) updateRejectReason(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyEsc {
		return m.reject("")
	}
	updated, _ := m.input.Update(msg)
	m.input = updated.(uiinput.Model)
	if !m.input.Done() {
		return m, nil
	}
	return m.reject(strings.TrimSpace(m.input.Value()))
}

func (m Model) reject(reason string) (tea.Model, tea.Cmd) {
	if reason == "" {
		reason = "（未填写理由）"
	}
	m.approval.result <- approvalResult{decision: tools.Decision{Approved: false, Reason: reason}}
	m.approval = nil
	m.mode = modeBusy
	m.input = uiinput.New(m.inputMaxBytes)
	m.resizeInput()
	m.appendEntry(roleStatus, "已驳回，理由会回传给模型："+reason)
	return m, nil
}

func (m *Model) handleExecutionNotice(proposal *approval.Proposal) {
	if proposal == nil {
		return
	}
	if proposal.Status == approval.StatusFailed {
		m.appendEntry(roleError, "执行失败："+proposal.Error)
		return
	}
	m.appendEntry(roleStatus, "执行完毕，输出已回传给模型。")
}
