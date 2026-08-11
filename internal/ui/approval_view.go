package ui

import (
	"fmt"
	"strings"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/tools"
)

// ApprovalCard 返回审核所需的完整信息。
func ApprovalCard(proposal *approval.Proposal, risk tools.RiskAssessment) string {
	if proposal == nil {
		return "待人工确认\n提案信息缺失"
	}
	what := risk.Purpose
	if what == "" {
		what = proposal.Tool
	}

	var view strings.Builder
	view.WriteString("待人工确认\n")
	fmt.Fprintf(&view, "提案  %s  (%s)\n", what, proposal.ID)
	fmt.Fprintf(&view, "风险  %s - %s\n", risk.Level, risk.Reason)
	if risk.Target != "" {
		fmt.Fprintf(&view, "节点  %s\n", risk.Target)
	}
	if risk.Command != "" {
		fmt.Fprintf(&view, "\n$ %s\n", risk.Command)
	} else {
		fmt.Fprintf(&view, "\n参数: %s\n", proposal.PrettyArgs())
	}
	view.WriteString("\n风险等级仅供参考，请以命令原文为准；回车默认选择执行，但不会自动确认。")
	return view.String()
}
