# Devin 平台 compactor 实施方案 v1

状态：R1（PR1–6）代码已实现；本地自动验证见第 9 节。默认关闭，真实模型质量验证通过后再灰度启用。日期：2026-10-23。代码基线：`d67081182`。依据：[机制分析与迁移设计](DEVIN_COMPACTION_MIGRATION.md)。本方案不依赖取得新版 CLI 二进制，也不把尚未验证的官方摘要模型或阈值写成平台要求。

目标：将现有 Fusion compactor 提取为公共组件，让普通 Devin 模型在显式托管会话中自动压缩，并提供平台自有的手动 compact。现有 Fusion 先保持行为等价；新会话逐步采用可恢复、可回查的存储。

## 1. 已确定的实现选择

| 项目 | 本次选择 |
| --- | --- |
| 压缩执行位置 | Sub2API 管理快照、调度、摘要和应用；摘要调用复用现有 Devin 普通推理链路 |
| 角色 | 公共核心接受 `main / lead / sidekick`；Fusion 编排仍由原 engine 负责 |
| 摘要模型 | `same_lane`，使用该 lane 已授权的具体模型；独立 override 后续再做 |
| 摘要格式 | 复用当前结构化历史摘要字段；新策略增加由服务端管理的来源引用，摘要不成为 system 指令 |
| 自动触发 | 继续采用平台自己的 spawn/apply/hard 三阶段；默认比例 60% / 75% / 90% |
| 首批接入 | Devin 的 Responses HTTP：普通托管会话、手动 compact、状态与历史读取 |
| 开启方式 | 管理员开放能力后，客户端显式选择 managed；普通转发的默认行为保持现状 |
| 新会话存储 | PostgreSQL 是 v2 的权威状态源；Redis 用于已有并发/额度机制，不保存另一份可独立提交的会话真相 |
| 旧 Fusion | 旧 Redis 格式与策略继续可读；首批只替换公共核心，不原地改写活跃会话 |
| 首版恢复能力 | 客户端可按授权 API 读取历史；模型自主 `history_read` 后续再接入 |

当前代码还限制单会话最多 256 条请求（`fusion/engine.go`），且 session JSON 和累计摘要归档分别存在 12 MiB 上限（`store.go`、`compaction.go`）。新托管会话改为独立请求记录与历史块，不能继续复制这几个数组到一个 JSON。

## 2. 交付范围

| 发布批次 | 范围与完成定义 |
| --- | --- |
| R1：可上线的最小闭环 | 公共核心；旧 Fusion 适配回归；v2 PostgreSQL 存储；普通 `main` 自动压缩；无状态手动 compact；托管 internal compact；状态/费用/授权历史 API |
| R2：完整会话管理 | 新建 Fusion 会话接入 v2；public checkpoint/rebase；Chat、Messages 与 WS 托管适配；真实流式输出的恢复合同 |
| R3：体验与成本优化 | 平台内部历史读取工具、独立摘要模型及回落、按模型校准 tokenizer、指定 Codex 客户端版本的 compact 适配、steering |

R1 的 managed 模式只接受明确的具体模型、文本和客户端 function tools，工具一次一个，`tool_choice` 支持 auto/none。模型窗口、授权和价格均需能验证。router、不透明 harness、图片/音视频、无法校验的签名输入及其他工具类型在 managed 模式明确拒绝，原转发模式仍按现有能力处理。

R1 支持非流式响应；`stream:true` 先生成并持久化完整响应，再按 Responses SSE 格式发送。沿用 Fusion 已有的响应回放方式，首版不会承诺逐 token 实时到达。失败不发出假的 completed 事件。真实流式输出在 R2 单独解决部分文本/工具已送达后的幂等与恢复问题。

R1 主要解决工具密集任务的历史增长。system/developer 与用户原始约束仍保留，固定指令、工具声明或当前输入本身过大时明确超限，不承诺无限会话。

## 3. 模块边界与代码落点

下面路径均相对于 `backend/`，为 R1 的实际落点。接口和操作步骤见 [运行手册](DEVIN_CONTEXT.md)。

