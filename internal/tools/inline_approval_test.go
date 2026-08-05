package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"diagnostic-system/internal/approval"
)

// 配了 Approver 就是「当场确认、当场执行」：批准后模型直接拿到真实输出，
// 不再有中间的 pending 态。
func TestInlineApprovalExecutesAndReturnsResult(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	ap := &stubApprover{decision: Decision{Approved: true, Decider: "alice"}}
	gate := NewGate(approval.NewStore(), WithApprover(ap))

	gated, err := gate.Wrap(ctx, real)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	if ap.calls.Load() != 1 {
		t.Fatalf("审核被问了 %d 次, want 1", ap.calls.Load())
	}
	if real.runs.Load() != 1 {
		t.Fatalf("真实工具执行 %d 次, want 1", real.runs.Load())
	}
	if real.lastArgs() != `{"sn":"SN001","cmd":"date"}` {
		t.Errorf("执行参数被改写: %s", real.lastArgs())
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != string(approval.StatusExecuted) {
		t.Errorf("status = %q, want executed", resp.Status)
	}
	// 模型必须拿到真实输出，否则它只能靠猜。
	if resp.Result != "restarted" {
		t.Errorf("result = %q, want restarted", resp.Result)
	}
}

// 审核人看到的风险评估必须是从实际参数里算出来的。
func TestInlineApprovalPassesRiskAssessment(t *testing.T) {
	ctx := context.Background()
	ap := &stubApprover{decision: Decision{Approved: false, Decider: "alice", Reason: "先不动"}}
	gate := NewGate(approval.NewStore(), WithApprover(ap))

	tunnelTool, err := NewTunnelTool()
	if err != nil {
		t.Fatalf("NewTunnelTool() error = %v", err)
	}
	gated, err := gate.Wrap(ctx, tunnelTool)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	if _, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"rm -rf /data/cache"}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	risk := ap.risk()
	if risk.Level != LevelDangerous {
		t.Errorf("风险等级 = %s, want 危险", risk.Level)
	}
	if risk.Command != "rm -rf /data/cache" {
		t.Errorf("展示给审核人的命令 = %q", risk.Command)
	}
	if risk.Target != "SN001" {
		t.Errorf("展示给审核人的节点 = %q", risk.Target)
	}
}

func TestInlineRejectionSkipsExecution(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	ap := &stubApprover{decision: Decision{Approved: false, Decider: "bob", Reason: "线上高峰期"}}
	store := approval.NewStore()
	gate := NewGate(store, WithApprover(ap))

	gated, _ := gate.Wrap(ctx, real)
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"reboot"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	if real.runs.Load() != 0 {
		t.Fatalf("被拒绝的操作执行了 %d 次", real.runs.Load())
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != "rejected" {
		t.Errorf("status = %q, want rejected", resp.Status)
	}
	// 驳回理由要回到模型手里，它才知道该换方案而不是原样重试。
	if !strings.Contains(resp.Reason, "线上高峰期") {
		t.Errorf("驳回理由没回传: %q", resp.Reason)
	}

	p, _ := store.Get(resp.ProposalID)
	if p.Status != approval.StatusRejected {
		t.Errorf("提案状态 = %s, want rejected", p.Status)
	}
	if p.Decider != "bob" {
		t.Errorf("决策人 = %q, want bob", p.Decider)
	}
}

// 问不到人（stdin 断了、卡片超时…）必须当作「不批准」。
// 这是 fail-safe 的方向问题：默认放行的话，一个 IO 错误就等于无人值守执行。
func TestApproverErrorDefaultsToNotExecuting(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	ap := &stubApprover{err: errors.New("stdin 已关闭")}
	store := approval.NewStore()
	gate := NewGate(store, WithApprover(ap))

	gated, _ := gate.Wrap(ctx, real)
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"reboot"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	if real.runs.Load() != 0 {
		t.Fatalf("取不到审核结果却执行了 %d 次", real.runs.Load())
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != "rejected" {
		t.Errorf("status = %q, want rejected", resp.Status)
	}

	p, _ := store.Get(resp.ProposalID)
	if p.Status != approval.StatusRejected {
		t.Errorf("提案状态 = %s, want rejected", p.Status)
	}
}

// 没配 Approver 时保持异步模式：只留提案，等外部 Execute/Reject。
// 钉钉那种「发卡片、等人点」的场景走这条路。
func TestWithoutApproverStaysPending(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	store := approval.NewStore()
	gate := NewGate(store)

	gated, _ := gate.Wrap(ctx, real)
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	if real.runs.Load() != 0 {
		t.Fatalf("异步模式下不该执行，实际执行了 %d 次", real.runs.Load())
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", resp.Status)
	}
	if len(store.Pending()) != 1 {
		t.Errorf("待审提案 = %d 条, want 1", len(store.Pending()))
	}
}

// 每一次调用都要单独问一次，不存在「批过一次就一直放行」。
func TestEveryCallIsReviewedSeparately(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	ap := &stubApprover{decision: Decision{Approved: true, Decider: "alice"}}
	gate := NewGate(approval.NewStore(), WithApprover(ap))

	gated, _ := gate.Wrap(ctx, real)
	for range 3 {
		if _, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`); err != nil {
			t.Fatalf("InvokableRun() error = %v", err)
		}
	}

	if ap.calls.Load() != 3 {
		t.Errorf("审核次数 = %d, want 3（有调用绕过了确认）", ap.calls.Load())
	}
	if real.runs.Load() != 3 {
		t.Errorf("执行次数 = %d, want 3", real.runs.Load())
	}
}

// 执行失败时模型要拿到 error，而不是一个看起来成功的空结果。
func TestInlineApprovalReportsExecutionFailure(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	real.err = errors.New("节点离线")
	ap := &stubApprover{decision: Decision{Approved: true, Decider: "alice"}}
	gate := NewGate(approval.NewStore(), WithApprover(ap))

	gated, _ := gate.Wrap(ctx, real)
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != string(approval.StatusFailed) {
		t.Errorf("status = %q, want failed", resp.Status)
	}
	if !strings.Contains(resp.Error, "节点离线") {
		t.Errorf("error = %q，没把失败原因给模型", resp.Error)
	}
}
