# makez MVP 技术设计

> 首版本机实现已落地，实际范围、启动和验收记录见 [implementation-v0.1.md](implementation-v0.1.md)。本文仍保留完整 MVP 设计目标；尚未完成的能力以实现说明为准。
版本：0.1 · 2026-10-18 · 实施基线草案

首版交付 Agent 无关的托管执行闭环：开发者提交异步任务，平台创建隔离环境、启动已接入的 Agent、保存进度和成果，并完成取消、故障恢复与资源清理。平台使用 Go 与 Temporal；沙箱通过内部 `SandboxProvider` 能力接口接入，首个实现为 OpenSandbox 的原生 API 与 Docker 后端。E2B 保留为后续适配方式。

本设计定义模块、接口语义和状态转换。历史接口草案见 [design/contracts.go](design/contracts.go)；当前运行代码以 [internal/core/contracts.go](../internal/core/contracts.go) 为准，已包含 SecretResolver、SecretInjection 和临时 SecretValues。

## 1. 首版范围与约束

| 项目 | MVP 决定 |
| --- | --- |
| 使用方式 | HTTP 提交任务，查询状态，以 SSE 获取可重放事件，下载成果 |
| 执行模型 | 一项 Task 最多产生一个业务 Run；一次 Run 使用专属沙箱 |
| Agent 接入 | 注册并固定版本的 CLI AgentAdapter；Agent 使用自己的推理与工具循环 |
| 沙箱接入 | OpenSandbox 原生生命周期与 execd API；本机 Docker 验证 |
| 编排 | 一个 TaskWorkflow 编排生命周期；CleanupWorkflow 处理延期清理 |
| 元数据 | PostgreSQL，保存 Task、Run、资源关联、事件与 Outbox |
| 成果 | 本机持久目录，按 workspace/task/run 分区；通过 ArtifactStore 模块访问 |
| 部署 | 一个 API 进程、一个 Worker 进程、Temporal、PostgreSQL、OpenSandbox 与 Docker |
| 身份 | 工作空间级 API Key；所有查询和成果访问都校验所属工作空间 |

首版不包含交互式终端、人工审批、多 Agent 协作、自动重新执行整个 Agent、跨后端迁移、快照恢复、Kubernetes 部署、自建算力调度器或面向第三方 SDK 的 E2B 网关。暂停和恢复不作为首版接口承诺。

首个真实 CLI 已接入 Codex（`codex@1`），通过工作区级 SecretRef 注入凭据，已使用当前 ChatGPT 登录的受限副本完成本机端到端验收。开发阶段继续使用可控测试 Agent 注入故障；真实模型执行与测试 Agent 验证分别保留。

相对上一版概览，明确三个实施决策：

1. `202 Accepted` 表示 Task 和启动意图已经可靠落库，Temporal 可以稍后受理；用 Outbox 消除数据库与 Temporal 双写缺口。
2. Activity 重试沿用 Run；MVP 不自动创建第二个 Run。用户重试创建新 Task，并记录 `retry_of_task_id`。
3. 执行结果与清理状态分离。正常路径先归档、清理，再发布结果；清理失败时可靠移交后台流程，结果附带清理状态。

## 2. 模块划分与依赖

下列目录为建议实现结构；当前只有设计文档和接口草案。

| 模块 | 职责 | 边界 |
| --- | --- | --- |
| `cmd/api`、`internal/api` | 鉴权、参数校验、提交、取消、查询、SSE、成果下载 | 不直接创建沙箱或执行 Agent |
| `internal/task` | Task/Run 事务、状态转换、幂等键、取消与完成竞争规则 | 不依赖具体沙箱 SDK |
| `internal/store` | PostgreSQL 查询、Outbox、事件游标、操作意图 | 业务状态唯一持久化入口 |
| `internal/dispatch` | 投递 START_TASK、CANCEL_TASK、START_CLEANUP 到 Temporal | API 进程后台协程；只保证命令送达，不调度实际执行步骤 |
| `internal/workflow` | TaskWorkflow、CleanupWorkflow、顺序、分支、定时器与取消 | 只使用可重放的 Workflow 逻辑，I/O 放入 Activity |
| `internal/activity` | 准备、创建、启动、等待、归档、停止、清理、发布状态 | 调用领域模块、两类适配器和存储 |
| `internal/sandbox` | SandboxProvider、后端注册表、OpenSandbox 实现 | 后端 ID、SDK 对象和协议差异不泄漏到 Workflow |
| `internal/agent` | AgentAdapter 注册、输入转启动计划、事件解析、结果解析 | 不直接操作 Temporal、沙箱生命周期或宿主机 |
| `internal/artifact` | 路径校验、校验和、原子归档、授权读取 | 成果存储独立于沙箱寿命 |
| `internal/reconcile` | 对账未确认操作、未投递命令、异常结束 Workflow、遗留资源 | Worker 后台协程；发现问题后启动/推动恢复流程，不重新实现任务队列 |

