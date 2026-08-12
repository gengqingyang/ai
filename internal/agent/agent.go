// Package agent 组装 ReAct agent。
//
// 当前只有一个通用 agent。后续按故障类型拆诊断子图时，这里会变成
// 「入口分类 → 各故障类型子图」的编排层，届时改用 compose.Graph，
// 但对外暴露的 Agent 接口可以保持不变。
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/config"
	"diagnostic-system/internal/intent"
	"diagnostic-system/internal/skills"
	"diagnostic-system/internal/tools"
)

// Agent 包装 ADK ChatModelAgent，屏蔽掉 Eino 的构造和事件细节。
type Agent struct {
	inner      *adk.ChatModelAgent
	codeInner  *adk.ChatModelAgent
	baseModel  model.ToolCallingChatModel
	classifier *intent.Classifier
	skillNames []string
}

// New 构造 ReAct agent。
//
// 挂载注册表里的全部工具。变更类工具在注册时已被 tools.Gate 包成「只生成
// 提案」的门面（Registry 强制这一点），所以模型看得见、也能提议变更动作，
// 但它手上那个工具没有执行能力——执行只发生在人工批准之后。
func New(ctx context.Context, cm model.ToolCallingChatModel, reg *tools.Registry, cfg *config.Config) (*Agent, error) {
	classifier, err := intent.New(cm)
	if err != nil {
		return nil, fmt.Errorf("创建意图分类器失败: %w", err)
	}

	skillHandler, skillMatters, err := skills.Load(ctx, cfg.SkillsDir)
	if err != nil {
		return nil, err
	}

	inner, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "diagnostic-assistant",
		Description: "CDN 业务智能诊断助手",
		Instruction: SystemPrompt,
		Model:       cm,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: reg.All()},
		},
		MaxIterations: cfg.MaxStep,
		Handlers:      []adk.ChatModelAgentMiddleware{skillHandler},
	})
	if err != nil {
		return nil, fmt.Errorf("创建 ADK ChatModelAgent 失败: %w", err)
	}
	codeTools, err := reg.Named(tools.CodeToolNames()...)
	if err != nil {
		return nil, fmt.Errorf("创建代码工具白名单失败: %w", err)
	}
	codeInner, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "code-repository-assistant",
		Description: "本地代码仓库只读问答助手",
		Instruction: CodePrompt,
		Model:       cm,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: codeTools},
		},
		MaxIterations: cfg.MaxStep,
	})
	if err != nil {
		return nil, fmt.Errorf("创建代码仓库 Agent 失败: %w", err)
	}

	skillNames := make([]string, 0, len(skillMatters))
	for _, matter := range skillMatters {
		skillNames = append(skillNames, matter.Name)
	}
	return &Agent{
		inner:      inner,
		codeInner:  codeInner,
		baseModel:  cm,
		classifier: classifier,
		skillNames: skillNames,
	}, nil
}

// SkillNames 返回启动时已校验并挂载的 Skill 名称。
func (a *Agent) SkillNames() []string {
	return append([]string(nil), a.skillNames...)
}

// Generate 非流式地跑一轮，返回最终回复。
func (a *Agent) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	classification, err := a.classify(ctx, msgs)
	if err != nil {
		return nil, err
	}
	input := WithSystemPrompt(msgs, classification)
	if classification.NeedsClarification {
		// 信息不足时绕开 ReAct，并在模型调用层禁用工具，硬性阻止节点命令。
		return a.baseModel.Generate(ctx, input, model.WithToolChoice(schema.ToolChoiceForbidden))
	}
	return a.run(ctx, withRoutingContext(msgs, classification), classification, false, nil)
}

// Stream 流式地跑一轮。ADK 会为每次模型输出产生事件；每收到一个文本片段就
// 回调 onChunk，但返回值只取最后一条不含 tool call 的 assistant 消息，供历史保存。
func (a *Agent) Stream(
	ctx context.Context,
	msgs []*schema.Message,
	onIntent func(intent.Result),
	onChunk func(string),
) (string, error) {
	classification, err := a.classify(ctx, msgs)
	if err != nil {
		return "", err
	}
	if onIntent != nil {
		onIntent(classification)
	}

	if classification.NeedsClarification {
		return StreamClarification(ctx, a.baseModel, WithSystemPrompt(msgs, classification), onChunk)
	}

	reply, err := a.run(ctx, withRoutingContext(msgs, classification), classification, true, onChunk)
	if err != nil {
		return "", err
	}
	if reply == nil {
		return "", nil
	}
	return reply.Content, nil
}

// run consumes ADK events and returns the last assistant message without tool
// calls. ADK emits every model turn, so intermediate explanations can be
// streamed before an approval prompt while only the final answer enters history.
func (a *Agent) run(
	ctx context.Context,
	msgs []*schema.Message,
	classification intent.Result,
	stream bool,
	onChunk func(string),
) (*schema.Message, error) {
	selected := a.inner
	if classification.Intent == intent.CodeRepositoryQuestion {
		selected = a.codeInner
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           selected,
		EnableStreaming: stream,
	})
	iter := runner.Run(ctx, msgs)

	var final *schema.Message
	segments := 0
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
		if output.Role != schema.Assistant {
			continue
		}

		msg, err := consumeMessageOutput(output, onChunk, &segments)
		if err != nil {
			return final, err
		}
		if msg != nil && len(msg.ToolCalls) == 0 {
			final = msg
		}
	}
}

func consumeMessageOutput(output *adk.MessageVariant, onChunk func(string), segments *int) (*schema.Message, error) {
	if !output.IsStreaming {
		msg := output.Message
		if msg != nil && msg.Content != "" && onChunk != nil {
			startSegment(onChunk, segments)
			onChunk(msg.Content)
		}
		return msg, nil
	}
	if output.MessageStream == nil {
		return nil, errors.New("ADK 返回了空的消息流")
	}
	defer output.MessageStream.Close()

	chunks := make([]*schema.Message, 0, 8)
	started := false
	for {
		chunk, err := output.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
		if chunk != nil && chunk.Content != "" && onChunk != nil {
			if !started {
				startSegment(onChunk, segments)
				started = true
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

func (a *Agent) classify(ctx context.Context, msgs []*schema.Message) (intent.Result, error) {
	result, err := a.classifier.Classify(ctx, msgs)
	if err != nil {
		return intent.Result{}, fmt.Errorf("意图识别失败: %w", err)
	}
	slog.Info("意图识别",
		"intent", result.Intent,
		"confidence", result.Confidence,
		"summary", result.Summary,
		"evidence", result.Evidence,
		"needs_clarification", result.NeedsClarification,
		"device_ids", result.DeviceIDs,
		"missing_information", result.MissingInformation,
	)
	return result, nil
}

// StreamClarification streams a reply while forbidding all tool calls.
func StreamClarification(
	ctx context.Context,
	baseModel model.ToolCallingChatModel,
	msgs []*schema.Message,
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
