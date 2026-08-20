package test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	. "diagnostic-system/internal/tools"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/approval"
)

// scriptedToolModel 先要求调一次变更工具，拿到结果后收尾。
// 它替代真实模型，让测试能完整跑一遍「工具调用 → 挂起 → 恢复 → 收尾」。
type scriptedToolModel struct {
	toolName string
	args     string

	calls    atomic.Int32
	lastSeen atomic.Value // []*schema.Message
}

func (m *scriptedToolModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *scriptedToolModel) Generate(
	_ context.Context, in []*schema.Message, _ ...model.Option,
) (*schema.Message, error) {
	m.calls.Add(1)
	m.lastSeen.Store(in)
	// 只看收到的消息来决定下一步，不看自己被调过几次：真实模型也只有历史可依。
	// 重启后新进程里的模型没有任何计数，只有从快照恢复回来的消息，这条路径必须
	// 和重启前走到同一个结果，否则测的就不是恢复而是测试桩的记忆。
	for _, msg := range in {
		if msg != nil && msg.Role == schema.Tool {
			return &schema.Message{Role: schema.Assistant, Content: "命令已执行完毕。"}, nil
		}
	}
	return &schema.Message{
		Role:    schema.Assistant,
		Content: "我先在 SN001 上执行这条命令。",
		ToolCalls: []schema.ToolCall{{
			ID:       "call_1",
			Function: schema.FunctionCall{Name: m.toolName, Arguments: m.args},
		}},
	}, nil
}

func (m *scriptedToolModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("测试只用非流式")
}

// toolResults 返回模型最后一次看到的消息里所有 tool 角色的正文。
// 「模型确实拿到了设备真实输出」这件事只能从这里验证。
func (m *scriptedToolModel) toolResults() []string {
	msgs, _ := m.lastSeen.Load().([]*schema.Message)
	var out []string
	for _, msg := range msgs {
		if msg != nil && msg.Role == schema.Tool {
			out = append(out, msg.Content)
		}
	}
	return out
}

// durableFixture 是一整套可重开的装配：状态文件和快照目录都落在同一个目录下，
// 因此可以丢掉内存对象、按同样的路径重新打开，模拟进程重启。
type durableFixture struct {
	dir         string
	store       *approval.Store
	gate        *Gate
	approver    *stubApprover
	real        *fakeMutating
	checkpoints *approval.CheckpointStore
	model       *scriptedToolModel
	runner      *adk.Runner
}

func newDurableFixture(t *testing.T, dir string, decision Decision, approverErr error) *durableFixture {
	t.Helper()
	ctx := context.Background()

	store, err := approval.OpenStore(approval.WithStateFile(filepath.Join(dir, "approvals.json")))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	approver := &stubApprover{decision: decision, err: approverErr}
	gate := NewGate(store, WithApprover(approver), WithDurablePause())

	real := newFakeMutating("restart_plugin")
	gated, err := gate.Wrap(ctx, real)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	checkpoints, err := approval.OpenCheckpointStore(filepath.Join(dir, "checkpoints"))
	if err != nil {
		t.Fatalf("OpenCheckpointStore() error = %v", err)
	}

	cm := &scriptedToolModel{toolName: "restart_plugin", args: `{"sn":"SN001","plugin":"cache"}`}
	flowAgent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "durable-test-agent",
		Description: "变更审核中断测试",
		Instruction: "测试用",
		Model:       cm,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: []tool.BaseTool{gated}},
		},
		MaxIterations: 4,
	})
	if err != nil {
		t.Fatalf("NewChatModelAgent() error = %v", err)
	}

	return &durableFixture{
		dir: dir, store: store, gate: gate, approver: approver, real: real,
		checkpoints: checkpoints, model: cm,
		runner: adk.NewRunner(ctx, adk.RunnerConfig{
			Agent: flowAgent, CheckPointStore: checkpoints,
		}),
	}
}

// drain 消费一段事件流，返回根因中断点和最后一条不含 tool call 的回复。
func drain(t *testing.T, iter *adk.AsyncIterator[*adk.AgentEvent]) ([]*adk.InterruptCtx, string) {
	t.Helper()
	var (
		pending []*adk.InterruptCtx
		final   string
	)
	for {
		event, ok := iter.Next()
		if !ok {
			return pending, final
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			t.Fatalf("事件流返回错误: %v", event.Err)
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			for _, ic := range event.Action.Interrupted.InterruptContexts {
				if ic != nil && ic.IsRootCause {
					pending = append(pending, ic)
				}
			}
		}
		out := event.Output
		if out == nil || out.MessageOutput == nil || out.MessageOutput.Message == nil {
			continue
		}
		msg := out.MessageOutput.Message
		if msg.Role == schema.Assistant && len(msg.ToolCalls) == 0 && msg.Content != "" {
			final = msg.Content
		}
	}
}

