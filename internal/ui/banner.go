package ui

import (
	"fmt"
	"strings"
)

// ToolSummary 是启动页需要的工具元数据。
type ToolSummary struct {
	Name    string
	Risk    string
	Domains []string
}

// StartupInfo 是启动装配层传给 UI 的结构化展示数据。
type StartupInfo struct {
	Config      string
	Tools       []ToolSummary
	Skills      []string
	SessionName string
	SessionID   string
	Messages    int
	Tokens      int
	// PendingApprovals 是上次退出时还没了结的变更提案数量。
	PendingApprovals int
}

// StartupBanner 生成启动时显示的摘要。
func StartupBanner(info StartupInfo) string {
	toolNames := make([]string, 0, len(info.Tools))
	for _, tool := range info.Tools {
		domains := "unscoped"
		if len(tool.Domains) > 0 {
			domains = strings.Join(tool.Domains, "+")
		}
		toolNames = append(toolNames, fmt.Sprintf("%s [%s; %s]", tool.Name, tool.Risk, domains))
	}
	loadedSkills := "无"
	if len(info.Skills) > 0 {
		loadedSkills = strings.Join(info.Skills, ", ")
	}
	banner := fmt.Sprintf("配置: %s\n工具: %s\nSkill: %s\n当前会话: %s [%s]，已加载 %d 条消息，约 %d tokens",
		info.Config, strings.Join(toolNames, ", "), loadedSkills,
		info.SessionName, info.SessionID, info.Messages, info.Tokens)
	if info.PendingApprovals > 0 {
		// 上次退出时还挂着的东西必须一进门就说，不能等人自己想起来去查：
		// 没人处理的提案意味着有一轮诊断停在半路，设备上的问题还在。
		banner += fmt.Sprintf("\n有 %d 条变更提案尚未了结，输入 /approvals 处理。", info.PendingApprovals)
	}
	return banner
}
