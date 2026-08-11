package tools

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"wx-gitlab.xunlei.cn/iaas_biz/scheduler_common/protos/tunnel/command"
	"wx-gitlab.xunlei.cn/iaas_biz/scheduler_common/tunnel/cluster"
)

// defaultTunnelAPI 是 tunnel 同步下发接口地址，可用 TUNNEL_API 环境变量覆盖。
const defaultTunnelAPI = "https://tw15a0050.onething.net:17813/p/10.128.4.223/5588/v1.0/cmd/sync"

// tunnelToolName 是工具名。风险评估要按工具名解析参数，所以抽成常量。
const tunnelToolName = "run_tunnel_cmd"

var tunnelSecretPattern = regexp.MustCompile(`(?i)(access_token|auth_token|api_key|token)=([^&\s"']+)`)

type tunnel struct {
	operator string
	initOnce sync.Once
	client   *cluster.SyncClient
}

// TunnelResult 是命令执行结果。
//
// Error 用 string 而不是 error：error 接口 JSON 序列化后是 {}，工具结果要回给
// 模型看，必须是可读文本。
type TunnelResult struct {
	Sn     string                    `json:"sn"`
	Error  string                    `json:"error,omitempty"`
	Result *command.CommandResultMsg `json:"result,omitempty"` // TunnelAgent 执行命令的输出信息
}

// Tunnel 是全局单例。operator 会随命令上报，用于对端审计，
// 启动时应当用 SetOperator 设成真实操作人。
var Tunnel = tunnel{
	operator: "eino",
}

// SetOperator 设置操作人标识。
func (x *tunnel) SetOperator(operator string) *tunnel {
	if operator != "" {
		x.operator = operator
	}
	return x
}

// RunCmd 通过 Tunnel 通道在指定节点上执行 shell 命令。
// 办公网和统计中心机器都能使用。
//
// 这是真实的远程执行，不是 mock。任意命令必须经过人工审核；唯一例外是
// evidence.go 内部不向包外暴露的固定只读模板。
func (x *tunnel) RunCmd(ctx context.Context, sn, cmd string) *TunnelResult {
	x.initOnce.Do(func() {
		api := os.Getenv("TUNNEL_API")
		if api == "" {
			api = defaultTunnelAPI
		}
		x.client = cluster.NewSyncClient().WithAPI(api)
	})

	result, err := x.client.Send(ctx, &cluster.SyncRequest{
		Sn:       sn,
		Cmd:      cmd,
		Operator: x.operator,
	})

	out := &TunnelResult{Sn: sn, Result: result}
	if err != nil {
		out.Error = tunnelSecretPattern.ReplaceAllString(err.Error(), "$1=[REDACTED]")
	}
	return out
}

// tunnelInput 是模型要填的参数。
//
// 字段必须导出：未导出字段既不会出现在生成的 JSON Schema 里、也无法被
// encoding/json 反序列化，模型根本传不进值，最终会执行一条空命令。
type tunnelInput struct {
	Sn  string `json:"sn" jsonschema:"required" jsonschema_description:"节点 SN"`
	Cmd string `json:"cmd" jsonschema:"required" jsonschema_description:"要在节点上执行的 shell 命令"`
	// Purpose 是给审核人看的一句话说明。要求模型自己写，是因为只有它知道
	// 这条命令是为什么发的——`ip -4 addr show` 到底是在查业务网卡还是在排
	// 路由，命令本身看不出来。注意这是模型的自述，没有任何校验，展示时必须
	// 和命令原文并排放，不能替代命令原文。
	Purpose string `json:"purpose" jsonschema:"required" jsonschema_description:"一句话说明这条命令要做什么（不超过 20 字），例如「查看节点系统时间」「重启异常的 nginx 服务」。会展示给审核人，写清楚点"`
}

// NewTunnelTool 构造节点命令执行工具。
//
// 这是变更类工具：注册时必须先经 Gate.Wrap 包装，Registry 会强制这一点。
// 直接把它交给 agent 意味着模型可以在客户设备上任意执行 shell。
func NewTunnelTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		tunnelToolName,
		"在指定节点上执行 shell 命令。用于诊断结论确定后的处置动作，或获取只读工具覆盖不到的现场信息。",
		func(ctx context.Context, in tunnelInput) (TunnelResult, error) {
			if in.Sn == "" {
				return TunnelResult{}, fmt.Errorf("sn 不能为空")
			}
			if in.Cmd == "" {
				return TunnelResult{}, fmt.Errorf("cmd 不能为空")
			}
			// Purpose 只用于审核展示，不参与执行。
			res := Tunnel.RunCmd(ctx, in.Sn, in.Cmd)
			if res.Error != "" {
				// 「没送到节点」和「命令跑了但结果不好」是两回事。前者要当成工具
				// 调用失败抛出去，上层才能把超时、节点离线这类原因原样提示出来，
				// 而不是塞进一个看着挺正常的返回值里让模型自己悟。
				return *res, fmt.Errorf("下发到节点 %s 失败: %s", in.Sn, res.Error)
			}
			return *res, nil
		},
	)
}
