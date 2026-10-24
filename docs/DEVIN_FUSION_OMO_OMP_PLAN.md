# OMO / OMP 个人 Fusion 接入与评测计划

状态：后端接入改动与离线验证已落地，客户端合同验收仍有待完成项。规划基线为 Sub2API `abd27dfce`、devin2api `e8547b6d60cb09fddc6debe44bafdfa69867a963`。用户主要使用 OMO / OMP，同时重视长任务稳定性、任务质量和费用；已同意按当前稳定版隔离验证，现已将真实 Fusion 评测改为 Linux 远端执行。尚未执行收费评测或变更生产配置。

OMO 暂按 OpenCode + oh-my-opencode / oh-my-openagent 插件理解；原生独立 OMO 或 Codex 插件版不自动纳入同一验收。客户端公开 `main` / `dev` 文档仅供制定计划，实施时必须冻结实际使用的版本。

### 本轮交付与证据（2026-10-23）

- 后端新增明确拒绝的持久有界恢复、账号 / 模型共享冷却和 Fusion 专用 OMP 会话头适配。发送前再次检查共享冷却；SQL 调度只延长既有冷却；任何正文、工具、usage 或原生 token / 费用 / 隐藏思考执行证据都会阻止按“未执行拒绝”重发。
- 稳定版已锁定：OpenCode `1.19.4`、OMO `5.1.44`、OMP `18.12.6`，运行时 Bun `1.4.4`。实际客户端到本地 SSE stub 的读文件 / 工具结果往返已验证；这是客户端协议探针，不是真实 Fusion 完整链。OMO 的迁移锁曾遇到只读目录警告后继续运行，不能据此宣称启动迁移已验收。
- OMP `18.12.6` 实测没有从 `models.yml` 的 `compat.sessionHeader` 发出会话头，因此提供基于公开 custom-provider API 的最小运行时扩展；不能仅靠服务端识别新头或静态配置完成接入。
- OMP 的 compaction 不是普通 `modelRoles` 角色，可能优先使用当前 Fusion 模型；仅指定 `smol` 等角色不能把压缩固定到具体模型。初期短 smoke 用公开 `compaction.enabled: false` 关闭自动压缩，手动 `/compact` 也暂不验收；这不能替代后续日常长任务的压缩兼容验收。
- 提供 [本机验证说明与工具](../tools/fusion-eval/README.md)、脱敏 fixture、版本清单、配置模板、离线费用汇总与成对预算参考账本。账本不会拦截真实客户端请求；全链路付费发送准入和工具执行去重仍需由本机运行器接入，不能把它当成已交付的完整收费评测运行器。
- 仍待本机验收：外层实际派发至 Fusion Worker、两个 Worker 交错、compact / fork / 取消边界、真实 Lead / Sidekick 工具闭环、30 次工具长流程，以及冻结的质量 / 时间 / 费用对照。尚无证据证明 Fusion 更省钱或更适合设为日常默认。

2026-10-24（北京时间）已新增面向 macOS 的本机 smoke 运行器，提供两个独立开发任务、配置生成、一次启动记录、文件断言和只读费用核对，入口见 [本机操作说明](../tools/fusion-eval/LOCAL_SMOKE.md)。四种客户端 / 任务组合的 `prepare` / `inspect` 已在云端离线验证；Python 工具 69 项、OMP 扩展 8 项测试通过，尚未在 macOS 或真实上游运行。运行器不自动重试收费任务；未知费用保留预算占用。用户 API 不能证明全部账务与在途预留完整，因此 smoke 结果仍为 `partial`，不能直接作为完整 A/B 成绩。

当前执行决定已改为 Linux 远端。现成客户端版本和运行器参数已确认，进入真实 smoke 前仍需绑定测试网关 URL、模型、专用 Key 与用户 JWT；环境就绪步骤见 [Linux 远端说明](../tools/fusion-eval/LINUX_REMOTE.md)。

## 1. 本期目标和推荐拓扑