Workflow 和 Activity 首版在同一个 Go Worker 进程注册，仍是不同执行边界。所有后端副作用由 Activity 执行。对账器只读资源并提交恢复意图，实际停止、销毁等操作交给 Activity。

Temporal 的历史存储与 makez 业务库逻辑分开。业务查询读取 PostgreSQL；Temporal 是流程执行记录，不替代 Task 列表、日志和成果库。完整提示词、日志、文件和凭据不进入 Workflow 参数或返回值，只传引用、ID、小型摘要和结果码。

本机 Temporal 必须使用可保留的持久存储配置，不能用“服务重启后历史消失”的配置通过恢复验收。ArtifactStore 本机目录必须位于沙箱之外，并与 Worker 使用同一持久挂载；跨机器部署时再替换存储实现。

## 3. 核心对象与持久化

| 对象 | 关键字段与约束 |
| --- | --- |
| Task | `id, workspace_id, idempotency_key, request_hash, input_ref, agent_id/version, runtime_profile_id/version, state, reason_code, deadline_at, cancel_requested_at, run_id, version, retry_of_task_id` |
| Run | `id, task_id, state, backend_id, adapter_version, runtime_profile_version, sandbox_ref, process_ref, output_cursor, parser_checkpoint, observer_epoch, result_ref, execution_verified_stopped, cleanup_state, version` |
| OperationIntent | `run_id, step, request_hash, phase, backend_id, external_ref, last_error, updated_at`；唯一 `(run_id, step)` |
| Outbox | `id, task_id/run_id, command, payload_ref, attempts, next_attempt_at, delivered_at`；每个逻辑命令唯一 |
| Event | `run_id, seq, source_offset, item_index, type, payload`；唯一 `(run_id, source_offset, item_index)` |
| Artifact | `run_id, logical_path, storage_key, size, sha256, media_type, required, status` |

