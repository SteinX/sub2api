# Devin 云端 E2E

测试环境在云端从当前 checkout 编译生产后端，使用独立 PostgreSQL、Redis 和临时应用配置。只需安全配置一个 Devin PAT：测试通过正式授权 API 验证 PAT、发现账号模型并提交本地 Devin 账号，自动创建分组、普通用户及公开 API Key，再访问 `/v1/*`。不要求手工提供模型 UID、组织 ID 或用户邮箱。不会连接已有 Sub2API 实例，也不使用 `SUB2API_API_KEY` / `SUB2API_USER_TOKEN`。

## 两种验证边界

| 模式 | 实际运行 | 能证明什么 |
| --- | --- | --- |
| `mock` | 当前生产后端 + PostgreSQL + Redis + 原生 Connect/protobuf HTTP fixture | 当前代码的认证、分组权限、公开 Key、模型同步、调度、协议转换、用量持久化及重启路径可协同工作 |
| `plan` | 相同隔离后端与数据库，真实 PAT 授权、添加本地账号和模型发现；不生成内容 | 本次 PAT 能通过原生 CLI 会话交换，并完成账号添加、模型选择和公开 Key 目录校验 |
| `live` | 相同隔离后端与数据库，直连独立 Devin 测试账号 | 所选模型与账号在本次执行时能经过公开网关完成真实 Devin 推理 |

Mock 不证明真实 Devin 可用；测试已有远端 Sub2API 只能证明该远端版本，不能验证当前 checkout。管理员账号探测成功也不能替代公开 Key 和分组调度测试。

## 启动与测试

需要 Linux、Bash、GNU coreutils `timeout`、Docker Engine（本地 `/var/run/docker.sock`）、Docker Compose v2，以及 `backend/go.mod` 要求的 Go 版本。协议测试不要求前端、额外 Python 包或真实账号。Docker 可拉取 `postgres:18-alpine`、`redis:8-alpine`；Go 模块需可下载。

```bash
cd /workspace/sub2api
make test-devin-e2e
```

当前托管环境已有工具时，可以先激活并使用对应工具链：

```bash
source /workspace/.sub2api-env/activate.sh
GOTOOLCHAIN=go1.27.3 GOMAXPROCS=2 GOFLAGS=-p=2 make test-devin-e2e
```

`deploy/devin-e2e.sh` 自动启动依赖、等待健康检查、编译当前后端、执行测试和关闭依赖。每次使用不同 Compose project，数据库在 tmpfs 内，端口只绑定到随机 loopback 端口；不复用开发环境的数据或固定的 8080/5432/6379 端口。测试以 `standard` 模式运行，包含真实分组权限和用户余额检查。

Mock 覆盖：

- 通过 `/admin/devin/auth-sessions` 的 Key 授权和 `/commit` 完成与页面相同的新账号添加流程；普通 Key 的 `/v1/models` 可发现该账号获授的模型。
- PAT 会话交换被拒绝、模型发现被拒绝或目录为空时不提交账号、不发起推理；错误只保留已知 RPC 阶段，不打印凭据和上游原始文本。
- `/v1/responses`、`/v1/chat/completions`、`/v1/messages` 的 JSON 和 SSE 结果；要求生成文本、完成事件及 JSON 用量有效。
- 公开 WebSocket Responses 预热不发起推理；function 工具调用、工具结果与 `previous_response_id` 续接保留原生会话。
- 长期 Key 在输出前收到 401 时只恢复一次；无效公开 Key 和无权绑定的独占分组被拒绝。
- 公开请求用量最终持久化到该用户和 Key；重启后依然能从数据库和调度快照处理公开请求。
- 自定义与推断 Fusion 配对经过公开 Key 调度可完成 Lead 委派、Sidekick 报告、Lead 审查三次原生调用，两个角色使用独立上下文；使用显式合成价格验证计费路径。
- 原生 `invalid_argument` 在输出前返回 HTTP 错误，在部分流式输出后返回终止失败，不会报告成功或重放推理。