const durableCheckpointID = "run-durable-test"

func (f *durableFixture) start(t *testing.T) ([]*adk.InterruptCtx, string) {
	t.Helper()
	return drain(t, f.runner.Run(
		context.Background(),
		[]*schema.Message{schema.UserMessage("重启 SN001 的 cache 插件")},
		adk.WithCheckPointID(durableCheckpointID),
	))
}

func (f *durableFixture) resume(t *testing.T, targets map[string]any) ([]*adk.InterruptCtx, string) {
	t.Helper()
	iter, err := f.runner.ResumeWithParams(context.Background(), durableCheckpointID,
		&adk.ResumeParams{Targets: targets}, adk.WithCheckPointID(durableCheckpointID))
	if err != nil {
		t.Fatalf("ResumeWithParams() error = %v", err)
	}
	return drain(t, iter)
}

// pauseOf 断言恰好挂起了一个变更点，并取出它的审批说明。
func pauseOf(t *testing.T, pending []*adk.InterruptCtx) (*adk.InterruptCtx, PauseInfo) {
	t.Helper()
	if len(pending) != 1 {
		t.Fatalf("根因中断点 = %d 个, want 1", len(pending))
	}
	info, ok := pending[0].Info.(PauseInfo)
	if !ok {
		t.Fatalf("中断说明类型 = %T, want tools.PauseInfo", pending[0].Info)
	}
	return pending[0], info
}

// bindPause 走一遍 agent 层在请人确认之前必做的那步：把提案钉在这一轮的
// 分支、快照和暂停点上。三样缺一不可，重启后就是靠它们把这一轮拼回来。
func bindPause(t *testing.T, g *Gate, proposalID, interruptID string) {
	t.Helper()
	if err := g.BindPause(proposalID, durableBinding(interruptID)); err != nil {
		t.Fatalf("BindPause() error = %v", err)
	}
}

func durableBinding(interruptID string) approval.InterruptBinding {
	return approval.InterruptBinding{
		CheckpointID: durableCheckpointID,
		InterruptID:  interruptID,
		Flow:         "plugin-diagnostic",
	}
}

// 开启中断式审核后，变更工具必须把本轮挂起，而不是在调用栈里等人：
// 挂起时谁都没被问过，设备上也什么都没发生。
func TestDurablePauseSuspendsBeforeAskingAnyone(t *testing.T) {
	f := newDurableFixture(t, t.TempDir(), Decision{Approved: true, Decider: "alice"}, nil)

	pending, final := f.start(t)
	ic, info := pauseOf(t, pending)

	if f.real.runs.Load() != 0 {
		t.Fatalf("挂起阶段真实工具被执行了 %d 次", f.real.runs.Load())
	}
	if f.approver.calls.Load() != 0 {
		t.Fatalf("挂起阶段就问了人 %d 次；决定应由 agent 层在恢复前取得", f.approver.calls.Load())
	}
	if final != "" {
		t.Errorf("本轮被挂起却给出了结论: %q", final)
	}

	if info.Tool != "restart_plugin" || info.Args != `{"sn":"SN001","plugin":"cache"}` {
		t.Errorf("审批说明 = %#v，工具或原始参数不对", info)
	}
	if info.ProposalID == "" || ic.ID == "" {
		t.Fatalf("审批说明缺少提案号或中断点标识: proposal=%q interrupt=%q", info.ProposalID, ic.ID)
	}
	p, ok := f.store.Get(info.ProposalID)
	if !ok || p.Status != approval.StatusPending {
		t.Fatalf("提案 %s 未登记为 pending: %#v", info.ProposalID, p)
	}

	// 执行上下文必须已经落盘，否则进程一退这一轮就没了。
	if _, exists, err := f.checkpoints.Get(context.Background(), durableCheckpointID); err != nil || !exists {
		t.Fatalf("快照未落盘: exists = %v, err = %v", exists, err)
	}
}

