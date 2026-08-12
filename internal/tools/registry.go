// Package tools 提供工具注册表。
//
// 注册表在标准的 eino tool 之外多带一个「风险等级」维度：只读工具可以让 agent
// 自由调用，变更类工具必须先经人工审核才允许真正执行。诊断系统跑在客户设备上，
// 这条边界是硬要求，所以放在注册环节强制声明，而不是靠调用方自觉。
package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/cloudwego/eino/components/tool"
)

// Risk 是工具的风险等级。
type Risk int

const (
	// RiskReadOnly 只读工具：查表、查指标、查状态、跑只读命令。agent 可自动调用。
	RiskReadOnly Risk = iota
	// RiskMutating 变更工具：重启插件、改配置、内核操作等。必须人工审核后执行。
	RiskMutating
)

func (r Risk) String() string {
	switch r {
	case RiskReadOnly:
		return "read-only"
	case RiskMutating:
		return "mutating"
	default:
		return fmt.Sprintf("unknown(%d)", int(r))
	}
}

// Entry 是注册表里的一条工具记录。
type Entry struct {
	Tool tool.BaseTool
	Risk Risk
	Name string
	Desc string
}

// Registry 是按风险分级的工具注册表。非并发安全：约定在启动阶段一次性注册完。
type Registry struct {
	entries map[string]Entry
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]Entry)}
}

// Register 登记一个工具。工具名从 Info 里取，重名会报错。
//
// 变更类工具必须是经 Gate.Wrap 包装过的 *GatedTool，否则拒绝注册。这条检查
// 是这个注册表存在的主要理由：它让「把能直接执行变更的工具交给模型」这件事
// 在启动阶段就崩掉，而不是等到某天真的在客户设备上执行了才发现。
func (r *Registry) Register(ctx context.Context, t tool.BaseTool, risk Risk) error {
	info, err := t.Info(ctx)
	if err != nil {
		return fmt.Errorf("读取工具信息失败: %w", err)
	}
	if info.Name == "" {
		return fmt.Errorf("工具名不能为空")
	}
	if _, dup := r.entries[info.Name]; dup {
		return fmt.Errorf("工具 %q 重复注册", info.Name)
	}
	if risk == RiskMutating {
		if _, gated := t.(*GatedTool); !gated {
			return fmt.Errorf("工具 %q 声明为变更类，必须先经 Gate.Wrap 包装再注册；"+
				"未包装的变更工具会被模型直接执行，禁止交给 agent", info.Name)
		}
	}
	r.entries[info.Name] = Entry{
		Tool: t,
		Risk: risk,
		Name: info.Name,
		Desc: info.Desc,
	}
	return nil
}

// MustRegister 是 Register 的 panic 版本，仅用于启动阶段注册内置工具。
func (r *Registry) MustRegister(ctx context.Context, t tool.BaseTool, risk Risk) {
	if err := r.Register(ctx, t, risk); err != nil {
		panic(err)
	}
}

// All 返回全部工具，按名字排序以保证 prompt 稳定（利于 prompt cache 命中）。
func (r *Registry) All() []tool.BaseTool {
	return r.filter(func(Entry) bool { return true })
}

// ReadOnly 返回只读工具。诊断采证阶段只挂这一批，读不坏设备，可以放开让 agent 深挖。
func (r *Registry) ReadOnly() []tool.BaseTool {
	return r.filter(func(e Entry) bool { return e.Risk == RiskReadOnly })
}

// Mutating 返回变更类工具。
func (r *Registry) Mutating() []tool.BaseTool {
	return r.filter(func(e Entry) bool { return e.Risk == RiskMutating })
}

// Named 按给定顺序返回工具。缺少任何工具都报错，避免安全工具白名单静默失效。
func (r *Registry) Named(names ...string) ([]tool.BaseTool, error) {
	out := make([]tool.BaseTool, 0, len(names))
	for _, name := range names {
		entry, ok := r.entries[name]
		if !ok {
			return nil, fmt.Errorf("工具 %q 未注册", name)
		}
		out = append(out, entry.Tool)
	}
	return out, nil
}

// RiskOf 查询某个工具的风险等级。
func (r *Registry) RiskOf(name string) (Risk, bool) {
	e, ok := r.entries[name]
	if !ok {
		return RiskReadOnly, false
	}
	return e.Risk, true
}

// Entries 返回全部记录，按名字排序，用于打印工具清单。
func (r *Registry) Entries() []Entry {
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) filter(keep func(Entry) bool) []tool.BaseTool {
	entries := r.Entries()
	out := make([]tool.BaseTool, 0, len(entries))
	for _, e := range entries {
		if keep(e) {
			out = append(out, e.Tool)
		}
	}
	return out
}