| 模块 | 文件/目录 | 负责内容 |
| --- | --- | --- |
| 公共压缩核心 | `internal/pkg/contextcompact/{types,budget,planner,summary,apply}.go`（已实现） | 策略、单位、快照、完整执行组切分、摘要校验、应用前条件；不依赖 Gin、Redis、PostgreSQL 或 Fusion profile |
| 旧 Fusion 适配 | `internal/pkg/devin/fusion/{compaction,maintenance,types}.go` | 将旧角色/策略映射到公共核心；保留 `fusion-summary-v1`、旧阈值单位、取消、计费与缓存行为 |
| Devin 物理调用 | `internal/service/devin_context_service.go` 的 `contextExecutor` | 为通用 engine 提供独立 Executor 接口，复用 `openai_gateway_devin_fusion.go` 的物理推理、余额预留和 usage billing |
| 普通托管编排 | `internal/pkg/managedcontext/`、`internal/service/devin_context_service.go` | 独立状态机管理 main lane、幂等、摘要调度；service 负责模型授权、账号绑定与共享并发 |
| 持久化 | `internal/repository/context_session_repo.go`（新增） | 短事务、revision CAS、执行租约、历史块、job/attempt、请求幂等与恢复扫描 |
| API | `internal/handler/context_handler.go`（新增），`internal/server/routes/gateway.go` | compact、状态与读取接口；复用鉴权、分组、模型准入、请求体限制与错误处理 |
| 普通请求分流 | `internal/handler/openai_gateway_handler.go` 的 Responses 分流 | 在账号调度前解析 managed 续接；命中旧会话时固定原账号；上游构造前展开本地历史 |
| 配置与注入 | `internal/config/`、service/handler/repository `wire.go`，生成的 Wire 文件 | 总开关、灰度范围、策略与存储依赖；按仓库生成流程更新生成文件 |
| 表结构 | `migrations/246_context_sessions.sql` | 三个 SQL 表与约束，由现有迁移框架应用；不维护重复的 Ent 状态 |

公共核心接受规范化的 `ContextItem`，至少带服务端 message ID、角色、内容、工具组、可见性版本和保护标记。它不能自行猜测某个工具是否真的改了文件；具体工具语义由协议适配器提供。Devin 特有的 reasoning/签名回放留在 provider 适配器，原文作为不透明数据保留，不送入摘要正文。

当前公共接口为 `Measure`、`PlanPrefix`、`ProtectedItems`、`BuildSummaryRequest`、`ParseSummary`、`Snapshot.Matches` 与 `ReplacePrefix`。数据库提交、调度和扣费留在编排层，避免一个大 `Compact()` 同时跨数据库、网络和账务。

## 4. 数据模型与一致性

### 4.1 新增表的逻辑合同

所有主键由服务端生成。资源查找必须附带当前 user/API key scope；不能只凭 session/job/message ID 查询。下表可在实现时合并物理表，但唯一性与事务边界必须保留。

| 表 | 关键字段及约束 |
| --- | --- |
| `context_sessions` | scope、id、schema_version、account_id、public_model、concrete_model、policy_snapshot、contract_digest、public_revision、status、expires_at、容量计数；`(scope,id)` 唯一 |
| `context_lanes` | session、lane、epoch、active_view_refs、protected_refs、pending_tool、lease_owner/token/until；`(scope,session,lane)` 唯一；refs 本身也有大小上限 |
| `context_history_blocks` | session、lane/visibility、block_id、seq 范围、content_digest、不可变内容、expires_at；每条消息有稳定 ID，块有独立字节上限 |
| `context_requests` | operation_id、可空 session、request_namespace、idempotency_key_hash、request_digest、status、response_id、响应块引用、error、费用关联；`(scope,request_namespace,idempotency_key_hash)` 与 scope 内 response ID 分别唯一 |
| `context_compaction_jobs` | lane、source snapshot、trigger、target、status、summary refs、reason、attempt refs、各阶段时间；同一 lane 至多一个未结束的自动摘要 job |
| `context_attempts` | attempt_id、operation_id、可空 session、kind（inference/compaction）、model/account、request_digest、dispatch_state、terminal usage/result、reservation_ref、billing_state；attempt ID 唯一 |

R1 将逻辑对象合并为三个物理表：`context_records` 统一 session/main、operation、job、attempt 的有限元数据与 CAS/租约；`context_history` 保存归属、可见性、摘要 hash 和 manifest；`context_blob_parts` 保存最多 1 MiB 的有序块。session active 只存稳定 message refs。

