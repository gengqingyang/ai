// Package agent 组装入口 Graph 和各故障类型的 ReAct 子流程。
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/config"
	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/skills"
	"diagnostic-system/internal/tools"
)

// Agent 包装 ADK ChatModelAgent，屏蔽掉 Eino 的构造和事件细节。
type Agent struct {
	flows      map[Flow]adk.Agent
	router     *Router
	evidence   *EvidencePipeline
	baseModel  model.ToolCallingChatModel
	skillNames []string
}

// New 构造分类路由 Graph 和带独立 prompt、工具白名单的故障子流程。
func New(ctx context.Context, cm model.ToolCallingChatModel, reg *tools.Registry, cfg *config.Config) (*Agent, error) {
	classifier, err := intent.New(cm)
	if err != nil {
		return nil, fmt.Errorf("创建意图分类器失败: %w", err)
	}
	router, err := NewRouter(ctx, classifier)
	if err != nil {
		return nil, err
	}
	evidence, err := NewEvidencePipeline(ctx, reg)
	if err != nil {
		return nil, err
	}

	skillHandler, skillMatters, err := skills.Load(ctx, cfg.SkillsDir)
	if err != nil {
		return nil, err
	}

	flows := make(map[Flow]adk.Agent, len(flowSpecs))
	for _, spec := range flowSpecs {
		flowTools, domainErr := toolsForFlow(reg, spec)
		if domainErr != nil {
			return nil, domainErr
		}
		handlers := []adk.ChatModelAgentMiddleware(nil)
		if spec.useSkills {
			handlers = []adk.ChatModelAgentMiddleware{skillHandler}
		}
		flowAgent, buildErr := newFlowAgent(ctx, cm, cfg.MaxStep, spec, flowTools, handlers)
		if buildErr != nil {
			return nil, buildErr
		}
		flows[spec.flow] = flowAgent
	}

	skillNames := make([]string, 0, len(skillMatters))
	for _, matter := range skillMatters {
		skillNames = append(skillNames, matter.Name)
	}
	return &Agent{
		flows:      flows,
		router:     router,
		evidence:   evidence,
		baseModel:  cm,
		skillNames: skillNames,
	}, nil
}

func toolsForFlow(reg *tools.Registry, spec flowSpec) ([]tool.BaseTool, error) {
	if len(spec.domains) == 0 {
		return nil, nil
	}
	flowTools, err := reg.RequireDomains(spec.domains...)
	if err != nil {
		return nil, fmt.Errorf("创建 %s 工具白名单失败: %w", spec.flow, err)
	}
	// 固定只读采证工具由前置 EvidencePipeline 执行。结论分支只保留代码工具
	// 和受审核的补充命令，避免模型重复采集同一份当前快照。
	filtered := make([]tool.BaseTool, 0, len(flowTools))
	for _, candidate := range flowTools {
		info, infoErr := candidate.Info(context.Background())
		if infoErr != nil {
			return nil, fmt.Errorf("读取 %s 分支工具信息失败: %w", spec.flow, infoErr)
		}
		if !isEvidenceTool(info.Name) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered, nil
}

type flowSpec struct {
	flow        Flow
	name        string
	description string
	prompt      string
	domains     []tools.Domain
	useSkills   bool
}

var flowSpecs = []flowSpec{
	{FlowInstallation, "installation-diagnostic", "装机异常专项诊断", InstallationPrompt,
		[]tools.Domain{tools.DomainInstallation, tools.DomainCode}, true},
	{FlowTraffic, "traffic-diagnostic", "业务流量异常诊断", TrafficPrompt,
		[]tools.Domain{tools.DomainTraffic}, true},
	{FlowPlugin, "plugin-diagnostic", "CDN 插件异常诊断", PluginPrompt,
		[]tools.Domain{tools.DomainPlugin}, true},
	{FlowKernel, "kernel-diagnostic", "内核升级失败诊断", KernelPrompt,
		[]tools.Domain{tools.DomainKernel}, true},
	{FlowNetwork, "network-diagnostic", "节点配网异常诊断", NetworkPrompt,
		[]tools.Domain{tools.DomainNetwork}, true},
	{FlowCode, "code-repository-assistant", "本地代码仓库只读问答", CodePrompt,
		[]tools.Domain{tools.DomainCode}, false},
	{FlowOther, "general-assistant", "非设备类问题处理", OtherPrompt, nil, false},
}

func newFlowAgent(
	ctx context.Context,
	cm model.ToolCallingChatModel,
	maxIterations int,
	spec flowSpec,
	flowTools []tool.BaseTool,
	handlers []adk.ChatModelAgentMiddleware,
) (adk.Agent, error) {
	inner, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        spec.name,
		Description: spec.description,
		Instruction: spec.prompt,
		Model:       cm,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: flowTools},
		},
		MaxIterations: maxIterations,
		Handlers:      handlers,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 %s 子流程失败: %w", spec.flow, err)
	}
	return inner, nil
}

