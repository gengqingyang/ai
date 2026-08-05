package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/approval"
)

// 闸门每次动作都写 slog，测试里丢掉，免得刷屏盖住失败信息。
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// stubApprover 是可编程的人工审核桩：记录被问了几次、问到了什么。
type stubApprover struct {
	decision Decision
	err      error

	calls    atomic.Int32
	lastRisk atomic.Value // RiskAssessment
	lastID   atomic.Value // string
}

func (s *stubApprover) Review(_ context.Context, p *approval.Proposal, risk RiskAssessment) (Decision, error) {
	s.calls.Add(1)
	s.lastRisk.Store(risk)
	s.lastID.Store(p.ID)
	if s.err != nil {
		return Decision{}, s.err
	}
	return s.decision, nil
}

func (s *stubApprover) risk() RiskAssessment {
	r, _ := s.lastRisk.Load().(RiskAssessment)
	return r
}

// fakeMutating 是一个假的变更工具，记录自己被真正执行了几次。
// 整套测试就是围绕这个计数器：模型碰它的时候它必须一动不动。
type fakeMutating struct {
	name  string
	runs  atomic.Int32
	gotIn atomic.Value // string，最后一次收到的参数
	err   error
}

func newFakeMutating(name string) *fakeMutating {
	return &fakeMutating{name: name}
}

func (f *fakeMutating) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "重启节点上的某个插件"}, nil
}

func (f *fakeMutating) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	f.runs.Add(1)
	f.gotIn.Store(argumentsInJSON)
	if f.err != nil {
		return "", f.err
	}
	return "restarted", nil
}

func (f *fakeMutating) lastArgs() string {
	v, _ := f.gotIn.Load().(string)
	return v
}

func newTestGate() (*Gate, *approval.Store) {
	store := approval.NewStore()
	return NewGate(store), store
}

// 核心安全属性：模型调用被包装后的工具，真实实现一次都不能被执行。
func TestGatedToolDoesNotExecute(t *testing.T) {
	ctx := context.Background()
	gate, store := newTestGate()
	real := newFakeMutating("restart_plugin")

	gated, err := gate.Wrap(ctx, real)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","plugin":"cache"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if got := real.runs.Load(); got != 0 {
		t.Fatalf("真实工具被执行了 %d 次，审核闸门形同虚设", got)
	}

	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("闸门返回不是合法 JSON: %v\n%s", err, out)
	}
	if resp.Status != "pending_approval" {
		t.Errorf("status = %q, want pending_approval", resp.Status)
	}
	if resp.ProposalID == "" {
		t.Error("闸门返回里没有提案 ID，审核人无从指认")
	}
	if resp.Tool != "restart_plugin" {
		t.Errorf("tool = %q, want restart_plugin", resp.Tool)
	}

	p, ok := store.Get(resp.ProposalID)
	if !ok {
		t.Fatal("提案未登记进 store")
	}
	if !p.Pending() {
		t.Errorf("提案状态 = %s, want pending", p.Status)
	}
	if p.Args != `{"sn":"SN001","plugin":"cache"}` {
		t.Errorf("提案参数被改写: %s", p.Args)
	}
}

// GatedTool 不该持有真实工具的引用——安全性来自结构，而不是来自「我们记得别调它」。
func TestGatedToolHoldsNoRealTool(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()

	gated, err := gate.Wrap(ctx, newFakeMutating("restart_plugin"))
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if gated.gate != gate {
		t.Error("GatedTool 未指向闸门")
	}
	// 字段只有 gate 和 info；真实实现只存在于 Gate.inner 里。
	if _, ok := gate.inner["restart_plugin"]; !ok {
		t.Error("真实工具未被闸门扣住")
	}
}

// 模型看到的描述必须写明「这只是提案」，否则它会把提交当成完成。
func TestWrapAnnotatesDescription(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()
	real := newFakeMutating("restart_plugin")

	gated, err := gate.Wrap(ctx, real)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	info, err := gated.Info(ctx)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if !strings.Contains(info.Desc, "重启节点上的某个插件") {
		t.Error("原始描述被丢掉了")
	}
	for _, must := range []string{"变更操作", "必须先由人工确认", "不要臆测"} {
		if !strings.Contains(info.Desc, must) {
			t.Errorf("工具描述缺少 %q", must)
		}
	}

	// 原工具的 ToolInfo 不能被就地改写。
	origInfo, _ := real.Info(ctx)
	if strings.Contains(origInfo.Desc, "变更操作") {
		t.Error("Wrap 污染了原工具的 ToolInfo")
	}
}

