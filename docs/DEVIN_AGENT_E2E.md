# Devin 的客户端进程验收

最终验收必须使用真实 PAT 关联的测试账号，经过当前 checkout 的生产后端、普通用户公开 Key 与分组，完成 Fusion 和非 Fusion 的相关用例。模拟 Devin 通过只是中间证据；`plan` 只证明接入与发现，不能替代生成验收。尚未运行、失败或结果不明的用例都不能记为通过。

## 版本与边界

| 客户端 | 固定版本 | 当前运行方式 |
| --- | --- | --- |
| OpenCode | `opencode-ai` / Linux binary `1.19.4` | `--pure`，作为无插件对照 |
| OMO | `oh-my-opencode 5.1.44`，宿主 OpenCode `1.19.4` | 加载真实插件，使用明确的只读 primary probe agent |
| OMP | `@oh-my-pi/pi-coding-agent 18.12.6` | 真实 CLI / OpenAI Chat provider，短流程关闭历史裁剪和后台模型功能 |
| Bun | `1.4.4` | OMP 与插件所需运行时 |

版本选择和配置格式复用 [PR #17 的固定记录](https://github.com/SteinX/sub2api/blob/f2de531383945df2fb0b456ab9c417ee013bdebb/tools/fusion-eval/versions.json)，依赖已另以 `package-lock.json` 锁定。本分支没有合并 PR #17 的生产恢复代码或重写其 Worker smoke 工具。

这套矩阵是“真实客户端进程 → 本次编译的后端 → 原生上游”。`mock` 的最后一段是合成 Devin，`live` 才是真实 Devin。原有 `make test-devin-e2e` 直接发送 Responses、Chat、Messages JSON/SSE/WebSocket 协议请求；这些协议请求不算对应 CLI 的进程验收。

## 可执行矩阵

以下每项针对 OpenCode、OMO、OMP，分别使用已发现的直接模型与隔离账号上的 `fusion-e2e-agents` 配对。每组串行，失败立即停止批次。

| 用例 | 必需证据 |
| --- | --- |
| PAT 接入和模型发现 | 正式 Key auth-session → commit；从账号授权目录选模型；分组和普通 Key 的 `/v1/models` 可见 |
| 简单文本与 streaming | 真实进程成功退出；真实 assistant 流中出现约定结果；请求实际为 SSE |
| 读工具与结果续接 | CLI 报告完成 read；公开请求含实际文件结果；由生产网关转换到原生工具历史 |
| 多轮 `--continue` | 客户端仍携带先前工具历史；有客户端会话头时身份保持；Fusion 的服务端会话一致 |
| 后端重启后续接 | 重启当前测试后端，保留隔离 SQL/Redis 和客户端状态；原会话与用量仍能读取 |
| 新会话隔离 | 新客户端状态目录和新会话；Fusion 不复用旧会话 |
| 发送前中断 | 在本地准入边界确认请求尚未转发，终止真实进程；原生生成请求数不增加 |
| 已确认未发送后的单次重试 | 同一客户端状态 `--continue`；只在上一项已证明没有转发时执行一次 |
| 用量 | 公开 Key 的 SQL usage 有 token 和费用；Fusion 每个实际角色 attempt 只有一条结算记录 |

Fusion 另外要求：持久状态到达 `TURN_COMPLETED`，Lead 至少规划与审查两次，Sidekick 至少一次，handoff 有报告，两条历史独立，role attempt 已结算。mock 同时检查实际原生请求的角色模型 UID 和 Cascade 上下文分离。仅选到 Fusion 名称或得到最终文本不算成功。

### 两个已捕获的兼容问题

OpenCode `1.19.4` 的文件读取还需 `external_directory` 权限；生成的配置只允许本次合成 `workspace/*`，其他工具保持拒绝。原配置的失败结构见 [记录](../tools/devin-e2e-clients/fixtures/opencode-1.19.4-read-permission.json)。修正没有增加请求上限。

OMP `18.12.6` 即使关闭自动 compact，仍可能把较早的 read 结果替换为 `[Superseded by a newer read of this file]`，导致持久 Fusion 的 `history_conflict`。因此测试配置明确使用：

```yaml
compaction:
  enabled: false
  supersedeReads: false
  dropUseless: false
retry:
  enabled: false
```

[失败前后的最小记录](../tools/devin-e2e-clients/fixtures/omp-18.12.6-read-replay.json) 保留该触发条件。网关仍拒绝改写历史，未为客户端裁剪放宽检查。此配置不等于完整长会话 compact/fork 已兼容。

OMP 默认裁剪配置的结果明确为失败，另写入 `agent-known-default-incompatibilities.json`；配置后的矩阵通过不能覆盖这项结果。关闭裁剪的代价是保留更多历史，可能增加上下文占用与输入费用，长会话需另行设计和验收。

## 启动、计划与停止

需要 Linux x64、Node 24、Python 3、[基础环境的 Go/Docker 依赖](DEVIN_E2E.md)。所有依赖安装在仓库工具目录，不升级全局 CLI。

OMO 启动迁移可能触及 `HOME/.omo`，XDG 配置不足以隔离它。因此只在只读用户目录的托管环境或可丢弃的 GitHub Actions 用户中执行；运行器拒绝直接使用可写的个人用户目录。不要更改 HOME 或写入已有个人配置来绕过该条件。

```bash
cd /workspace/sub2api
source /workspace/.sub2api-env/activate.sh
npm ci --prefix tools/devin-e2e-clients --ignore-scripts --omit=optional --no-audit --no-fund
python3 -m unittest discover -s tools/devin-e2e-clients -p 'test_*.py'
GOTOOLCHAIN=go1.27.3 GOMAXPROCS=2 GOFLAGS=-p=2 make test-devin-agent-e2e
```

定位单一客户端/模型组合可设置 `DEVIN_E2E_RUN='^TestDevinAgentClients$/^omp$/^fusion$'`，其余组不会运行。不要以筛选后的结果代替完整矩阵。

真实运行仍仅需要安全配置 `DEVIN_E2E_PAT`，不要求用户手工提供 model UID。账号目录必须提供直接调用、工具能力、至少 32768 context 和 1024 output 的可用模型。按正数已知 credit multiplier 排序，优先使用不同的两个成员；只有一个成员时明确记录同型号双角色，不能声称跨型号验收。计划查询本地模型价卡，未知价格不当作免费，价格缺失时禁止 live 生成。

```bash
# 接入、添加临时账号、模型与价卡发现；不启动客户端生成
make test-devin-agent-plan

# 仅在环境接入和该具体付费批次获得授权后执行
DEVIN_E2E_ALLOW_PAID=true DEVIN_E2E_AGENT_BATCH=canary make test-devin-agent-live
DEVIN_E2E_ALLOW_PAID=true DEVIN_E2E_AGENT_BATCH=matrix make test-devin-agent-live
```

必须先审阅 `agent-plan.json` 的真实 UID、两角色、价格、版本和限制，再决定是否允许相应批次。`ALLOW_PAID` 是执行门控，不是供应商金额上限；没有授权时不得设置来试跑。

| 批次 | 进程与公开请求 | 限制 |
| --- | --- | --- |
| `canary`，live 默认 | 1 个 OMP 非 Fusion 读工具进程；预期 2、最多 3 个转发请求 | 临时 Key 本地额度 `$0.25` |
| `matrix` | 3 客户端 × 2 模型类型 × 7 次进程启动 = 42；至少 42、最多 48 个转发请求 | 每组最多 8 次转发、12 次 HTTP 尝试；临时 Key 本地额度 `$1` |

并发固定为 1，单进程 75 秒超时，输出请求在准入边界限制到最多 1024 token。Fusion 每 turn 最多 1 次委派、6 次内部角色调用、2 个客户端工具、90 秒、`$0.10` 平台成本预算；后台保温与压缩关闭。SDK 重试仍计入公开请求上限，运行器不自动重跑失败/不明发送；网关原有认证恢复仍可能增加上游请求。token、Key 额度和平台预留都不是供应商硬限额，真实费用还取决于输入、角色调用、缓存、恢复和供应商计费。没有完整价卡和真实调用记录前，不给出虚构的美元保证。

停止和依赖清理与基础 runner 相同：退出或 Ctrl-C 删除本次容器/网络，停止后端；每个进程组有超时和终止路径。客户端自身的私有会话仅在临时测试目录，测试结束删除。PAT 与派生会话不进入客户端环境；客户端只拿到本次新建的公开 Key。

## 尚不能声明通过的范围

真实 PAT → `server.codeium.com` 的运行时凭据引用、现有任务的配置版本绑定及出站放行必须先核实，详见[运行时说明](DEVIN_E2E.md#托管域名-secret-与版本绑定)。发布配置不等于既有实例已应用；不能提取 Secret、猜测引用或绕过策略。

### 2026-10-25 新环境检查点

北京时间 10:26:32 的平台观测确认新实例为 `running`、网络策略为 `enforced`，`DEVIN_E2E_PAT` 为 `ready`；绑定版本为 `cecfgver_6adceccbc1b48196805a0077b2262aa9`。此前的旧实例绑定阻碍已解除。

本次补齐了运行器对平台代理的显式账号绑定及 CA 信任。首次新增合成回归因只覆盖大写代理变量而失败；修正测试同时设置大小写变量后，最终结果如下。该次失败保留在私有测试产物中，不计为通过。

| 验证 | 新环境结果 |
| --- | --- |
| 协议及运行器回归 | 30 项通过，1 个按需客户端套件跳过；84.896 秒 |
| 配置后的三客户端 × Fusion/非 Fusion | 47 项通过，其中 42 个实际进程用例；0 失败、0 跳过；223.816 秒 |
| Python 运行器边界 | 8 项通过 |
| 真实 PAT 零生成 `agent-plan` | 54.990 秒通过：原生交换、模型发现、临时账号提交、公开 Key 目录与价卡 |
| 真实客户端生成 | 尚未执行；具体付费批次待确认 |
| OMP 默认裁剪 | 沿用已记录的 `devin_fusion_history_conflict` 失败；配置后的通过不覆盖此项 |

真实目录提供 239 个满足工具与上下文要求的候选，其中 193 个有可用本地价卡。计划选定 `deepseek-v4-flash-high` 为普通模型和 Lead，`deepseek-v4-1-flash-1017-low` 为 Sidekick；两者输入/输出价卡分别为每 token `$0.00000015` / `$0.00000060`，不代表供应商费用保证。完整计划与 Go JSON 事件分别保留在本地 `.cache/devin-e2e/newenv-agent-plan-final/agent-plan.json`、各运行目录的 `results.jsonl`。运行使用 Go 1.27.3，测试容器、网络和后端均已清理。真实生成次数为 0，PAT 与派生会话未写入归档产物。

本矩阵不证明 OMO Sisyphus 外层派发、OMP `task` 子 Worker、两个 Worker 交错或完整开发工具链。它们应追加使用 [PR #17 的两个开发 smoke](https://github.com/SteinX/sub2api/blob/f2de531383945df2fb0b456ab9c417ee013bdebb/tools/fusion-eval/LOCAL_SMOKE.md)，并观察真实派发与文件断言；不能将 primary probe 的通过当作 Worker 通过。

模型已实际执行后的中断、未知结果核对、带副作用工具去重、compact/fork、动态 MCP、图像和客户端 UI 应单列批次。现有协议测试覆盖原生输出前后错误和不重放规则，但不能冒充这些真实客户端场景的执行记录；未知结果须停止并核对原会话和账务后再决定重试。

仓库还提供 Codex Responses 与 Claude Code Messages 配置。候选固定版本已解析为 Codex `0.164.0`、Claude Code `2.1.310`，参考模板在工具目录；这两个 CLI 尚未计入本次实际进程矩阵。只有在完成对应版本、隔离运行和工具/会话合同验证后，才能纳入真实验收，不能据协议探针声称普遍 agent 兼容。

CI 自动跑协议 mock 与真实客户端进程 mock，均不提供真实 PAT。手动 main 工作流可选择 `suite=agents`、`plan/live` 和批次；`live` 仍要求显式 `allow_paid`。归档仅含结构结果、版本/二进制来源、模型计划、角色数量/用量状态及会话哈希；不上传原始 stdout、思考、工具参数/结果、客户端会话文件或后端私有日志。