本期目标是让 OMO / OMP 中一个常用执行 Worker 能可靠使用现有网关 Fusion，并能用同条件对照判断是否值得日常启用。先以单用户、一条上游账号、一组明确的 Lead / Sidekick 为验收配置；这不是删除已有多账号或其他协议能力。

默认拓扑：OMO / OMP 外层编排使用具体模型；一个明确配置的执行 Worker 使用 `fusion-personal-worker` 之类的 Fusion profile；其余探索、检索、后台、摘要和标题等调用显式使用具体模型。Fusion Worker 内只保留实际需要的文件、编辑、命令和验证工具，不递归派发其他 Agent。工具权限需由客户端或适配层落实，不能仅靠提示词要求 Sidekick 不再委派。

这保留客户端外层编排，也把两级编排的费用和会话边界限制在一个可观察的工作单元内。主 Agent 直接使用 Fusion 属于后续独立实验，不作为首期默认配置；不能把 Worker 验收结论外推到该拓扑。

主验收链为 OpenAI Chat Completions JSON / SSE：OMO 使用独立 OpenAI-compatible provider，OMP 使用独立 provider 和明确的 `openai-completions` API。Responses 为第二协议回归；已有 Anthropic 和 WebSocket 保持兼容测试，不把所有客户端、所有传输和所有配对都列为新增承诺。若钉住的客户端实际必须使用 Responses，则记录为该客户端的主链并执行同等端到端验收。

OMP 原生 `devin-agent` provider 调用 Devin Connect RPC，不能只把它的上游地址替换为 Sub2API；本期使用网关公开兼容 API。

## 2. 已有能力与本期真正需要补的部分

| 项目 | 当前依据 | 本期处理 |
| --- | --- | --- |
| 双角色持久会话、结果重放、幂等、补账 | `backend/internal/pkg/devin/fusion/engine.go`，`engine_test.go` | 复用并补真实客户端故障验证，不另搬普通请求完成缓存 |
| 内部上下文压缩、Lead 保温、工具批次串行发布 | `fusion/compaction.go`、`fusion/cache_keepalive.go`、`fusion/queued_tools_test.go` | 验证、计量和调优，不作为全新功能重复开发 |
| Fusion Chat 阶段认证恢复 | `fusion/recovery.go`、`devin_fusion_recovery.go` | 已补 Prepare / Chat 明确拒绝的一次认证恢复和持久物理尝试记录；待真实上游验证 |
| 限流与模型容量处理 | `pkg/devin/rejection.go`、`devin_fusion_admission.go` | 已补有限错误分类、可信等待提示、有界恢复和共享准入；待真实上游验证 |
| 客户端会话身份 | `devin_fusion_session.go`、`tools/fusion-eval/omp-fusion-session.ts` | 已补 Fusion 专用头适配与 OMP 扩展；跨 Worker / epoch 仍待实际验收 |
| 固定工具与公共历史合同 | `fusion/contract.go:100`、`:160`，`fusion/engine.go:78` | 明确客户端 compact / fork / 工具变更的 epoch 边界，不能静默合并历史 |
| 可重复真实评测 | 现有主要是 mock / miniredis 与文档中的历史实验 | 补版本化客户端 fixture、评测运行器和全链路费用汇总 |

已有 Fusion 会在断连后最多为当前物理推理额外 drain 90 秒，仍受 turn 截止约束；得到的持久结果可以恢复。没有终态、执行结果不明的请求进入 `UNCERTAIN`，不承诺任意断线都能无损续跑。

## 3. 本期接入范围

### A. 客户端接入与会话边界（P0）