合成 fixture 只存在于 `devine2e` build tag 的测试包，不进入发布二进制。为了验证正式新账号流程使用的默认原生地址，mock 配置一次性的本地账号代理，并让后端在保留现有 CA 的基础上信任临时测试 CA；TLS 校验保持开启，连接只到本地 fixture。plan/live 不使用这套代理或测试 CA。fixture 使用仓库的 protobuf schema，不能独立发现该 schema 与真实上游同时发生的偏差；需要真实验证补足。上述协议套件尚不覆盖浏览器 OAuth 登录、图片、Fusion 故障恢复与后台压缩或客户端 UI；另有[真实客户端进程矩阵](DEVIN_AGENT_E2E.md)，覆盖固定版本 OpenCode、OMO、OMP 的短流程。

## 真实 Devin 上游

使用独立测试身份的现有 PAT，通过安全配置提供 **`DEVIN_E2E_PAT`** 运行时凭据引用，测试会自动关联该凭据对应的原生账号并走完账号添加流程。若平台明确支持对该原生请求的凭据替换，也可使用平台提供的代理占位符；测试不自行提取真实 Secret。不要把真实 PAT 写进命令、聊天、仓库或报告；测试不会创建外部 Devin 用户，不会导出或修改用户侧服务的凭据。旧变量 `DEVIN_E2E_CREDENTIAL` 暂保留为兼容别名。