// 完整跑一遍：挂起 → 绑定 → 取得批准 → 恢复 → 同一轮里把真实输出交回模型。
func TestDurablePauseResumesWithRealResultAfterApproval(t *testing.T) {
	f := newDurableFixture(t, t.TempDir(), Decision{Approved: true, Decider: "alice"}, nil)
	ctx := context.Background()

	pending, _ := f.start(t)
	ic, info := pauseOf(t, pending)

	bindPause(t, f.gate, info.ProposalID, ic.ID)
	bound, _ := f.store.Get(info.ProposalID)
	if bound.CheckpointID != durableCheckpointID || bound.InterruptID != ic.ID {
		t.Fatalf("中断关联未落盘: checkpoint=%q interrupt=%q", bound.CheckpointID, bound.InterruptID)
	}

	decided, err := f.gate.Resolve(ctx, info.ProposalID)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if decided.Status != approval.StatusApproved || decided.Decider != "alice" {
		t.Fatalf("决定未持久化: status=%s decider=%q", decided.Status, decided.Decider)
	}
	// 决定必须在恢复之前就落盘：中间崩了也不能变成「批过又要重批」。
	if f.real.runs.Load() != 0 {
		t.Fatalf("Resolve 阶段就下发了 %d 次；下发只能发生在恢复之后", f.real.runs.Load())
	}

	_, final := f.resume(t, map[string]any{ic.ID: nil})

	if f.real.runs.Load() != 1 {
		t.Fatalf("真实工具执行 %d 次, want 1", f.real.runs.Load())
	}
	if f.real.lastArgs() != `{"sn":"SN001","plugin":"cache"}` {
		t.Errorf("下发参数被改写: %s", f.real.lastArgs())
	}
	if final != "命令已执行完毕。" {
		t.Errorf("最终回复 = %q", final)
	}

	done, _ := f.store.Get(info.ProposalID)
	if done.Status != approval.StatusExecuted {
		t.Fatalf("提案状态 = %s, want executed", done.Status)
	}

	// 模型要在同一轮里拿到设备真实输出，而不是「已提交」这种含糊回执。
	results := f.model.toolResults()
	if len(results) != 1 {
		t.Fatalf("模型看到的工具结果 = %d 条, want 1", len(results))
	}
	var resp gateResponse
	if err := json.Unmarshal([]byte(results[0]), &resp); err != nil {
		t.Fatalf("工具结果不是合法 JSON: %v\n%s", err, results[0])
	}
	if resp.Status != string(approval.StatusExecuted) || resp.Result != "restarted" {
		t.Errorf("回喂模型的结果 = %#v，没带上真实输出", resp)
	}
}

// 驳回同样走恢复：设备上什么都不做，模型拿到理由。
func TestDurablePauseResumesWithRejection(t *testing.T) {
	f := newDurableFixture(t, t.TempDir(),
		Decision{Approved: false, Decider: "bob", Reason: "线上高峰期"}, nil)
	ctx := context.Background()

	pending, _ := f.start(t)
	ic, info := pauseOf(t, pending)
	bindPause(t, f.gate, info.ProposalID, ic.ID)
	if _, err := f.gate.Resolve(ctx, info.ProposalID); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if _, final := f.resume(t, map[string]any{ic.ID: nil}); final != "命令已执行完毕。" {
		t.Errorf("最终回复 = %q", final)
	}
	if f.real.runs.Load() != 0 {
		t.Fatalf("被驳回的操作执行了 %d 次", f.real.runs.Load())
	}

	p, _ := f.store.Get(info.ProposalID)
	if p.Status != approval.StatusRejected || p.Decider != "bob" {
		t.Fatalf("提案状态 = %s, 决策人 = %q", p.Status, p.Decider)
	}

	results := f.model.toolResults()
	if len(results) != 1 {
		t.Fatalf("模型看到的工具结果 = %d 条, want 1", len(results))
	}
	var resp gateResponse
	if err := json.Unmarshal([]byte(results[0]), &resp); err != nil {
		t.Fatalf("工具结果不是合法 JSON: %v", err)
	}
	if resp.Status != "rejected" || !strings.Contains(resp.Reason, "线上高峰期") {
		t.Errorf("回喂模型的驳回结果 = %#v", resp)
	}
}

// 取不到人的决定时，中断式审核也必须朝安全那边倒。
func TestDurablePauseTreatsApproverErrorAsRejection(t *testing.T) {
	f := newDurableFixture(t, t.TempDir(), Decision{Approved: true, Decider: "alice"},
		errors.New("界面已关闭"))
	ctx := context.Background()

	pending, _ := f.start(t)
	ic, info := pauseOf(t, pending)
	bindPause(t, f.gate, info.ProposalID, ic.ID)
	decided, err := f.gate.Resolve(ctx, info.ProposalID)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if decided.Status != approval.StatusRejected {
		t.Fatalf("审核出错后提案状态 = %s, want rejected", decided.Status)
	}

	f.resume(t, map[string]any{ic.ID: nil})
	if f.real.runs.Load() != 0 {
		t.Fatalf("取不到人工确认却执行了 %d 次", f.real.runs.Load())
	}
}

