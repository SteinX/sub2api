# Devin 平台托管上下文 R1

普通 Devin 模型可以显式开启平台托管会话，按同一具体模型生成结构化历史摘要；另提供无状态和会话内手动 compact。旧 Fusion 共用压缩核心，仍沿用原 Redis 会话与策略。机制依据见 [分析](DEVIN_COMPACTION_MIGRATION.md)，范围见 [实施方案](DEVIN_COMPACTION_IMPLEMENTATION_PLAN.md)。

本版本接入 Responses HTTP，支持文本及客户端 function tools，每次一个工具调用。`stream:true` 在完整结果落盘和计费后回放 SSE。Chat、Messages、WebSocket、真实增量流式、Fusion v2 会话和 public rebase 属于后续范围。

## 启用

GUI 入口：**账号管理 → 编辑 Devin 账号 → 托管上下文压缩（Compactor）**。选择“为此账号开启”后，可以分别切换“自动压缩历史上下文”和“允许手动 compact”，两项默认开启。保存后生效，无需修改配置文件或重启。开关只授予该上游账号能力，客户端仍须按下文显式使用 managed 模式或 compact 接口。

账号模式与配置优先级：

| 账号模式 | 行为 |
| --- | --- |
| 跟随全局配置 | 默认模式；使用 `enabled`、`manual_compact`、`auto_compaction` 及必填 key/model 灰度名单。默认关闭 |
| 为此账号开启 | 独立启用该账号；无需打开全局 `enabled`，空 key/model 名单不再限制此账号；已填写的 key/model/account 名单仍作为额外限制 |
| 为此账号关闭 | 即使全局已开启也禁止该账号后续托管推理、自动和手动压缩 |

`gateway.devin_context.force_disabled: true` 是优先于账号和全局默认值的紧急停用配置。状态、授权历史、取消和已知费用恢复保持可用。关闭自动压缩保留 hard 容量检查；开启或关闭账号的自动压缩会用于已有会话的下一次推进。已经发往上游的请求仍需结算，开关不代替会话取消接口。

账号设置存储在 `extra` 的 `devin_context_enabled`、`devin_context_manual_compact` 和 `devin_context_auto_compaction`，只接受 boolean 或 null。enabled 缺省/null 表示跟随全局，显式开启时另两项缺省/null 表示开启。恢复“跟随全局配置”会把这三个字段设为 null。普通编辑、extra 合并和批量接口均执行类型与平台校验。

配置样例在 `deploy/config.example.yaml` 的 `gateway.devin_context`。以下是供“跟随全局配置”账号使用的原灰度方式，`api_key_ids` 和 `model_uids` 为空时该路径无人获准；`account_ids` 为空表示不额外限制已经授权的 Devin 账号。

```yaml
gateway:
  devin_context:
    enabled: true
    force_disabled: false
    manual_compact: true
    auto_compaction: true
    api_key_ids: [123]            # 替换为内部测试 key ID
    model_uids: [gpt-5.5]        # 替换为该账号已授权的具体 UID
    account_ids: [456]
```

要求：已完成数据库迁移、可用的共享 Redis、持久 usage billing、具体模型的明确价格，以及标准模式下启用 `billing.inflight_reservation.enabled`。首版使用 Devin 分组。模型目录必须能验证具体窗口、工具能力和直接推理路径；账号对该 UID 的模型重映射需先移除或改为原 UID，不静默绕过账号映射。router、Fusion 配对名、未知 harness、媒体、客户端 reasoning/签名、custom/freeform 工具、`store:true`、结构化输出及强制指定工具暂不接入本模式。

配置文件由服务启动时载入，GUI 账号设置在每个付费阶段从当前账号读取。会话固定账号、模型、预算 policy 和工具/指令合同；每个付费阶段仍重新校验当前 key、分组、账号开关、目录授权和余额。分组改变后不能继续原会话的付费工作。维护结算使用原始归属，不因 key 或账号停用而丢弃已知费用。

## 客户端合同