租约和历史/任务提交均以 PostgreSQL 为准；Redis 并发槽不可用时停止新的付费请求。服务重启可仅通过 PostgreSQL 找回待处理事项。session JSON 只存有限元数据，不内嵌所有历史、响应、attempt 和归档索引。

无状态 compact 也有独立 operation/job/attempt，但不创建可续接 session；其 job 的 session/lane 快照为空，改为绑定本次输入 digest。幂等 namespace 区分创建会话、指定会话内推理、stateless compact 和 internal compact；不能让两个接口意外共用一次扣费或结果。无状态输入与返回 artifact 按公开的短期重试/保留窗口处理，不隐含永久保存。

写历史块、更新引用和相应容量计数使用同一数据库事务。响应完成后先持久化 terminal 结果，恢复/完成幂等计费，再原子发布公共 response ID 与视图；网络发送放在提交之后。失败会留下可恢复记录，不产生指向不存在内容的会话。

### 4.2 摘要快照

```text
SnapshotToken = {
  scope, session_id, lane, branch_id, lane_epoch,
  source_end_id, source_digest,
  contract_revision, visibility_policy_revision,
  concrete_model_revision, summary_policy_revision
}
```

摘要绑定固定前缀，而非整个可变 session revision。前台合法追加尾部可以继续；回退、分叉、改写前缀、更换模型/工具合同/可见性规则使快照失效。应用事务锁定当前 lane，检查快照与 pending tools，在最新尾部基础上生成新视图、验证预算并递增 epoch。

前台同一 lane 同时只能有一个执行者。长调用使用带 fencing token 的可续租执行权，不持有数据库长事务。租约过期后的旧执行者不能改会话视图，但它收到的真实终态结果仍允许按 attempt ID 幂等补记和结算。执行权过期不证明上游请求未发生。

### 4.3 三个状态轴

| 状态轴 | 状态与含义 |
| --- | --- |
| 摘要任务 | prepared → running → ready → applied；另有 failed/stale/cancelled/uncertain |
| 物理调用 | prepared → dispatching → terminal；dispatching 后失去结果为 unknown，不能自动再次发送 |
| 账务 | reserved → pending_settlement → settled；确认未发送才可释放；结果不明进入待核对 |

`ready` 不等于已应用，`cancelled` 不等于免费。摘要已完成但不再采用时仍按真实 usage 计费。终态已持久化而账务提交失败时，只重试使用相同 attempt ID 的结算。

本设计不假定 Devin 提供可按 attempt ID 查回结果的上游接口。没有可靠终态证据的 unknown 保持待人工核对，允许读状态但停止新的付费推进；自动恢复只覆盖本地已持久化证据足够的阶段。

已有 `usageBillingRepository.Apply` 按 request ID/API key 去重；复用该合同，以稳定的物理 attempt ID 作为扣费键。费用事务与会话事务通过可恢复状态衔接，不要求网络调用参与数据库事务。现有 `native_compaction_v2` 字段专用于 OpenAI 原生协议，不能借作 Devin 平台摘要标志。

### 4.4 容量、保留期和清理

新 v2 初始建议值：24 小时会话 TTL、每会话 64 MiB 历史预算、4,096 条请求预算、每块目标 256 KiB/最大 1 MiB、每 API key 最多 8 个活跃托管会话。这些都是待灰度校准的平台配置。超过单块大小的消息使用有序块 manifest 完整重组；物理分块不等于裁剪消息，逻辑工具组边界仍由前缀规划器维护。

按用户/API key 再设置总存储与 maintenance 费用上限；session 级上限无法单独约束滥建会话。创建策略必须给出每 turn 的总费用/物理请求上限，沿用账户配额和余额预留。接近上限先拒绝新付费工作，不在调用模型之后才发现历史无法保存。

清理分开处理：历史按会话期限与审计保留规则清理；幂等记录至少覆盖公布的请求重试窗口；未结算 attempt/账务去重记录不随会话到期直接删除，按账务恢复与保留规则处理。额度与未结算记录达到上限时停止准入并告警，不能无限积压。

## 5. 压缩算法与执行顺序

### 5.1 预算