- Task 表示用户提交的工作意图；Run 表示实际执行。排队取消的 Task 可以没有 Run。MVP 对 `runs.task_id` 加唯一约束。
- Temporal Workflow ID 为 `makez/task/{workspace_id}/{task_id}`。Temporal Run ID 是 Temporal 自己的执行标识，与业务 Run ID 不同。[标识说明](https://docs.temporal.io/workflow-execution/workflowid-runid)
- Task、Run 的终态不可回退；`cleanup_state` 可以在终态之后继续更新。
- 每个 Run 固定后端、适配器和运行配置版本；恢复期间不重新按“默认配置”选择后端。
- 状态写入使用事务与 `version` 条件更新；迟到 Activity 不能把终态改回 RUNNING。
- 新的 WaitAgent 尝试递增 `observer_epoch`。旧监听器提交的事件与游标必须带 epoch，不匹配则拒绝。这个机制只防止旧观察者写业务数据，不能代替后端操作幂等。

OperationIntent 的阶段为 `PREPARED → DISPATCHING → CONFIRMED`，异常时进入 `UNCERTAIN` 或明确的 `REJECTED`。必须先保存意图再发送创建/启动请求。重试时沿用逻辑操作键，不把 Temporal Activity attempt 编号放入键中。固定 step 为 `sandbox` 和 `agent`：Inspect/Delete 使用原创建键，Process/Stop 使用原启动键；清理不能换一个新键查找资源。

## 4. SandboxProvider 契约

这是 makez 内部 Go 接口。完整类型见 [SandboxProvider](design/contracts.go)，核心签名如下：

```go
type SandboxProvider interface {
    BackendID() string
    Capabilities(ctx context.Context, profileID, profileVersion string) (SandboxCapabilities, error)
    Ensure(ctx context.Context, request SandboxRequest) (SandboxRef, error)
    Inspect(ctx context.Context, key OperationKey) (SandboxObservation, error)
    Renew(ctx context.Context, sandbox SandboxRef, expiresAt time.Time) error
    Delete(ctx context.Context, key OperationKey) error

    Start(ctx context.Context, sandbox SandboxRef, request StartRequest) (ProcessRef, error)
    Process(ctx context.Context, sandbox SandboxRef, key OperationKey) (ProcessObservation, error)
    Output(ctx context.Context, process ProcessRef, cursor string, maxBytes int) (OutputPage, error)
    Stop(ctx context.Context, sandbox SandboxRef, key OperationKey, policy StopPolicy) error

    ReadFile(ctx context.Context, sandbox SandboxRef, path string) (io.ReadCloser, error)
    WriteFile(ctx context.Context, sandbox SandboxRef, path string, contents io.Reader, size int64) error
}
```

### 4.1 必须实现的语义

| 方法 | 成功条件与重试规则 |
| --- | --- |
| `Ensure` | 同一键、同一请求返回同一已确认资源；同键不同内容返回 CONFLICT。创建结果未知时不发出第二次创建请求，转 Inspect/对账 |
| `Inspect` | 按归属标签和操作键查询；READY 必须只有一个匹配资源。返回多个匹配项时停止启动 Agent，并登记所有资源待清理 |
| `Renew` | 只延长已确认资源的有效期，不超过 Run 的资源保留上限；重复调用可安全执行 |
| `Delete` | 只删除该操作键拥有的资源；只有确认全部已删除才返回成功。异步删除必须继续查询；重复删除已确认不存在的资源视为成功 |
| `Start` | 同一逻辑启动键只能产生一次 Agent 启动；返回稳定执行凭据。请求断开不等于 Agent 未启动 |
| `Process` | 通过启动键恢复执行凭据与退出结果；裸 PID 不足以防止 PID 重用。NOT_STARTED 必须是权威结论，查不到记录不能冒充它 |
| `Output` | 游标可重放、偏移稳定、批次有大小上限；EOF 表示进程已退出且输出已读完。日志缺失返回 OUTPUT_GAP，不伪造连续数据 |
| `Stop` | 先持久关闭启动键，阻止迟到 Start，再优雅/强制停止整个进程组。只有键已关闭且进程已停止才成功；无法确认则尝试删除专属沙箱 |
| 文件方法 | 路径限定在 Run 工作目录；拒绝路径穿越、符号链接逃逸和超限写入。输入写入采用临时文件加原子替换 |

取消一个方法的 `context` 只结束这次控制请求，不隐含“远端进程已经停止”。停止进程必须显式调用 Stop；必要时升级为删除沙箱，并验证结果。

### 4.2 能力与运行配置

MVP 接入门槛包括后台进程、按操作键查找、可重放输出、进程停止、文件操作和有限资源寿命。后端未原生提供时，由该适配器补齐并通过一致性测试；补不齐就不能承担需要恢复保证的任务。

`runtime_profile_id + version` 映射到后端镜像或模板、资源限制、工作目录和网络策略。映射后的镜像摘要/模板版本随 Run 冻结。若任务要求网络白名单而后端无法实现，受理时拒绝，不降级为任意出网。

`SandboxRef` 保存 `backend_id + resource_id + operation_key`；`ProcessRef` 保存沙箱引用与稳定执行凭据。它们不包含管理凭据或长期访问令牌。

### 4.3 OpenSandbox 首版适配

OpenSandbox 提供生命周期 API、命令执行、进程状态与日志读取、文件访问；具体能力以选定版本的原生 API 为准。[官方架构](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/architecture/index.md)

首版使用原生 API，不为接入 OpenSandbox 额外搭建 E2B 网关。创建时附加不可混淆的安装实例、workspace、Task、Run 和操作键标签；创建成功先保存资源引用，再允许启动 Agent。固定镜像的入口只启动控制组件，不自动运行 Agent。

进程幂等使用沙箱镜像内的最小启动辅助程序：

1. 以启动键加锁，核对请求摘要，在受保护的控制目录写入启动意图。
2. 启动唯一子进程组，保存执行凭据、输出文件和退出记录；重复调用只读取已有凭据。
3. 崩溃发生在“已写意图、尚未确认子进程”的窗口时返回 UNKNOWN，不重新启动。Stop 在同一启动锁下写入持久取消墓碑；后续同键 Start 必须拒绝，Process 返回 STOPPED 时表示启动键关闭且无存活进程。
4. 根据绝对执行期限终止进程组；Agent 以受限用户运行，不能修改启动记录。辅助程序只管理进程与回执，不实现 Agent 循环。

原生 API 若经验证提供同等幂等与恢复保证，可以替代辅助程序，但不能仅凭方法名称相似就删除这些保证。创建操作暂不承诺严格一次：网络结果未知时禁止盲目重建，使用标签发现、有限 TTL 与遗留资源清理控制后果。对这类请求关闭 SDK/HTTP 客户端内部的自动重发，重试权统一交给显式对账逻辑。

## 5. AgentAdapter 契约

```go
type AgentAdapter interface {
    Identity() AdapterIdentity
    Requirements() []Capability
    Plan(input AgentInput) (AgentPlan, error)
    Decode(page OutputPage, previousState []byte) (DecodedOutput, error)
    Collect(ctx context.Context, workspace WorkspaceReader, exit ProcessExit) (AgentResult, error)
}
```

| 方法 | 职责 |
| --- | --- |
| `Identity` | 返回固定的 Agent 类型与适配版本 |
| `Requirements` | 声明运行所需沙箱能力，在创建资源前验证 |
| `Plan` | 把任务转为 argv、非敏感环境变量、输入文件、停止策略、必须产出的文件路径 |
| `Decode` | 将输出解析为统一事件，返回有界的解析检查点，支持跨批次 JSONL/UTF-8 分片 |
| `Collect` | 在进程结束后只读工作目录，校验该 Agent 的结果格式，返回候选成果和语义结果 |

Plan 与 Decode 对同一输入和版本应产生确定结果。首版进程退出码非零、结果格式错误或必需成果缺失，均不能判定成功。Agent 声称完成不等于平台已归档成功。主进程退出也不代表所有子进程停止：Finalize 先关闭启动键并确认整个进程组静止，必要时执行 Stop，再排空输出和读取结果。

Adapter 不负责申请沙箱、重试、写数据库、上传成果或决定业务终态。优雅停止信号由 Plan 的 StopPolicy 声明，实际停止由平台与 Provider 执行。MVP 不提供 Adapter 的 restart/resume 方法；仅支持接续仍存活的进程。

输入提示词和 Plan 内容仅在 Activity 内处理，保存为受控引用；Workflow 只获取 plan/result 引用。外部凭据通过 SecretRef 在启动时解析，不能进入 Plan 的 Env、事件、历史或成果。来自用户的文本作为数据文件传入，不拼接为 shell 指令。

事件使用 `log / progress / tool / result_hint` 等显示类型，不能作为平台控制命令。事件的稳定来源键为 `run_id + source_offset + item_index`。数据库在同一事务中写事件、解析检查点与 NextCursor；重试重复读取不会重复展示。解析缓存和单条事件均设上限，超长内容放入日志文件并保留截断标记。

## 6. HTTP API 与可靠受理

| API | 行为 |
| --- | --- |
| `POST /v1/tasks` | 必须携带 Idempotency-Key；提交 agent、输入引用、运行配置与期限；新任务返回 202、Task ID 与 QUEUED |
| `GET /v1/tasks/{id}` | 返回 Task、当前 Run、reason、成果引用、cleanup_state 与停止确认标志 |
| `POST /v1/tasks/{id}/cancel` | 幂等请求取消；返回当前状态；终态不改写 |
| `GET /v1/tasks/{id}/events` | SSE，以数据库事件 seq 支持 Last-Event-ID 重放；断线不影响执行 |
| `GET /v1/tasks/{id}/artifacts` | 返回已发布成果清单 |
| `GET /v1/artifacts/{id}/content` | 校验归属后读取成果；不向调用方暴露沙箱管理地址 |

同工作空间、同幂等键、相同规范化请求返回已有 Task；不同请求内容返回 409。请求 hash 包含 Agent/运行配置的固定版本、输入内容引用与执行约束。数据库故障时不能返回“已受理”。

受理事务一次写入 QUEUED Task 和 START_TASK Outbox。Dispatcher 使用稳定 Workflow ID 投递，遇到响应丢失时按该 ID 查证或重发；配置不复用已有 Workflow ID，并保留数据库去重记录，不依赖 Temporal 历史保留期限来做业务去重。

Outbox 采用重试退避，不承载 Agent 执行步骤。Temporal 不可用时任务可继续排队，查询显示 dispatch_error；超过排队期限的任务由对账/投递流程终结。服务不可用期间状态更新时间可能延迟，但实际执行前必须重新验证绝对期限。

## 7. Task 状态机

| 状态 | 含义 |
| --- | --- |
| QUEUED | 已可靠受理，尚未为它创建业务 Run |
| RUNNING | Run 已建立，环境准备、执行或归档进行中 |
| CANCELING | 取消意图已落库，正在确认停止与收尾 |
| SUCCEEDED | Agent 结果成功且必需成果已可靠发布 |
| FAILED | 无法完成；通过 reason_code 区分原因 |
| CANCELED | 取消生效，确认没有仍在运行或可能迟到启动的 Agent |

| 起点 | 事件与条件 | 终点 |
| --- | --- | --- |
| QUEUED | InitializeRun 事务成功，未取消且未过期 | RUNNING |
| QUEUED | 取消先于 InitializeRun 提交，尚无 Run | CANCELED |
| QUEUED | 排队超时、配置不再可用或确定的派发失败 | FAILED |
| RUNNING | 成功归档并提交终态事务 | SUCCEEDED |
| RUNNING | 执行/结果/归档失败或期限已到 | FAILED |
| RUNNING | 取消请求事务成功 | CANCELING |
| CANCELING | 确认未启动或已完全停止，收尾完成 | CANCELED |
| CANCELING | 限时恢复后仍无法确认进程已停止 | FAILED，reason=STOP_UNCONFIRMED |

API 取消与 InitializeRun、PublishOutcome 都锁定同一 Task 行并使用条件更新。QUEUED 取消后，即使已有启动请求在网络中，迟到的 Workflow 也必须先读取 Task；看到 CANCELED 就退出，不申请任何资源。

取消与完成竞争时，以业务事务提交顺序裁定：终态先提交则取消返回该终态；取消先提交，则最终不能发布 SUCCEEDED，已退出的成功进程也按取消收尾，并保留原始退出结果供审计。

超时使用 FAILED + DEADLINE_EXCEEDED，MVP 不额外增加 TIMED_OUT 状态。Task 的业务状态不与 Temporal 的运行状态一一对应：例如 Agent 业务失败可以由一个正常完成的 Workflow 返回 FAILED 结果。

[Task 交互状态图](../diagrams/makez-task-state.html)

## 8. Run 状态机与清理状态

| 状态 | 含义 |
| --- | --- |
| PREPARING | 固定配置、申请/核对沙箱、准备输入与工作目录 |
| STARTING | 已登记启动意图，正在取得唯一进程凭据 |
| RUNNING | 进程已确认，持续观察输出、状态与期限 |
| STOPPING | 取消、超时或失败收尾，确认停止；未启动时此步骤可直接通过 |
| FINALIZING | 排空输出、解析结果、归档、尝试清理、发布业务终态 |
| SUCCEEDED / FAILED / CANCELED | 业务执行终态；含结果码与停止确认信息 |

合法转换：

- 正常：`PREPARING → STARTING → RUNNING → FINALIZING → SUCCEEDED`。
- 主进程正常退出但结果或归档错误：`RUNNING → FINALIZING → FAILED`。
- 准备、启动、运行阶段的取消或异常：`PREPARING / STARTING / RUNNING → STOPPING → FINALIZING → CANCELED 或 FAILED`。
- FINALIZING 只能进入终态。迟到取消通过最终结果裁定处理，不倒退阶段。
- 网络中断和 Activity 重试本身不改变 Run 状态；恢复进度记录在事件与 last_error 中。

[Run 交互状态图](../diagrams/makez-run-state.html)

清理状态独立保存：

| cleanup_state | 含义 |
| --- | --- |
| NOT_REQUIRED | 已确认从未分配资源，且无未决创建请求 |
| PENDING | 已知或可能存在资源，正在清理或已可靠移交 CleanupWorkflow |
| DONE | 确认所有归属资源消失，所有未决操作已完成对账 |
| BLOCKED | 清理预算耗尽仍不能确认，需要告警与人工处理 |

Task 成功可以附带 cleanup=PENDING，但必须已经确认 Agent 停止、成果发布完成，且延期清理意图已可靠保存。对停止结果未知的 Run，不能发布 SUCCEEDED 或 CANCELED；发布 FAILED + STOP_UNCONFIRMED，并保留 `execution_verified_stopped=false`。只要启动意图曾进入 DISPATCHING，就必须确认启动键已关闭或整个沙箱已删除；当前查无进程不构成停止证明。此时 Finalize 不读取仍可能变化的目录来判定成功，只保留已持久化的事件与成果并安排清理。

终态结果、Artifact 清单、Task/Run 更新以及必要的 START_CLEANUP Outbox 在同一事务提交。后续清理状态变化不会覆写已确认的执行结果。

## 9. Temporal 执行流程

| 步骤 | Activity 与行为 |
| --- | --- |
| 1 | InitializeRun：事务验证 Task、取消与期限，创建唯一 Run，冻结版本 |
| 2 | PreparePlan：解析输入、生成并保存 AgentPlan，返回引用 |
| 3 | EnsureSandbox：先登记创建意图，创建/核对环境，保存资源句柄 |
| 4 | PrepareWorkspace：幂等写输入文件和已授权资产 |
| 5 | StartAgent：先登记启动意图，取得稳定进程凭据并保存 |
| 6 | WaitAgent：按游标读取输出、保存事件、检查进程、独立发送心跳和维护 TTL |
| 7 | Finalize：关闭启动键/确认整个进程组停止，排空日志、CollectResult、ArchiveArtifacts、尝试 CleanupSandbox、PublishOutcome |

流程在启动时建立绝对业务期限的 Workflow Timer；到期取消执行 Activity，进入 StopAgent 与 Finalize。不能用与业务期限相同的 Workflow Execution Timeout 直接截断整个流程，否则没有清理时间。

取消走 Outbox → CancelWorkflow。长 Activity 配置 HeartbeatTimeout 并响应取消；流程使用独立、限时的取消清理上下文执行停止和归档。不能依赖 Worker 本地 defer 保证释放外部资源。[Temporal 取消机制](https://docs.temporal.io/develop/go/workflows/cancellation)

WaitAgent 以主进程退出作为阶段结束信号；不等待后台子进程自行关闭输出，Finalize 停止整个进程组后再排空日志。StartAgent 与 WaitAgent 必须分离。已确认完成的 Activity 结果可由历史复用；未确认的 Activity 可能重新执行。Activity 从检查点接续需要业务代码主动实现。[Activity 语义](https://docs.temporal.io/activities)

### 9.1 重试分类

| 操作 | Temporal 策略 | 业务条件 |
| --- | --- | --- |
| 创建沙箱、启动 Agent | 初次副作用调用不盲目自动重试；MaximumAttempts=1 | 超时转对账，只在确认已有结果或权威未执行时继续 |
| 查询、监听 | 在总期限内退避重试 | 使用固定句柄和已提交游标；不能因此新建 Run |
| 写输入、归档 | 有界重试 | 固定路径/内容摘要，临时文件加原子提交 |
| 停止、删除 | 有界重试 | 始终针对原 Run，且后端满足幂等停止/删除 |
| 发布状态 | 幂等重试 | 唯一键和 version 防止重复提交、终态倒退 |
| 整个 TaskWorkflow | 不配置自动 Workflow 重跑 | 不用“重跑整个流程”掩盖副作用状态未知 |

错误码 `INVALID_INPUT / UNSUPPORTED_CAPABILITY / PERMISSION_DENIED / CONFLICT` 为非重试错误。只读请求的 TRANSIENT 可以重试；副作用调用在超时或响应丢失时必须按 OUTCOME_UNKNOWN 处理，而不是仅根据 HTTP 5xx 决定重发。

### 9.2 MVP 初始时间预算

以下为配置初值，联调后可调整，不能散落为各层硬编码。

| 参数 | 初值与限制 |
| --- | --- |
| 排队预算 | 10 分钟，执行前重新检查 |
| Task 绝对执行期限 | 默认提交后 60 分钟 |
| Activity 心跳 | 约 10 秒；HeartbeatTimeout 60 秒 |
| 沙箱 TTL | 5 分钟；活动期间约每 60 秒续期 |
| 资源绝对保留上限 | Task deadline + 15 分钟；任何旧 Worker 都不能续过该上限 |
| 停止宽限 | 15 秒，随后强制终止；仍未知则尝试删除专属沙箱 |
| 成果归档预算 | 最多 10 分钟；不能超过资源保留上限 |
| 前台清理预算 | 2 分钟，失败可靠移交 CleanupWorkflow |
| 后台清理预算 | 30 分钟后仍无法验证则 BLOCKED 并告警 |

Temporal 心跳不会延长沙箱 TTL。Provider 启动请求也包含绝对执行期限；控制面中断时，沙箱内启动辅助程序和有限 TTL 提供有界停止保障。过期本身仍不替代删除确认。

## 10. 异常恢复与对账

| 场景 | 恢复动作 | 结果与禁止事项 |
| --- | --- | --- |
| API 在提交事务前崩溃 | 客户端以同一幂等键重试 | 未受理，不产生孤立 Task |
| 已落库但未启动 Workflow | Dispatcher 重投 START_TASK | 保留同一 Task 和 Workflow ID |
| StartWorkflow 响应丢失 | 按固定 Workflow ID 查证/重复投递 | 不创建新 Task 或新业务 Run |
| 创建沙箱响应丢失 | 按标签与操作键查询，确认唯一资源后绑定 | 查不到也不立即证明请求未执行；在对账窗口内只读重查 |
| 创建出多个归属资源 | 禁止启动任何 Agent，登记所有资源清理 | FAILED + RESOURCE_AMBIGUOUS |
| StartAgent 响应丢失 | 查询启动回执，恢复执行凭据 | 回执不明确时不启动第二个 Agent |
| Worker 在等待期间崩溃 | 重试 WaitAgent，递增观察 epoch，读取游标并查询进程 | 存活则接续，已退出则排空输出与归档 |
| 沙箱/进程确认丢失 | 保存已有事件与部分成果，终结 Run | FAILED + RESOURCE_LOST；MVP 不自动重跑 Agent |
| 日志游标过期或输出缺失 | 通过受控日志文件尝试补齐；无法补齐则报告缺口 | MVP 以 OUTPUT_GAP 失败，不把缺失事件当作完整结果 |
| Agent 非零退出/结果格式错误 | 归档诊断与可用成果，再清理 | FAILED + AGENT_EXIT_NONZERO / INVALID_RESULT |
| 归档中 Worker 崩溃 | 按内容摘要与临时对象重试发布 | 不重复产生可见成果，不先删除唯一副本 |
| 必需成果归档预算耗尽 | 记录 ARTIFACT_PERSIST_FAILED；保留已有持久成果，执行有界清理 | Task FAILED；到保留上限可删除未归档临时数据，明确记录损失 |
| 取消/期限已到 | 停止进程组、核对状态、保存可用结果、清理 | 确认停止才能 CANCELED；停止未知则 FAILED + STOP_UNCONFIRMED |
| 删除请求丢失或清理失败 | CleanupWorkflow 按固定键反复查询与删除 | 到预算后 BLOCKED；不因曾经调用 Delete 就报告 DONE |
| Temporal/数据库暂时不可用 | 停止推进新副作用；等恢复后对账 | 已运行进程可能继续；独立期限/TTL 防止无界运行 |
| Workflow 被强制终止或意外失败 | Reconciler 检查无活跃 Workflow 的非终态 Run，登记停止与清理工作 | 不自动重开已终止 TaskWorkflow；业务结果待停止核对后更新 |

[进程恢复交互流程图](../diagrams/makez-recovery-flow.html)

Reconciler 在启动时和周期扫描以下集合：未投递 Outbox、UNCERTAIN 操作、缺少活跃 Workflow 的非终态 Run、终态但清理未完成的 Run。它以已保存的操作键查资源，绝不能枚举后删除不属于本安装实例/工作空间的沙箱。只有明确查证 Workflow 已终结，或完成启动意图对账后确认其不存在，才能接管非终态 Run；Temporal 查询超时不表示 Workflow 已消失。

CleanupWorkflow ID 固定为 `makez/cleanup/{workspace_id}/{run_id}`。重复移交不会创建并行清理流程。没有资源引用但存在未决创建请求时，继续按键查找，直到确认后端请求已完成且无资源，或者报告 BLOCKED；有限 TTL 只是补充，不是“已经清理”的证据。对于失去 TaskWorkflow 的非终态 Run，CleanupWorkflow 还负责关闭启动键、停止并执行 PublishOutcome：确认取消则 CANCELED，否则 FAILED + WORKFLOW_INTERRUPTED/STOP_UNCONFIRMED；不会让业务状态永久停在 RUNNING。

取消不能撤销 Agent 已对外产生的副作用。用户重试时创建新的 Task；若带 `retry_of_task_id`，API 必须验证此前 Run 的执行已确认停止，并向用户暴露旧任务的失败/部分成果状态。禁止在旧执行仍不明时自动或隐式重试。

## 11. 成果一致性与安全边界

归档按 `workspace/task/run/logical_path + content_hash` 定位对象；先写临时文件，校验大小与摘要，完成持久化提交后再登记为可见。清单完整发布后才能判定成功。读取文件与写入归档均设置大小、路径和文件数限制，禁止归档宿主路径、控制目录、密钥文件或目录穿越链接。

数据库保存资源标识和 SecretRef，不保存沙箱管理密钥。Agent 只接收该任务所需的短期权限或受控代理访问能力。Docker socket、Temporal 凭据、数据库凭据与 Provider 管理凭据不能进入 Agent 环境。

API 每次查 Task、事件与成果都校验工作空间，不依赖 ID 难以猜测来隔离。MVP 固定管理员注册的 Agent 和运行镜像；调用方不能通过 argv、镜像或挂载参数任意取得宿主权限。本机 Docker 验证不等于完成面向不可信多租户的生产隔离验收。

## 12. 实施顺序与验收

| 阶段 | 交付与通过条件 |
| --- | --- |
| A. 契约与模型 | PostgreSQL 表、状态转换、API 幂等受理和 Outbox；进程在事务前后被杀均不丢已受理任务 |
| B. 执行闭环 | OpenSandboxProvider、启动辅助程序、测试 AgentAdapter；创建→执行→事件→成果→清理通过 |
| C. 持久编排 | TaskWorkflow、Activity、心跳、期限、取消、CleanupWorkflow；Worker 与 Temporal 重启后可恢复 |
| D. 真实 Agent | 至少一个真实 CLI 适配器；原生循环运行、成功/失败结果可靠识别、成果可下载 |
| E. 恢复验收 | 以下故障注入全部通过，未确认资源可在查询中看到并告警 |

必须覆盖的故障测试：

1. 同幂等键并发提交只产生一个 Task；同键不同请求返回 409。
2. Task 已提交、Temporal 不可用，恢复后只启动一个业务 Run；排队取消不启动 Agent。
3. 在创建/启动请求发出后断网或杀 Worker，恢复后没有第二个 Agent 执行；未知结果明确呈现。
4. WaitAgent 中杀 Worker，继续同一进程、游标与解析检查点；旧 epoch 写入被拒绝。
5. 取消与完成同时发生，按事务规则只得到一个终态；取消后的 Agent 不继续运行。
6. 沙箱被外部删除，Task 以 RESOURCE_LOST 结束，且不会自动重建并重新执行。
7. 归档中断后恢复，必需成果完整可读；不能出现 SUCCEEDED 但文件不存在。
8. 删除 API 不可用时成果仍可读，清理状态保持 PENDING/BLOCKED，恢复后变为 DONE。
9. 过期运行、迟到创建、旧 Worker 续期不能突破资源保留上限；外部副作用状态未知时不报告已停止。
10. 不同工作空间不能读取对方 Task/事件/成果；路径穿越和控制目录写入被拒绝。

MVP 完成标准：在本机持久部署中通过以上验收，接通真实 Agent，并能对任何失败任务回答“在哪一步失败、Agent 是否还在运行、成果在哪里、资源是否已经清理”。

## 13. 图与接口索引

- [架构总览](../diagrams/makez-architecture.html)
- [沙箱能力边界](../diagrams/makez-sandbox-providers.html)
- [Temporal 架构](../diagrams/makez-temporal-architecture.html)
- [任务执行时序](../diagrams/makez-task-lifecycle.html)
- [Task 状态机](../diagrams/makez-task-state.html)
- [Run 状态机](../diagrams/makez-run-state.html)
- [进程异常恢复](../diagrams/makez-recovery-flow.html)
- [Go 接口与类型草案](design/contracts.go)

交互图是本地生成文件；状态表、接口语义与恢复规则以本技术设计为准。
