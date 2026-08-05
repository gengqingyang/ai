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
	"sync"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/config"
	"diagnostic-system/internal/tools"
)

// Agent 包装 react.Agent，屏蔽掉 eino 的构造细节。
type Agent struct {
	inner *react.Agent
}

// New 构造 ReAct agent。
//
// 挂载注册表里的全部工具。变更类工具在注册时已被 tools.Gate 包成「只生成
// 提案」的门面（Registry 强制这一点），所以模型看得见、也能提议变更动作，
// 但它手上那个工具没有执行能力——执行只发生在人工批准之后。
func New(ctx context.Context, cm model.ToolCallingChatModel, reg *tools.Registry, cfg *config.Config) (*Agent, error) {
	inner, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: cm,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: reg.All(),
		},
		MaxStep: cfg.MaxStep,
		// 部分兼容端点会先吐文本、后吐 tool call，eino 的默认检查器只看
		// 第一个 chunk，会把「有工具调用」误判成「没有」。这里读完整段流。
		StreamToolCallChecker: streamToolCallChecker,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 react agent 失败: %w", err)
	}
	return &Agent{inner: inner}, nil
}

// Generate 非流式地跑一轮，返回最终回复。
func (a *Agent) Generate(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	return a.inner.Generate(ctx, withSystemPrompt(msgs))
}

// Stream 流式地跑一轮。每收到一个文本片段就回调 onChunk，返回最终回复文本。
//
// 文字是从 ChatModel 的组件回调里抓的，不是从返回的那条流里抓的：ReAct 一轮
// 里模型会说好几次话（「我先看看节点时间」→ 调工具 → 「时间正常」），但只有
// 最后一次会流到调用方手上，中间那几次全被 agent 内部消费掉了。只读返回流的
// 话，屏幕上就是敲完问题一片空白，直到审核框突然弹出来。接到组件回调上，每
// 一段都能边生成边打。
//
// 返回值仍取自最终输出流——那是这一轮真正的回复，写进历史的必须是它。
func (a *Agent) Stream(ctx context.Context, msgs []*schema.Message, onChunk func(string)) (string, error) {
	var opts []einoagent.AgentOption
	if onChunk != nil {
		opts = append(opts, einoagent.WithComposeOptions(
			compose.WithCallbacks(newStreamPrinter(onChunk)),
		))
	}

	sr, err := a.inner.Stream(ctx, withSystemPrompt(msgs), opts...)
	if err != nil {
		return "", err
	}
	defer sr.Close()

	// 这里只负责把最终回复拼出来，不再打印——正文已经由上面的回调实时打过了，
	// 再打一遍就是重影。
	var full []byte
	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return string(full), nil
		}
		if err != nil {
			return string(full), err
		}
		full = append(full, chunk.Content...)
	}
}

// streamPrinter 把模型每一段输出实时喂给 onChunk。
type streamPrinter struct {
	onChunk func(string)

	mu       sync.Mutex
	segments int // 已经打印过正文的模型输出段数
}

func newStreamPrinter(onChunk func(string)) callbacks.Handler {
	p := &streamPrinter{onChunk: onChunk}
	return callbacks.NewHandlerBuilder().
		OnEndWithStreamOutputFn(p.onModelOutput).
		Build()
}

// onModelOutput 同步读完模型这一段输出，边读边打。
//
// 同步而不是丢个 goroutine 去读：读完才返回，后面的审核框、执行回执就一定排在
// 这段文字之后打印，不会插进句子中间。拿到的流是 eino 给回调单独复制的一份，
// 用链表缓冲，读它既抢不走下游的数据、也卡不住下游；chunk 由模型组件在另一
// 个 goroutine 里产出，所以在这里读到底也不会自锁。
//
// eino 要求回调必须关掉这份副本，否则漏 goroutine。
func (p *streamPrinter) onModelOutput(ctx context.Context, info *callbacks.RunInfo,
	out *schema.StreamReader[callbacks.CallbackOutput]) context.Context {

	if info == nil || info.Component != components.ComponentOfChatModel {
		out.Close()
		return ctx
	}
	defer out.Close()

	p.mu.Lock()
	defer p.mu.Unlock()

	first := true
	for {
		chunk, err := out.Recv()
		if err != nil {
			// io.EOF 是正常收尾；其它错误主流程那条流也会报，这里是旁路，
			// 安静退出就行，别把半截错误打到用户脸上。
			return ctx
		}
		o := model.ConvCallbackOutput(chunk)
		if o == nil || o.Message == nil || o.Message.Content == "" {
			continue // tool call 的分片没有正文
		}
		if first {
			first = false
			p.segments++
			if p.segments > 1 {
				// 上一段之后夹着审核框、执行回执，空一行再接着说。
				p.onChunk("\n")
			}
		}
		p.onChunk(o.Message.Content)
	}
}

// withSystemPrompt 在消息列表前插入 system 消息。
//
// 调用方（session）只维护 user/assistant 历史，system prompt 由这里统一注入，
// 保证它永远在最前面、且不会被历史裁剪掉。
func withSystemPrompt(msgs []*schema.Message) []*schema.Message {
	if len(msgs) > 0 && msgs[0].Role == schema.System {
		return msgs
	}
	out := make([]*schema.Message, 0, len(msgs)+1)
	out = append(out, schema.SystemMessage(SystemPrompt))
	return append(out, msgs...)
}

// streamToolCallChecker 一直读到出现 tool call 或流结束为止。
//
// eino 要求这个函数在返回前关掉 modelOutput 流。
func streamToolCallChecker(_ context.Context, modelOutput *schema.StreamReader[*schema.Message]) (bool, error) {
	defer modelOutput.Close()
	for {
		msg, err := modelOutput.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if len(msg.ToolCalls) > 0 {
			return true, nil
		}
	}
}