```text
safe_input = concrete_context_window - reserved_output - estimation_margin
spawn = 0.60 * safe_input
apply = 0.75 * safe_input
hard  = 0.90 * safe_input
target_after < 0.60 * safe_input  # 自动压缩须回到 spawn 以下
stateless_default_target = 0.50 * safe_input
```

R1 返回 `context_budget.estimated_input_tokens`、`estimate_method`、`safe_input_tokens` 和实际 `history_bytes`；输入估算取 normalized/wire 序列化大小的较大值，历史配额独立限制。R1 沿用最终请求序列化字节数的保守估算，标记 `conservative_utf8_v1`，保留至少 `max(256, 5% window)` 的估算余量；它不是各厂商精确 tokenizer。请求在模型配置应用完成后再核对窗口，摘要请求也独立计量。

旧 Fusion threshold 数字仍解释为旧保守输入单位，不能静默转换为精确 token。v2 策略单独版本化；后续 tokenizer 方法切换仅影响新策略。会话 context 查询显示与压缩器相同的计量方法，避免向用户显示另一套可用容量。

新策略的 summary output cap 独立于前台本轮 `max_output_tokens`：默认 1,024，超时 30 秒，每 turn 每 lane 最多 2 个摘要 attempt，且共享 turn 总请求/费用上限。这样用户限制最终回答长度不会意外把摘要裁成几个 token。旧 Fusion 适配阶段继续保留原有行为。

### 5.2 前缀与保留内容

1. 固定保留 system/developer、用户原始目标及约束、未完成工具组；Fusion 还保留当前有效交接。
2. 尾部以完整执行组选择，至少保留当前用户输入和最近一个完成组，在剩余预算内增加近期上下文；不再以“最后六个 item”作为 v2 的唯一规则。
3. 只能摘要已完成前缀。pending call、孤立 result、无法确认的签名关系不能进入有损前缀。
4. protected + 最小尾部已超过 hard，或摘要请求本身不能容纳任何合法前缀时，调用模型前返回明确容量错误。
5. 摘要包含目标、状态、决策、修改、证据、检查、未决事项、后续步骤和文件线索；来源 ID/range 由服务端附加并验证。工具声明为空、tool choice 为 none。
6. 验证结构、无工具调用、来源范围、最终请求大小和有效收缩。默认目标是回落到 spawn 以下，避免刚应用就再次触发；不能达到目标时有限重试或明确失败，不递归无界摘要。

用户约束识别首版采取保守策略：保留用户消息原文，不仅依赖 LLM 提取“关键约束”。这一部分增长过大时要求新会话；结构化约束覆盖和 public rebase 放在后续迭代。

### 5.3 正常推理前的检查

```text
鉴权/模型准入 → 解析会话与固定账号 → 请求幂等/lineage 校验
→ 保存本轮增量 → 恢复已知 attempt 与费用 → 计算最终输入预算
→ 收集摘要结果并检查是否可应用
→ spawn: 有后台容量则准备固定前缀摘要，前台继续
→ apply: ready 摘要在安全边界提交，随后重算预算
→ hard: 停止下一次正常推理，等待已有 job 或同步执行有限摘要
→ 确认小于 hard → 创建前台 attempt → 调用 Devin → 持久化/结算/发布
```

后台线程只写 job/attempt 结果，不能直接改 lane。R1 由当前前台 owner 或显式手动 compact 的执行者应用摘要。没有空闲后台槽时跳过 spawn；账户/用户并发为 1 时，hard 阶段顺序执行摘要与推理，避免等待自己占有的槽。每实例保留有界 runner，并增加共享的 user/account 准入；不能把现有每实例容量 16 误当成集群总上限。

soft 摘要的明确失败在未到 hard 时可继续前台；到 hard 无可用摘要就停止。任何结果不明的付费请求先进入恢复状态，不通过换账号/换模型重发规避。

## 6. API 合同

### 6.1 普通托管 Responses

首次调用现有 `POST /v1/responses`，增加：

```http
X-Sub2API-Context-Mode: managed
Idempotency-Key: <本次逻辑请求的唯一键>
```

首个请求提供具体模型、instructions、tools 和输入，服务端生成 session ID，响应头返回 `X-Sub2API-Context-Session`。同一创建幂等键在 API key scope 内必须命中同一 session/account，首次调度也不能并发创建两个会话。