1. 固定 OpenCode 本体 + OMO 插件版本，或 OMP 包版本；同时记录扩展、MCP、工具权限、Agent 模型映射及 provider API 类型。输出解析后的实际映射，不能只记录配置文件中显式写出的部分。
2. 为两个客户端各提供独立 provider 配方和一个 Fusion Worker 配方。OMP 未配置的角色可能继承默认模型，必须显式核对 `default`、`task`、`smol`、`slow`、`tiny`、`memory`、`advisor` 及摘要等实际调用的模型，避免后台任务继承 Fusion。
3. 用真实客户端连接本地可控 API stub，收集脱敏请求 fixture：初轮、工具续接、新 Worker、同一外层会话的第二个 Worker、重试、取消、compact、fork，以及系统提示和工具 schema 是否变化。
4. 确认每个 Worker 的稳定运行时会话 ID 和每个请求的稳定重试身份。OMP 公开实现的类型列出 `compat.sessionHeader` / `x-litellm-session-id`，但 `18.12.6` 实测会过滤该配置，因此本轮同时增加 Fusion 专用头识别和最小运行时扩展。不要改动普通协议的全局粘性语义；扩展的请求级隔离也不能替代真实 Worker / compact / fork 验收。
5. 禁止把固定静态 `X-Session-Id` 写进通用 provider 配置；不同会话、不同 Worker、fork 必须隔离。不能用相同 prompt 或 `prompt_cache_key` 合并 Fusion 会话。每个 HTTP 请求的幂等键也不能直接等于整个会话 ID。
6. 当前合同包含 instructions、tools 和 tool choice。一个 Worker epoch 内保持合同稳定；客户端 compact、rewind、fork、模型或工具合同变化，在无待回传工具、当前物理结果已知的边界创建新 Fusion session，并记录父 epoch 关联。新 epoch 可从客户端显式提供的摘要开始，不声称保留了旧角色的全部私有上下文。
7. 有未完成工具或未知推理时，先执行显式取消 / 结果核对流程；不得仅为绕过冲突重发写盘、命令或未知推理。`cancel_requested` 只确认取消标记已保存，不证明上游推理或客户端命令未执行；未知结果继续保留原 epoch / attempt 与预留，不能借新 epoch 自动重发。无法正确映射的日常客户端操作算接入阻塞，不能把预期的 409 当作端到端通过。
8. 若客户端会动态发现工具，先核对是否能在该 Worker 生命周期内冻结实际工具集合。保留必要的检索、MCP 和工作区功能；不通过清空所有工具来宣称兼容。任意动态 schema 变更不在本期承诺内，无法满足此合同的版本明确标记未验收。

### B. 适合 Fusion 的上游恢复与准入（P0）

借鉴 devin2api 的错误分类、冷却和模型容量控制策略，使用现有 Fusion attempt / 预算 / Redis / 账务入口；不引入第二套账号池、SQLite 状态或面板。

- 在上游边界提取有限的类型化信息：认证失败、账号限流、具体模型容量、经过校验的等待时间，以及能否确认请求被拒绝执行。原始错误正文、凭据和 JWT 不进入客户端响应或普通日志。
- Chat 阶段明确的未执行认证拒绝，长期 Key 最多恢复一次；仍固定原账号、原 Lead / Sidekick 和具体模型。不把临时会话过期直接永久标成账号坏掉，恢复失败后才沿用现有状态处理。
- 账号 429 与模型容量问题分别记录。缺少明确拒绝证据的 5xx、超时、EOF、无内容或半截输出不能只凭“还没首字”就重发。
- 首期建议初次生成发送一次，明确拒绝后的额外生成发送合计至多两次，包含认证恢复后的重新发送；401 恢复仍最多一次。全部发送计入 profile 调用限制和 run 总预算；等待同时受剩余 turn 截止时间、调用次数、余额 / 额度和取消状态限制。可信等待提示超过可用预算时停止，不能提前重试来绕过等待。
- 每次物理生成发送都要有持久 attempt 或 subattempt 身份和计数，不能隐藏在一个不可观察的透明循环内。已知 usage 先结算；未知结果继续保留预留。认证 RPC 与生成发送分开统计。
- 推理、摘要、预热共享上游准入；后台维护不争抢前台恢复机会。客户端和网关的重试不能相乘成无界发送。
- 不自动更换账号、模型、effort 或 Fusion 配对。已发布的工具调用不能因恢复而重新执行。

