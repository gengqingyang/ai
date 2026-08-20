# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

本仓库的代码注释、文档和终端文案统一使用中文，新增内容请保持一致。

## 常用命令

```bash
make run      # go run ./cmd/chat，启动交互式诊断助手（自动读取当前目录 .env）
make build    # 编译到 bin/chat
make fmt      # go fmt ./...
make vet      # go vet ./...
make test     # go test ./...
make tidy     # go mod tidy

go test ./test -run TestGateRequiresApproval   # 跑单个用例（全部测试都在 test 包）
go test ./test -run 'TestCode.*' -v
go test -race ./test                            # 需要 C toolchain

LLM_MODEL=xxx go run ./cmd/chat                 # 真实环境变量覆盖 .env
ENV_FILE=/path/to/prod.env go run ./cmd/chat    # 换配置文件
tail -f audit.log | jq                          # 审批流转（JSONL）
tail -f diagnostic.log | jq                     # 运行日志（slog JSON）
```

提交前跑 `make fmt`、`make vet`、`make test`。

## 架构总览

Go 1.24 + [Eino](https://github.com/cloudwego/eino) 的 CDN 智能诊断命令行助手。一轮对话的完整链路：

```
用户输入 (internal/ui)
  → chat.App 组装消息 + 会话历史 (internal/chat, internal/session)
  → agent.Router：compose.Graph 分类节点 → 8 个 Flow 分支 (internal/agent/router.go)
  → agent.EvidencePipeline：按 Flow 前置执行固定只读采证 + 校验 (internal/agent/evidence.go)
  → 对应 Flow 的 ADK ChatModelAgent（独立 prompt + 域内工具白名单）
  → 变更类工具经 tools.Gate 同步弹审批卡给人确认
  → ADK 事件流回调 → ui 消息通道 → 根 Model 渲染
```

分层边界必须守住：

- `cmd/chat/main.go` 是薄入口；所有装配集中在 `internal/application/run.go`（配置 → 模型 → 审批存储 → Gate → 工具注册 → Agent → 会话 → UI）。
- `internal/chat` **不导入 Bubble Tea**，是无终端依赖的对话业务层。终端只有 `internal/ui` 一个常驻 Bubble Tea 生命周期，根 `ui.Model` 从启动持有到退出；`ui/input`、`ui/menu` 是嵌入式子模型，不创建嵌套 program。
- `internal/agent` 只消费 ADK 事件并通过回调上报文本，不接触终端。
- 后台 Agent 只往 `chan tea.Msg` 投递事件，绝不直接读写终端。

## 关键不变量（改动时容易踩）

**工具注册表是双维度的：Risk × Domain**（`internal/tools/registry.go`）。
- `RiskMutating` 的工具如果不是 `*GatedTool`，`Register` 会直接报错——漏 `gate.Wrap` 在**启动阶段崩掉**，不会等到真在客户设备上执行。新增变更类工具照着 `internal/application/run.go` 的 `registerTools` 写：先 `gate.Wrap(ctx, tool)`，再 `RegisterInDomains(..., RiskMutating, domains...)`。
- Domain（installation/traffic/plugin/kernel/network/code）决定哪个 Flow 能看到该工具。`agent.toolsForFlow` 用 `reg.RequireDomains` 取白名单，并**主动过滤掉五个固定采证工具**——它们由前置 EvidencePipeline 执行，不重复暴露给模型。

**审批闸门是结构性约束，不靠 prompt**（`internal/tools/gate.go` + `internal/approval/`）。
- 真实实现扣在 `Gate.inner` 里，`GatedTool` 结构体没有它的引用。
- 确认发生在 `GatedTool.InvokableRun` **同一轮之内**，同步等 UI 回复；`ui.Approver` 不读 stdin。
- 状态机 `pending → approved → executing → executed/failed/unknown`（或 `pending → rejected`），带前置状态校验；进入 `executing` 必须先原子持久化抢占执行权。进程在取得执行权后崩溃，启动恢复把 `executing` 保守转成 `unknown`（不可自动重试的终态）。
- 批准时回放提案里存的**原始参数**，不做二次加工。
- 失败一律朝安全那边倒：报错、Ctrl-C、UI 关闭都当「未批准」。不要为了让测试通过而弱化闸门断言。

**工具入参字段必须导出。** 未导出字段既不进 `utils.InferTool` 生成的 JSON Schema，也无法被 `encoding/json` 反序列化，模型永远传不进值，最后执行一条空命令。`test/tools_gate_test.go` 有针对 `run_tunnel_cmd` 的 schema 断言防回归。

**只读节点工具不接受 Shell。** 五类采证工具（`internal/tools/evidence*.go`）只收校验过的节点 ID（流量工具额外收 1–5 秒采样窗口），命令模板固化在 `evidence_commands.go`，节点 ID 不拼进命令。任意命令走受 Gate 保护的 `run_tunnel_cmd`，不能因为「这次看起来只读」就绕开审批。

**只读工具返回结论，不返回原始数据。** Go 层把节点输出解析成统一的 `EvidenceReport`（`data_source`/`collected_at`/状态/摘要/证据项/局限说明）。不要把几百个采样点丢给模型。

**代码仓库后端只读且强制隔离**（`internal/repository/`）。`LocalRepoBackend` 实现 Eino filesystem 协议，但 `Write`/`Edit` 固定失败，无 Shell。查询限定在当前命名仓库的索引内；`.git`、`.env`、私钥、日志、聊天历史、`.repositories.json`、`.approvals.json`、二进制、软链接、>2MB 文件在扫描阶段排除。仓库目录只持久化根路径、Git commit、索引版本和更新时间，不保存源码正文。

**风险等级只是给审核人的提示，不是准入判断**（`internal/tools/risk.go`）。按 `|`、`&&`、`;` 切段逐段判取最坏值，剥 `sudo`/环境变量前缀后取 basename，`$( )`/反引号/`>` 一律判危险。任何等级都仍需人点头，没有「低风险自动放行」。

**mock 数据必须自报家门**：返回值带 `data_source=mock`。

## 新增能力时的落点

| 想做的事 | 改哪里 |
| --- | --- |
| 换模型供应商 / 调重试 | `internal/llm/model.go`、`retry.go`（接口统一是 `model.ToolCallingChatModel`，上层不动） |
| 新增故障类型 | `internal/intent/`（枚举 + prompt）→ `agent/router.go` 的 `Flow` 与 `flowFor` → `agent/agent.go` 的 `flowSpecs` → `agent/prompt.go` → `tools.Domain` |
| 新增只读采证 | `tools/evidence_commands.go` 加模板 → `evidence_parse.go` 加解析 → `run.go` 按 Domain 注册 → `agent/evidence.go` 的 `evidenceSpecs` |
| 新增变更工具 | `tools/` 实现 → `run.go` 里 `gate.Wrap` + `RiskMutating` 注册 |
| 新增 Skill | `skills/<名称>/SKILL.md`（YAML frontmatter 需 `name` + `description`）；只支持 inline，`fork`/`fork_with_context` 在启动阶段被拒 |
| 改 UI | `ui/update.go` 路由事件与按键、`ui/view.go` 布局、`ui/approval_*.go` 审批卡 |

## 意图分类与上下文

分类器（`internal/intent/`）只绑定不可执行的 `report_intent` schema，OpenAI 和 Claude 都用强制 tool choice 返回同一结构。稳定枚举：`installation_failure`、`traffic_anomaly`、`plugin_failure`、`kernel_upgrade_failure`、`network_configuration_failure`、`code_repository_question`、`other`、`unknown`。

- 分类只带最近 6 条用户/助手消息，更早历史压到 1200 字符；最新用户消息保留原始多模态内容，截图参与分类。
- `unknown`、置信度 < 0.6、或分类器标记信息不足 → 澄清分支；流量/插件/内核/配网分支缺设备 ID 也强制澄清。澄清路径在模型调用层**禁用全部工具**。
- 分类失败终止本轮并保留原始错误，不静默猜一个类别继续执行。
- 会话历史（`internal/session/`）只累积 user 和 assistant 的最终消息，不含 tool call 往返。system prompt 与本轮路由元数据都不进持久化历史。索引 `.chat_history.json` + 各会话正文 `.chat_history_sessions/`，均以 `0600` 原子写入。

## 配置

优先级：真实环境变量 > `.env` 文件 > 内置默认值。程序启动自动读 `.env`，不需要 source。完整变量表见 `README.md`；常用的几个：`LLM_PROVIDER`(openai/claude)、`LLM_BASE_URL`、`LLM_AUTH_TOKEN`/`LLM_API_KEY`、`LLM_MODEL`、`LLM_CONTEXT_TOKENS`(1M)、`AGENT_HISTORY_TOKENS`(900K)、`AGENT_MAX_STEP`、`AGENT_SKILLS_DIR`、`TOOL_TIMEOUT`(60s，只圈住批准后真正下发的那一段)、`AUDIT_LOG`、`LOG_FILE`、`OPERATOR`。

新增环境变量要在 `README.md` 记录并给安全默认值。`.env`、`audit.log`、`diagnostic.log`、`.chat_history*`、`.repositories.json`、`.approvals.json` 都不入库。

## 测试约定

所有测试放 `test/`，统一 `package test`，通过 dot-import 访问被测包（`. "diagnostic-system/internal/tools"`）。命名 `<area>_test.go`，用例 `TestBehavior` / `TestComponentBehavior`。解析器、校验和风险规则优先表驱动。用 `t.TempDir`、`t.Setenv` 和 fake（如 `test/evidence_tools_test.go` 的 fake runner），不要连真实凭证、模型端点或节点。`test/tools_timeout_test.go` 里有一个刻意不看 `ctx` 的假工具，守「超时 = 结果未知，不是没执行」这条语义。

## 相关文档

- `README.md`：面向使用者的完整说明（交互命令、审批流程演示、环境变量全表、目录结构、设计约定原文）
- `docs/requirements.md`：需求、优先级与验收标准
- `AGENTS.md`：与本文件重叠的仓库通用规范