func TestExecuteRunsRealToolWithOriginalArgs(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()
	real := newFakeMutating("restart_plugin")

	gated, err := gate.Wrap(ctx, real)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}

	const args = `{"sn":"SN001","plugin":"cache"}`
	out, err := gated.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("解析闸门返回失败: %v", err)
	}

	done, err := gate.Execute(ctx, resp.ProposalID, "alice")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := real.runs.Load(); got != 1 {
		t.Fatalf("真实工具执行 %d 次, want 1", got)
	}
	// 审核人看到的参数和实际执行的参数必须是同一个东西。
	if real.lastArgs() != args {
		t.Errorf("执行参数 = %s, want %s", real.lastArgs(), args)
	}
	if done.Status != approval.StatusExecuted {
		t.Errorf("提案状态 = %s, want %s", done.Status, approval.StatusExecuted)
	}
	if done.Result != "restarted" {
		t.Errorf("执行输出 = %q", done.Result)
	}
	if done.Decider != "alice" {
		t.Errorf("批准人 = %q, want alice", done.Decider)
	}
}

func TestExecuteRecordsFailure(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()
	real := newFakeMutating("restart_plugin")
	real.err = errors.New("ssh 不通")

	gated, _ := gate.Wrap(ctx, real)
	out, _ := gated.InvokableRun(ctx, "{}")
	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("解析闸门返回失败: %v", err)
	}

	done, err := gate.Execute(ctx, resp.ProposalID, "alice")
	if err != nil {
		t.Fatalf("Execute() 不该因工具执行失败而报错: %v", err)
	}
	if done.Status != approval.StatusFailed {
		t.Errorf("提案状态 = %s, want %s", done.Status, approval.StatusFailed)
	}
	if !strings.Contains(done.Error, "ssh 不通") {
		t.Errorf("执行错误未记录: %q", done.Error)
	}
}

func TestRejectPreventsExecution(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()
	real := newFakeMutating("restart_plugin")

	gated, _ := gate.Wrap(ctx, real)
	out, _ := gated.InvokableRun(ctx, "{}")
	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("解析闸门返回失败: %v", err)
	}

	p, err := gate.Reject(resp.ProposalID, "bob", "影响面太大")
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if p.Status != approval.StatusRejected {
		t.Errorf("提案状态 = %s, want %s", p.Status, approval.StatusRejected)
	}

	if _, err := gate.Execute(ctx, resp.ProposalID, "alice"); err == nil {
		t.Fatal("已驳回的提案仍被执行")
	}
	if got := real.runs.Load(); got != 0 {
		t.Fatalf("已驳回的提案执行了真实工具 %d 次", got)
	}
}

// 同一条提案被批两次，命令就会在客户设备上跑两遍。
func TestExecuteTwiceRunsOnlyOnce(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()
	real := newFakeMutating("restart_plugin")

	gated, _ := gate.Wrap(ctx, real)
	out, _ := gated.InvokableRun(ctx, "{}")
	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("解析闸门返回失败: %v", err)
	}

	if _, err := gate.Execute(ctx, resp.ProposalID, "alice"); err != nil {
		t.Fatalf("首次 Execute() error = %v", err)
	}
	if _, err := gate.Execute(ctx, resp.ProposalID, "alice"); err == nil {
		t.Fatal("同一条提案被执行了两次")
	}
	if got := real.runs.Load(); got != 1 {
		t.Fatalf("真实工具执行 %d 次, want 1", got)
	}
}

func TestExecuteUnknownProposal(t *testing.T) {
	gate, _ := newTestGate()
	if _, err := gate.Execute(context.Background(), "P999", "alice"); err == nil {
		t.Error("Execute() 未知提案应报错")
	}
}

func TestWrapRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()

	if _, err := gate.Wrap(ctx, newFakeMutating("restart_plugin")); err != nil {
		t.Fatalf("首次 Wrap() error = %v", err)
	}
	if _, err := gate.Wrap(ctx, newFakeMutating("restart_plugin")); err == nil {
		t.Error("同名工具被重复包装，Gate.inner 里的实现会被悄悄换掉")
	}
}

// 注册表这条检查是最后一道防线：漏包装的变更工具必须让进程启动就失败。
func TestRegistryRejectsUnwrappedMutatingTool(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()

	err := reg.Register(ctx, newFakeMutating("restart_plugin"), RiskMutating)
	if err == nil {
		t.Fatal("未包装的变更工具被注册进去了")
	}
	if !strings.Contains(err.Error(), "Gate.Wrap") {
		t.Errorf("报错未指出该怎么修: %v", err)
	}
	if len(reg.All()) != 0 {
		t.Error("注册失败却留下了记录")
	}
}

func TestRegistryAcceptsGatedMutatingTool(t *testing.T) {
	ctx := context.Background()
	gate, _ := newTestGate()
	reg := NewRegistry()

	gated, err := gate.Wrap(ctx, newFakeMutating("restart_plugin"))
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := reg.Register(ctx, gated, RiskMutating); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	risk, ok := reg.RiskOf("restart_plugin")
	if !ok || risk != RiskMutating {
		t.Errorf("RiskOf() = %v, %v; want mutating, true", risk, ok)
	}
	if len(reg.Mutating()) != 1 {
		t.Errorf("Mutating() 返回 %d 个", len(reg.Mutating()))
	}
	if len(reg.ReadOnly()) != 0 {
		t.Errorf("变更工具混进了只读列表")
	}
}