### C. 长任务与费用可见性（P0）

- 复用现有持久响应与补账，覆盖“响应已保存但客户端未收到”“工具已执行但回传丢失”“生成结果已知但入账失败”的恢复。
- 对未知结果给出可定位的 session / attempt 状态；区分可重放、仅需补账、需新会话和结果未知。停止状态不伪装成普通成功，未知费用不记为零。
- 评测汇总关联外层任务、Worker run、Fusion session、epoch、角色、purpose 和物理 attempt。至少可区分推理、压缩、保温、明确拒绝、重试等待和未知预留，不记录或展示私有推理正文。
- 并发上限为 1 时前台仍能完成，后台保温可跳过；并发为 2 时验证后台留前台槽和取消回收。单人使用也不能让摘要 / 预热堵住前台。
- 已统一 `DEVIN_FUSION.md` 的 WS 范围描述，按“代码支持 / 服务层已测 / 真实客户端已验收”分别记录证据。

### D. 保温与压缩调优（验收后再决定参数）

先保持现有机制与有界预算，以常用配对做独立 on / off 实验，再决定个人配置是否开启和采用何种间隔。SSE keepalive 不计为上游缓存保温；只增加 cache-read 比例不足以证明净收益。

本期不新增 Sidekick 通用保温、无限后台续跑或自动改写模型提示。客户端自身压缩与网关双角色压缩分别验证。

## 4. 明确延后

- 普通 Devin 请求的 detached completion cache、普通会话通用保温。
- 多账号配额降权、跨账号自动迁移会话、复杂账号池策略。
- Devin 原生托管 WebSearch、文档和视频输入、CLI 凭据文件持续重读；客户端已有且合同稳定的搜索 / MCP 工具仍应保留并测试。
- 任意动态工具发现、运行中任意 steering、并行外部工具、多 Sidekick、递归 Fusion 和主 Agent 全局默认 Fusion。
- 官方私有 Fusion harness 的复刻与等价性证明，旧代理私有签名或旧会话的无缝迁移。

不原样搬入 devin2api 的透明 reopen / resume 泵或 `sanitizeRequest`：前者需重新接入物理发送与费用账本，后者还会改写用户文本和工具参数，不能视为无语义变化的兼容措施。

## 5. 实施顺序与交付物

| 阶段 | 工作与交付物 | 进入下一阶段的条件 |
| --- | --- | --- |
| 0：确认客户端合同 | 版本清单、解析后的角色模型映射、脱敏 wire fixture、会话与工具 digest、两份 provider / Worker 配方 | OMO / OMP 各自有可行的会话、工具和 compact 边界；未满足者明确阻塞 |
| 1：完成接入与恢复 | 最小 Fusion 专用客户端适配、认证恢复、拒绝分类与有界准入、必要的诊断字段、故障测试与范围文档 | 下述离线正确性门槛全部通过，原有协议与账务回归通过 |
| 2：真实客户端试点 | 版本化 `tools/fusion-eval/` 运行器、任务 manifest、隔离工作区、请求 / 工具 / 账务关联、OMO / OMP 完整 smoke | 两客户端均能完成完整工作链；无串会话、重复执行或重复记账 |
| 3：冻结评测与配置选择 | 新任务配对评测、缓存 A/B、质量 / 时间 / 费用报告、推荐与备用个人配置、复现命令 | 有足够证据选择按需或日常启用；不以小样本宣称普遍优势 |

每阶段独立形成可审查变更；失败时保留原具体模型 Worker 配方作为切回路径，不删除会话结果和账务。进入真实上游阶段前固定预算；本计划不代表已执行或已获无限费用额度。

## 6. 测试矩阵

