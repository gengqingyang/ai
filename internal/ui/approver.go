package ui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/tools"
)

// ErrNoApproval 表示 UI 已关闭或没有可用的人工审核入口。
var ErrNoApproval = errors.New("未取得人工确认，界面已关闭")

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

// Approver 把 Gate 的同步审核请求桥接到 Bubble Tea 根模型。
type Approver struct {
	ctx      context.Context
	events   chan<- tea.Msg
	operator string
}

var _ tools.Approver = (*Approver)(nil)
var _ tools.Noticer = (*Approver)(nil)

func NewApprover(ctx context.Context, events chan<- tea.Msg, operator string) *Approver {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Approver{ctx: ctx, events: events, operator: operator}
}

func (a *Approver) Review(ctx context.Context, proposal *approval.Proposal,
	risk tools.RiskAssessment) (tools.Decision, error) {
	if err := ctx.Err(); err != nil {
		return tools.Decision{}, err
	}
	if a.events == nil {
		return tools.Decision{}, ErrNoApproval
	}

	result := make(chan approvalResult, 1)
	request := approvalRequestMsg{proposal: proposal, risk: risk, result: result}
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

func (a *Approver) Notice(proposal *approval.Proposal) {
	if proposal == nil || a.events == nil {
		return
	}
	copy := *proposal
	select {
	case a.events <- executionNoticeMsg{proposal: &copy}:
	case <-a.ctx.Done():
	}
}
