// Command chat 是一个命令行诊断助手。
//
// 用途是把「配置 → 模型 → 工具 → 人工确认 → 执行 → 多轮对话」这条链路打通，
// 作为诊断系统的最小可运行骨架。后续接钉钉机器人时，把这里的 REPL 换成
// Stream 模式的消息回调、把 terminalApprover 换成互动卡片即可，
// internal/* 全部可复用。
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"diagnostic-system/internal/agent"
	"diagnostic-system/internal/approval"
	"diagnostic-system/internal/config"
	"diagnostic-system/internal/llm"
	"diagnostic-system/internal/logging"
	"diagnostic-system/internal/session"
	"diagnostic-system/internal/tools"
)

// app 把一轮对话需要的东西打包，省得到处传参。
//
// 审核相关的状态不在这里：确认发生在 Gate 内部，执行记录进 audit.log，
// REPL 只管对话本身。
type app struct {
	agent *agent.Agent
	sess  *session.Session
	in    *bufio.Scanner
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n错误: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Ctrl-C 取消当前请求并退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	closeLog, err := logging.Setup(cfg)
	if err != nil {
		return err
	}
	defer closeLog()
	logging.InstallEinoCallbacks()
	slog.Info("启动", "config", cfg.Redacted())

	cm, err := llm.NewChatModel(ctx, cfg)
	if err != nil {
		return err
	}

	// stdin 由 app 统一持有：REPL 读用户输入，审核确认也读它。
	// 两处不会同时读——模型跑的时候 REPL 阻塞在等输出。
	in := bufio.NewScanner(os.Stdin)
	// 单行输入放宽到 1MB，方便直接粘贴日志片段。
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// operator 会写进审计日志，也随命令上报给 tunnel 对端。
	tools.Tunnel.SetOperator(cfg.Operator)

	// 是终端就用方向键菜单确认；管道 / cron 之类退回逐行 y/n。
	approver := &terminalApprover{in: in, operator: cfg.Operator}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		approver.tty = os.Stdin
	}

	var storeOpts []approval.Option
	if cfg.AuditLog != "" {
		storeOpts = append(storeOpts, approval.WithAuditLog(cfg.AuditLog))
	}
	gate := tools.NewGate(approval.NewStore(storeOpts...),
		tools.WithApprover(approver),
		tools.WithTimeout(cfg.ToolTimeout),
	)

	reg, err := registerTools(ctx, gate)
	if err != nil {
		return err
	}

	ag, err := agent.New(ctx, cm, reg, cfg)
	if err != nil {
		return err
	}

	a := &app{
		agent: ag,
		sess:  session.New(cfg.HistoryTurns),
		in:    in,
	}

	printBanner(cfg, reg)
	return a.repl(ctx)
}

// registerTools 集中注册工具并声明风险等级。新增工具只改这一个函数。
//
// 变更类工具必须先过 gate.Wrap；漏包装的话 Registry 会直接拒绝注册，
// 启动就崩，不会带着隐患跑起来。
func registerTools(ctx context.Context, gate *tools.Gate) (*tools.Registry, error) {
	reg := tools.NewRegistry()

	nowTool, err := tools.NewNowTool()
	if err != nil {
		return nil, fmt.Errorf("构造 now 工具失败: %w", err)
	}
	if err := reg.Register(ctx, nowTool, tools.RiskReadOnly); err != nil {
		return nil, err
	}

	tunnelTool, err := tools.NewTunnelTool()
	if err != nil {
		return nil, fmt.Errorf("构造 run_tunnel_cmd 工具失败: %w", err)
	}
	gatedTunnel, err := gate.Wrap(ctx, tunnelTool)
	if err != nil {
		return nil, fmt.Errorf("包装 run_tunnel_cmd 失败: %w", err)
	}
	if err := reg.Register(ctx, gatedTunnel, tools.RiskMutating); err != nil {
		return nil, err
	}

	return reg, nil
}