| 层级 | 必验场景 | 通过条件 |
| --- | --- | --- |
| 协议与工具 | Chat 主链 JSON / SSE；Responses 固定 custom / namespace；已有 Anthropic 与 WS 回归；零参数、批工具串行、思考签名 | 合同及终态正确；只有已声明工具可调用；内部委派不泄漏到公共工具 |
| 客户端完整链 | 外层派发 → Fusion Lead 委派 → Sidekick 真实读写 / 测试 → Lead 复核 → Worker 报告 → 外层验收 | 两客户端各至少两个完整 smoke；文件产物、工具退出码、最终断言与结论一致 |
| 会话与边界 | 同一外层会话交错两个 Worker；父子、fork、重开、compact、工具合同变化；重复请求与冲突结果 | 不串历史、角色或工具结果；合法恢复不重复发送；非法操作在付费前明确处理 |
| 拒绝与恢复 | Prepare / Chat 的 401；429 有 / 无 / 非法等待提示；具体模型容量拒绝；连续失败；客户端同时重试 | 认证恢复至多一次；所有物理发送可追踪并受限；不换模型、不误判授权、取消等待及时结束 |
| 未知结果 | 首内容前 EOF / 超时、正文或工具片段后断流、usage 缺失 / 全零、进程在结果 / 账务 / 响应各边界退出 | 没有终态证据不自动重发；已知 usage 只结算一次；未知预留不归零 |
| 客户端工具重试 | 写文件或命令已执行，工具结果回传丢失 / 重复 / 冲突；Worker 被取消 | 同一 call ID 不重复执行有副作用的工具；冲突被拒绝；一个 Worker 的取消不取消其他 Worker |
| 长任务 | 每客户端一条累计至少 30 次工具往返、至少 3 个用户 turn 的受控流程；各 turn 遵守默认 20 次内部请求限制；触发内部压缩；客户端压缩 / fork | 保留目标与必要约束，工具配对不丢；正确跨 epoch；预算耗尽后零新发送 |
| 维护与资源 | 并发 1 / 2；前台等待；预热 / 摘要竞争；重启；迟到 usage；撤销权限；Redis 或账务不可用 | 前台不被后台占满；已发生费用仍可恢复；新付费调用拒绝或受限 |

工具执行去重需要客户端 / 评测适配器配合，单靠服务端响应重放不能保证本地命令 exactly-once。交付按 Worker epoch + call ID 持久保存执行状态和已知结果的适配：已知完成的重复回包只回传结果；本地命令执行状态不明时停止并保留待核对状态，不以自动重跑冒充恢复。验收报告必须同时展示网关物理调用数与客户端实际工具执行数。默认 20 次内部请求的耗尽路径保留独立测试；如需单 turn 超过 30 次工具调用，另建明确的测试 profile 和费用 / 耗时预算，不用默认 profile 冒充可完成。

复用现有 `fusion/engine_test.go` 的 executor / store fixture、miniredis、服务层 mock Connect；新增真正经过本机 HTTP handler 的测试。若实际主链用 WS，再增加完整 HTTP upgrade → Lead / Sidekick → 工具 → 重连的端到端用例；现有直接调用 WS 桥接的测试不替代该项。

本轮已执行 Devin 包回归、Fusion race 测试以及下列服务 / handler / middleware 的定向回归；这不代表仓库全量测试或真实付费链已通过。复现时使用满足 `backend/go.mod` 的 Go 版本，当前云环境另需先激活工具：

```sh
cd backend
GOTOOLCHAIN=auto go test -mod=readonly ./internal/pkg/devin/... -count=1
GOTOOLCHAIN=auto go test -mod=readonly -race ./internal/pkg/devin/fusion -count=1
GOTOOLCHAIN=auto go test -mod=readonly -tags=unit ./internal/service ./internal/handler ./internal/server/middleware -run 'TestFusion|TestDevin|TestCORS|Test.*Session' -count=1
```

本轮结果：上述 Devin 包回归、Fusion race 测试和三个服务包的定向回归均通过；Python 探针 / 账本 21 项测试、OMP 扩展 8 项测试（57 个断言）及发布版类型严格检查通过。补采的 OMO / OMP 扩展短往返中，系统提示、工具 schema、tool choice 摘要保持稳定；这只覆盖该合成读文件流程。探针 fixture 只证明相应客户端到本地 stub 的传输行为，不加入模型评测分母。

