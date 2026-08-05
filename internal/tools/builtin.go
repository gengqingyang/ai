package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// NewNowTool 返回一个查当前时间的只读工具。
//
// 这是最小的可用样例：模型不知道「现在」是什么时候，问到相对时间就得调它。
func NewNowTool() (tool.InvokableTool, error) {
	type input struct {
		Timezone string `json:"timezone" jsonschema_description:"IANA 时区名，例如 Asia/Shanghai。留空则用服务器本地时区"`
	}
	type output struct {
		Time     string `json:"time"`
		Weekday  string `json:"weekday"`
		Timezone string `json:"timezone"`
	}

	return utils.InferTool(
		"now",
		"获取诊断服务本机的当前日期和时间。判断「今天」「最近一小时」等相对时间时调用。"+
			"注意：这不是节点上的时间——要查节点时间或比对时钟漂移，必须在节点上执行 date。",
		func(ctx context.Context, in input) (output, error) {
			loc := time.Local
			if in.Timezone != "" {
				l, err := time.LoadLocation(in.Timezone)
				if err != nil {
					return output{}, fmt.Errorf("无法识别的时区 %q: %w", in.Timezone, err)
				}
				loc = l
			}
			now := time.Now().In(loc)
			return output{
				Time:     now.Format("2006-01-02 15:04:05"),
				Weekday:  now.Weekday().String(),
				Timezone: loc.String(),
			}, nil
		},
	)
}

// DeviceStatus 是设备状态查询的返回结构。
//
// 注意返回的是「结论」而不是原始数据点：时序指标在这一层就聚合成均值/环比/是否
// 越线，模型拿到的是可直接推理的结构化判断。真接数据表后保持这个约定。
type DeviceStatus struct {
	DeviceID     string   `json:"device_id"`
	Online       bool     `json:"online"`
	InstallState string   `json:"install_state" jsonschema_description:"装机状态"`
	KernelVer    string   `json:"kernel_version"`
	CPUUsage     string   `json:"cpu_usage" jsonschema_description:"CPU 水位与是否越线的结论"`
	Bandwidth    string   `json:"bandwidth" jsonschema_description:"带宽趋势结论，含环比"`
	Plugins      []string `json:"abnormal_plugins" jsonschema_description:"状态异常的插件列表，空表示全部正常"`
	Summary      string   `json:"summary" jsonschema_description:"一句话体检结论"`
	// DataSource 标明数据来源。接真实数据源前固定是 mock，模型据此在回答里
	// 声明数据不可信 —— 免得 mock 数据被当成真实诊断依据往下传。
	DataSource string `json:"data_source" jsonschema_description:"数据来源。mock 表示是假数据，不可作为真实诊断依据"`
}

// deviceStatusInput 是 query_device_status 的入参。
//
// 字段必须导出，否则 encoding/json 填不进去、jsonschema 也不会声明这个参数，
// 模型永远传不进值。
type deviceStatusInput struct {
	DeviceID string `json:"device_id" jsonschema:"required" jsonschema_description:"设备 ID / 节点 ID"`
}

// NewDeviceStatusTool 返回设备状态查询工具（只读）。
//
// 当前是写死的 mock 数据，仅用于打通链路。接真实数据源时把闭包里的返回换成
// 对内部数据表的参数化查询即可，函数签名和返回结构不用变。
func NewDeviceStatusTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"query_device_status",
		"查询指定设备的综合体检结论，包含在线状态、装机状态、内核版本、CPU 水位、带宽趋势和异常插件列表。诊断设备类问题时优先调用。注意：当前返回的是 mock 数据。",
		func(ctx context.Context, in deviceStatusInput) (DeviceStatus, error) {
			if in.DeviceID == "" {
				return DeviceStatus{}, fmt.Errorf("device_id 不能为空")
			}
			// TODO(接数据源): 替换为对设备数据表的参数化查询。
			return DeviceStatus{
				DeviceID:     in.DeviceID,
				Online:       true,
				InstallState: "已完成",
				KernelVer:    "5.15.0-mock",
				CPUUsage:     "均值 35%，未越线",
				Bandwidth:    "近 1 小时均值 92Mbps，较昨日同期下降 38%，14:20 起持续走低",
				Plugins:      []string{"mock-plugin-a"},
				Summary:      "带宽异常下滑且有插件状态异常，资源水位正常",
				DataSource:   "mock",
			}, nil
		},
	)
}
