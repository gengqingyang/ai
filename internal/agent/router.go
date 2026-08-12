package agent

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/intent"
)

const classifyNode = "classify"

// Flow 是诊断 Graph 对外稳定的分支标识。
type Flow string

const (
	FlowClarification Flow = "clarification"
	FlowInstallation  Flow = "installation"
	FlowTraffic       Flow = "traffic"
	FlowPlugin        Flow = "plugin"
	FlowKernel        Flow = "kernel"
	FlowNetwork       Flow = "network"
	FlowCode          Flow = "code"
	FlowOther         Flow = "other"
)

var allFlows = []Flow{
	FlowClarification,
	FlowInstallation,
	FlowTraffic,
	FlowPlugin,
	FlowKernel,
	FlowNetwork,
	FlowCode,
	FlowOther,
}

// IntentClassifier 是入口 Graph 依赖的最小分类接口。
type IntentClassifier interface {
	Classify(context.Context, []*schema.Message) (intent.Result, error)
}

// Route 是 Graph 的结构化输出，包含已校验的分类及最终分支。
type Route struct {
	Classification intent.Result
	Flow           Flow
}

type classifiedInput struct {
	classification intent.Result
}

// Router 使用 Eino compose.Graph 执行“分类 -> 故障分支路由”。各分支的
// ReAct 工具图由 Agent 运行层执行，Router 本身不执行任何工具。
type Router struct {
	runnable compose.Runnable[[]*schema.Message, Route]
}

// NewRouter 编译入口分类路由图。
func NewRouter(ctx context.Context, classifier IntentClassifier) (*Router, error) {
	if classifier == nil {
		return nil, fmt.Errorf("意图分类器不能为空")
	}

	graph := compose.NewGraph[[]*schema.Message, Route]()
	if err := graph.AddLambdaNode(classifyNode, compose.InvokableLambda(
		func(ctx context.Context, messages []*schema.Message) (classifiedInput, error) {
			classification, err := classifier.Classify(ctx, messages)
			if err != nil {
				return classifiedInput{}, fmt.Errorf("执行 Graph 意图分类节点失败: %w", err)
			}
			return classifiedInput{classification: classification}, nil
		},
	)); err != nil {
		return nil, fmt.Errorf("添加意图分类节点失败: %w", err)
	}

	endNodes := make(map[string]bool, len(allFlows))
	for _, flow := range allFlows {
		flow := flow	
		endNodes[string(flow)] = true
		if err := graph.AddLambdaNode(string(flow), compose.InvokableLambda(
			func(_ context.Context, input classifiedInput) (Route, error) {
				return Route{Classification: input.classification, Flow: flow}, nil
			},
		)); err != nil {
			return nil, fmt.Errorf("添加 %s 诊断分支失败: %w", flow, err)
		}
		if err := graph.AddEdge(string(flow), compose.END); err != nil {
			return nil, fmt.Errorf("连接 %s 诊断分支失败: %w", flow, err)
		}
	}

	branch := compose.NewGraphBranch(
		func(_ context.Context, input classifiedInput) (string, error) {
			return string(flowFor(input.classification)), nil
		},
		endNodes,
	)
	if err := graph.AddBranch(classifyNode, branch); err != nil {
		return nil, fmt.Errorf("添加故障类型路由失败: %w", err)
	}
	if err := graph.AddEdge(compose.START, classifyNode); err != nil {
		return nil, fmt.Errorf("连接意图分类节点失败: %w", err)
	}

	runnable, err := graph.Compile(ctx, compose.WithGraphName("diagnostic-flow-router"))
	if err != nil {
		return nil, fmt.Errorf("编译诊断路由 Graph 失败: %w", err)
	}
	return &Router{runnable: runnable}, nil
}

// Select 执行一次分类和分支选择。
func (r *Router) Select(ctx context.Context, messages []*schema.Message) (Route, error) {
	if r == nil || r.runnable == nil {
		return Route{}, fmt.Errorf("诊断路由 Graph 未初始化")
	}
	return r.runnable.Invoke(ctx, messages)
}

func flowFor(result intent.Result) Flow {
	if result.NeedsClarification || result.Intent == intent.Unknown {
		return FlowClarification
	}
	switch result.Intent {
	case intent.InstallationFailure:
		return FlowInstallation
	case intent.TrafficAnomaly:
		return FlowTraffic
	case intent.PluginFailure:
		return FlowPlugin
	case intent.KernelUpgradeFailure:
		return FlowKernel
	case intent.NetworkConfigurationFailure:
		return FlowNetwork
	case intent.CodeRepositoryQuestion:
		return FlowCode
	default:
		return FlowOther
	}
}
