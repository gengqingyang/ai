package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cloudwego/eino/adk"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/tools"
)

// newCheckpointID 为一轮执行生成快照标识。
//
// 用随机值而不是递增序号：快照文件名会落到磁盘上，随机值不泄露「今天批过几条」
// 这类信息，也不会在并发轮次之间撞车。字符集限制在 hex 内，正好落在
// approval.CheckpointStore 允许的文件名范围里。
func newCheckpointID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成执行快照标识失败: %w", err)
	}
	return "run-" + hex.EncodeToString(raw[:]), nil
}

// resolvePauses 把本段收集到的中断点推进到「有决定」，返回可以恢复的目标集合。
//
// 每个中断点的处理顺序是固定的：先落盘 proposal_id ↔ checkpoint_id ↔
// interrupt_id 的关联，再去问人。反过来的话，人已经点了「执行」而关联还没写下去
// 时进程退出，重启后就找不到该恢复哪一轮了。
//
// 返回的 map 只用 interrupt ID 指明「恢复谁」，value 一律为 nil：决定本身走
// 提案存储，不经过恢复通道，模型和客户端都无法从这条路径影响是否下发。
func (a *Agent) resolvePauses(
	ctx context.Context,
	checkpointID string,
	flow Flow,
	pending []*adk.InterruptCtx,
	onProgress func(string),
) (map[string]any, error) {
	targets := make(map[string]any, len(pending))
	for _, ic := range pending {
		info, ok := ic.Info.(tools.PauseInfo)
		if !ok {
			// 不认识的中断点没法安全推进：不知道它在等什么，更不知道谁该批。
			return nil, fmt.Errorf("中断点 %s 缺少可识别的审批信息，本轮已停止", ic.ID)
		}
		binding := approval.InterruptBinding{
			CheckpointID: checkpointID,
			InterruptID:  ic.ID,
			Flow:         string(flow),
		}
		if err := a.pauses.BindPause(info.ProposalID, binding); err != nil {
			return nil, fmt.Errorf("关联提案 %s 与执行快照失败，操作未执行: %w", info.ProposalID, err)
		}
		slog.Info("变更操作挂起等待人工确认",
			"proposal", info.ProposalID, "tool", info.Tool, "flow", flow,
			"checkpoint", checkpointID, "interrupt", ic.ID)

		p, err := a.pauses.Resolve(ctx, info.ProposalID)
		if err != nil {
			return nil, fmt.Errorf("获取提案 %s 的人工决定失败，操作未执行: %w", info.ProposalID, err)
		}
		if p.Status == approval.StatusPending {
			// 异步模式：没有审核入口，提案留给外部程序处理。快照必须保留，
			// 否则这一轮的上下文就没了，批准后也无从继续。
			notifyProgress(onProgress, fmt.Sprintf(
				"提案 %s 已登记并等待人工审核，本轮在此暂停；重启后可用 /approvals 接着处理。", p.ID))
			continue
		}
		targets[ic.ID] = nil
	}
	return targets, nil
}

// ResumeProposal 接着跑完某条提案挂起的那一轮诊断。
//
// 这是重启之后唯一的入口：进程换了，但提案里记着分支、快照和暂停点，凭这三样
// 就能把当初那一轮原地拼回来，让模型在同一段上下文里听到执行结果或驳回理由，
// 而不是从头再想一遍。
//
// 恢复本身不携带任何决定——Targets 的值一律为 nil，批没批仍然回提案存储读。
// 所以对还没有决定的提案调用它是安全的：变更工具会原样再挂起一次，由 pump
// 按同一条路径去取人工决定。
func (a *Agent) ResumeProposal(
	ctx context.Context,
	p *approval.Proposal,
	onProgress func(string),
	onReasoning func(string),
	onChunk func(string),
) (string, error) {
	if p == nil {
		return "", errors.New("提案为空，无法恢复")
	}
	if a.checkpoints == nil || a.pauses == nil {
		return "", errors.New("当前未启用中断恢复，无法接续被挂起的诊断")
	}
	if !p.Resumable() {
		return "", fmt.Errorf("提案 %s 当前状态为 %s 且暂停点关联不完整，无法恢复", p.ID, p.Status)
	}
	binding := p.Binding()
	flow := Flow(binding.Flow)
	selected, ok := a.flows[flow]
	if !ok || selected == nil {
		return "", fmt.Errorf("提案 %s 记录的诊断分支 %q 未初始化，无法恢复", p.ID, binding.Flow)
	}

	turn := &turnRunner{
		checkpointID: binding.CheckpointID,
		flow:         flow,
		durable:      true,
		runOpts:      []adk.AgentRunOption{adk.WithCheckPointID(binding.CheckpointID)},
	}
	turn.runner = adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: selected, EnableStreaming: true, CheckPointStore: a.checkpoints,
	})

	slog.Info("恢复被挂起的诊断",
		"proposal", p.ID, "status", string(p.Status), "flow", flow,
		"checkpoint", binding.CheckpointID, "interrupt", binding.InterruptID)

	iter, err := turn.runner.ResumeWithParams(ctx, binding.CheckpointID,
		&adk.ResumeParams{Targets: map[string]any{binding.InterruptID: nil}}, turn.runOpts...)
	if err != nil {
		return "", fmt.Errorf("加载提案 %s 的执行快照失败: %w", p.ID, err)
	}
	reply, err := a.pump(ctx, turn, iter, &runState{}, onProgress, onReasoning, onChunk)
	if err != nil {
		return "", err
	}
	if reply == nil {
		return "", nil
	}
	return reply.Content, nil
}

// discardCheckpoint 清掉不再需要的执行快照。
//
// 快照里含有本轮完整的模型消息（可能包括截图 base64），没有待恢复的中断就
// 不该继续留在磁盘上。删之前先把指向它的提案关联抹掉，两者必须一起消失：留下
// 关联而没有快照，审批入口就会列出一条点不动的「可恢复」提案。
//
// 两步都只记日志：它们都不影响本轮结论的正确性，也不会让任何变更被多下发一次。
func (a *Agent) discardCheckpoint(ctx context.Context, checkpointID string) {
	if checkpointID == "" || a.checkpoints == nil {
		return
	}
	if a.pauses != nil {
		if err := a.pauses.ReleasePauses(checkpointID); err != nil {
			slog.Warn("清理提案的中断关联失败", "checkpoint", checkpointID, "err", err)
		}
	}
	deleter, ok := a.checkpoints.(adk.CheckPointDeleter)
	if !ok {
		return
	}
	if err := deleter.Delete(ctx, checkpointID); err != nil {
		slog.Warn("清理执行快照失败", "checkpoint", checkpointID, "err", err)
	}
}
