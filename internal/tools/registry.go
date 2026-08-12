// Package tools 提供带风险等级和业务域的工具注册表。
package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/cloudwego/eino/components/tool"
)

// Domain 是工具所属的业务域。风险等级决定工具能否直接执行，业务域决定
// 哪条诊断子图能看到它；两者是互相独立的安全边界。
type Domain string

const (
	DomainCode         Domain = "code"
	DomainInstallation Domain = "installation"
	DomainTraffic      Domain = "traffic"
	DomainPlugin       Domain = "plugin"
	DomainKernel       Domain = "kernel"
	DomainNetwork      Domain = "network"
)

// Valid 报告业务域是否为内置稳定值。
func (d Domain) Valid() bool {
	switch d {
	case DomainCode, DomainInstallation, DomainTraffic, DomainPlugin, DomainKernel, DomainNetwork:
		return true
	default:
		return false
	}
}

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
	Tool    tool.BaseTool
	Risk    Risk
	Domains []Domain
	Name    string
	Desc    string
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
	return r.RegisterInDomains(ctx, t, risk)
}

// RegisterInDomains 登记工具及其业务域。domains 为空仅用于兼容没有子图路由的
// 调用方；应用装配内置工具时必须显式声明，避免工具意外进入诊断分支。
func (r *Registry) RegisterInDomains(
	ctx context.Context,
	t tool.BaseTool,
	risk Risk,
	domains ...Domain,
) error {
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
	normalizedDomains, err := normalizeDomains(domains)
	if err != nil {
		return fmt.Errorf("工具 %q 的业务域无效: %w", info.Name, err)
	}
	r.entries[info.Name] = Entry{
		Tool:    t,
		Risk:    risk,
		Domains: normalizedDomains,
		Name:    info.Name,
		Desc:    info.Desc,
	}
	return nil
}

// MustRegister 是 Register 的 panic 版本，仅用于启动阶段注册内置工具。
func (r *Registry) MustRegister(ctx context.Context, t tool.BaseTool, risk Risk) {
	if err := r.Register(ctx, t, risk); err != nil {
		panic(err)
	}
}

// MustRegisterInDomains 是 RegisterInDomains 的 panic 版本，仅用于启动装配。
func (r *Registry) MustRegisterInDomains(
	ctx context.Context,
	t tool.BaseTool,
	risk Risk,
	domains ...Domain,
) {
	if err := r.RegisterInDomains(ctx, t, risk, domains...); err != nil {
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

// InDomains 返回至少属于一个指定业务域的工具。无业务域工具不会被选中，
// 这样旧调用或漏标注不会扩大任何诊断分支的工具权限。
func (r *Registry) InDomains(domains ...Domain) []tool.BaseTool {
	wanted := make(map[Domain]struct{}, len(domains))
	for _, domain := range domains {
		if domain.Valid() {
			wanted[domain] = struct{}{}
		}
	}
	return r.filter(func(e Entry) bool {
		for _, domain := range e.Domains {
			if _, ok := wanted[domain]; ok {
				return true
			}
		}
		return false
	})
}

// RequireDomains 是诊断子图使用的严格业务域查询。请求中存在未知域，或任一
// 业务域没有注册工具时直接失败，避免配置遗漏后静默启动一个无采证能力的分支。
func (r *Registry) RequireDomains(domains ...Domain) ([]tool.BaseTool, error) {
	wanted := make(map[Domain]bool, len(domains))
	for _, domain := range domains {
		if !domain.Valid() {
			return nil, fmt.Errorf("未知业务域 %q", domain)
		}
		wanted[domain] = false
	}
	for _, entry := range r.Entries() {
		for _, domain := range entry.Domains {
			if _, ok := wanted[domain]; ok {
				wanted[domain] = true
			}
		}
	}
	for _, domain := range domains {
		if !wanted[domain] {
			return nil, fmt.Errorf("业务域 %q 没有注册工具", domain)
		}
	}
	return r.InDomains(domains...), nil
}

// DomainsOf 返回工具的业务域副本。
func (r *Registry) DomainsOf(name string) ([]Domain, bool) {
	e, ok := r.entries[name]
	if !ok {
		return nil, false
	}
	return append([]Domain(nil), e.Domains...), true
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
		e.Domains = append([]Domain(nil), e.Domains...)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func normalizeDomains(domains []Domain) ([]Domain, error) {
	seen := make(map[Domain]struct{}, len(domains))
	out := make([]Domain, 0, len(domains))
	for _, domain := range domains {
		if !domain.Valid() {
			return nil, fmt.Errorf("未知业务域 %q", domain)
		}
		if _, ok := seen[domain]; ok {
			continue
		}
		seen[domain] = struct{}{}
		out = append(out, domain)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
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