R1 的托管首请求按新对话处理，不导入无法核对来源的 assistant/tool 历史；既有历史可先经过 stateless 整理，再作为明确标记的历史背景输入。对已有会话，每次付费调用仍检查当前 key、分组模型权限、账号状态与余额；固定 policy/account 不代表永久缓存授权。快照不保存认证 token。

后续请求同时提供 session 头和 `previous_response_id`，`input` 只传增量。previous response 必须是该 session 当前公共 head；旧 head 不隐式分叉。R1 不根据重复提示词或 prompt cache key 合并会话，也不在 full-history 与 delta 之间自动猜测。

instructions/tools 省略时继承已存合同，重复提交时必须规范化后相同；修改合同要求新会话。`previous_response_id` 在平台本地解析，展开为活动视图后从上游请求清除，不能直接传给当前不支持它的 Devin 构造器。

客户端 tool call/result 保持外部执行语义；必须先返回正确 pending call ID 对应的结果才能发起下一轮。平台不执行用户 shell 或文件工具。模型意外产生不受支持的并行调用时不发布非法工具状态，保存已发生 usage 并返回明确错误。

### 6.2 手动 compact

新接口 `POST /v1/context/compact` 使用独立鉴权后的 handler，不通过提示词猜测控制操作。

stateless 请求按显式 model 做模型白名单校验；internal 与查询接口先在当前 scope 查会话，再验证该会话 public model 的当前准入。它们没有 `body.model` 时，不能直接跳过检查，也不能机械套用只读取该字段的中间件。

| 模式 | 输入 | 输出 |
| --- | --- | --- |
| stateless，R1 | mode、具体 model、完整 input、instructions/tools、可选 target_input_tokens、幂等键 | 200：可直接回放的 input、artifact_id、before/after 估算及方法、summary usage、status=compacted/no_op |
| internal，R1 | mode、session_id、lane=main、expected_revision、幂等键 | 200：同步 job_id/status/applied；查询得到失败原因和已知 usage；session 查询返回新的 epoch |
| public，R2 | session、公共 revision、幂等键 | 返回 prepared checkpoint；客户端接受后原子切换公共视图 |

stateless 示例：

```json
{
  "mode": "stateless",
  "model": "<concrete-model-uid>",
  "instructions": "<固定指令>",
  "tools": [],
  "input": [{"type": "message", "role": "user", "content": "<历史示意>"}],
  "target_input_tokens": 24000
}
```

internal 示例：

```json
{
  "mode": "internal",
  "session_id": "ctx_example",
  "lane": "main",
  "expected_revision": 12
}
```

manual 跳过 spawn 阈值，但仍检查授权、幂等、pending tools、可摘要前缀、预算和费用。没有可摘要前缀且输入已满足要求时返回 no_op，不调用模型；固定内容超过目标时返回 `target_unreachable`，不能成功返回一份宣称达标的原输入。首次 stateless input 的既有工具组只作为请求数据校验配对，不据此创建可恢复的托管会话。

`artifact_id` 是平台记录标识，不赋予跨 API key 的访问权。输出普通消息，不生成冒充 OpenAI 加密内容的 compaction item；`/responses/compact` 和 `compaction_trigger` 的现有 unsupported 行为保留到专门适配完成。

### 6.3 状态与回查

| 接口（R1 新增） | 返回内容 |
| --- | --- |
| `GET /v1/context/sessions/:id` | 公开 revision/head、状态、期限、各 lane 允许公开的 epoch、容量估算、前台/maintenance 用量、待结算数量 |
| `GET /v1/context/compactions/:job_id` | 任务阶段、trigger、采用状态、失败原因、估算变化与已知用量；不暴露内部 prompt/私有 reasoning |
| `GET /v1/context/sessions/:id/history?cursor=...` | 有界分页的授权可见消息与稳定 ID；不提供无界全文扫描 |
| `POST /v1/context/sessions/:id/cancel` | 幂等停止会话与新任务准入；迟到结果继续结算 |

工具输出替换/脱敏后的 model-visible 版本是摘要与模型恢复的统一来源。R1 没有模型内部读取工具；客户端读出原文后需显式作为新输入提供。R2 引入 Fusion 后，公开历史 API 不能因此返回 Sidekick 私有上下文。