首次请求同时传 `X-Sub2API-Context-Mode: managed` 和 `Idempotency-Key`，以新的用户对话开始。请求体最多 2 MiB。

```sh
curl "$BASE_URL/v1/responses" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'X-Sub2API-Context-Mode: managed' \
  -H 'Idempotency-Key: task-001-turn-001' \
  -d '{"model":"gpt-5.5","input":"检查代码，保留现有 API。","max_output_tokens":1024,"tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}'
```

响应头返回 `X-Sub2API-Context-Session` 和 `X-Sub2API-Context-Revision`，body 中是 `resp_ctx_...` 公共 response ID。后续必须同时传 session 头和当前 `previous_response_id`，`input` 只传增量。旧 head 不产生隐式分叉。新增逻辑请求使用新幂等键；重试使用相同键和相同 body，命中结果时返回 `X-Sub2API-Context-Replayed: true`。成功后回放响应头的 revision 是读取时的当前会话 revision；手动修改前应查询状态取得最新值。

```json
{
  "model": "gpt-5.5",
  "previous_response_id": "resp_ctx_...",
  "input": [{
    "type": "function_call_output",
    "call_id": "响应中发布的 call_id",
    "output": "客户端实际读取的内容"
  }]
}
```

存在 pending tool 时必须先返回该 call ID 的唯一结果。平台不运行客户端工具。后续可以省略 instructions/tools/tool_choice 以继承；显式更改它们需新建会话。只接受 `tool_choice:auto/none`，并关闭并行调用。`max_output_tokens` 默认 1,024，不能高于策略或具体模型上限。

自动压缩按实际待发送请求的保守输入估算设置 spawn/apply/hard：60%/75%/90%。摘要使用同一模型，独立输出上限默认 1,024、超时 30 秒，按具体模型能力再收紧；前台较小的输出预算不截短摘要预算。空闲时提前生成，在下一次合法前台边界应用；到 hard 可以顺序等待/生成摘要。没有后台并发槽时跳过提前生成，账户并发为 1 时不会占住槽等待自己。

只压缩已完成前缀。所有原始 user/system/developer 消息和最新完整工具组保留，摘要作为带来源 digest 的历史数据。自动摘要目标是回落到 60% 以下；固定内容、保留尾部或用户约束本身超大时明确失败。估算方法是 `conservative_utf8_v1`，不是精确 tokenizer；窗口中另留输出和 `max(256, 5% window)` 余量。

## 手动 compact

`POST /v1/context/compact` 独立于 OpenAI `/responses/compact`；返回可读摘要数据，不生成提供方的加密 compaction item。请求均需身份和幂等键。

无状态模式传完整历史，原始文本、完整工具组均可，不能包含 pending tool、客户端 reasoning 或 `previous_response_id`。结果已经满足目标时 `status:no_op`，不调用模型。

```json
{
  "mode": "stateless",
  "model": "gpt-5.5",
  "input": [
    {"role":"user","content":"保留 API 兼容性。"},
    {"role":"assistant","content":"已完成工作的历史内容……"},
    {"role":"user","content":"继续验证。"}
  ],
  "target_input_tokens": 16000
}
```

返回 HTTP 200：`input`、`artifact_id`、`status`、before/after 保守估算、`estimate_method`、`summary_usage`。返回 input 可直接用于普通完整历史请求；managed 首请求仍按新对话校验，不导入 assistant/tool 历史。若要转入 managed，可将摘要明确作为用户提供的历史背景文本。

会话内模式同步执行，返回 HTTP 200 的 job 与采用状态。无需也不能另传模型来换路由。先完成当前请求和 pending tool，再使用最新 revision：

```json
{
  "mode": "internal",
  "session_id": "ctx_...",
  "lane": "main",
  "expected_revision": 7
}
```

成功或已创建 job 的失败响应通过 `X-Sub2API-Context-Compaction` 提供 job ID；stateless 摘要失败时也保留可查询的已知用量。`needs_retry` 表示需要客户端用原键恢复。

同键重试读取同一 job。`ready` 表示已生成；`applied` 才表示已经替换活动视图。摘要不可达目标时返回失败状态/原因，仍结算已经发生的真实 usage。

