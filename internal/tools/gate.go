package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/approval"
)

// Decision 是人工审核的结果。
type Decision struct {
	// Approved 为 true 表示批准执行。
	Approved bool
	// Decider 是做决定的人，写进审计日志。
	Decider string
	// Reason 是驳回理由，会回喂给模型让它换方案。
	Reason string
}

// Approver 向人索取一条操作提案的审核决定。
//
// 命令行实现是在终端里打印命令并等 y/n；也可接入其他外部审核程序，
// Gate 这一层不用改。返回 error 表示没能取到决定（比如输入流断了），
// 这种情况按「不执行」处理——拿不到人的确认就绝不动手。
type Approver interface {
	Review(ctx context.Context, p *approval.Proposal, risk RiskAssessment) (Decision, error)
}

// Noticer 是 Approver 的可选扩展：实现了它，就能在执行结束后拿到最终提案，
// 用来给审核人一个回执（跑完了没有、失败是因为超时还是别的）。
//
// 不实现也能用——回执只影响体验，凭证在审计日志里。
type Noticer interface {
	Notice(p *approval.Proposal)
}

// ErrToolTimeout 表示工具执行超时。
//
// 单独立一个哨兵值，是为了让上层能把它和普通失败区分开：超时意味着
// 「不知道跑没跑成」，命令可能还在节点上继续执行，这话得跟人说清楚。
var ErrToolTimeout = errors.New("调用超时")

// Gate 是变更类工具的人工审核闸门。
//
// 变更工具经 Wrap 包装后交给模型。模型调用它时，Gate 先登记一条提案，
// 再把提案交给 Approver 请人过目：
//
//	批准 → 回放提案里的原始参数真正执行，把真实结果返回给模型继续推理
//	驳回 → 什么都不做，把驳回理由返回给模型
//
// 没有配置 Approver 时退化成异步模式：只登记提案就返回，等外部调用
// Execute/Reject（外部管理程序或值班同事事后处理等场景）。
//
// 无论哪种模式，模型手上那个工具都没有执行能力——真实实现只存在于
// Gate.inner 里。这是结构上的隔离，不是靠 prompt 约束模型「不要执行」。
type Gate struct {
	store    *approval.Store
	approver Approver
	timeout  time.Duration

	mu    sync.RWMutex
	inner map[string]tool.InvokableTool
}

// GateOption 是 Gate 的可选配置。
type GateOption func(*Gate)

// WithApprover 配置人工审核入口，开启「当场确认、当场执行」模式。
func WithApprover(a Approver) GateOption {
	return func(g *Gate) { g.approver = a }
}

// WithTimeout 限制单次工具执行的时长，0 或负数表示不限。
//
// 只圈住批准之后真正下发的那一段，等人点头的时间不算——审核多想两分钟不该
// 把命令的超时耗光。
func WithTimeout(d time.Duration) GateOption {
	return func(g *Gate) { g.timeout = d }
}

