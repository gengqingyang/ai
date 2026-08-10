package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/tools"
)

// ErrNoApproval 表示 UI 已关闭或没有可用的人工审核入口。Gate 会按拒绝处理。
var ErrNoApproval = errors.New("未取得人工确认，界面已关闭")

// 菜单项顺序。OptRun 在前，光标默认停在执行，但仍需用户明确按回车。
const (
	OptRun = iota
	OptDeny
)

type approvalResult struct {
	decision tools.Decision
	err      error
}

type approvalRequestMsg struct {
	proposal *approval.Proposal
	risk     tools.RiskAssessment
	result   chan<- approvalResult
}

type executionNoticeMsg struct {
	proposal *approval.Proposal
}

// UIApprover 把 Gate 的同步审核请求桥接到常驻 Bubble Tea 程序。
// 它不读 stdin，也不写 stdout；Review 会一直等到根 Model 回传明确决定。
type UIApprover struct {
	ctx      context.Context
	events   chan<- tea.Msg
	operator string
}

var _ tools.Approver = (*UIApprover)(nil)
var _ tools.Noticer = (*UIApprover)(nil)

// NewUIApprover 创建只通过 Bubble Tea 消息通道交互的审核入口。
func NewUIApprover(ctx context.Context, events chan<- tea.Msg, operator string) *UIApprover {
	if ctx == nil {
		ctx = context.Background()
	}
	return &UIApprover{ctx: ctx, events: events, operator: operator}
}

// Review 将提案送进根 Model，并等待 UI 返回批准或驳回结果。
func (a *UIApprover) Review(ctx context.Context, p *approval.Proposal, risk tools.RiskAssessment) (tools.Decision, error) {
	if err := ctx.Err(); err != nil {
		return tools.Decision{}, err
	}
	if a.events == nil {
		return tools.Decision{}, ErrNoApproval
	}

	result := make(chan approvalResult, 1)
	request := approvalRequestMsg{proposal: p, risk: risk, result: result}
	select {
	case a.events <- request:
	case <-ctx.Done():
		return tools.Decision{}, ctx.Err()
	case <-a.ctx.Done():
		return tools.Decision{}, ErrNoApproval
	}

	select {
	case answer := <-result:
		if answer.err != nil {
			return tools.Decision{}, answer.err
		}
		if answer.decision.Decider == "" {
			answer.decision.Decider = a.operator
		}
		return answer.decision, nil
	case <-ctx.Done():
		return tools.Decision{}, ctx.Err()
	case <-a.ctx.Done():
		return tools.Decision{}, ErrNoApproval
	}
}

// Notice 将执行结果送回根 Model 展示。
func (a *UIApprover) Notice(p *approval.Proposal) {
	if p == nil || a.events == nil {
		return
	}
	copy := *p
	select {
	case a.events <- executionNoticeMsg{proposal: &copy}:
	case <-a.ctx.Done():
	}
}

// ApprovalCard 返回审核所需的完整信息，由根 Model 纳入统一 View。
func ApprovalCard(p *approval.Proposal, risk tools.RiskAssessment) string {
	if p == nil {
		return "待人工确认\n提案信息缺失"
	}
	what := risk.Purpose
	if what == "" {
		what = p.Tool
	}

	var view strings.Builder
	view.WriteString("待人工确认\n")
	fmt.Fprintf(&view, "提案  %s  (%s)\n", what, p.ID)
	fmt.Fprintf(&view, "风险  %s - %s\n", risk.Level, risk.Reason)
	if risk.Target != "" {
		fmt.Fprintf(&view, "节点  %s\n", risk.Target)
	}
	if risk.Command != "" {
		fmt.Fprintf(&view, "\n$ %s\n", risk.Command)
	} else {
		fmt.Fprintf(&view, "\n参数: %s\n", p.PrettyArgs())
	}
	view.WriteString("\n风险等级仅供参考，请以命令原文为准；回车默认选择执行，但不会自动确认。")
	return view.String()
}