[官方鉴权说明](https://docs.devin.ai/api-reference/authentication)和 [PAT 说明](https://docs.devin.ai/api-reference/personal-access-tokens)区分人类 PAT 与服务用户 Key；两者都使用 `cog_` 前缀。前缀不能证明能通过 Sub2API 所用的原生 CLI 会话交换。测试会实际经过 `GetSelfDevinSessionToken` 和 `GetCliModelConfigs` 验证，遇到 401/403 或空目录立即报告不兼容的阶段，不会改用服务用户、浏览器会话或猜测其他接口。旧 `apk_*`、`apk_user_*`、CLI JWT 和 PAT 不是可互换的凭据。

Key 模式明确使用原生 Key 交换，不再根据运行时引用是否有 `cog_` 前缀或包含句点来改判成 CLI JWT。凭据文件的 `import` 模式仍保留原有类型推断。测试只验证引用存在且没有换行，不据此声称 Secret 缺失、有效或已获权限。

合成引用用例只证明引用能按 Key 模式原样进入原生交换请求，不证明平台代理会替换请求头或 JSON 中的引用；传输替换能力仍需平台契约和实际接入验证。

### 托管域名 Secret 与版本绑定

分别核对三个状态：保存并发布的环境版本、当前实例的 `source_config_version_id`，以及该实例实际就绪的凭据引用和出站策略。发布成功不意味着既有实例已经切换版本；进程中没有 `DEVIN_E2E_PAT` 也不能证明域名 Secret 未配置。缺少引用时，脚本会在启动前报告当前绑定不可用，而不要求导出 Secret 明文。

仅使用平台文档和绑定元数据提供的引用，通过继承的代理和 CA 发送标准原生请求。绑定到某个域名，不足以自行推断它会自动注入哪个 HTTP 头、替换 JSON metadata 或提供哪个进程变量。当前实例没有相应引用或注入契约时停止并报告；不读取 Secret 存储文件取值、不猜测占位符、不手工复制凭据、不关闭代理或 TLS。目标域名的网络放行与凭据绑定也应分别核实。

若当前实例仍绑定旧版本，应使用平台明确支持的原地应用/重新绑定流程保留原工作区。没有这样的操作接口时报告能力缺失，不重复发布、不创建替代任务或重建工作区来绕过用户控制的环境编辑流程。

先完成凭据传输与临时存储范围的确认，再运行不发起推理的 plan：

```bash
# 唯一必需的 Secret DEVIN_E2E_PAT 已由秘密管理注入。
make test-devin-plan

# 审阅 smoke-plan.json 的所选模型和 token 上限，获得付费调用授权后：
DEVIN_E2E_ALLOW_PAID=true make test-devin-live
```

模型从已添加账号的目录自动选择：排除禁用、容量受限、Fusion、未知 harness、需要 `AssignModel` 的路由和输出上限不足的模型；优先选择正数、有限的 `credit_multiplier` 最低者，同价按 UID 排序。零值或缺失价格视为未知，全部未知时按 UID 稳定选择并明确报告未知价格，不声称免费。`DEVIN_E2E_MODEL` 仅用于人工定向复现时覆盖选择，仍必须属于该账号可用的直接模型，不是必填项。该策略依据账号目录中的相对 credit 倍率，不将其解释成美元报价或保证最低实际费用。

Live 执行六个网关推理请求（三种 HTTP 协议 × JSON/SSE），正常网关恢复机制可能重试失败请求。每次默认限制 1024 个输出 token，可通过 `DEVIN_E2E_MAX_OUTPUT_TOKENS` 在 1–32768 范围调整。必须显式设置 `DEVIN_E2E_ALLOW_PAID=true`，否则在依赖启动或 PAT 传输之前退出。输出上限与请求数都不是供应商金额硬上限；付费授权与低额度测试账号仍需由用户安排。plan 不发起生成请求，但会进行真实认证和模型发现，不能在没有凭据传输授权时运行。

### 传输目标与存储范围

PAT 经 loopback 送入临时 Sub2API 的授权 API；该实例通过 HTTPS 将 PAT 放入 `X-Api-Key` 和 JSON metadata，发送到 **`server.codeium.com`** 的 `/exa.seat_management_pb.SeatManagementService/GetSelfDevinSessionToken`。交换得到的会话凭据用于同域的模型目录 RPC，以及 live 模式的后续原生请求。此流程无需用 PAT 调用 `api.devin.ai/v3/self`、枚举组织或创建外部账号。

PAT 和派生会话只暂存在本次隔离实例的 PostgreSQL tmpfs 和 Redis 内存；测试退出后删除容器。凭据不写入模型计划和 CI 归档，后端日志按已知凭据及敏感字段脱敏，测试结果不输出授权响应正文。真实 PAT 不通过命令参数传给后端进程，也不作为该进程的继承环境变量；它只通过正式授权 API 交给隔离实例。原始 PAT 不发送到已有远端 Sub2API。

托管云环境应允许访问 `server.codeium.com`，以及上游实际返回的 API server 域名。仅放行 `static.devin.ai` 不能访问原生 RPC。浏览器授权额外涉及 `api.devin.ai`，不属于本套测试。保留平台提供的代理、凭据域名绑定与 CA 验证；域名被策略拒绝时需要通过环境配置流程更新，不能通过关闭代理绕过。凭据若是代理注入占位符，必须能在正确目标域名的请求中注入，不能把限定到远端 Sub2API 的 Key 当作 Devin 凭据。

## CI

`Devin E2E` workflow 对后端改动的 PR 和 main push 自动运行 mock；不向 PR 提供真实凭据。

如需真实接入，在 GitHub Environment `devin-e2e-live` 中配置唯一必需的 secret **`DEVIN_E2E_PAT`**，按团队要求配置审批人与分支限制。main 手动选择 `plan` 可完成授权、添加账号和发现；选择 `live` 还必须勾选 `allow_paid`，否则不启动真实作业。PR、push 和默认手动操作只运行 mock。本次代码添加不会创建这些持久配置、生成 PAT 或触发真实传输。

## 结果与停止

默认输出目录为 `.cache/devin-e2e/<UTC 时间>-<进程号>/`，可通过 `DEVIN_E2E_ARTIFACTS` 指定绝对路径：

- `results.jsonl`：Go 测试事件，明确包含 mock/live 模式；测试失败会返回非零退出码。
- `provenance.txt`：checkout commit、未提交文件数、Go 版本和后端二进制 SHA-256。
- `smoke-plan.json`：自动发现的模型、相对 credit 倍率（若已知）、请求数、输出上限及付费开关；明确没有供应商金额硬上限。
- `backend-private.log`：脱敏后的本地诊断日志。仍不上传或分享。
- `server`：本次从源码构建的后端。

CI 只上传测试结果、provenance 和模型计划，不上传后端日志、二进制、应用配置或数据库。临时管理员密码、JWT 和用户 Key 不打印；临时应用目录由 Go 测试清理。该目录和本地构建产物默认不纳入 Git。

正常退出或 Ctrl-C 会停止后端并删除本次 Compose 容器和网络。若进程被强制杀死，先用 `docker compose ls` 确认本次 `devin-e2e-<进程号>` project，再清理该 project：

```bash
DEVIN_E2E_DB_PASSWORD=unused docker compose -p devin-e2e-<进程号> -f deploy/docker-compose.devin-e2e.yml down --volumes --remove-orphans
```

清理命令只针对确认的测试 project。可按需删除本次私有结果目录。
