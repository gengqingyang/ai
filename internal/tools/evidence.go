package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

const (
	ToolInstallationEvidence = "get_installation_evidence"
	ToolTrafficEvidence      = "get_traffic_evidence"
	ToolPluginEvidence       = "get_plugin_evidence"
	ToolKernelEvidence       = "get_kernel_evidence"
	ToolNetworkEvidence      = "get_network_evidence"
)

// ReadOnlyCommandOutput 是受限节点命令执行器返回的最小结果。
type ReadOnlyCommandOutput struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}

// ReadOnlyCommandRunner 只供固定模板的采证工具使用，不接受模型生成的命令。
type ReadOnlyCommandRunner interface {
	RunReadOnly(ctx context.Context, node, command string) (ReadOnlyCommandOutput, error)
}

type tunnelReadOnlyRunner struct {
	timeout time.Duration
}

// NewTunnelEvidenceTools 创建使用 Tunnel 通道的五类固定模板采证工具。
// 真实 runner 不向包外暴露，避免调用方拿它执行任意命令。
func NewTunnelEvidenceTools(timeout time.Duration) ([]tool.BaseTool, error) {
	return NewEvidenceTools(&tunnelReadOnlyRunner{timeout: timeout})
}

func (r *tunnelReadOnlyRunner) RunReadOnly(
	ctx context.Context,
	node string,
	command string,
) (ReadOnlyCommandOutput, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	result := Tunnel.RunCmd(ctx, node, command)
	if result.Error != "" {
		return ReadOnlyCommandOutput{}, errors.New(result.Error)
	}
	if result.Result == nil {
		return ReadOnlyCommandOutput{}, errors.New("Tunnel 未返回命令结果")
	}
	output := ReadOnlyCommandOutput{
		ExitCode: result.Result.GetCode(),
		Stdout:   result.Result.GetStdout(),
		Stderr:   result.Result.GetStderr(),
	}
	if output.ExitCode != 0 {
		return output, fmt.Errorf("节点只读采证命令退出码 %d: %s",
			output.ExitCode, truncateError(output.Stderr, 512))
	}
	return output, nil
}

type nodeEvidenceInput struct {
	Node string `json:"node" jsonschema:"required" jsonschema_description:"节点 SN、设备 ID 或节点 ID"`
}

type trafficEvidenceInput struct {
	Node          string `json:"node" jsonschema:"required" jsonschema_description:"节点 SN、设备 ID 或节点 ID"`
	SampleSeconds int    `json:"sample_seconds,omitempty" jsonschema_description:"网卡计数器采样秒数，范围 1 到 5，默认 1"`
}

