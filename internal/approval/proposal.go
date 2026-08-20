// Package approval 实现变更操作的人工审核状态机。
//
// 诊断跑在客户设备上，任何变更动作都不允许模型直接执行。模型能做的只是
// 生成一条「操作提案」（Proposal），提案必须由人批准之后，才由系统回放
// 原始参数真正执行。每一次状态流转都写审计日志。
package approval

import (
	"encoding/json"
	"fmt"
	"time"
)

// Status 是提案状态。
type Status string

const (
	// StatusPending 等待人工审核。
	StatusPending Status = "pending"
	// StatusApproved 已批准、尚未执行。批准与执行分成两个状态，审计上才能
	// 区分"批了但没跑成"和"根本没批"。
	StatusApproved Status = "approved"
	// StatusRejected 被人工驳回，不会执行。
	StatusRejected Status = "rejected"
	// StatusExecuting 已原子取得执行权，真实工具可能已经收到请求。该状态下
	// 绝不能再次执行；若进程重启，只能保守恢复为 unknown。
	StatusExecuting Status = "executing"
	// StatusExecuted 已批准且执行成功。
	StatusExecuted Status = "executed"
	// StatusFailed 已批准但执行失败。
	StatusFailed Status = "failed"
	// StatusUnknown 表示命令可能已经生效，但系统无法确认最终结果。禁止自动重试。
	StatusUnknown Status = "unknown"
)

// Proposal 是一条变更操作提案。
type Proposal struct {
	// ID 是人可读的短 ID，审核时用它指认提案。
	ID string `json:"id"`
	// Tool 是被调用的工具名。
	Tool string `json:"tool"`
	// Args 是模型给出的原始 JSON 参数。批准后原样回放，不做二次加工，
	// 避免"审的是一个东西、执行的是另一个东西"。
	Args string `json:"args"`
	// IdempotencyKey 是提案级稳定幂等键。它随原始参数持久化，用于审计和后续
	// checkpoint 关联；真正的最多下发一次由 executing 状态的原子抢占保证。
	IdempotencyKey string `json:"idempotency_key"`
	// CheckpointID 和 InterruptID 把提案钉在 Eino 的暂停点上，必须与提案一起保存。
	// Flow 记录挂起时所在的诊断分支——重启后要按同一个分支重建 Runner 才能恢复
	// 那一轮，光有快照不知道该交给谁跑。三者要么都为空，要么同时存在。
	CheckpointID string `json:"checkpoint_id,omitempty"`
	InterruptID  string `json:"interrupt_id,omitempty"`
	Flow         string `json:"flow,omitempty"`

	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`

	// Decider 是做出批准/驳回决定的人。
	Decider   string     `json:"decider,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	// RejectReason 是驳回理由，会回喂给模型让它换方案。
	RejectReason string     `json:"reject_reason,omitempty"`
	ExecutingAt  *time.Time `json:"executing_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`

	// Result 是执行输出，Error 是执行错误。
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Pending 报告提案是否仍在等待审核。
func (p *Proposal) Pending() bool { return p.Status == StatusPending }

// PrettyArgs 把原始参数 JSON 缩进后返回，便于在终端里核对。
// 解析失败时原样返回，绝不隐藏内容——审核界面不能对审核人有任何遮蔽。
func (p *Proposal) PrettyArgs() string {
	var v any
	if err := json.Unmarshal([]byte(p.Args), &v); err != nil {
		return p.Args
	}
	b, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return p.Args
	}
	return string(b)
}

// String 返回一行摘要。
func (p *Proposal) String() string {
	return fmt.Sprintf("[%s] %s %s", p.ID, p.Tool, p.Status)
}

// clone 返回副本，避免调用方拿到 Store 内部指针后随意改状态。
func (p *Proposal) clone() *Proposal {
	cp := *p
	if p.DecidedAt != nil {
		t := *p.DecidedAt
		cp.DecidedAt = &t
	}
	if p.ExecutingAt != nil {
		t := *p.ExecutingAt
		cp.ExecutingAt = &t
	}
	if p.FinishedAt != nil {
		t := *p.FinishedAt
		cp.FinishedAt = &t
	}
	return &cp
}