// SkillNames 返回启动时已校验并挂载的 Skill 名称。
func (a *Agent) SkillNames() []string {
	return append([]string(nil), a.skillNames...)
}

// Generate 非流式地跑一轮，返回最终回复。
func (a *Agent) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	route, err := a.route(ctx, msgs)
	if err != nil {
		return nil, err
	}
	input := WithSystemPrompt(msgs, route.Classification)
	if route.Flow == FlowClarification {
		// 信息不足时绕开 ReAct，并在模型调用层禁用工具，硬性阻止节点命令。
		return a.baseModel.Generate(ctx, input, model.WithToolChoice(schema.ToolChoiceForbidden))
	}
	flowInput, err := a.withEvidence(ctx, msgs, route)
	if err != nil {
		return nil, err
	}
	return a.run(ctx, flowInput, route.Flow, false, nil, nil, nil)
}

// Stream 流式地跑一轮。ADK 会为每次模型输出产生事件；每收到一个文本片段就
// 回调 onChunk，但返回值只取最后一条不含 tool call 的 assistant 消息，供历史保存。
func (a *Agent) Stream(
	ctx context.Context,
	msgs []*schema.Message,
	onIntent func(intent.Result),
	onChunk func(string),
) (string, error) {
	return a.stream(ctx, msgs, onIntent, nil, nil, onChunk)
}

// StreamWithProgress 在普通回复外上报 Agent 阶段、工具轨迹和模型提供的思考摘要。
func (a *Agent) StreamWithProgress(
	ctx context.Context,
	msgs []*schema.Message,
	onIntent func(intent.Result),
	onProgress func(string),
	onReasoning func(string),
	onChunk func(string),
) (string, error) {
	return a.stream(ctx, msgs, onIntent, onProgress, onReasoning, onChunk)
}

func (a *Agent) stream(
	ctx context.Context,
	msgs []*schema.Message,
	onIntent func(intent.Result),
	onProgress func(string),
	onReasoning func(string),
	onChunk func(string),
) (string, error) {
	route, err := a.route(ctx, msgs)
	if err != nil {
		return "", err
	}
	if onIntent != nil {
		onIntent(route.Classification)
	}
	notifyProgress(onProgress, flowStartedText(route.Flow))

	if route.Flow == FlowClarification {
		return streamClarification(
			ctx, a.baseModel, WithSystemPrompt(msgs, route.Classification), onReasoning, onChunk,
		)
	}

	if _, ok := evidenceSpecs[route.Flow]; ok {
		notifyProgress(onProgress, "正在并行采集并校验只读设备证据。")
	}
	flowInput, err := a.withEvidence(ctx, msgs, route)
	if err != nil {
		return "", err
	}
	if _, ok := evidenceSpecs[route.Flow]; ok {
		notifyProgress(onProgress, "只读设备证据已完成，正在交给模型分析。")
	}
	reply, err := a.run(ctx, flowInput, route.Flow, true, onProgress, onReasoning, onChunk)
	if err != nil {
		return "", err
	}
	if reply == nil {
		return "", nil
	}
	return reply.Content, nil
}