更改前端配方或 profile 设置时执行仓库的前端 lint / typecheck 和相关 Vitest；更改路由、恢复或账务入口时补对应 handler / 集成检查，不只验证 executor mock。

## 7. 质量、时间与费用的冻结评测

### 对照与样本

- A：指定 Worker 使用具体 Lead 模型；B：同一个 Worker 使用该 Lead + 固定 Sidekick 的 Fusion。外层编排、其他 Agent、工具、题目、模型 effort、任务费用上限与时间上限保持一致。
- 第一轮用 8 个未参与开发的新任务：2 个函数任务、4 个多文件修复、2 个长工具流程；预先平衡分配 OMO / OMP 各 4 题，每题 A / B 各一次，共 16 runs。每题开始前为 A / B 成对保留预算；不完整配对保留原始结果和全部费用并单列，不挑选单臂成绩参与配对比较。不用该分配评价哪个客户端更强。
- 开发 fixture、冒烟题和既有 12 道 HumanEval / MBPP 题只做回归，不进入新成绩。冻结实现后保留隐藏验收，不把失败 run 在修复后覆盖成成功；修复后另开 cohort。
- 固定 Sub2API SHA、客户端 / 插件 / 扩展版本、任务与工作区哈希、模型 UID / effort、价卡及倍率、profile / 压缩 / 工具配置、预算和运行顺序。A / B 顺序随机化，每次使用独立工作区；记录实际缓存用量和上游波动。
- 核心质量对照先显式关闭 Fusion 保温，保温另做实验。两臂尽量使用独立且等条件的初始前缀，记录实际缓存桶；上游缓存和负载无法完全控制，费用 / 时延差异不能全部归因于编排。客户端外层实际调用次数可以因 Worker 产物改变，必须按实际总数计量，不能当作固定常数扣掉。

### 每次运行记录

| 维度 | 必填记录 |
| --- | --- |
| 质量 | 隐藏测试 / 独立断言结果、最终改动、是否完成目标、是否有真实测试证据、返工与外层重派次数 |
| 稳定性 | 协议 / 上游 / 工具 / 预算 / 超时 / UNKNOWN 分别归因；恢复结果；物理调用和工具去重证据 |
| 时间 | 首个公共工具或正文耗时、完成耗时、工具执行时间、重试等待、计划空闲时间；SSE keepalive 不算首字 |
| 费用 | Worker 小计和整个用户任务总计；外层 / 其他 Agent + Lead / Sidekick + 压缩 + 保温；已知结算和未知预留分列 |
| 用量 | 每次物理发送的模型、角色、purpose、input / output / cache-read / cache-write 与价卡版本 |

失败、预算耗尽、超时和 UNKNOWN 都保留在已分配样本分母；未因总预算而启动的题目单列为未执行。最终模型口头说测试通过不能替代工具输出和独立验收。

评测调用统一经过独立测试 Key 和可计量价卡；混接网关外部 provider 时必须先补齐全链路记录与发送准入，否则只报告可观测部分，不宣称整体更省钱。网关配置价卡换算费用与 Devin 实际套餐配额消耗分别报告，不能冒充供应商最终账单。

### 独立缓存与长等待实验

相同配对、任务与空闲时长下，比较保温关闭 / 开启后的 Lead 恢复耗时和含保温费用的总成本，空闲时间单列。至少包含正常短等待、超过预热间隔的等待、前台并发受限而跳过预热三类。压缩设置固定；需要测压缩收益时另开实验，不能同时改两项后归因。

两组使用独立随机前缀并记录实际缓存桶；只换 session / Cascade ID 不足以隔离上游缓存。加速时钟只验证调度正确性，真实 TTL 与净收益必须用真实等待实验验证。样本不足就记录证据不足，不用一次高 cache-hit 推导长期节省。

## 8. 预算、停止规则与决策

