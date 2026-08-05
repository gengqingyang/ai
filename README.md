# CDN 智能诊断系统 · Eino 骨架

基于 [Eino](https://github.com/cloudwego/eino)（CloudWeGo，Go）搭的 agent 骨架。当前是一个可跑的命令行诊断助手，已打通「配置 → 模型 → 工具 → agent → 人工审核 → 多轮对话」这条链路，后续按故障类型逐步长成完整诊断系统。

## 快速开始

```bash
# 1. 配置凭证：程序启动时会自动读取当前目录的 .env，不需要 source
cp .env.example .env && vim .env

# 2. 跑起来
make run
```

交互命令：

| 命令 | 作用 |
| --- | --- |
| `/history` | 回看本会话的对话历史 |
| `/reset` | 清空对话历史 |
| `/help` | 帮助 |
| `/exit` | 退出（Ctrl-C 同样退出） |

平时直接说话就行。模型要在设备上执行命令时，会当场把命令和风险等级摆出来等你确认：

```
你 > 帮我查下 SN001 这台机器的时间

需要在节点上执行 date 读取系统时间。这是只读查询，不改变机器状态。

┌─ 待人工确认 ───────────────────────────────────────────────
│ 提案  查看节点系统时间  (P001)
│ 风险  只读 —— date 是只读查询命令
│ 节点  SN001
│
│   $ date
│
│ 风险等级由关键词匹配得出，仅供参考，请以上面的命令原文为准
└────────────────────────────────────────────────────────────

执行吗？ ↑↓ 选择，回车确认（y 执行 / n 拒绝）
  ❯ 执行
    拒绝
▶ 已批准（root），正在节点上执行…
✓ 执行完毕，输出已回给模型

Thu Jul 30 14:22:03 CST 2026 —— 节点时间正常，与当前时间无偏移。
```

模型说的每一句都是边生成边打的，包括调工具之前那句「我先在 SN001 上执行 date」——ReAct 一轮里模型会开口好几次，只有最后一次会流到 REPL 手上，中间那几次得从 ChatModel 的组件回调里接（`internal/agent/agent.go` 的 `streamPrinter`）。否则屏幕上就是敲完问题一片空白，直到审核框突然弹出来。

「提案」那行是**模型自己写的操作意图**（`run_tunnel_cmd` 的 `purpose` 参数），不是工具名——审核人关心的是这条命令要干什么。但它没有经过任何校验，所以永远和命令原文并排放：**判断以 `$` 那一行为准**。

执行记录不往对话里刷，全在 `audit.log` 里，要查就 `tail -f audit.log | jq`。

**光标默认停在「执行」**——确认多数时候都是放行，高频路径不该每次多按一下方向键。回车即下发到节点，输出直接回到模型手里，同一轮里继续推理；选「拒绝」后可以补一句理由，理由回喂给模型让它换方案。所以卡片把命令原文单独一行摊开：回车之前看清楚那一行。

按键：`↑↓`（或 `←→`、`j`/`k`）移动，回车确认，`y`/`n` 直接定音，`Ctrl-C` 中断确认（按未批准处理）。stdin 不是终端时（管道、cron）自动退回逐行 `y`/`n` 问答。

配置的优先级是**真实环境变量 > .env 文件 > 内置默认值**，所以可以临时覆盖单项而不改文件：

```bash
LLM_MODEL=gpt-5.6-sol go run ./cmd/chat
ENV_FILE=/path/to/prod.env go run ./cmd/chat   # 指定别的配置文件
```

> 注意：如果你的 token 只配在 `~/.claude/settings.json` 的 `env` 块里，那它只对 Claude Code 的子进程生效，普通 shell 里读不到——这种情况必须建 `.env`（或自己 export）。

主要环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LLM_PROVIDER` | `openai` | 模型协议：`openai` / `claude` |
| `LLM_BASE_URL` / `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` | 官方端点 | 走中转网关时填；OpenAI 模式自动补 `/v1` |
| `LLM_AUTH_TOKEN` / `LLM_API_KEY` | — | 二选一，必填 |
| `LLM_MODEL` | `gpt-5.6-sol` | 模型 ID |
| `AGENT_MAX_STEP` | `12` | ReAct 最大步数 |
| `AGENT_HISTORY_TURNS` | `10` | 保留的对话轮数 |
| `TOOL_TIMEOUT` | `60s` | 单次工具执行超时（写 `60` 按秒算）；不含等人审核的时间 |
| `AUDIT_LOG` | `audit.log` | 审计日志路径，留空表示不落盘（生产不应留空） |
| `LOG_FILE` | `diagnostic.log` | 运行日志路径；填 `stderr` 打终端，留空不记 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`；`DEBUG=true` 时自动降到 debug |
| `OPERATOR` | `$USER` | 审核人/操作人标识，写进审计日志、并随命令上报给 tunnel 对端 |
| `TUNNEL_API` | 内置地址 | tunnel 同步下发接口 |

## 人工审核闸门

节点是客户设备，误操作会真的把机器搞坏，所以**模型不允许直接执行任何变更**。这条约束是结构性的，不是靠 prompt 求模型自觉：

```
模型 ──调用──> GatedTool ──登记提案──> 风险评估 ──> 弹给人确认
                  ↑                                    │
            没有执行能力，                        批准 ↓ 拒绝 ↘
            不持有真实工具的引用          Gate.Execute      驳回理由
                                        回放原始参数         ↓
                                              ↓          回喂模型
                                          真实工具 ──> 真实输出回喂模型
```

- `tools.Gate.Wrap` 把变更工具包成必须过闸门的门面，真实实现被扣在 `Gate.inner` 里，`GatedTool` 结构体里根本没有它的引用。
- 确认发生在**模型这一轮之内**：`GatedTool.InvokableRun` 里同步问人，拿到答复才决定是否放行。批准后模型拿到 `status=executed` 和设备真实输出，拒绝则拿到 `status=rejected` 和理由——它不需要、也没机会去猜发生了什么。
- `Registry.Register` 拒绝注册非 `*GatedTool` 的 `RiskMutating` 工具——漏包装会在**启动阶段崩掉**，而不是等某天真在客户设备上跑了才发现。
- 批准时回放的是提案里存的**原始参数**，不做任何二次加工：审核人看到的和实际执行的必须是同一个东西。
- 状态机 `pending → approved → executed/failed`（或 `pending → rejected`）带前置状态校验，一条提案不会被重复批准、也不会绕过批准直接执行。
- 每次状态流转追加一行 JSON 到 `AUDIT_LOG`，含提案 ID、工具、完整参数、审核人、时间、执行结果。
- **失败方向朝安全那边倒**：审核环节报错、Ctrl-C 中断、stdin 断掉（管道喂完、终端断了），全部当作「未批准」。「没人在场」不等于「有人敲了回车」——否则挂在 cron 里跑一次就是无人值守执行。菜单也不读 REPL 那个 `bufio.Scanner`，而是直接读 tty 原始字节：scanner 里可能攒着用户提前敲进去的内容，那些内容不该替人回答「要不要在客户设备上执行」。

不配 `Approver` 时闸门退回异步模式：只登记提案、返回 `pending_approval`，等外部调 `Execute`/`Reject`。钉钉那种「发互动卡片、等人点」的场景走这条路，`internal/*` 不用改。

### 风险等级

确认框里的风险等级由 `tools/risk.go` 对命令做启发式判断得出，**只是给审核人的提示，不是准入判断**——任何等级都仍然要人点头。

| 等级 | 含义 | 例子 |
| --- | --- | --- |
| 只读（绿） | 每一段都命中只读白名单 | `date`、`df -h`、`systemctl status nginx`、`ps aux \| grep x` |
| 未知（黄） | 认不出来。**绝大多数命令落在这里** | `onething-cli check`、`top`（交互模式） |
| 危险（红） | 命中删除/重启/改配置/装卸软件等特征 | `rm -rf`、`systemctl restart`、`ip addr add`、`sed -i` |

判断刻意保守：按 `|`、`&&`、`;` 等切段后逐段判，取最坏值；剥掉 `sudo` 和环境变量前缀、取 basename，防止 `sudo /bin/rm` 蒙混过去；出现 `$( )`、反引号、`>` 这类静态分析不了的结构一律判危险。之所以不做「低风险自动放行」，是因为这套判断说到底是字符串匹配，判错的代价是在客户设备上跑了不该跑的命令。

新增变更类工具时照着 `cmd/chat/main.go` 的 `registerTools` 写：先 `gate.Wrap`，再以 `RiskMutating` 注册。

### 超时与报错

`TOOL_TIMEOUT`（默认 60s）只圈住**批准之后真正下发的那一段**——人对着卡片想两分钟不该把命令的超时耗光。

OpenAI 兼容端点的空闲连接会在 30 秒后回收，避免长工具调用结束后复用网关已经关闭的 keep-alive 连接。模型请求如果在收到响应头之前遇到 EOF/网络错误，或收到 408、429、502、503、504，会以 500ms、1s 的退避最多重试两次；已经开始返回正文的流不会重试，避免重复输出或重复 tool call。

到点就把控制权还回来，不等底层调用收场：同步 HTTP 客户端未必理会 `ctx`，等下去就是整个终端跟着卡死。代价是那条命令可能还在节点上继续跑，所以超时的措辞一律是「结果未知」而不是「没执行」——`internal/tools/timeout_test.go` 里有一个压根不看 `ctx` 的假工具专门守这条。

失败原因会同时出现在三处，谁都不用去翻日志猜：

```
▶ 已批准（root），正在节点上执行…
✗ 执行失败：调用超时：等了 60s 仍未返回，命令可能还在节点上跑，请稍后自行核实结果
```

终端红字回执、审计日志的 `error` 字段、回喂给模型的 `error` 字段，用的是同一句话。`context deadline exceeded` 这类原文会先翻成人话再往外递——对着审核人弹英文原文等于没说。system prompt 另外要求模型碰到超时如实说明结果未知、改用只读命令核实，而不是把同一条命令再发一遍。

## 日志

两条流，刻意分开：

| | 内容 | 用途 |
| --- | --- | --- |
| `AUDIT_LOG` | 提案的每次状态流转（JSONL） | 「谁在什么时候批准了什么」的凭证，不受日志级别过滤 |
| `LOG_FILE` | 模型调用、工具耗时、错误（`log/slog` JSON） | 排障 |

运行日志默认落文件而不是终端——打在终端会把对话冲得看不清。要实时看就 `LOG_FILE=stderr`，或另开一个窗口 `tail -f diagnostic.log | jq`。工具调用的开始/结束通过 eino 的全局 callback 记录，带 `elapsed_ms`。

## 目录结构

```
cmd/chat/main.go          命令行入口（REPL + 流式输出）
cmd/chat/approver.go      终端确认框：展示命令与风险等级，等人拍板
cmd/chat/menu.go          方向键选择菜单（raw 模式按键处理）
internal/
  config/config.go        配置定义与校验
  config/dotenv.go        零依赖 .env 加载器
  logging/logging.go      运行日志 + eino 全局 callback
  llm/model.go            ChatModel 工厂 —— 换模型供应商只改这里
  agent/agent.go          ReAct agent 组装 + system prompt 注入 + 逐段流式输出 + tool call 流式适配
  agent/prompt.go         system prompt
  approval/proposal.go    提案定义与状态常量
  approval/store.go       审核状态机 + JSONL 审计日志
  tools/registry.go       工具注册表（风险分级，强制变更工具过闸门）
  tools/gate.go           人工审核闸门
  tools/risk.go           shell 命令风险启发式评估
  tools/builtin.go        内置只读工具（当前为 mock）
  tools/tunnel.go         节点命令执行（真实远程执行，变更类）
  session/session.go      多轮对话历史管理与裁剪
```

## 几个设计约定

**工具按风险分级，边界在注册环节强制。** `tools.Registry` 注册工具时必须声明 `RiskReadOnly` 或 `RiskMutating`。只读工具（诊断采证）读不坏设备，放开让模型深挖；变更类工具必须先经 `Gate.Wrap`，否则注册直接报错。

**工具返回结论，不返回原始数据。** 见 `tools.DeviceStatus`：时序指标在 Go 层就聚合成「均值 / 环比 / 拐点 / 是否越线」，模型拿到的是可直接推理的结构化判断。不要把几百个采样点丢给模型——慢、费 token、还容易读错。

**mock 数据自报家门。** `DeviceStatus.DataSource` 固定返回 `mock`，system prompt 要求模型在回答里说明数据来源不可信。接真实数据源前，别让 mock 结论被当成真实诊断依据往下传。

**工具入参字段必须导出。** 未导出字段既不会出现在 `utils.InferTool` 生成的 JSON Schema 里、也无法被 `encoding/json` 反序列化——模型永远传不进值，最后会执行一条空命令。`tools/gate_test.go` 里有针对 `run_tunnel_cmd` 的 schema 断言防这个回归。

**流式 tool call 的坑。** Eino 默认的 `StreamToolCallChecker` 只看第一个 chunk，而部分模型或兼容网关会先吐文本、后吐 tool call，导致误判为「没有工具调用」。`internal/agent/agent.go` 里的 `streamToolCallChecker` 会一直读到出现 tool call 或流结束为止，对 OpenAI 和 Claude 两种协议都适用。

**流式输出接在组件回调上，不是接在返回流上。** react agent 只把最后一次模型输出交给调用方，中间那几次（「我先查一下节点时间」）全被内部消费掉了。`streamPrinter` 挂在 `OnEndWithStreamOutputFn` 上抓每一次 ChatModel 输出，并且**同步**读完再返回——这样审核框和执行回执一定排在那段文字之后打印，不会插进句子中间。拿到的是 eino 给回调复制的独立副本（链表缓冲、chunk 由组件在另一个 goroutine 里产出），读它既抢不走下游的数据，也不会把下游卡住。写进对话历史的仍然是最终输出流里的文本。

**历史只存最终消息。** `session` 只累积 user 和 assistant 的最终消息，不含 agent 内部的 tool call 往返，因此按轮数裁剪时不会切出孤立的 tool message。system prompt 由 `agent` 层在每次调用时注入（`withSystemPrompt`），永远在最前面，也不会被历史裁剪掉。

**确认卡在工具调用里，而不是卡在轮次之间。** 人工确认发生在 `InvokableRun` 内部，模型这一轮还没结束——它拿到的直接就是执行结果或驳回理由。REPL 和确认框共用同一个 stdin，安全：模型跑的时候 REPL 正阻塞在等流式输出，同一时刻只有一个读者。

## 测试

```bash
go test ./...
```

`internal/approval` 和 `internal/tools` 的用例覆盖了审核闸门的核心不变量：未确认时真实实现执行 0 次、每次调用都单独问一次（不存在「批过一次就一直放行」）、审核环节报错时默认不执行、未批准不能进入执行态、同一提案不会被执行两次、驳回后不可执行、漏包装的变更工具无法注册。`tools/timeout_test.go` 另外守着超时那条线：卡死的工具必须在超时后交还控制权、失败原因里要出现「调用超时」、以及审核等待的时间不计入执行超时。`tools/risk_test.go` 有约 45 条命令的分级用例。这些用例全程不连接任何真实节点。

（本机没有 gcc，`go test -race` 跑不起来；并发用例目前只在无 race detector 下验证。）

## 换模型供应商

通过 `LLM_PROVIDER=openai` 使用 OpenAI 兼容的 `/v1/chat/completions`，通过 `LLM_PROVIDER=claude` 回退 Anthropic `/v1/messages`。两种实现都封装在 `internal/llm/model.go`，接口都是 `model.ToolCallingChatModel`，上层不用动。

## 下一步

按之前定的路线，骨架之上依次接：

1. **把数据表封装成参数化只读工具** —— 替换 `tools/builtin.go` 里的 mock，一类问题一个「体检式」聚合工具。不要用 text-to-SQL。
2. **入口分类 + 故障类型子图** —— 用 `compose.Graph` 替换单一 ReAct agent，做成「分类 → 并行采证 → 推理」，每类故障一条子图。目标是 2~3 次模型调用出首个结论。
3. **装机异常子图** —— Vision 抽取截图报错 → 阶段模型归类 → 打包源码检索定位失败脚本 → 结合代码上下文推理。设备登录不上，靠截图 + 源码。
4. ~~**变更工具 + 人工审核闸门**~~ —— 已完成（见上）。待补：提案落库以支持跨会话/异步审核，当前是内存实现，进程重启待审提案会丢。
5. **钉钉接入** —— Stream 模式（不需要公网回调地址）替换 REPL，把 `cmd/chat/approver.go` 换成互动卡片实现（照样实现 `tools.Approver` 接口即可），`internal/*` 全部可复用。

## 版本

- eino `v0.9.13`
- eino-ext/components/model/openai `v0.1.13`
- eino-ext/components/model/claude `v0.1.24`
- Go 1.24+