func (a *Agent) withEvidence(ctx context.Context, msgs []*schema.Message, route Route) ([]*schema.Message, error) {
	input := withRoutingContext(msgs, route.Classification)
	if _, ok := evidenceSpecs[route.Flow]; !ok {
		return input, nil
	}
	bundle, err := a.evidence.Run(ctx, EvidenceRequest{
		Flow: route.Flow, Classification: route.Classification,
	})
	if err != nil {
		return nil, fmt.Errorf("执行诊断采证 Graph 失败: %w", err)
	}
	out := make([]*schema.Message, 0, len(input)+1)
	out = append(out, input[0], schema.SystemMessage(bundle.routingContext()))
	return append(out, input[1:]...), nil
}

// run consumes ADK events and returns the last assistant message without tool
// calls. ADK emits every model turn, so intermediate explanations can be
// streamed before an approval prompt while only the final answer enters history.
func (a *Agent) run(
	ctx context.Context,
	msgs []*schema.Message,
	flow Flow,
	stream bool,
	onProgress func(string),
	onReasoning func(string),
	onChunk func(string),
) (*schema.Message, error) {
	selected, ok := a.flows[flow]
	if !ok || selected == nil {
		return nil, fmt.Errorf("诊断分支 %q 未初始化", flow)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           selected,
		EnableStreaming: stream,
	})
	iter := runner.Run(ctx, msgs)

	var final *schema.Message
	answerSegments := 0
	reasoningSegments := 0
	for {
		event, ok := iter.Next()
		if !ok {
			return final, nil
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			return final, event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		output := event.Output.MessageOutput
		if output.Role == schema.Tool {
			name := output.ToolName
			if name == "" {
				name = "工具"
			}
			notifyProgress(onProgress, fmt.Sprintf("%s 已完成，正在分析返回结果。", name))
			continue
		}
		if output.Role != schema.Assistant {
			continue
		}

		msg, err := consumeMessageOutput(
			output, onReasoning, onChunk, &reasoningSegments, &answerSegments,
		)
		if err != nil {
			return final, err
		}
		if msg != nil && len(msg.ToolCalls) > 0 {
			notifyProgress(onProgress, "调用工具："+toolCallNames(msg.ToolCalls))
		}
		if msg != nil && len(msg.ToolCalls) == 0 {
			final = msg
		}
	}
}

