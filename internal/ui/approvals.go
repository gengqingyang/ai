package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/chat"
)

// runApprovals 处理 /approvals：不带参数就列出还没了结的提案，
// 带 approve/reject 就落决定并把挂起的那一轮接着跑完。
//
// 决定之后那一段和普通提问走同一条路：进 busy 模式、流式回放模型输出、
// 结束时统一收尾。对用户来说，重启前后看到的是同一件事的两半。
func (m Model) runApprovals(line string) (tea.Model, tea.Cmd) {
	if len(strings.Fields(line)) < 2 {
		m.appendEntry(roleSystem, approvalsText(m.app.PendingApprovals()))
		return m, nil
	}

	m.appendEntry(roleUser, line)
	m.streaming = ""
	m.streamRole = roleAssistant
	m.assistantStreamed = false
	m.mode = modeBusy
	requestCtx, cancel := context.WithCancel(m.ctx)
	m.requestCancel = cancel

	cmd := func() tea.Msg {
		decision, err := m.app.RunApprovalCommand(
			requestCtx,
			line,
			func(progress string) {
				sendEvent(requestCtx, m.events, agentProgressMsg{text: progress})
			},
			func(chunk string) {
				sendEvent(requestCtx, m.events, reasoningChunkMsg{chunk: chunk})
			},
			func(chunk string) {
				sendEvent(requestCtx, m.events, assistantChunkMsg{chunk: chunk})
			},
		)
		done := askDoneMsg{reply: decision.Reply, err: err, note: approvalNote(decision)}
		if m.events == nil {
			return done
		}
		sendEvent(requestCtx, m.events, done)
		return nil
	}
	return m, cmd
}

// approvalNote 用一句话交代提案最终落在哪儿。
//
// 状态原样报出来，不做任何美化：unknown 就是「不知道跑没跑成」，把它说成成功
// 会让人跳过核实。
func approvalNote(decision chat.ApprovalDecision) string {
	p := decision.Proposal
	if p == nil {
		return ""
	}
	note := fmt.Sprintf("提案 %s（%s）当前状态：%s。", p.ID, p.Tool, p.Status)
	switch p.Status {
	case approval.StatusUnknown:
		note += "命令可能已经生效但结果未知，请自行核实，禁止重试同一提案。"
	case approval.StatusFailed:
		note += "执行失败，原因见上。"
	case approval.StatusRejected:
		note += "未执行任何操作。"
	}
	if !decision.Resumed && p.Status != approval.StatusPending {
		note += "（原先挂起的那一轮已无法接续，模型不会收到这条结果。）"
	}
	return note
}

func approvalsText(items []*approval.Proposal) string {
	if len(items) == 0 {
		return "没有待处理的变更提案。"
	}
	var out strings.Builder
	out.WriteString("待处理的变更提案：\n")
	for _, p := range items {
		marker := " "
		if p.Resumable() {
			// 打上标记的提案背后还挂着一整轮诊断，处理完模型会接着说下去。
			marker = "*"
		}
		fmt.Fprintf(&out, "%s %s · %s · %s · %s\n  %s\n",
			marker, p.ID, p.Tool, p.Status,
			p.CreatedAt.Local().Format("01-02 15:04"), oneLine(p.Args))
	}
	out.WriteString("\n用 /approvals approve <ID> 批准并执行，" +
		"/approvals reject <ID> [理由] 驳回。带 * 的提案会接着跑完被挂起的那一轮诊断。")
	return out.String()
}