// NewGate 创建闸门。
func NewGate(store *approval.Store, opts ...GateOption) *Gate {
	g := &Gate{
		store: store,
		inner: make(map[string]tool.InvokableTool),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Wrap 把一个变更类工具包成「必须先过审」的门面。
//
// 返回的 *GatedTool 才是允许交给模型的东西；真实实现被 Gate 私下扣住。
func (g *Gate) Wrap(ctx context.Context, t tool.InvokableTool) (*GatedTool, error) {
	info, err := t.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取工具信息失败: %w", err)
	}
	if info.Name == "" {
		return nil, fmt.Errorf("工具名不能为空")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.inner[info.Name]; dup {
		return nil, fmt.Errorf("工具 %q 已被闸门包装过", info.Name)
	}
	g.inner[info.Name] = t

	// 复制一份 ToolInfo，在描述里说明这是要过人工审核的动作。
	gatedInfo := *info
	gatedInfo.Desc = info.Desc +
		"\n\n【变更操作】本工具的每次调用都必须先由人工确认才会真正执行。" +
		"你会拿到明确的执行结果或驳回理由，在拿到之前不要臆测发生了什么。" +
		"调用前请在回复里说明：你要执行什么、为什么、预期影响是什么。"

	return &GatedTool{gate: g, info: &gatedInfo}, nil
}

// Execute 批准并执行一条提案。
//
// 回放的是提案里存的原始参数，不做任何二次加工——审核人看到的和实际执行的
// 必须是同一个东西。
func (g *Gate) Execute(ctx context.Context, id, decider string) (*approval.Proposal, error) {
	p, err := g.store.Approve(id, decider)
	if err != nil {
		return nil, err
	}

	g.mu.RLock()
	inner, ok := g.inner[p.Tool]
	g.mu.RUnlock()
	if !ok {
		// 批准了却找不到实现，属于装配错误。标成失败，别把提案留在已批准状态。
		execErr := fmt.Errorf("工具 %q 未在闸门注册，无法执行", p.Tool)
		if _, mErr := g.store.MarkExecuted(id, "", execErr); mErr != nil {
			return nil, fmt.Errorf("%w（且状态回写失败: %v）", execErr, mErr)
		}
		return nil, execErr
	}

	slog.Info("执行已批准的操作", "proposal", p.ID, "tool", p.Tool, "decider", decider, "args", p.Args)
	result, runErr := g.run(ctx, inner, p.Args)
	done, err := g.store.MarkExecuted(id, result, runErr)
	if err != nil {
		return nil, err
	}
	slog.Info("操作执行完毕", "proposal", done.ID, "status", string(done.Status), "error", done.Error)
	// 给审核人一个回执：批完之后终端不能一直静着，尤其是失败和超时。
	if n, ok := g.approver.(Noticer); ok {
		n.Notice(done)
	}
	return done, nil
}

// run 执行真实工具，带超时。
//
// 超时到了就直接返回，不等那个 goroutine 收场：底层 HTTP 客户端不一定理会
// ctx，等下去就是整个终端跟着一起卡死。代价是它可能还在后台跑一会儿——所以
// 超时的说法是「不知道跑没跑成」，而不是「没跑」。
func (g *Gate) run(ctx context.Context, inner tool.InvokableTool, args string) (string, error) {
	if g.timeout <= 0 {
		return inner.InvokableRun(ctx, args)
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	type outcome struct {
		result string
		err    error
	}
	// 缓冲 1：超时之后没人再读这个 channel，写完就得让它走，不能挂住。
	done := make(chan outcome, 1)
	go func() {
		r, err := inner.InvokableRun(ctx, args)
		done <- outcome{r, err}
	}()

	select {
	case o := <-done:
		return o.result, DescribeRunError(o.err, g.timeout)
	case <-ctx.Done():
		return "", DescribeRunError(ctx.Err(), g.timeout)
	}
}

// DescribeRunError 把执行错误翻成人和模型都能直接读的一句话。
//
// 「context deadline exceeded」对着审核人弹出来等于没说。
func DescribeRunError(err error, timeout time.Duration) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w：等了 %s 仍未返回，命令可能还在节点上跑，请稍后自行核实结果", ErrToolTimeout, timeout)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("调用被取消（Ctrl-C 或会话结束）: %w", err)
	default:
		return UnwrapToolError(err)
	}
}

// UnwrapToolError 剥掉 eino 给工具错误套的外壳。
//
// 原文长这样：「[LocalFunc] failed to invoke tool, toolName=run_tunnel_cmd,
// err=下发到节点 SN001 失败: ...」。前半截对审核人没有任何信息量，工具名提案里
// 本来就写着——留下真正的原因就行。
func UnwrapToolError(err error) error {
	msg := err.Error()
	if !strings.HasPrefix(msg, "[LocalFunc]") {
		return err
	}
	if _, cause, ok := strings.Cut(msg, ", err="); ok && cause != "" {
		return errors.New(cause)
	}
	return err
}

// Reject 驳回一条提案。
func (g *Gate) Reject(id, decider, reason string) (*approval.Proposal, error) {
	p, err := g.store.Reject(id, decider, reason)
	if err != nil {
		return nil, err
	}
	slog.Info("操作被驳回", "proposal", p.ID, "tool", p.Tool, "decider", decider, "reason", reason)
	return p, nil
}

// Store 暴露底层存储，供上层查询待审列表。
func (g *Gate) Store() *approval.Store { return g.store }

// GatedTool 是变更类工具交给模型的门面：它只能发起审核，不能自己执行。
//
// 它刻意不持有真实工具的引用——真实实现只在 Gate 里。
type GatedTool struct {
	gate *Gate
	info *schema.ToolInfo
}

// 编译期确认 GatedTool 满足 eino 的工具接口。
var _ tool.InvokableTool = (*GatedTool)(nil)

// Info 返回附加了审核说明的工具信息。
func (t *GatedTool) Info(context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

// gateResponse 是模型拿到的返回结构。
//
// 三种可能：executed（人批了、真跑了）、rejected（人否了）、
// pending_approval（异步模式，还没人看）。模型必须按字面意思理解，
// 不能把「已提交」当成「已完成」。
type gateResponse struct {
	Status     string `json:"status"`
	ProposalID string `json:"proposal_id"`
	Tool       string `json:"tool"`
	Message    string `json:"message"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	Reason     string `json:"reject_reason,omitempty"`
}

func (r gateResponse) marshal() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("序列化闸门返回失败: %w", err)
	}
	return string(b), nil
}

// InvokableRun 登记提案 → 请人审核 → 批准了才执行。
//
// 这个方法是模型唯一能碰到的入口，它自己没有任何执行路径：真正的调用发生在
// Gate.Execute 里，而 Execute 只有在 Store.Approve 成功之后才会走到工具。
func (t *GatedTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	p := t.gate.store.Create(t.info.Name, argumentsInJSON)
	risk := AssessRisk(p.Tool, p.Args)
	slog.Info("模型发起变更操作，待人工确认",
		"proposal", p.ID, "tool", p.Tool, "risk", risk.Level.String(), "args", p.Args)

	// 没配审核入口 → 异步模式：留下提案，等外部处理。
	if t.gate.approver == nil {
		return gateResponse{
			Status:     "pending_approval",
			ProposalID: p.ID,
			Tool:       p.Tool,
			Message: fmt.Sprintf("操作提案 %s 已提交，等待人工审核。尚未执行任何操作，"+
				"你现在无法得知执行结果，请如实告知用户提案已提交并说明本次变更的理由和预期影响。", p.ID),
		}.marshal()
	}

	d, err := t.gate.approver.Review(ctx, p, risk)
	if err != nil {
		// 取不到人的决定就当作没批准。宁可这一轮失败，也不能默认放行。
		reason := fmt.Sprintf("未能取得人工确认: %v", err)
		if _, rErr := t.gate.Reject(p.ID, "system", reason); rErr != nil {
			slog.Error("驳回失败", "proposal", p.ID, "err", rErr)
		}
		return gateResponse{
			Status:     "rejected",
			ProposalID: p.ID,
			Tool:       p.Tool,
			Reason:     reason,
			Message:    "没能拿到人工确认，操作未执行。请告知用户审核环节出了问题。",
		}.marshal()
	}

	if !d.Approved {
		reason := d.Reason
		if reason == "" {
			reason = "（未填写理由）"
		}
		if _, rErr := t.gate.Reject(p.ID, deciderOr(d.Decider), reason); rErr != nil {
			return "", fmt.Errorf("记录驳回失败: %w", rErr)
		}
		return gateResponse{
			Status:     "rejected",
			ProposalID: p.ID,
			Tool:       p.Tool,
			Reason:     reason,
			Message: "人工审核未通过，操作没有执行。请根据驳回理由调整方案，" +
				"不要重复提交同样的操作，也不要假设它以任何形式生效了。",
		}.marshal()
	}

	done, err := t.gate.Execute(ctx, p.ID, deciderOr(d.Decider))
	if err != nil {
		return "", fmt.Errorf("执行提案 %s 失败: %w", p.ID, err)
	}

	resp := gateResponse{
		Status:     string(done.Status),
		ProposalID: done.ID,
		Tool:       done.Tool,
		Result:     done.Result,
		Error:      done.Error,
	}
	if done.Status == approval.StatusFailed {
		resp.Message = "人工已批准，但执行失败。请依据下面的 error 判断原因，不要臆造输出内容；" +
			"如果是调用超时，说明结果未知（命令可能已经生效），要如实告诉用户，不要直接重试同一条命令。"
	} else {
		resp.Message = "人工已批准并执行完毕，下面 result 是设备返回的真实输出。请据此继续分析。"
	}
	return resp.marshal()
}

func deciderOr(name string) string {
	if name == "" {
		return "unknown-operator"
	}
	return name
}