func consumeMessageOutput(
	output *adk.MessageVariant,
	onReasoning func(string),
	onChunk func(string),
	reasoningSegments *int,
	answerSegments *int,
) (*schema.Message, error) {
	if !output.IsStreaming {
		msg := output.Message
		if msg != nil && msg.ReasoningContent != "" && onReasoning != nil {
			startSegment(onReasoning, reasoningSegments)
			onReasoning(msg.ReasoningContent)
		}
		if msg != nil && msg.Content != "" && onChunk != nil {
			startSegment(onChunk, answerSegments)
			onChunk(msg.Content)
		}
		return msg, nil
	}
	if output.MessageStream == nil {
		return nil, errors.New("ADK 返回了空的消息流")
	}
	defer output.MessageStream.Close()

	chunks := make([]*schema.Message, 0, 8)
	answerStarted := false
	reasoningStarted := false
	for {
		chunk, err := output.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
		if chunk != nil && chunk.ReasoningContent != "" && onReasoning != nil {
			if !reasoningStarted {
				startSegment(onReasoning, reasoningSegments)
				reasoningStarted = true
			}
			onReasoning(chunk.ReasoningContent)
		}
		if chunk != nil && chunk.Content != "" && onChunk != nil {
			if !answerStarted {
				startSegment(onChunk, answerSegments)
				answerStarted = true
			}
			onChunk(chunk.Content)
		}
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	msg, err := schema.ConcatMessages(chunks)
	if err != nil {
		return nil, fmt.Errorf("合并 ADK 回复失败: %w", err)
	}
	return msg, nil
}

func startSegment(onChunk func(string), segments *int) {
	if *segments > 0 {
		onChunk("\n")
	}
	(*segments)++
}

func notifyProgress(callback func(string), text string) {
	if callback != nil && text != "" {
		callback(text)
	}
}

func toolCallNames(calls []schema.ToolCall) string {
	names := make([]string, 0, len(calls))
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		name := call.Function.Name
		if name == "" {
			name = "未知工具"
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return strings.Join(names, "、")
}

func flowStartedText(flow Flow) string {
	labels := map[Flow]string{
		FlowClarification: "信息澄清",
		FlowInstallation:  "装机异常诊断",
		FlowTraffic:       "业务流量诊断",
		FlowPlugin:        "插件异常诊断",
		FlowKernel:        "内核升级诊断",
		FlowNetwork:       "网络配置诊断",
		FlowCode:          "代码仓库问答",
		FlowOther:         "通用问答",
	}
	return fmt.Sprintf("已进入%s流程，正在规划下一步。", labels[flow])
}

func (a *Agent) route(ctx context.Context, msgs []*schema.Message) (Route, error) {
	route, err := a.router.Select(ctx, msgs)
	if err != nil {
		return Route{}, fmt.Errorf("意图识别失败: %w", err)
	}
	result := route.Classification
	slog.Info("意图识别",
		"intent", result.Intent,
		"flow", route.Flow,
		"confidence", result.Confidence,
		"summary", result.Summary,
		"evidence", result.Evidence,
		"needs_clarification", result.NeedsClarification,
		"device_ids", result.DeviceIDs,
		"missing_information", result.MissingInformation,
	)
	return route, nil
}

// StreamClarification streams a reply while forbidding all tool calls.
func StreamClarification(
	ctx context.Context,
	baseModel model.ToolCallingChatModel,
	msgs []*schema.Message,
	onChunk func(string),
) (string, error) {
	return streamClarification(ctx, baseModel, msgs, nil, onChunk)
}

func streamClarification(
	ctx context.Context,
	baseModel model.ToolCallingChatModel,
	msgs []*schema.Message,
	onReasoning func(string),
	onChunk func(string),
) (string, error) {
	sr, err := baseModel.Stream(
		ctx,
		msgs,
		model.WithToolChoice(schema.ToolChoiceForbidden),
	)
	if err != nil {
		return "", err
	}
	defer sr.Close()

	chunks := make([]*schema.Message, 0, 8)
	reasoningStarted := false
	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			if len(chunks) == 0 {
				return "", nil
			}
			merged, mergeErr := schema.ConcatMessages(chunks)
			if mergeErr != nil {
				return "", fmt.Errorf("合并澄清回复失败: %w", mergeErr)
			}
			return merged.Content, nil
		}
		if err != nil {
			return "", err
		}
		chunks = append(chunks, chunk)
		if onReasoning != nil && chunk != nil && chunk.ReasoningContent != "" {
			if !reasoningStarted {
				reasoningStarted = true
			}
			onReasoning(chunk.ReasoningContent)
		}
		if onChunk != nil && chunk != nil && chunk.Content != "" {
			onChunk(chunk.Content)
		}
	}
}

// WithSystemPrompt 在消息列表前插入全局约束和本轮意图路由元数据。
//
// 调用方（session）只维护 user/assistant 历史，system prompt 由这里统一注入，
// 保证它永远在最前面、且不会被历史裁剪掉。
func WithSystemPrompt(msgs []*schema.Message, classification intent.Result) []*schema.Message {
	out := make([]*schema.Message, 0, len(msgs)+2)
	if len(msgs) > 0 && msgs[0].Role == schema.System {
		out = append(out, msgs[0])
		msgs = msgs[1:]
	} else {
		out = append(out, schema.SystemMessage(SystemPrompt))
	}
	out = append(out, schema.SystemMessage(classification.RoutingContext()))
	return append(out, msgs...)
}

func withRoutingContext(msgs []*schema.Message, classification intent.Result) []*schema.Message {
	out := make([]*schema.Message, 0, len(msgs)+1)
	out = append(out, schema.SystemMessage(classification.RoutingContext()))
	return append(out, msgs...)
}