## 状态、历史和取消

| 接口 | 返回内容 |
| --- | --- |
| `GET /v1/context/sessions/:id` | status、revision、epoch、head、期限、容量、推理/维护 usage、维护实际费用、待结算与结果未知数量、最近发送时的预算快照 |
| `GET /v1/context/compactions/:job_id` | trigger、阶段、是否采用、before/after、原因、已知 usage 和结算状态 |
| `GET /v1/context/sessions/:id/history?cursor=0&limit=20` | 公开历史，cursor 单调递增，limit 为 1–50 |
| `GET /v1/context/sessions/:id/history/:message_id?offset=0` | base64 原始字节分段，最多 256 KiB，含 sha256、total_bytes、next_offset |
| `POST /v1/context/sessions/:id/cancel` | 停止该会话后续付费推进，并请求取消平台持有的推理/摘要 |

历史页内联单项最多 256 KiB、每页合计最多 1 MiB；更大内容通过 `content_ref` 指向消息 ID，由分段接口完整读取。客户端拼接 base64 解码后的字节，核验整体 sha256 后再解析 UTF-8/JSON。公开历史不返回私有 reasoning、摘要 prompt 或 provider result。scope 固定当前 user + API key，换 key 无法读取原会话。

预算快照表示最近准备发送的请求；下一轮追加输入后会重算。usage 从独立 attempt 账本聚合，因此失败或取消后的已知付费结果仍可计入。取消不终止已经运行的客户端 shell/文件工具，也不抹掉已发生费用；本版取消后的 session 不隐式恢复。

| HTTP / code | 处理 |
| --- | --- |
| 404 `context_disabled` / `context_model_unavailable` | 检查开关、白名单与当前分组授权 |
| 404 `context_not_found` | 资源不在当前 key scope 或已清理 |
| 409 `context_lineage_conflict` / `context_contract_changed` | 查询当前 head；更换合同需新会话 |
| 409 `context_tool_pending` / `context_request_incomplete` | 补 pending tool，或用原幂等键恢复请求 |
| 409 `context_attempt_uncertain` | 无可靠上游终态，不换账号/模型重发；进入账务核对 |
| 410 `context_expired` | 会话期限已到，按新对话开始 |
| 413 `context_limit` / `context_history_limit` / `context_operation_limit` | 固定内容、窗口、历史或请求元数据配额不足 |
| 402 `context_budget_exhausted` | 单 turn、单会话或用户滚动 24 小时维护预算不足 |
| 503 `context_billing_pending` / `context_unavailable` | 同键重试；恢复只重放已持久结果/结算 |

## 持久化与恢复

迁移 `247_context_sessions.sql` 新增三个 SQL 表，不引入另一份 Ent 状态：`context_records` 保存有限 JSON 元数据、会话租约、operation/job/attempt；`context_history` 保存不可变内容 manifest 和归属；`context_blob_parts` 保存最多 1 MiB 的有序内容块。会话视图和新历史在一个 PostgreSQL 事务提交，CAS 检查 revision、租约 token 和存活时间。长调用期间没有数据库长事务。

每个物理调用在派发前写 prepared/dispatching journal；终态 usage/body 独立于会话租约保存，账务用 `context:<attempt_id>` 去重。只有费用已结算、公共响应已提交后才向客户端返回内容。进程崩溃后，客户端按原键恢复已知结果；后台每 30 秒扫描最多 50 个待恢复 attempt，不派发新的推理。失败的结算记录轮转，避免一个失效归属长期阻塞整批。

prepared 超时标记未发送；dispatching 超时且无终态证据进入 unknown。unknown 不自动重试。停用功能仍运行已知费用恢复与 TTL 清理。应用退出先停止/排空托管 runner，再关闭计费和数据库依赖。

默认保留与配额：