func (a *app) repl(ctx context.Context) error {
	for {
		fmt.Print("\n\033[36m你 >\033[0m ")
		if !a.in.Scan() {
			fmt.Println()
			return a.in.Err()
		}
		line := strings.TrimSpace(a.in.Text())
		if line == "" {
			continue
		}

		handled, err := a.runCommand(line)
		if err != nil {
			if errors.Is(err, errQuit) {
				return nil
			}
			fmt.Fprintf(os.Stderr, "\033[31m%v\033[0m\n", err)
			continue
		}
		if handled {
			continue
		}

		if err := a.ask(ctx, line); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			// 单轮出错不退出整个会话，报错后继续等下一句。
			slog.Error("对话轮失败", "err", err)
			fmt.Fprintf(os.Stderr, "\n\033[31m本轮失败: %s\033[0m\n", humanErr(err))
		}
	}
}

// humanErr 把底层错误换成人话。
//
// 「context deadline exceeded」这种原文对着终端弹出来等于没说，人得知道
// 是超时了、还是模型那边不通。
func humanErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，模型没有在规定时间内返回。可以再问一次，或把问题拆小一点。"
	case errors.Is(err, context.Canceled):
		return "请求已取消。"
	default:
		return err.Error()
	}
}

var errQuit = errors.New("quit")

// runCommand 处理以 / 开头的指令，返回是否已被消费。
func (a *app) runCommand(line string) (bool, error) {
	if !strings.HasPrefix(line, "/") {
		return false, nil
	}

	switch strings.Fields(line)[0] {
	case "/exit", "/quit":
		return true, errQuit
	case "/reset":
		a.sess.Reset()
		fmt.Println("已清空对话历史。")
		return true, nil
	case "/history":
		a.printHistory()
		return true, nil
	case "/help":
		printHelp()
		return true, nil
	default:
		return true, fmt.Errorf("未知命令 %s，输入 /help 查看可用命令", strings.Fields(line)[0])
	}
}

// ask 跑一轮对话。
//
// 期间模型如果要在设备上执行命令，terminalApprover 会当场弹出确认；
// 批准后命令真正执行，结果直接作为工具返回值回到模型手里，同一轮里继续推理。
func (a *app) ask(ctx context.Context, input string) error {
	a.sess.AppendUser(input)
	slog.Info("用户提问", "text", input)

	fmt.Print("\n\033[32m助手 >\033[0m ")
	reply, err := a.agent.Stream(ctx, a.sess.Messages(), func(chunk string) {
		fmt.Print(chunk)
	})
	fmt.Println()

	if err != nil {
		return err
	}
	if reply == "" {
		// 操作被驳回后模型经常就不说话了——驳回提示已经打过，不用再报错吓人。
		slog.Warn("模型返回了空回复")
		fmt.Println("\033[2m（模型没有再补充说明）\033[0m")
		return nil
	}
	a.sess.AppendAssistant(reply)
	slog.Info("模型回复", "chars", len([]rune(reply)))
	return nil
}

func (a *app) printHistory() {
	msgs := a.sess.Messages()
	if len(msgs) == 0 {
		fmt.Println("（历史为空）")
		return
	}
	for _, m := range msgs {
		fmt.Printf("\033[2m[%s]\033[0m %s\n", m.Role, oneLine(m.Content))
	}
}

// oneLine 把多行内容压成一行摘要，用于列表展示。
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 90 {
		return string(r[:90]) + "…"
	}
	return s
}

func printBanner(cfg *config.Config, reg *tools.Registry) {
	fmt.Println("CDN 诊断助手（Eino 骨架）")
	fmt.Println("配置:", cfg.Redacted())
	fmt.Println("已注册工具:")
	for _, e := range reg.Entries() {
		mark := ""
		if e.Risk == tools.RiskMutating {
			mark = " —— 每次执行前都会弹出确认"
		}
		fmt.Printf("  - %-22s [%s]%s\n", e.Name, e.Risk, mark)
	}
	printHelp()
}

func printHelp() {
	fmt.Println("直接说话就行。模型要在设备上执行命令时，会先把命令和风险等级摆出来等你确认。")
	fmt.Println("命令: /history 看历史 | /reset 清空历史 | /help 帮助 | /exit 退出（Ctrl-C 同样退出）")
}
