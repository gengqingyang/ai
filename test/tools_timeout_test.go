package test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "diagnostic-system/internal/tools"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/approval"
)

// hangingTool 卡住不返回，直到测试放行。模拟节点没响应、tunnel 网关吞了请求。
type hangingTool struct {
	release  chan struct{}
	started  atomic.Int32
	returned atomic.Int32
}

func newHangingTool(t *testing.T) *hangingTool {
	h := &hangingTool{release: make(chan struct{})}
	// 测试结束时放行，别把 goroutine 留到进程退出。
	t.Cleanup(func() { close(h.release) })
	return h
}

func (h *hangingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "run_tunnel_cmd", Desc: "卡住的工具"}, nil
}

func (h *hangingTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	h.started.Add(1)
	// 故意不 select ctx.Done()：真实的同步 HTTP 客户端未必理会 ctx，
	// 闸门的超时必须在这种情况下也能生效。
	<-h.release
	h.returned.Add(1)
	return "late", nil
}

// 超时必须真的把控制权还回来，并且说清楚是超时——而不是卡死整个终端。
func TestExecutionTimesOut(t *testing.T) {
	ctx := context.Background()
	hang := newHangingTool(t)
	ap := &stubApprover{decision: Decision{Approved: true, Decider: "alice"}}
	store := approval.NewStore()
	gate := NewGate(store, WithApprover(ap), WithTimeout(50*time.Millisecond))

	gated, err := gate.Wrap(ctx, hang)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	done := make(chan string, 1)
	go func() {
		out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"sleep 999"}`)
		if err != nil {
			t.Errorf("InvokableRun() error = %v", err)
		}
		done <- out
	}()

	var out string
	select {
	case out = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("超时没生效，调用一直没返回")
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != string(approval.StatusUnknown) {
		t.Errorf("status = %q, want unknown", resp.Status)
	}
	// 模型和人都要看到「超时」两个字，而不是 context deadline exceeded。
	if !strings.Contains(resp.Error, "调用超时") {
		t.Errorf("error = %q，没说清楚是超时", resp.Error)
	}
	if hang.returned.Load() != 0 {
		t.Error("超时后不该等工具自己收场")
	}

	p, _ := store.Get(resp.ProposalID)
	if p.Status != approval.StatusUnknown {
		t.Errorf("提案状态 = %s, want unknown", p.Status)
	}
	if !strings.Contains(p.Error, "调用超时") {
		t.Errorf("审计日志里的失败原因 = %q", p.Error)
	}
}

// 超时只圈住执行那一段。人想两分钟再点「执行」，不该把命令的超时耗光。
func TestTimeoutExcludesApprovalWait(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	ap := &slowApprover{delay: 120 * time.Millisecond, decision: Decision{Approved: true, Decider: "alice"}}
	gate := NewGate(approval.NewStore(), WithApprover(ap), WithTimeout(60*time.Millisecond))

	gated, _ := gate.Wrap(ctx, real)
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if resp.Status != string(approval.StatusExecuted) {
		t.Fatalf("status = %q, want executed（审核耗时被算进了执行超时）", resp.Status)
	}
	if real.runs.Load() != 1 {
		t.Errorf("执行次数 = %d, want 1", real.runs.Load())
	}
}

// 不配超时就不限时，保持原来的行为。
func TestZeroTimeoutMeansNoLimit(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	ap := &stubApprover{decision: Decision{Approved: true, Decider: "alice"}}
	gate := NewGate(approval.NewStore(), WithApprover(ap), WithTimeout(0))

	gated, _ := gate.Wrap(ctx, real)
	if _, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if real.runs.Load() != 1 {
		t.Errorf("执行次数 = %d, want 1", real.runs.Load())
	}
}

func TestCallerDeadlineWithoutGateTimeoutIsUnknown(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	real.err = context.DeadlineExceeded
	ap := &stubApprover{decision: Decision{Approved: true, Decider: "alice"}}
	store := approval.NewStore()
	gate := NewGate(store, WithApprover(ap), WithTimeout(0))

	gated, _ := gate.Wrap(ctx, real)
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != string(approval.StatusUnknown) || !strings.Contains(resp.Error, "期限") {
		t.Fatalf("deadline 返回=%#v, want unknown", resp)
	}
}

// 实现了 Noticer 的审核入口要拿到最终提案，终端才有回执可打。
func TestNoticerReceivesOutcome(t *testing.T) {
	ctx := context.Background()
	real := newFakeMutating("run_tunnel_cmd")
	real.err = errors.New("节点离线")
	ap := &noticingApprover{stubApprover: stubApprover{decision: Decision{Approved: true, Decider: "alice"}}}
	gate := NewGate(approval.NewStore(), WithApprover(ap))

	gated, _ := gate.Wrap(ctx, real)
	if _, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"date"}`); err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	got, _ := ap.last.Load().(*approval.Proposal)
	if got == nil {
		t.Fatal("没收到执行回执")
	}
	if got.Status != approval.StatusFailed {
		t.Errorf("回执状态 = %s, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "节点离线") {
		t.Errorf("回执里没有失败原因: %q", got.Error)
	}
}

func TestDescribeRunError(t *testing.T) {
	timeout := 30 * time.Second

	if got := DescribeRunError(nil, timeout); got != nil {
		t.Errorf("describeRunError(nil) = %v, want nil", got)
	}

	got := DescribeRunError(context.DeadlineExceeded, timeout)
	if !errors.Is(got, ErrToolTimeout) {
		t.Errorf("超时没有归到 ErrToolTimeout: %v", got)
	}
	if !strings.Contains(got.Error(), "30s") {
		t.Errorf("错误里没说等了多久: %q", got)
	}

	own := errors.New("节点离线")
	if got := DescribeRunError(own, timeout); !errors.Is(got, own) {
		t.Errorf("普通错误被改写了: %v", got)
	}
}

// eino 会给工具错误套一层「[LocalFunc] failed to invoke tool, toolName=…」。
// 那截前缀对审核人没有信息量，弹到终端上只会盖住真正的原因。
func TestUnwrapToolError(t *testing.T) {
	wrapped := errors.New("[LocalFunc] failed to invoke tool, toolName=run_tunnel_cmd, err=下发到节点 SN001 失败: http status code [400]")
	got := UnwrapToolError(wrapped).Error()
	if got != "下发到节点 SN001 失败: http status code [400]" {
		t.Errorf("剥壳结果 = %q", got)
	}

	plain := errors.New("节点离线")
	if UnwrapToolError(plain) != plain {
		t.Error("不带外壳的错误不该被动")
	}
}

// slowApprover 模拟人在终端前想了一会儿才点。
type slowApprover struct {
	delay    time.Duration
	decision Decision
}

func (s *slowApprover) Review(_ context.Context, _ *approval.Proposal, _ RiskAssessment) (Decision, error) {
	time.Sleep(s.delay)
	return s.decision, nil
}

// noticingApprover 是实现了可选 Noticer 接口的审核桩。
type noticingApprover struct {
	stubApprover
	last atomic.Value // *approval.Proposal
}

func (n *noticingApprover) Notice(p *approval.Proposal) { n.last.Store(p) }
