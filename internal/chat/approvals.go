package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"diagnostic-system/internal/approval"
)

// ApprovalGate 是审批命令需要的能力，由 tools.Gate 提供。
//
// 这里只借它的三件事：查提案、批准后下发、驳回。风险评估、真实工具隔离和审计
// 仍然留在闸门里——聊天层不碰真实工具，也不自己判断能不能执行。
type ApprovalGate interface {
	Store() *approval.Store
	Execute(ctx context.Context, id, decider string) (*approval.Proposal, error)
	Reject(id, decider, reason string) (*approval.Proposal, error)
}

// ResumeAgent 能接着跑完某条提案挂起的那一轮诊断。
//
// 有它才谈得上「重启后继续」：决定落盘之后要把当初那一轮拼回来，模型才能在
// 同一段上下文里听到真实结果，而不是从头再想一遍。
type ResumeAgent interface {
	ResumeProposal(ctx context.Context, p *approval.Proposal,
		onProgress, onReasoning, onChunk func(string)) (string, error)
}

// EnableApprovals 挂上审批入口，开启 /approvals。
//
// operator 是这台终端前坐着的人，会作为决定人写进状态和审计日志。
func (a *App) EnableApprovals(gate ApprovalGate, operator string) {
	a.approvals = gate
	a.operator = strings.TrimSpace(operator)
}

// PendingApprovals 返回还需要人处理、或还挂着没跑完的提案，按登记顺序。
//
// 两类都要列：等人点头的（pending），以及决定已经落盘但那一轮还没接回来的
// （进程在中途退出过）。只看 pending 会漏掉后一类，那正是重启后最容易丢的东西。
func (a *App) PendingApprovals() []*approval.Proposal {
	if a.approvals == nil {
		return nil
	}
	var out []*approval.Proposal
	for _, p := range a.approvals.Store().All() {
		if p.Pending() || p.Resumable() {
			out = append(out, p)
		}
	}
	return out
}

// ApprovalDecision 是一次审批命令的结果，供 UI 组织展示。
type ApprovalDecision struct {
	// Proposal 是决定落盘之后的提案。
	Proposal *approval.Proposal
	// Reply 是恢复那一轮之后模型给出的结论，没有恢复时为空。
	Reply string
	// Resumed 表示挂起的那一轮已经被接回来跑完。
	Resumed bool
}

// ApproveProposal 批准一条提案，然后把它挂起的那一轮接着跑完。
//
// 顺序是刻意的，也是整条链路的安全前提：决定先原子落盘，再恢复。恢复通道里不
// 带任何决定，被唤醒的变更工具回提案存储读状态和原始参数，因此这里既不可能
// 执行到审核人没看过的参数，也不可能因为中途崩溃就把批准弄丢。
//
// 找不到快照的提案（比如异步模式下登记的）退回直接下发：那一轮的上下文已经
// 没了，但命令本身仍按提案里存的原始参数执行，模型只是听不到结果。
func (a *App) ApproveProposal(ctx context.Context, id string,
	onProgress, onReasoning, onChunk func(string)) (ApprovalDecision, error) {
	p, err := a.lookupApproval(id)
	if err != nil {
		return ApprovalDecision{}, err
	}
	if !p.Resumable() {
		// 没有可恢复的那一轮，只能直接下发。Execute 自己会校验状态并原子抢占
		// 执行权，同一提案仍然最多下发一次。
		done, execErr := a.approvals.Execute(ctx, p.ID, a.decider())
		if execErr != nil {
			return ApprovalDecision{Proposal: done}, execErr
		}
		return ApprovalDecision{Proposal: done}, nil
	}
	if p.Pending() {
		if p, err = a.approvals.Store().Approve(p.ID, a.decider()); err != nil {
			return ApprovalDecision{}, fmt.Errorf("批准提案 %s 失败: %w", id, err)
		}
	}
	notify(onProgress, fmt.Sprintf(
		"提案 %s 已批准并落盘，正在接续被挂起的那一轮诊断。", p.ID))
	return a.resume(ctx, p, onProgress, onReasoning, onChunk)
}

