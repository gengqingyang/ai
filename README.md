# CDN 智能诊断系统 · Eino 骨架

基于 [Eino](https://github.com/cloudwego/eino)（CloudWeGo，Go）搭的 agent 骨架。当前是一个可跑的命令行诊断助手，已打通「配置 → 模型 → 意图识别 → Skill middleware / 工具 → ADK agent → 人工审核 → 多轮对话」这条链路，后续按故障类型逐步长成完整诊断系统。

## 快速开始

```bash
# 1. 配置凭证：程序启动时会自动读取当前目录的 .env，不需要 source
vim .env

# 2. 跑起来
make run
```

交互命令：

| 命令 | 作用 |
| --- | --- |
| `/sessions` | 用 Bubble Tea 列表选择已有会话，也可直接新建 |
| `/new [名称]` | 新建一个空会话并立即切换 |
| `/switch <ID 或名称>` | 按短 ID 或唯一名称切换 |
| `/image <路径或 URL> [问题]` | 发送图片并提问，支持 PNG/JPEG/GIF/WebP |
| `/repo add <路径> [名称]` | 添加、索引并切换到本地代码仓库；含空格参数可加引号 |
| `/repos` | 列出已添加仓库和当前仓库 |
| `/repo use <名称>` | 切换当前代码仓库 |
| `/repo reindex` | 增量更新当前仓库索引 |
| `/approvals` | 列出还没了结的变更提案，含重启前挂起的那些 |
| `/approvals approve <ID>` | 批准并执行，然后接着跑完被挂起的那一轮诊断 |
| `/approvals reject <ID> [理由]` | 驳回，并把理由交回模型 |
| `/history` | 回看当前会话的对话历史 |
| `/reset` | 清空当前会话的内存及本地历史 |
| `/help` | 帮助 |
| `/exit` | 退出（Ctrl-C 同样退出） |

平时直接说话就行。模型要在设备上执行命令时，会当场把命令和风险等级摆出来等你确认：

```
你 > 帮我查下 SN001 这台机器的时间

需要在节点上执行 date 读取系统时间。这是只读查询，不改变机器状态。

审批 > 待人工确认
       提案  查看节点系统时间  (P001)
       风险  只读 - date 是只读查询命令
       节点  SN001

       $ date

       风险等级仅供参考，请以命令原文为准；回车默认选择执行，但不会自动确认。

执行吗？ ↑↓ 选择，回车确认（y 执行 / n 拒绝）
  ❯ 执行
    拒绝
▶ 已批准（root），正在节点上执行…
✓ 执行完毕，输出已回给模型

Thu Jul 30 14:22:03 CST 2026 —— 节点时间正常，与当前时间无偏移。
```

每轮进入诊断 Agent 前都会先显示结构化意图结果：

```
意图 > 业务不跑量 · 93%
助手 > 我先检查 SN001 的调度和流量指标……
```

## Skill 能力

主 Agent 使用 Eino ADK `ChatModelAgent`，并挂载官方 Skill middleware。启动时从 `AGENT_SKILLS_DIR` 加载 Skill；根目录的每个一级子目录可放一个 `SKILL.md`：

```text
skills/
  cdn-evidence/
    SKILL.md
```

`SKILL.md` 使用 YAML frontmatter 描述名称和适用场景，正文保存完整工作流：

```markdown
---
name: cdn-evidence
description: CDN 节点故障诊断的系统化采证流程。
---

# 完整 Skill 指令
...
```

模型平时只看到 Skill 的名称和描述；用户请求匹配时，模型调用只读的 `skill` 工具按需加载完整正文，实现渐进式披露。启动阶段会解析全部 Skill，并拒绝空名称、空描述、重名、非法 frontmatter 和当前未配置隔离边界的 `fork` / `fork_with_context` 模式。当前只支持 inline Skill；Skill 工具只读取本地指令，不进入变更工具注册表，也不能绕过节点命令的人工审批 Gate。

## 结构化只读采证

Agent 注册了五个使用固定命令模板的节点采证工具：

| 工具 | 证据范围 |
| --- | --- |
| `get_installation_evidence` | 系统就绪状态、失败服务、资源水位、装机相关进程 |
| `get_traffic_evidence` | 默认网卡的收发速率、包速率、错误和丢包增量 |
| `get_plugin_evidence` | 核心插件的 systemd 状态、退出码和可执行文件位置 |
| `get_kernel_evidence` | 当前内核、默认启动内核和已安装内核 |
| `get_network_evidence` | 接口、IPv4 地址、默认路由、DNS 和累计错误计数 |

这些工具只接受节点 ID；流量工具额外接受 1～5 秒采样窗口。模型不能传入 Shell，节点 ID 也不会拼接进命令。Go 层将输出解析成统一的 `EvidenceReport`，包含 `data_source`、`collected_at`、状态、摘要、证据项和局限说明，再交给模型推理。任意 `run_tunnel_cmd` 仍属于变更类工具，即使具体命令看起来只读也必须经过人工审批。

当前流量证据只代表节点默认网卡，不等于 CDN 业务请求量；装机证据是节点当前快照，不替代装机平台的历史阶段数据。工具会在结果中显式返回这些限制。

## 本地代码仓库问答

先添加一个仓库：

```text
/repo add /path/to/installer installer
/image ./install-error.png 帮我定位这个装机错误
```

代码问答提供 `list_files`、`search_code`、`read_file`、`find_symbol`、`find_references`、`get_definition` 和 `get_repository_revision` 七个只读工具。文件访问通过 Eino filesystem 协议的 `LocalRepoBackend`，写入和编辑固定拒绝，也不提供 Shell。Go 文件建立 AST 定义、引用和调用位置索引，其他 UTF-8 文本使用逐行检索。

安全扫描默认排除 `.git`、`.env`、私钥、日志、聊天历史及其会话目录、`.repositories.json`、`.approvals.json`、中断快照目录 `.checkpoints` 及 `.ckpt` 文件、二进制、生成目录、软链接和超过 2MB 的文件。工具只接受当前仓库内已进入索引的相对路径，越界路径和被排除文件无法读取。仓库目录只持久化规范根路径、Git commit、索引版本和更新时间，不保存源码正文。

入口使用 `compose.Graph` 把分类结果路由到装机、流量、插件、内核、配网、代码问答、澄清或其他分支。五类诊断各有独立 prompt；工具注册表同时记录风险和业务域，每个分支只绑定自己的工具白名单。纯代码分支只挂载上述七个工具，看不到 Tunnel 和设备工具。

装机分支采用“截图错误文本 → 源码定义/引用 → 可选设备证据”的顺序；设备 ID 不是源码诊断前置条件，设备无法连接时仍可给出带 `path:line` 的候选原因。输出要求把截图事实、源码事实和设备事实分开，版本无法核实时不得把候选写成已确认根因。可索引源码文件或 Git commit 变化后结果会标记 `stale=true`，并通过 `stale_reasons` 区分提交变化和工作区文件变化；运行 `/repo reindex` 后恢复。

`read_file` 单页最多返回 200 行。模型请求更大范围时工具会自动截取一页，并用 `has_more` 和 `next_start_line` 指示下一页；越过文件末尾返回结构化 `eof`，不会因为普通分页参数导致整轮对话失败。

## 意图识别

入口分类器使用独立的 report_intent 输出 schema，不执行任何工具。OpenAI 和 Claude 都通过强制 tool choice 返回同一份结构化结果，包含意图、置信度、摘要、证据、设备 ID、缺失信息和是否需要澄清。

稳定意图枚举如下，`compose.Graph` 的诊断分支直接复用这些值：

| 枚举 | 含义 |
| --- | --- |
| installation_failure | 装机异常 |
| traffic_anomaly | 业务不跑量 |
| plugin_failure | 插件异常 |
| kernel_upgrade_failure | 内核升级失败 |
| network_configuration_failure | 配网异常 |
| code_repository_question | 本地代码仓库问答 |
| other | 其他已明确问题 |
| unknown | 信息不足，无法可靠分类 |

分类请求只携带最近 6 条用户/助手消息；更早历史压缩到最多 1200 字符。最新用户消息保留原始多模态内容，因此截图也参与分类。unknown、置信度低于 0.6 或分类器主动标记信息不足时进入澄清；流量、插件、内核和配网分支缺少设备 ID 时也强制澄清。装机截图包含可搜索错误时不因缺设备 ID 阻塞源码诊断。澄清路径在模型调用层禁用全部工具，不会执行节点命令。

模型说的每一句都是边生成边打的，包括调工具之前那句「我先在 SN001 上执行 date」。ADK 会为每次模型输出产生事件，终端逐个消费事件中的消息流；中间说明会实时展示，只有最后一条不含 tool call 的 assistant 消息写进会话历史。

「提案」那行是**模型自己写的操作意图**（`run_tunnel_cmd` 的 `purpose` 参数），不是工具名——审核人关心的是这条命令要干什么。但它没有经过任何校验，所以永远和命令原文并排放：**判断以 `$` 那一行为准**。

执行记录不往对话里刷，全在 `audit.log` 里，要查就 `tail -f audit.log | jq`。

**光标默认停在「执行」**——确认多数时候都是放行，高频路径不该每次多按一下方向键。回车即下发到节点，输出直接回到模型手里，同一轮里继续推理；选「拒绝」后可以补一句理由，理由回喂给模型让它换方案。所以卡片把命令原文单独一行摊开：回车之前看清楚那一行。

整个终端只有一个常驻 Bubble Tea 程序。主输入、流式回复、意图、Agent 执行过程、模型返回的思考摘要、错误、会话列表、审批卡、驳回理由和执行回执都由同一个 `Model/Update/View` 管理；后台 Agent 只向 UI 消息通道投递事件，不直接读写终端。当前执行器已经是 Eino ADK `ChatModelAgent`，会展示流程阶段和工具开始/完成轨迹；供应商返回 `reasoning_content` 时会额外显示“思考”条目。模型内部隐藏推理不会被伪造或输出。

确认菜单支持 `↑↓`（或 `←→`、`j`/`k`）移动，回车确认，`y`/`n` 直接选择，`Esc`/`q` 安全取消审批；`Ctrl-C` 取消当前请求并退出程序。主输入按 Unicode 字符编辑，因此中文退格一次删除一个完整字符；支持 `↑/↓` 浏览当前会话已提交的输入，支持 `←/→`、`Home/End`、`Delete`、`Ctrl-W`、`Ctrl-U` 和 `Ctrl-K` 编辑。长内容可在默认交互模式中使用鼠标滚轮或 `PgUp/PgDn` 滚动查看。需要复制时按 `F2` 进入复制模式，直接用鼠标拖选文字并使用系统复制快捷键；按 `F2` 或 `Esc` 返回交互模式，恢复鼠标滚轮和滚动条拖拽。复制模式中仍可使用 `PgUp/PgDn` 滚动。请求处理期间输入栏保持显示但不可编辑。

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
| `LLM_MAX_TOKENS` | `4096` | 单次回复上限，不是上下文窗口 |
| `LLM_CONTEXT_TOKENS` | `1000000` | 模型完整上下文窗口 |
| `AGENT_MAX_STEP` | `12` | ADK ReAct 最大迭代数 |
| `AGENT_SKILLS_DIR` | `skills` | 本地 Skill 根目录；扫描一级子目录中的 `SKILL.md` |
| `AGENT_HISTORY_TURNS` | `0` | 历史轮数硬上限；`0` 表示不限 |
| `AGENT_HISTORY_TOKENS` | `900000` | 历史消息预算，给 system、工具和回复预留 100K |
| `AGENT_HISTORY_FILE` | `.chat_history.json` | 多会话索引；历史正文存入相邻的 `.chat_history_sessions/` |
| `AGENT_REPOSITORY_FILE` | `.repositories.json` | 命名代码仓库目录；只保存路径和索引元数据，不保存源码 |
| `AGENT_APPROVAL_FILE` | `.approvals.json` | 提案、原始参数、决定、执行权和结果的持久化状态文件 |
| `AGENT_CHECKPOINT_DIR` | `.checkpoints` | 中断快照目录（`0700`/`0600`），保存等待审核时那一轮的执行上下文；留空表示不落盘，中断无法跨重启恢复 |
| `AGENT_IMAGE_MAX_BYTES` | `20971520` | 单张本地图片最大字节数（默认 20MB） |
| `AGENT_IMAGE_DETAIL` | `auto` | 图片理解精度：`auto` / `low` / `high` |
| `TOOL_TIMEOUT` | `60s` | 单次工具执行超时（写 `60` 按秒算）；不含等人审核的时间 |
| `AUDIT_LOG` | `audit.log` | 审计日志路径，留空表示不落盘（生产不应留空） |
| `LOG_FILE` | `diagnostic.log` | 运行日志路径；填 `stderr` 时由 Bubble Tea 日志区展示，留空不记 |
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
- 状态机 `pending → approved → executing → executed/failed/unknown`（或 `pending → rejected`）带前置状态校验。进入 `executing` 必须先原子持久化抢占执行权，只有抢占成功的调用才可触发真实工具。
- `AGENT_APPROVAL_FILE` 使用版本化 JSON、`0600` 权限和原子替换保存提案、原始参数、幂等键及 checkpoint/interrupt 关联。替换前写盘失败会回滚内存；替换后目录同步失败会保留更严格的新状态并阻止后续动作，避免执行权退回。重启后待审和已批准提案仍可查询。
- 配了 `AGENT_CHECKPOINT_DIR` 时，等人确认的那段时间不占调用栈：变更工具把本轮挂成 Eino 中断并把执行上下文落盘，决定取得后再恢复同一轮，详见下面的「暂停与恢复」。
- 若进程在取得执行权后、结果落盘前退出，启动恢复会把 `executing` 保守转换为 `unknown`。此状态表示命令可能已经生效，是不可自动重试的终态；恢复调用也不能修改原审核人或原始参数。
- 每次状态流转追加一行 JSON 到 `AUDIT_LOG`，含提案 ID、工具、完整参数、审核人、时间、执行结果。
- **失败方向朝安全那边倒**：审核环节报错、Ctrl-C 中断或 Bubble Tea 界面关闭，全部当作「未批准」。`ui.Approver` 不读取 stdin，而是把提案送进根 Model 后同步等待明确决定；没有收到 UI 回传就绝不执行。

### 暂停与恢复

「等人点头」不再要求进程一直活着。`GatedTool` 登记提案后把本轮挂成 Eino `StatefulInterrupt`，Runner 把这一轮的执行上下文写进 `AGENT_CHECKPOINT_DIR`；`proposal_id`、`checkpoint_id`、`interrupt_id` 和挂起时所在的诊断分支一起存进提案状态文件——光有快照不知道该交给哪个分支跑。

```
模型这一轮 ──> GatedTool 登记提案 ──> 挂起 + 落快照
                                        │
                      进程可以在这里退出 │
                                        ↓
重启 ──> /approvals 看到带 * 的提案 ──> approve / reject 先原子落盘
                                        ↓
                  按提案记录的分支重建 Runner，Resume 同一份快照
                                        ↓
        被唤醒的变更工具回 Store 读决定和原始参数 ──> 真实输出 / 驳回理由
```

顺序是刻意的，也是这条链路的安全前提：**决定先落盘，再恢复**。恢复通道（`ResumeParams.Targets`）的值一律为 `nil`，不携带任何决定；被唤醒的工具回 `approval.Store` 读状态和原始参数。因此模型和恢复入口都无法从这条路径影响要不要在设备上动手，中途崩溃也不会把已经做出的批准弄丢——快照只表示「流程停在哪」，不表示「批没批」。同理，对还没有决定的提案调用恢复是安全的：工具会原样再挂起一次。

快照目录 `0700`、文件 `0600` 原子替换；`checkpoint_id` 只允许字母、数字、`-` 和 `_`，作为文件名 fail-closed。那一轮跑完或出错后，快照连同提案上的关联一起删除——它含本轮完整模型消息（可能包括截图 base64），没有待恢复的中断就不该继续留在盘上。`/approvals` 同时列出待审提案和「决定已落盘但那一轮还没接回来」的提案，后者正是重启后最容易丢的东西。

`AGENT_CHECKPOINT_DIR` 留空则退回同步审核：审批流程不变，但等待期间进程不能退出。不配 `Approver` 时闸门退回异步模式：只登记并持久化提案、返回 `pending_approval`，等外部管理程序调用 `Execute`/`Reject`；快照会保留，事后仍可接着跑完那一轮。

### 风险等级

确认框里的风险等级由 `tools/risk.go` 对命令做启发式判断得出，**只是给审核人的提示，不是准入判断**——任何等级都仍然要人点头。

| 等级 | 含义 | 例子 |
| --- | --- | --- |
| 只读（绿） | 每一段都命中只读白名单 | `date`、`df -h`、`systemctl status nginx`、`ps aux \| grep x` |
| 未知（黄） | 认不出来。**绝大多数命令落在这里** | `onething-cli check`、`top`（交互模式） |
| 危险（红） | 命中删除/重启/改配置/装卸软件等特征 | `rm -rf`、`systemctl restart`、`ip addr add`、`sed -i` |

判断刻意保守：按 `|`、`&&`、`;` 等切段后逐段判，取最坏值；剥掉 `sudo` 和环境变量前缀、取 basename，防止 `sudo /bin/rm` 蒙混过去；出现 `$( )`、反引号、`>` 这类静态分析不了的结构一律判危险。之所以不做「低风险自动放行」，是因为这套判断说到底是字符串匹配，判错的代价是在客户设备上跑了不该跑的命令。

新增变更类工具时照着 `internal/chat/chat.go` 的 `registerTools` 写：先 `gate.Wrap`，再以 `RiskMutating` 注册。

### 超时与报错

`TOOL_TIMEOUT`（默认 60s）只圈住**批准之后真正下发的那一段**——人对着卡片想两分钟不该把命令的超时耗光。

OpenAI 兼容端点的空闲连接会在 30 秒后回收，避免长工具调用结束后复用网关已经关闭的 keep-alive 连接。模型请求如果在收到响应头之前遇到 EOF/网络错误，或收到 408、429、502、503、504，会以 500ms、1s 的退避最多重试两次；已经开始返回正文的流不会重试，避免重复输出或重复 tool call。

到点就把控制权还回来，不等底层调用收场：同步 HTTP 客户端未必理会 `ctx`，等下去就是整个终端跟着卡死。代价是那条命令可能还在节点上继续跑，所以超时的措辞一律是「结果未知」而不是「没执行」——`test/tools_timeout_test.go` 里有一个压根不看 `ctx` 的假工具专门守这条。

失败原因会同时出现在三处，谁都不用去翻日志猜：

```
▶ 已批准（root），正在节点上执行…
✗ 执行结果未知：调用超时：等了 60s 仍未返回，命令可能还在节点上跑，请稍后自行核实结果；请核实实际状态，禁止自动重试。
```

终端红字回执、状态文件、审计日志的 `error` 字段和回喂给模型的 `error` 字段使用同一语义。`context deadline exceeded` 这类原文会先翻成人话再往外递。超时或执行期间取消统一进入 `unknown`；system prompt 要求模型改用只读证据核实，而不是重发同一条命令。

## 日志

两条流，刻意分开：

| | 内容 | 用途 |
| --- | --- | --- |
| `AUDIT_LOG` | 提案的每次状态流转（JSONL） | 「谁在什么时候批准了什么」的凭证，不受日志级别过滤 |
| `LOG_FILE` | 模型调用、工具耗时、错误（`log/slog` JSON） | 排障 |

运行日志默认落文件。配置 `LOG_FILE=stderr` 时不会绕过全屏界面直接写系统 stderr，而是作为 Bubble Tea 消息进入日志区；也可另开一个窗口 `tail -f diagnostic.log | jq`。工具调用的开始/结束通过 eino 的全局 callback 记录，带 `elapsed_ms`。

## 目录结构

```
cmd/chat/main.go          命令行薄入口
internal/
  application/run.go      配置、模型、工具、会话和 UI 的启动装配
  chat/chat.go            无终端依赖的对话/会话业务
  chat/image.go           图片命令解析 + 本地/远程图片多模态消息
  chat/approvals.go       /approvals 命令：落决定并接续被挂起的那一轮
  ui/model.go             根 Model 状态与 Init
  ui/banner.go            结构化启动信息的展示文案
  ui/update.go            根 Model 的事件和按键路由
  ui/view.go              根 Model 的 View 与布局渲染
  ui/conversation.go      提交命令和流式对话状态
  ui/approver.go          Gate 与 Bubble Tea 的审批消息桥
  ui/approval_update.go   根 Model 的审批状态更新
  ui/approval_view.go     审批卡内容渲染
  ui/approvals.go         /approvals 的列表渲染与恢复期流式输出
  ui/sessions.go          会话选择 UI
  ui/events.go            后台事件定义与消息通道
  ui/error_model.go       独立启动错误 Model
  ui/input/model.go       可嵌入的 Unicode 输入 Model
  ui/menu/model.go        可嵌入的选择菜单 Model
  config/config.go        配置定义与校验
  config/dotenv.go        零依赖 .env 加载器
  logging/logging.go      运行日志 + eino 全局 callback
  llm/model.go            ChatModel 工厂 —— 换模型供应商只改这里
  intent/                  结构化入口分类、稳定意图枚举与路由上下文
  repository/              安全仓库目录、Eino 只读 Backend、AST/文本索引和 Git 元数据
  skills/skills.go        只读本地 Skill 后端 + frontmatter 校验 + middleware 构造
  agent/agent.go          ADK ChatModelAgent 组装 + Skill handler + ADK 事件流消费
  agent/prompt.go         system prompt
  agent/pause.go          中断挂起、决定回查与挂起轮次的恢复
  approval/proposal.go    提案定义与状态常量
  approval/store.go       审核状态机 + JSONL 审计日志
  approval/checkpoint.go  Eino 中断快照的落盘存储（0700/0600、原子替换）
  tools/registry.go       工具注册表（风险分级，强制变更工具过闸门）
  tools/evidence.go       五类结构化只读采证工具及 Tunnel runner
  tools/evidence_commands.go 固定只读命令模板
  tools/evidence_parse.go 节点输出解析、聚合和状态判定
  tools/code.go           七个结构化只读代码工具
  tools/gate.go           人工审核闸门
  tools/risk.go           shell 命令风险启发式评估
  tools/tunnel.go         节点命令执行（真实远程执行，变更类）
  session/session.go      多轮对话历史管理与裁剪
  session/cache.go        对话历史 JSON 缓存与原子写入
  session/store.go        多会话索引、创建/切换及旧缓存迁移
skills/                   本地 SKILL.md 目录
test/                     全部 Go 测试用例（统一 package test）
```

## 几个设计约定

**工具按风险分级，边界在注册环节强制。** `tools.Registry` 注册工具时必须声明 `RiskReadOnly` 或 `RiskMutating`。只读工具（诊断采证）读不坏设备，放开让模型深挖；变更类工具必须先经 `Gate.Wrap`，否则注册直接报错。

**只读工具返回结论，不返回原始数据。** 后续接入数据源时，时序指标应在 Go 层聚合成「均值 / 环比 / 拐点 / 是否越线」，模型拿到可直接推理的结构化判断。不要把几百个采样点丢给模型——慢、费 token、还容易读错。

**只读节点工具不接受 Shell。** 五类采证工具只接收经过校验的节点 ID，并运行代码内固定的命令模板；流量采样秒数限制为 1～5。任意命令继续使用受 Gate 保护的 `run_tunnel_cmd`，不能因为某次命令被判断为只读就绕开人工审批。

**代码仓库只开放安全索引。** `LocalRepoBackend` 实现 Eino filesystem 协议，但 `Write` 和 `Edit` 固定失败，也没有 Shell 能力。所有文件查询都限定在当前命名仓库的索引内；软链接、凭证、日志、二进制和生成文件在扫描阶段即被排除。纯代码意图使用独立 Agent，只挂代码工具白名单。

**mock 数据必须自报家门。** 若开发期重新加入 mock 工具，返回值必须带 `data_source=mock`，system prompt 要求模型说明数据不可信，避免被当成真实诊断依据。

**工具入参字段必须导出。** 未导出字段既不会出现在 `utils.InferTool` 生成的 JSON Schema 里、也无法被 `encoding/json` 反序列化——模型永远传不进值，最后会执行一条空命令。`test/tools_gate_test.go` 里有针对 `run_tunnel_cmd` 的 schema 断言防这个回归。

**流式输出消费 ADK 事件。** `ChatModelAgent` 会把模型输出和工具结果都发成事件。`internal/agent/agent.go` 只消费 assistant 事件并通过回调上报文本，不接触终端；`internal/ui` 再把文本、审批和工具回执统一渲染。这样模型调用工具前的说明、审核卡和最终结论保持正确顺序，同时会话历史只记录最后一条不含 tool call 的 assistant 消息。

**终端交互只有一个 Bubble Tea 生命周期。** `internal/ui` 是独立 UI 包，根 `ui.Model` 从启动到退出一直持有终端；`ui/input.Model` 和 `ui/menu.Model` 只是嵌入式子模型。输入框接收 `KeyRunes`，光标、退格和 Delete 都以完整 Unicode 字符为单位。提交后只切换根 Model 的模式，不退出输入程序，也不会为审批或会话列表创建嵌套程序。`internal/chat` 不导入 Bubble Tea，启动装配集中在 `internal/application`。

**Skill 只走渐进式披露。** Skill middleware 把名称和描述放进 `skill` 工具 schema，完整正文只有模型明确调用后才进入上下文。文件后端限制在 `AGENT_SKILLS_DIR` 内且只开放读取；符号链接解析后也必须仍在根目录内。fork Skill 需要单独设计子 Agent 的工具集和审批继承策略，当前在启动阶段直接拒绝。

**1M 上下文按 token 预算管理。** `LLM_CONTEXT_TOKENS=1000000` 声明完整窗口，`AGENT_HISTORY_TOKENS=900000` 控制历史预算，剩余空间留给 system prompt、工具 schema 和模型回复。token 数按文本与图片保守估算；裁剪时只删除完整的最旧轮次，始终保留最近一轮。`LLM_MAX_TOKENS` 仍然只是单次回复上限，不能与上下文窗口混为一谈。

**意图分类是独立结构化边界。** 分类器只绑定不可执行的 report_intent schema，输出先经过枚举、置信度和字段校验，再作为“不是用户指令”的系统路由元数据交给下游。分类失败会终止本轮并保留原始错误；不会静默猜一个类别继续执行。低置信度和 unknown 直接绕开 ReAct，使用禁用工具的模型调用向用户澄清。

**历史按 session 隔离，并在重启后恢复。** `AGENT_HISTORY_FILE` 保存会话索引和最后选中的会话，各会话正文独立存入相邻的 `.chat_history_sessions/`，避免更新一个会话时重写其他会话的大段图片/base64 历史。输入 `/sessions` 可在根 UI 内选择，`/new [名称]` 创建空会话，`/switch <ID 或名称>` 直接切换；`/history` 和 `/reset` 只作用于当前会话。已有的单会话 `.chat_history.json` 会在首次启动时自动迁移，迁移后的正文成功落盘后才原子替换索引。

`session` 仍只累积 user 和 assistant 的最终消息，不含 agent 内部的 tool call 往返；图片消息的文本、URL 或 base64、MIME 和 detail 会一并缓存。索引和每个会话文件均以 `0600` 权限原子写入。全局 system prompt 由 ADK Agent 的 `Instruction` 注入，本轮意图路由元数据紧随其后，两者都不会进入持久化历史。

**图片输入在 OpenAI 和 Claude 间共用。** 输入 `/image ./error.png 帮我定位报错` 即可发送本地图片；路径含空格时可加引号，或写成 `/image /path/a b.png -- 帮我定位报错`。本地图片读取为原始 base64，Eino 的 OpenAI 适配器会转成 data URL，Claude 适配器会转成原始 base64 image block；HTTP/HTTPS URL 则直接传给模型。

**确认卡在工具调用里，而不是卡在轮次之间。** 人工确认发生在 `InvokableRun` 内部，模型这一轮还没结束——它拿到的直接就是执行结果或驳回理由。后台 `ui.Approver.Review` 等待根 Model 的回复；终端输入始终只有 Bubble Tea 一个读者。

## 测试

```bash
go test ./...
```

`test/intent_test.go` 覆盖强制结构化工具调用、稳定枚举校验、代码/装机路由、低置信度澄清、上下文限制和图片保留。`test/repository_test.go` 覆盖 AST 定义/引用、精确行号、增量重索引、版本过期、Eino 只读 Backend、路径穿越、密钥和软链接隔离；`test/code_tools_test.go` 覆盖七个代码工具的 Schema、结构化输出和只读注册。`test/evidence_tools_test.go` 使用 fake runner 覆盖五类节点采证。`test/approval_checkpoint_test.go` 覆盖快照的原子读写、删除、ID 字符集与目录权限；`test/tools_durable_approval_test.go` 覆盖问人之前先挂起、批准后带真实结果恢复、驳回理由回喂、审核报错按驳回处理、跨重启恢复同一轮和同一提案最多下发一次。UI、审批和工具测试继续覆盖 Bubble Tea 命令入口、Unicode 输入、会话、人工确认和变更工具强制包装。自动化测试不会连接真实模型或节点。

需要检查并发问题时运行 `go test -race ./test`。

## 换模型供应商

通过 `LLM_PROVIDER=openai` 使用 OpenAI 兼容的 `/v1/chat/completions`，通过 `LLM_PROVIDER=claude` 回退 Anthropic `/v1/messages`。两种实现都封装在 `internal/llm/model.go`，接口都是 `model.ToolCallingChatModel`，上层不用动。

## 下一步

完整范围、优先级与验收标准见 [`docs/requirements.md`](docs/requirements.md)。当前路线：

1. **外部审核鉴权** —— 提案持久化、原子执行权、`StatefulInterrupt` 暂停与 Runner Resume 均已完成；剩下的是给外部审核入口做身份校验，当前决定人直接取自 `OPERATOR`。
2. **真实异常诊断** —— 基于后续上传的真实装机异常及其他故障材料迭代诊断链路。
3. **质量与可观测性** —— 保留单元测试和静态检查，并记录分类、检索、模型和工具阶段指标。

## 版本

- eino `v0.9.13`
- eino-ext/components/model/openai `v0.1.13`
- eino-ext/components/model/claude `v0.1.24`
- Go 1.24+