// M4 的核心：中断期间进程退出后，凭落盘的提案和快照仍能把这一轮跑完。
// 新装配连一个内存对象都不共享，只共享磁盘上的两个路径。
func TestDurablePauseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	before := newDurableFixture(t, dir, Decision{Approved: true, Decider: "alice"}, nil)
	pending, _ := before.start(t)
	ic, info := pauseOf(t, pending)
	bindPause(t, before.gate, info.ProposalID, ic.ID)

	// —— 进程在这里退出：before 的一切都不再使用 ——
	after := newDurableFixture(t, dir, Decision{Approved: true, Decider: "alice"}, nil)

	restored, ok := after.store.Get(info.ProposalID)
	if !ok {
		t.Fatalf("重启后丢失提案 %s", info.ProposalID)
	}
	if restored.Status != approval.StatusPending {
		t.Fatalf("重启后提案状态 = %s, want pending", restored.Status)
	}
	if restored.CheckpointID != durableCheckpointID || restored.InterruptID != ic.ID {
		t.Fatalf("重启后中断关联丢失: checkpoint=%q interrupt=%q",
			restored.CheckpointID, restored.InterruptID)
	}
	if restored.Args != `{"sn":"SN001","plugin":"cache"}` {
		t.Errorf("重启后原始参数被改写: %s", restored.Args)
	}

	if _, err := after.gate.Resolve(ctx, restored.ID); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	// 恢复只需要提案里存着的两个 ID，不需要上一进程的任何内存状态。
	_, final := after.resume(t, map[string]any{restored.InterruptID: nil})

	if after.real.runs.Load() != 1 {
		t.Fatalf("重启后执行 %d 次, want 1", after.real.runs.Load())
	}
	if before.real.runs.Load() != 0 {
		t.Fatalf("旧装配的工具被执行了 %d 次", before.real.runs.Load())
	}
	if final != "命令已执行完毕。" {
		t.Errorf("重启恢复后的回复 = %q", final)
	}
	done, _ := after.store.Get(restored.ID)
	if done.Status != approval.StatusExecuted || done.Decider != "alice" {
		t.Fatalf("重启恢复后提案 = %#v", done)
	}
}

// 同一提案最多下发一次：拿同一份快照再恢复一遍，工具不能被再跑一次，
// 模型应当拿到重放的既有结果。
func TestDurablePauseNeverDispatchesTwice(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	f := newDurableFixture(t, dir, Decision{Approved: true, Decider: "alice"}, nil)
	pending, _ := f.start(t)
	ic, info := pauseOf(t, pending)
	bindPause(t, f.gate, info.ProposalID, ic.ID)
	if _, err := f.gate.Resolve(ctx, info.ProposalID); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	f.resume(t, map[string]any{ic.ID: nil})
	if f.real.runs.Load() != 1 {
		t.Fatalf("首次恢复执行 %d 次, want 1", f.real.runs.Load())
	}

	// 重复恢复模拟「审核人点了两次」或恢复动作被重试。
	replay := newDurableFixture(t, dir, Decision{Approved: true, Decider: "alice"}, nil)
	replay.resume(t, map[string]any{ic.ID: nil})

	if replay.real.runs.Load() != 0 {
		t.Fatalf("重复恢复又下发了 %d 次；同一提案只能下发一次", replay.real.runs.Load())
	}
	results := replay.model.toolResults()
	if len(results) != 1 {
		t.Fatalf("重放时模型看到的工具结果 = %d 条, want 1", len(results))
	}
	var resp gateResponse
	if err := json.Unmarshal([]byte(results[0]), &resp); err != nil {
		t.Fatalf("工具结果不是合法 JSON: %v", err)
	}
	if resp.Status != string(approval.StatusExecuted) || resp.ProposalID != info.ProposalID {
		t.Errorf("重放结果 = %#v，应当原样回放既有结论", resp)
	}
}

// 不在 eino 运行上下文里直接调用时（现有同步测试和外部调用方），
// 必须退回同步审核，而不是抛出一个没人处理的中断。
func TestDurablePauseFallsBackToSynchronousOutsideRun(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("restart_plugin")
	ap := &stubApprover{decision: Decision{Approved: true, Decider: "alice"}}
	gate := NewGate(approval.NewStore(), WithApprover(ap), WithDurablePause())

	gated, err := gate.Wrap(ctx, real)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","plugin":"cache"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if ap.calls.Load() != 1 || real.runs.Load() != 1 {
		t.Fatalf("审核 %d 次、执行 %d 次, want 1/1", ap.calls.Load(), real.runs.Load())
	}
	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != string(approval.StatusExecuted) {
		t.Errorf("status = %q, want executed", resp.Status)
	}
}