### 6.4 错误与重试

| 情况 | 合同 |
| --- | --- |
| 非法 managed 输入/模型/能力 | 400，付费调用前拒绝 |
| 余额/配额不足 | 402，复用现有账务准入 |
| revision/工具结果/幂等键冲突，session busy | 409；返回稳定错误码，不自行换 session 重试 |
| 未知或跨 scope 的资源 | 404，避免暴露其他身份资源是否存在 |
| 当前 scope 内可确认的已过期会话 | 410，需要新会话 |
| 固定内容、目标或历史额度无法满足 | 413，指出受限部分；不进行无效付费循环 |
| 上游调用结果未知 | 409 `context_attempt_uncertain`；状态查询可继续，禁止新付费重发 |
| 存储/并发准入暂不可用 | 503；同一幂等键查询或重试，恢复不自动补发物理调用；已知未发送按错误返回 |

客户端断开连接不等于会话 cancel。响应已提交但发送失败时，重试相同幂等键回放同一结果；上游流未有终态就断开时按 unknown 合同处理。幂等键与正文不一致始终拒绝。

cancel 只停止平台的新推理/摘要准入并请求取消平台持有的调用，不会终止已经运行的客户端 shell 或文件工具。R1 不在同一 session 上隐式恢复；后续 steering 使用独立 turn 级合同。

## 7. PR 拆分与依赖

R1 按下表顺序推进；不需要等待新版 CLI 逆向调查。

| PR | 内容与依赖 | 合并验收 |
| --- | --- | --- |
| 1：核心提取 | 建立 contextcompact；Fusion adapter；不改变旧策略与存储 | 现有 compaction/async/cancel/usage/role-isolation 测试通过，旧 profile/会话 fixture 可读 |
| 2：v2 存储 | PostgreSQL 表、唯一约束、CAS/fencing、请求/历史分块、TTL 清理；依赖 PR1 类型合同 | 两实例争用、过期 owner 提交、跨 scope、断事务与历史配额测试通过；历史超过旧 12 MiB 不依赖大 JSON |
| 3：公共执行与恢复 | 提取 Devin executor、后台准入、attempt journal、账务恢复扫描；依赖 PR2 | 各崩溃点可恢复；已知结果不会再调用 provider，unknown 不盲目派发；扣费至多一次 |
| 4：手动 compact | stateless/internal API，状态与历史查询，准入与错误合同；依赖 PR1–3 | no_op 零调用；幂等结果一致；summary 不达标明确失败；读取不越权 |
| 5：普通 managed | main lane、固定账号、增量续接、预算检查、自动三阈值、取消；依赖 PR1–3，PR4 共用管理接口 | 非流式与缓冲 SSE、连续工具请求、后台追加尾部、多次压缩后可继续 |
| 6：灰度与运维 | 策略配置、指标、运行手册、质量回放集、功能开关；依赖 PR4–5 | 模拟恢复与真实模型质量评估达标后才开放指定 API key |

PR4 可先交付 stateless 子集；internal 必须等 v2 会话可创建后再向用户开放。R2 的 Fusion 新存储、public rebase、协议适配分别独立 PR，不与 PR1 的行为等价提取混在一起。

## 8. 验证与上线门槛

### 8.1 自动验证

- 核心：阈值边界、预算单位、中文/代码、大工具声明、大尾部、完整 call/result、用户约束、摘要 schema/来源/收缩目标。
- 生命周期：摘要期间追加消息；pending tools 时禁止不合法应用；重复提交只增加一次 epoch；取消/分叉/来源改写使旧结果不应用。
- 持久化：在 prepared、dispatching、terminal、settled、applied 各阶段注入进程退出；恢复只做证据允许的后续动作。
- 账务：摘要未采用仍结算；相同 attempt 重复结算不重复扣费；费用已提交但 job 更新失败仍能恢复；前台与 maintenance 分开统计。
- 多实例：两个 worker 抢同一 job、租约过期、旧 worker 迟到、全局并发为 1；不重复派发，不发生等待自身槽的死锁。
- API：scope 隔离、账号固定、重复创建、过期/旧 previous response、合同变化、断线后响应回放；未启用 managed 的转发行为不变。
- 长会话：超过 256 个请求、累计历史超过 12 MiB、至少三次压缩；符合新额度时仍可续接，存储和请求耗时不随全量历史线性重写。