建议真实试点总发送准入预算先设为 **US$20**，这是待实施的计划参数，不是本轮已经发生的费用：smoke 预留 $4；16 个配对 runs 各至多 $0.75，共 $12；缓存 / 长等实验预留 $4。每个 run 上限覆盖完整外层任务、所有 Agent、压缩与保温，不能只限制 Fusion Worker。

上述额度必须先按真实价卡和保守发送预留确认可行；若最低完整工作链装不进预算，先记录并调整计划、缩小题量，不在运行中偷偷提高总额度或降低验收要求。额度不是供应商收费绝对上界：实际 usage 可能超过估价，未知发送仍可能消耗上游额度。

发送前以已结算费用、在途保守预留和未知预留共同检查剩余预算；停止后仍接收并结算已知迟到 usage，但不继续发新请求。未知预留不能被当作可用余额。达到单任务上限按该预算下失败记录；达到总上限停止后续题目，不自动扩额。

发布门槛与个人配置选择分开：

1. 确定性故障测试全部通过；重复记账、跨 Worker 串线、未知推理自动重发和重复有副作用工具执行均为零容忍。
2. 两客户端的主链完整 smoke 通过；普通 compact / fork / 取消操作若仍会丢任务或无法正确续接，该客户端版本不能标为日常已验收。
3. 冻结任务逐题展示质量、时间与全链路费用；先看 Fusion 是否减少未完成任务，再比较成功任务成本和等待时间。成功任务成本为该组全部已执行任务的已确认费用（包含失败、预算耗尽和重试）除以成功任务数；UNKNOWN 预留另列，成功数为零时记为未定义。不合成未经约定的加权分数。
4. 若 Fusion 更贵、更慢，且没有多完成任务，不推荐设为日常默认。若收益只出现在困难任务，交付按需 Fusion Worker 和普通具体模型 Worker 两份配方。
5. 8 对样本只用于个人配置筛选，不足以声称显著质量提升、生产可靠率或通用成本优势。结果接近或波动较大时，另定预算扩展冻结样本，不能用开发中复测补成绩。

## 9. 最终验收包

- 两客户端的版本清单、provider / Worker 配方、角色最终映射和普通模型切回配方。
- 会话 / epoch / 工具合同说明，支持与未支持的实际客户端操作清单。
- 版本化 fixture、故障注入、真实客户端运行器、任务 manifest 与复现命令。
- 每次 run 的结果、工具轨迹、用量、已知费用、未知预留和失败归因；汇总质量 / 时间 / 成本对照与缓存实验。
- 明确区分现有能力、本期新增、仅 mock 验证、真实客户端已验收和仍未知的边界。

## 10. 资料与证据边界

- 当前实现与历史诊断：[Fusion 说明](DEVIN_FUSION.md)、[Devin 使用](devin-usage.md)。旧 12 题包含修复与复测；575 个模型 ID 检查未发送推理，均不能代替本期冻结成绩。
- 移植参考：[devin2api 固定提交](https://github.com/WncFht/devin2api/tree/e8547b6d60cb09fddc6debe44bafdfa69867a963)。复用策略，不直接复用其流级发送与状态存储假设。
- OMP 官方：[模型与角色配置](https://github.com/can1357/oh-my-pi/blob/main/docs/models.md)、[协议约束](https://github.com/can1357/oh-my-pi/blob/main/docs/provider-endpoint-constraints.md)、[Task 工具](https://github.com/can1357/oh-my-pi/blob/main/docs/tools/task.md)、[会话头注入](https://github.com/can1357/oh-my-pi/blob/main/packages/ai/src/providers/inference-headers.ts)。公开分支仅作规划参考，阶段 0 记录实际版本 / SHA。
- OMO / OpenCode 官方：[Provider](https://opencode.ai/docs/providers/)、[OMO 编排](https://github.com/code-yeongyu/oh-my-openagent/blob/dev/docs/guide/orchestration.md)、[OMO 配置](https://github.com/code-yeongyu/oh-my-openagent/blob/dev/docs/reference/configuration.md)。不能把不同 OMO 发行形态或不同 OpenCode 版本混作一次验收。