// 内置只读工具本身也要能注册，顺带保证 InferTool 的 schema 没写坏。
func TestRegisterBuiltinReadOnlyTools(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()

	nowTool, err := NewNowTool()
	if err != nil {
		t.Fatalf("NewNowTool() error = %v", err)
	}
	if err := reg.Register(ctx, nowTool, RiskReadOnly); err != nil {
		t.Fatalf("注册 now 失败: %v", err)
	}

	statusTool, err := NewDeviceStatusTool()
	if err != nil {
		t.Fatalf("NewDeviceStatusTool() error = %v", err)
	}
	if err := reg.Register(ctx, statusTool, RiskReadOnly); err != nil {
		t.Fatalf("注册 query_device_status 失败: %v", err)
	}

	if err := reg.Register(ctx, nowTool, RiskReadOnly); err == nil {
		t.Error("重名工具被重复注册")
	}
	if len(reg.ReadOnly()) != 2 {
		t.Errorf("ReadOnly() 返回 %d 个, want 2", len(reg.ReadOnly()))
	}
}

// device_id 必须是导出字段才能被 JSON 填进去；这里同时验证 schema 声明了它。
func TestDeviceStatusToolAcceptsDeviceID(t *testing.T) {
	ctx := context.Background()
	statusTool, err := NewDeviceStatusTool()
	if err != nil {
		t.Fatalf("NewDeviceStatusTool() error = %v", err)
	}

	out, err := statusTool.InvokableRun(ctx, `{"device_id":"SN001"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	var got DeviceStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("返回不是合法 JSON: %v\n%s", err, out)
	}
	if got.DeviceID != "SN001" {
		t.Errorf("device_id 没被传进去: %q", got.DeviceID)
	}
	// mock 数据必须自报家门，否则模型会拿它当真实诊断依据。
	if got.DataSource != "mock" {
		t.Errorf("data_source = %q, want mock", got.DataSource)
	}

	if _, err := statusTool.InvokableRun(ctx, `{}`); err == nil {
		t.Error("空 device_id 应报错")
	}
}

// tunnel 工具的 schema 必须真的声明 sn / cmd 两个参数。
// 之前 Cmd 写成未导出的 cmd，schema 里根本没有这个参数，模型永远传不进命令。
// 这里只检查 schema，不触发任何远程执行。
func TestTunnelToolSchemaDeclaresParams(t *testing.T) {
	ctx := context.Background()
	tunnelTool, err := NewTunnelTool()
	if err != nil {
		t.Fatalf("NewTunnelTool() error = %v", err)
	}

	info, err := tunnelTool.Info(ctx)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "run_tunnel_cmd" {
		t.Errorf("工具名 = %q", info.Name)
	}

	sch, err := info.ToJSONSchema()
	if err != nil {
		t.Fatalf("导出 schema 失败: %v", err)
	}
	if sch == nil {
		t.Fatal("工具没有声明任何参数")
	}

	required := strings.Join(sch.Required, ",")
	for _, must := range []string{"sn", "cmd"} {
		if !strings.Contains(required, must) {
			t.Errorf("schema 未把 %q 列为必填，模型可能不传这个值（required=%v）", must, sch.Required)
		}
	}

	b, err := json.Marshal(sch)
	if err != nil {
		t.Fatalf("序列化 schema 失败: %v", err)
	}
	schemaJSON := string(b)
	for _, must := range []string{`"sn"`, `"cmd"`} {
		if !strings.Contains(schemaJSON, must) {
			t.Errorf("schema 缺少参数 %s，模型传不进这个值:\n%s", must, schemaJSON)
		}
	}
}

// 装配级验证：模拟 cmd/chat 的注册流程，确认 run_tunnel_cmd 落进注册表时
// 是被闸门包过的。不触发任何远程执行。
func TestTunnelToolMustBeGatedToRegister(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry()

	tunnelTool, err := NewTunnelTool()
	if err != nil {
		t.Fatalf("NewTunnelTool() error = %v", err)
	}

	// 直接注册（漏了 Wrap）必须失败。
	if err := reg.Register(ctx, tunnelTool, RiskMutating); err == nil {
		t.Fatal("run_tunnel_cmd 未经闸门就注册成功了")
	}

	gate, store := newTestGate()
	gated, err := gate.Wrap(ctx, tunnelTool)
	if err != nil {
		t.Fatalf("Wrap() error = %v", err)
	}
	if err := reg.Register(ctx, gated, RiskMutating); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	// 模型调用它只会留下一条待审提案，不会连到任何节点。
	out, err := gated.InvokableRun(ctx, `{"sn":"SN001","cmd":"systemctl restart nginx"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	var resp gateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("解析闸门返回失败: %v", err)
	}
	pending := store.Pending()
	if len(pending) != 1 {
		t.Fatalf("待审提案 = %d 条, want 1", len(pending))
	}
	if !strings.Contains(pending[0].Args, "systemctl restart nginx") {
		t.Errorf("提案里看不到实际命令，审核人无从判断: %s", pending[0].Args)
	}
}