| 项目 | 默认 |
| --- | --- |
| session TTL / 幂等重试窗口 | 24 小时 / 48 小时 |
| 历史保留 | session 到期后再保留 24 小时；stateless 结果保留 48 小时 |
| attempt 保留 | 至少 48 小时；长 TTL 策略至少覆盖 TTL + 24 小时 |
| 历史配额 | 每 session 64 MiB，每 user 跨 key 256 MiB，包含所有保留的私有/物理响应 |
| 活跃会话 | 每 key 8、每 user 32 |
| session 请求上限 | 4,096；不继承旧 256 请求限制 |
| 单 turn | 最多 4 个物理调用，估计/实际累计预算 5 USD，180 秒 |
| maintenance | 每 session 10 USD，每 user 跨 key 滚动 24 小时 50 USD，每 turn 最多 2 次摘要 |
| 保留 operation 元数据 | 每 key 最多 min(max_requests × max_sessions_per_key, 16,384)，每 user 最多 min(max_requests × max_sessions_per_user, 65,536) |

付费前预留历史输出空间和余额/额度；实际保留字节由 SQL 事务核算，取消/过期数据在真正清理前仍占保留配额。若上游返回异常大的内容，尽量保留终态 usage 并停止发布，不能为保存 body 而丢弃账务。未结算 attempt 不因 TTL 直接删除；关联 session 和 operation 元数据也保留，避免 unknown 的原幂等键过期后被当作新请求再次派发。未决调用积压达到 key/user 有界阈值后拒绝新的 operation/session，原键读取和恢复仍可用。

## 运维与灰度

日志事件：`devin.context_attempt`（物理调用）、`devin.context_event`（summary_started/ready/applied/failed/uncertain 等）、`devin.context_counters`（实例内累计次数与后台活动数）、`devin.context_settlement_pending` / `*_owner_unavailable` / `*_account_unavailable`（需核对）。事件标签不放正文、摘要或 session ID；账务日志沿用已有 attempt/账号审计。

SQL 可按 `kind/status` 统计积压与按 `context_history.user_id` 统计容量。接口中的压缩估算比和维护费用可以形成灰度报告；实例 counters 随重启清零，财务统计以数据库 usage billing/attempt 为准。R1 未另建 Prometheus exporter。

紧急停用全部账号时设置 `force_disabled: true`；只关闭某个账号时在 GUI 选择“为此账号关闭”。`enabled: false` 只关闭继承配置的原灰度路径，显式开启的账号仍可使用。停用后保留状态、授权历史读取、取消、已知费用恢复和清理；需要排空时先停止新流量，等当前操作完成。关闭自动压缩不取消 hard 容量检查。数据库迁移只新增对象；回滚应用前先排空 v2 会话，不能把 v2 会话交给不认识其 schema 的旧版本。

上线前使用固定的真实模型任务集比较完整历史与压缩版本：早期约束、中英文、长工具输出、文件/符号、失败后成功测试、历史提示注入、摘要期间追加工具结果、取消迟到结果。要求预定义关键约束和证据检查全部通过，记录模型 UID/日期、输入来源、摘要、任务结果、维护成本和等待时间。当前自动测试验证状态机与协议，不替代真实模型的摘要质量评估；未完成灰度质量验证前保持默认关闭。

## 本地验证

```sh
cd backend
go test -race ./internal/pkg/contextcompact ./internal/pkg/managedcontext ./internal/pkg/devin/fusion
go test -tags=unit ./internal/config ./internal/service ./internal/handler ./internal/server/routes ./cmd/server \
  -run 'Context|Fusion|Devin|GatewayRoutes|Cleanup'
CONTEXT_TEST_DATABASE_URL='postgres://user@127.0.0.1:5432/testdb?sslmode=disable' \
  go test -tags=contextintegration ./internal/repository -run TestContextStorePostgres
go build ./cmd/server
```

PostgreSQL 测试使用随机独立 schema，重复应用 migration，退出时删除该 schema；不要指向生产数据库。需要可监听本地端口的测试环境。测试涵盖真实 SQL 配额/CAS/fencing、不可变大块、授权隔离、恢复与幂等扣费、长会话、多次压缩、缓冲 SSE 和旧 Fusion 回归。
