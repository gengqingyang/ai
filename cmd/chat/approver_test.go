package main

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/tools"
)

func newTestApprover(input string) (*terminalApprover, *strings.Builder) {
	var out strings.Builder
	return &terminalApprover{
		in:       bufio.NewScanner(strings.NewReader(input)),
		operator: "tester",
		out:      &out,
	}, &out
}

func testProposal() (*approval.Proposal, tools.RiskAssessment) {
	p := approval.NewStore().Create("run_tunnel_cmd", `{"sn":"SN001","cmd":"date","purpose":"查看节点系统时间"}`)
	return p, tools.AssessRisk(p.Tool, p.Args)
}

func TestTerminalApproverAnswers(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantOK     bool
		wantReason string
	}{
		{"回车默认执行", "\n", true, ""},
		{"y 执行", "y\n", true, ""},
		{"YES 大小写不敏感", "YES\n", true, ""},
		{"中文执行", "执行\n", true, ""},
		{"空格也算空行", "   \n", true, ""},
		{"n 拒绝后追问理由", "n\n线上高峰期\n", false, "线上高峰期"},
		{"n 带行内理由", "n 线上高峰期\n", false, "线上高峰期"},
		{"中文拒绝", "拒绝 等窗口期\n", false, "等窗口期"},
		{"拒绝但不写理由", "n\n\n", false, "（未填写理由）"},
		{"认不出的输入会重问", "什么\nn 不干\n", false, "不干"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newTestApprover(tc.input)
			p, risk := testProposal()

			d, err := a.Review(context.Background(), p, risk)
			if err != nil {
				t.Fatalf("Review() error = %v", err)
			}
			if d.Approved != tc.wantOK {
				t.Errorf("Approved = %v, want %v", d.Approved, tc.wantOK)
			}
			if d.Decider != "tester" {
				t.Errorf("Decider = %q, want tester", d.Decider)
			}
			if tc.wantReason != "" && d.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.wantReason)
			}
		})
	}
}

// 回车默认放行，但「没人在场」不等于「有人敲了回车」：stdin 断掉时必须报错，
// 由 Gate 那边按未批准处理。否则挂在 cron 里跑一次就是无人值守执行。
func TestTerminalApproverEOFIsNotApproval(t *testing.T) {
	a, _ := newTestApprover("")
	p, risk := testProposal()

	d, err := a.Review(context.Background(), p, risk)
	if !errors.Is(err, errNoApproval) {
		t.Fatalf("err = %v, want errNoApproval", err)
	}
	if d.Approved {
		t.Fatal("stdin 已关闭却返回了批准")
	}
}

func TestTerminalApproverContextCancel(t *testing.T) {
	a, _ := newTestApprover("y\n")
	p, risk := testProposal()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d, err := a.Review(ctx, p, risk)
	if err == nil {
		t.Fatal("ctx 已取消却没报错")
	}
	if d.Approved {
		t.Fatal("ctx 已取消却返回了批准")
	}
}

// 审核人是靠这张卡片做判断的：操作含义、命令原文、风险等级、节点，一个都不能少，
// 而且必须都在框里，不能散落到框外面去。
func TestTerminalApproverRendersCommandAndRisk(t *testing.T) {
	a, out := newTestApprover("n 不执行\n")
	p, risk := testProposal()

	if _, err := a.Review(context.Background(), p, risk); err != nil {
		t.Fatalf("Review() error = %v", err)
	}

	printed := out.String()
	for _, want := range []string{"查看节点系统时间", "$ date", "只读", "SN001", p.ID, "回车默认"} {
		if !strings.Contains(printed, want) {
			t.Errorf("确认卡片里没有 %q:\n%s", want, printed)
		}
	}

	// 框内的每一行都要带边框字符，否则视觉上就散了。
	body, _, ok := strings.Cut(printed, "\033[33m└")
	if !ok {
		t.Fatalf("卡片没有收尾:\n%s", printed)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(body), "\n") {
		if line == "" || strings.Contains(line, "┌") {
			continue
		}
		if !strings.Contains(line, "│") {
			t.Errorf("这一行跑到框外面去了: %q", line)
		}
	}
}

// 模型没填 purpose 时退回工具名，不能开天窗。
func TestTerminalApproverFallsBackToToolName(t *testing.T) {
	p := approval.NewStore().Create("run_tunnel_cmd", `{"sn":"SN001","cmd":"date"}`)

	a, out := newTestApprover("n 不执行\n")
	if _, err := a.Review(context.Background(), p, tools.AssessRisk(p.Tool, p.Args)); err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if !strings.Contains(out.String(), "run_tunnel_cmd") {
		t.Errorf("没写操作含义、也没退回工具名:\n%s", out.String())
	}
}

// 命令原文不做截断：审核人看到的必须就是即将执行的东西。
func TestTerminalApproverDoesNotTruncateCommand(t *testing.T) {
	long := "journalctl -u onething-agent --since '2026-07-30 00:00:00' --no-pager | grep -i error"
	store := approval.NewStore()
	p := store.Create("run_tunnel_cmd", `{"sn":"SN001","cmd":`+quote(long)+`}`)

	a, out := newTestApprover("n 不执行\n")
	if _, err := a.Review(context.Background(), p, tools.AssessRisk(p.Tool, p.Args)); err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if !strings.Contains(out.String(), long) {
		t.Errorf("长命令被截断了:\n%s", out.String())
	}
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