// RejectProposal 驳回一条提案，然后恢复那一轮好让模型听到驳回理由。
//
// 恢复只是为了把话说完：被唤醒的变更工具看到 rejected 就直接返回理由，一步都
// 不会走到真实工具。恢复不了也无所谓，驳回本身已经落盘，设备上什么都没发生。
func (a *App) RejectProposal(ctx context.Context, id, reason string,
	onProgress, onReasoning, onChunk func(string)) (ApprovalDecision, error) {
	p, err := a.lookupApproval(id)
	if err != nil {
		return ApprovalDecision{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "（未填写理由）"
	}
	if p.Pending() {
		if p, err = a.approvals.Reject(p.ID, a.decider(), reason); err != nil {
			return ApprovalDecision{}, fmt.Errorf("驳回提案 %s 失败: %w", id, err)
		}
	}
	if !p.Resumable() {
		return ApprovalDecision{Proposal: p}, nil
	}
	notify(onProgress, fmt.Sprintf(
		"提案 %s 已驳回并落盘，正在把驳回理由交回模型。", p.ID))
	return a.resume(ctx, p, onProgress, onReasoning, onChunk)
}

func notify(onProgress func(string), text string) {
	if onProgress != nil {
		onProgress(text)
	}
}

func (a *App) resume(ctx context.Context, p *approval.Proposal,
	onProgress, onReasoning, onChunk func(string)) (ApprovalDecision, error) {
	resumer, ok := a.agent.(ResumeAgent)
	if !ok {
		return ApprovalDecision{Proposal: p},
			fmt.Errorf("决定已记录，但当前 Agent 不支持接续被挂起的诊断（提案 %s）", p.ID)
	}
	reply, err := resumer.ResumeProposal(ctx, p, onProgress, onReasoning, onChunk)
	// 无论恢复成功与否都回读一次：命令可能已经下发，最新状态只在存储里。
	latest, _ := a.approvals.Store().Get(p.ID)
	if latest == nil {
		latest = p
	}
	if err != nil {
		return ApprovalDecision{Proposal: latest}, err
	}
	if reply != "" && a.sess != nil {
		if err := a.sess.AppendAssistant(reply); err != nil {
			return ApprovalDecision{Proposal: latest, Reply: reply, Resumed: true},
				fmt.Errorf("保存助手回复失败: %w", err)
		}
	}
	return ApprovalDecision{Proposal: latest, Reply: reply, Resumed: true}, nil
}

func (a *App) lookupApproval(id string) (*approval.Proposal, error) {
	if a.approvals == nil {
		return nil, errors.New("审批入口未配置")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("请给出提案 ID")
	}
	p, ok := a.approvals.Store().Get(id)
	if !ok {
		return nil, fmt.Errorf("提案 %s 不存在", id)
	}
	return p, nil
}

func (a *App) decider() string {
	if a.operator == "" {
		return "unknown-operator"
	}
	return a.operator
}

// RunApprovalCommand 执行 /approvals 及其子命令。
func (a *App) RunApprovalCommand(ctx context.Context, line string,
	onProgress, onReasoning, onChunk func(string)) (ApprovalDecision, error) {
	if a.approvals == nil {
		return ApprovalDecision{}, errors.New("审批入口未配置，/approvals 不可用")
	}
	fields, err := commandWords(line)
	if err != nil {
		return ApprovalDecision{}, err
	}
	if len(fields) < 2 {
		// 裸 /approvals 就是列表，由 UI 用 PendingApprovals 渲染。
		return ApprovalDecision{}, nil
	}
	switch fields[1] {
	case "approve":
		if len(fields) != 3 {
			return ApprovalDecision{}, errors.New("用法: /approvals approve <提案 ID>")
		}
		return a.ApproveProposal(ctx, fields[2], onProgress, onReasoning, onChunk)
	case "reject":
		if len(fields) < 3 {
			return ApprovalDecision{}, errors.New("用法: /approvals reject <提案 ID> [理由]")
		}
		return a.RejectProposal(ctx, fields[2], strings.Join(fields[3:], " "),
			onProgress, onReasoning, onChunk)
	default:
		return ApprovalDecision{}, fmt.Errorf(
			"未知审批命令 %q，可用: /approvals | /approvals approve <ID> | /approvals reject <ID> [理由]",
			fields[1])
	}
}