// EvidenceItem 是一项已经结构化的节点证据。
type EvidenceItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Value  string `json:"value"`
	Unit   string `json:"unit,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// EvidenceReport 是五类只读采证工具共享的输出契约。
type EvidenceReport struct {
	Domain      string         `json:"domain"`
	Node        string         `json:"node"`
	DataSource  string         `json:"data_source"`
	CollectedAt string         `json:"collected_at,omitempty"`
	Status      string         `json:"status"`
	Summary     string         `json:"summary"`
	Items       []EvidenceItem `json:"items"`
	Limitations []string       `json:"limitations,omitempty"`
}

type evidenceParser func(string, *EvidenceReport) error

// NewEvidenceTools 创建五个固定模板、结构化输出的节点只读采证工具。
func NewEvidenceTools(runner ReadOnlyCommandRunner) ([]tool.BaseTool, error) {
	if runner == nil {
		return nil, errors.New("只读采证命令执行器不能为空")
	}

	installation, err := utils.InferTool(
		ToolInstallationEvidence,
		"查询节点装机就绪度、系统状态、资源水位和安装相关进程；只执行固定只读命令。",
		func(ctx context.Context, in nodeEvidenceInput) (EvidenceReport, error) {
			return collectEvidence(ctx, runner, in.Node, "installation",
				"tunnel:systemd/procfs", installationEvidenceCommand, parseInstallationEvidence,
				[]string{"当前快照不能替代装机平台的历史阶段记录。"})
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造装机采证工具失败: %w", err)
	}

	traffic, err := utils.InferTool(
		ToolTrafficEvidence,
		"采样节点默认网卡的收发速率、包速率、错误和丢包增量；不代表 CDN 业务请求量。",
		func(ctx context.Context, in trafficEvidenceInput) (EvidenceReport, error) {
			seconds := in.SampleSeconds
			if seconds == 0 {
				seconds = 1
			}
			if seconds < 1 || seconds > 5 {
				return EvidenceReport{}, errors.New("sample_seconds 必须在 1 到 5 之间")
			}
			command := strings.ReplaceAll(trafficEvidenceCommand, "{{SAMPLE_SECONDS}}", strconv.Itoa(seconds))
			return collectEvidence(ctx, runner, in.Node, "traffic",
				"tunnel:sysfs", command, parseTrafficEvidence,
				[]string{"仅反映默认网卡流量，不等同于 CDN 业务请求量或调度流量。"})
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造流量采证工具失败: %w", err)
	}

	plugins, err := utils.InferTool(
		ToolPluginEvidence,
		"查询节点核心插件服务的加载、运行、退出状态和可执行文件位置；只执行固定只读命令。",
		func(ctx context.Context, in nodeEvidenceInput) (EvidenceReport, error) {
			return collectEvidence(ctx, runner, in.Node, "plugin",
				"tunnel:systemd/procfs", pluginEvidenceCommand, parsePluginEvidence, nil)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造插件采证工具失败: %w", err)
	}

	kernel, err := utils.InferTool(
		ToolKernelEvidence,
		"查询节点当前、默认启动和已安装内核，判断升级是否已安装并切换；只执行固定只读命令。",
		func(ctx context.Context, in nodeEvidenceInput) (EvidenceReport, error) {
			return collectEvidence(ctx, runner, in.Node, "kernel",
				"tunnel:uname/rpm/grubby", kernelEvidenceCommand, parseKernelEvidence, nil)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造内核采证工具失败: %w", err)
	}

	network, err := utils.InferTool(
		ToolNetworkEvidence,
		"查询节点接口、地址、默认路由、DNS 和网卡错误计数；只执行固定只读命令。",
		func(ctx context.Context, in nodeEvidenceInput) (EvidenceReport, error) {
			return collectEvidence(ctx, runner, in.Node, "network",
				"tunnel:iproute/procfs", networkEvidenceCommand, parseNetworkEvidence,
				[]string{"错误和丢包为开机后的累计计数，需要结合流量采样判断是否仍在增长。"})
		},
	)
	if err != nil {
		return nil, fmt.Errorf("构造网络采证工具失败: %w", err)
	}

	return []tool.BaseTool{installation, traffic, plugins, kernel, network}, nil
}

func collectEvidence(
	ctx context.Context,
	runner ReadOnlyCommandRunner,
	node string,
	domain string,
	dataSource string,
	command string,
	parser evidenceParser,
	limitations []string,
) (EvidenceReport, error) {
	node = strings.TrimSpace(node)
	if err := validateNodeID(node); err != nil {
		return EvidenceReport{}, err
	}
	output, err := runner.RunReadOnly(ctx, node, command)
	if err != nil {
		return EvidenceReport{}, fmt.Errorf("采集节点 %s 的 %s 证据失败: %w", node, domain, err)
	}
	report := EvidenceReport{
		Domain:      domain,
		Node:        node,
		DataSource:  dataSource,
		Items:       make([]EvidenceItem, 0, 12),
		Limitations: append([]string(nil), limitations...),
	}
	if err := parser(output.Stdout, &report); err != nil {
		return EvidenceReport{}, fmt.Errorf("解析节点 %s 的 %s 证据失败: %w", node, domain, err)
	}
	finalizeEvidence(&report)
	return report, nil
}

func validateNodeID(node string) error {
	if node == "" {
		return errors.New("node 不能为空")
	}
	if len(node) > 128 {
		return errors.New("node 长度不能超过 128")
	}
	for _, r := range node {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return fmt.Errorf("node 包含非法字符 %q", r)
	}
	return nil
}

func truncateError(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