测试写在实际边界：公共核心单元测试、PostgreSQL/Redis 集成测试、fake provider 状态机测试、Responses 客户端合同测试。保留已有 Fusion 测试作为回归，不通过只验证 mock 摘要 JSON 就宣布质量达标。

### 8.2 真实模型质量验证

用固定任务集覆盖早期约束、长工具输出、失败后恢复、文件路径/符号、测试结论与中英文历史。对相同任务比较未压缩基准与平台压缩，记录最终任务完成率、关键约束保留、证据可回查性、总费用（含摘要与缓存重建）、等待时间。

硬门槛是工具合同和账务一致性零已知错误；每个验收任务要求预先定义的关键约束和证据检查全部通过。质量集规模与模型清单随灰度配置记录，不能把小样本通过等同于全模型保证。上线前补齐真实模型验证；没有账号流量时保持能力关闭。

### 8.3 灰度与回滚

功能开关至少分为 `managed_context`、`manual_compact`、`auto_compaction`；按 API key/账号/模型白名单启用。账号编辑 GUI 支持“跟随全局 / 开启 / 关闭”及独立的自动、手动开关；账号显式开启可独立授权，已配置的灰度范围仍限制请求，`force_disabled` 可紧急停用全部路径。具体优先级见 [运行手册](DEVIN_CONTEXT.md)。先内部任务，再小范围生产新会话。记录 started/generated/applied/discarded/failed/uncertain、采用率、压缩比例、hard 等待时间、maintenance 成本、存储量、恢复积压；指标标签不放 session ID、用户正文或摘要全文。

关闭 managed 拒绝新的 managed 推进与手动 compact；保留状态、授权历史、取消、已知费用恢复和清理。回滚前先停止新流量，由支持对应 schema/policy 的版本排空当前操作。关闭 auto_compaction 仍保留 hard 检查。数据库改动只增不删；旧 Fusion 可立即停用新适配开关，但已经产生的 v2 会话不能降级给完全不识别 v2 的旧二进制处理。

## 9. 本次交付与验证

R1 六个工作包均已落地（2026-10-23）：公共核心与旧 Fusion 适配、PostgreSQL 存储、独立 managed engine 和物理调用适配、恢复扫描、两个 compact 模式及查询/取消 API、配置/白名单/有界日志计数与运行手册。未提交到远端或启用真实流量。

实现中收敛了三个细节：内部手动 compact 同步返回 200；自动目标从固定 50% 调整为严格低于 spawn（60%），允许保留的大工具组在安全预算内继续；逻辑实体合并为三个物理表，所有大内容保存在分块历史里。摘要失败、未采用和迟到结果仍按观测 usage 结算。用户滚动 24 小时维护预算与跨 key 存储预算在数据库事务中约束。

自动测试覆盖：核心边界与原 Fusion 格式/提示词固定值、271 次工具续接和超过 12 MiB 历史、异步追加尾部、取消/迟到结果、unknown 转 terminal 的恢复竞态、公共响应发布后推进新 head 的重试、幂等扣费和恢复、JSON/缓冲 SSE、默认关闭与权限、真实 PostgreSQL 多 owner 抢租约/旧 token/事务回滚/分块与可见性/用户配额。

命令见 [本地验证](DEVIN_CONTEXT.md#本地验证)。质量门槛仍按 8.2 节执行；fake provider 和本地 SQL 的通过不证明真实上游模型摘要质量达标。R2/R3 继续按第 2 节独立迭代，不属于本次 R1 交付。

本地最终验证（Go 1.27.3）：公共核心/managed/Fusion 的 `-race` 通过；config/service/handler/routes/server 的 Context/Fusion/Devin/GatewayRoutes/Cleanup 专项回归通过；真实 PostgreSQL 随机 schema 的 `TestContextStorePostgres` 通过；后端 `go build -mod=readonly` 通过。数据库测试也已接入标准 integration harness。测试曾因沙箱禁止 miniredis 监听而失败，授予本地测试网络能力后重跑通过。这里未声明全仓库单测、完整 lint 或真实模型质量验收通过。
