package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/tools"
)

// errNoApproval 表示没能取得人工确认（输入流断了等）。按不执行处理。
var errNoApproval = errors.New("未取得人工确认，输入已结束")

// terminalApprover 在终端里把待执行的命令摆给人看，等一个「执行 / 拒绝」。
//
// 光标默认停在「执行」：确认多数时候都是放行，高频路径不该每次多敲一个字母。
// 代价是手滑敲回车会真下发命令——所以卡片把命令原文单独一行摊开，看清楚再回车。
//
// 它和 REPL 共用同一个 stdin scanner：模型发起调用时，REPL 那边正阻塞在等
// 模型输出，不会有两处同时读 stdin。
type terminalApprover struct {
	in       *bufio.Scanner
	operator string
	// out 留给测试重定向，零值走 stdout。
	out io.Writer
	// tty 非空时用方向键菜单。管道、cron 这类非终端场景留空，退回逐行 y/n。
	tty *os.File
}

var _ tools.Approver = (*terminalApprover)(nil)
var _ tools.Noticer = (*terminalApprover)(nil)

func (a *terminalApprover) writer() io.Writer {
	if a.out == nil {
		return os.Stdout
	}
	return a.out
}

func (a *terminalApprover) printf(format string, args ...any) {
	fmt.Fprintf(a.writer(), format, args...)
}

// Review 打印操作详情并等待人工决定。
func (a *terminalApprover) Review(ctx context.Context, p *approval.Proposal, risk tools.RiskAssessment) (tools.Decision, error) {
	if err := ctx.Err(); err != nil {
		return tools.Decision{}, err
	}
	a.render(p, risk)

	if a.tty != nil {
		return a.pick()
	}
	return a.readLines(ctx)
}

// 菜单项顺序。optRun 在前，光标默认就落在「执行」上。
const (
	optRun = iota
	optDeny
)

// pick 用方向键菜单收决定，光标默认落在「执行」。
func (a *terminalApprover) pick() (tools.Decision, error) {
	a.printf("\n\033[1m执行吗？\033[0m \033[2m↑↓ 选择，回车确认（y 执行 / n 拒绝）\033[0m\n")

	i, err := selectOption(a.tty, a.writer(),
		[]string{"执行", "拒绝"}, optRun,
		map[byte]int{'y': optRun, 'Y': optRun, 'n': optDeny, 'N': optDeny},
	)
	if err != nil {
		// 中断、断流都算「没取得确认」，交给 Gate 按未批准处理。
		return tools.Decision{}, err
	}

	if i == optRun {
		return a.approve(), nil
	}
	return a.reject(a.askReason()), nil
}

// readLines 是非终端场景（管道、cron、测试）下的退路：逐行读 y/n。
func (a *terminalApprover) readLines(ctx context.Context) (tools.Decision, error) {
	for {
		if err := ctx.Err(); err != nil {
			return tools.Decision{}, err
		}

		a.printf("\n\033[1m执行吗？\033[0m [Y] 执行（回车默认）  [n] 拒绝  > ")
		if !a.in.Scan() {
			if err := a.in.Err(); err != nil {
				return tools.Decision{}, fmt.Errorf("读取确认输入失败: %w", err)
			}
			// stdin 关了（管道喂完、终端断了）。这不是「有人敲了回车」，是
			// 压根没人在看——回车的默认值不适用于这里，一律不执行。
			return tools.Decision{}, errNoApproval
		}

		line := strings.TrimSpace(a.in.Text())
		verb, rest, _ := strings.Cut(line, " ")

		switch strings.ToLower(verb) {
		case "y", "yes", "是", "执行", "":
			return a.approve(), nil
		case "n", "no", "否", "拒绝":
			reason := strings.TrimSpace(rest)
			if reason == "" {
				reason = a.askReason()
			}
			return a.reject(reason), nil
		default:
			a.printf("\033[31m听不懂 %q。回车或 y 执行、n 拒绝（可以写成 `n 理由`）。\033[0m\n", line)
		}
	}
}

func (a *terminalApprover) approve() tools.Decision {
	// 下发是同步的，慢的时候要等好几秒。不给个回执的话，人分不清是在跑还是卡住了。
	a.printf("\033[33m▶ 已批准（%s），正在节点上执行…\033[0m\n", a.operator)
	return tools.Decision{Approved: true, Decider: a.operator}
}

func (a *terminalApprover) reject(reason string) tools.Decision {
	a.printf("\033[31m✗ 已驳回，理由会回喂给模型\033[0m\n")
	return tools.Decision{Approved: false, Decider: a.operator, Reason: reason}
}

// Notice 是执行完的回执。
//
// 批准之后到模型再开口之间可能隔着几十秒，这段时间终端不能一直静着；失败尤其
// 要当场说清楚——超时、节点离线这些原因只写进日志的话，人在终端前只会看到模型
// 含糊其辞地说「没能拿到结果」。
func (a *terminalApprover) Notice(p *approval.Proposal) {
	if p == nil {
		return
	}
	if p.Status == approval.StatusFailed {
		a.printf("\033[31m✗ 执行失败：%s\033[0m\n", p.Error)
		return
	}
	a.printf("\033[2m✓ 执行完毕，输出已回给模型\033[0m\n")
}

// askReason 追问驳回理由。理由会回喂给模型，让它换个方案而不是原样重试。
func (a *terminalApprover) askReason() string {
	a.printf("拒绝理由（直接回车跳过，会回喂给模型）> ")
	if !a.in.Scan() {
		return "（未填写理由）"
	}
	if reason := strings.TrimSpace(a.in.Text()); reason != "" {
		return reason
	}
	return "（未填写理由）"
}

// render 打印审核卡片。
//
// 该看的东西全在框里：这条操作要干什么、风险多高、动哪台机器、具体跑什么命令。
// 命令原文单独一行、不做任何截断或转义——审核人按下回车的那一刻，看到的必须
// 就是即将在设备上跑的东西。
func (a *terminalApprover) render(p *approval.Proposal, risk tools.RiskAssessment) {
	const bar = "\033[33m│\033[0m"
	// 逐字符 repeat，不要对 rule 做字节切片——"─" 是三字节，切一半会出乱码。
	a.printf("\n\033[33m┌─ 待人工确认 %s\033[0m\n", strings.Repeat("─", 47))

	// 「提案」写这条操作的含义，而不是工具名——审核人关心的是要干什么。
	// 模型没给说明时才退回工具名，至少能看出走的是哪条通道。
	what := risk.Purpose
	if what == "" {
		what = p.Tool
	}
	a.printf("%s 提案  %s  \033[2m(%s)\033[0m\n", bar, what, p.ID)
	a.printf("%s 风险  \033[%sm%s\033[0m —— %s\n", bar, risk.Level.Color(), risk.Level, risk.Reason)
	if risk.Target != "" {
		a.printf("%s 节点  %s\n", bar, risk.Target)
	}

	a.printf("%s\n", bar)
	if risk.Command != "" {
		a.printf("%s   \033[1m$ %s\033[0m\n", bar, risk.Command)
	} else {
		// 非命令类工具没法抽出一行命令，就把完整参数摊开。
		a.printf("%s   参数: %s\n", bar, p.PrettyArgs())
	}
	a.printf("%s\n", bar)

	a.printf("%s \033[2m风险等级由关键词匹配得出，仅供参考，请以上面的命令原文为准\033[0m\n", bar)
	a.printf("\033[33m└%s\033[0m\n", strings.Repeat("─", 60))
}
