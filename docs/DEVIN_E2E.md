# Devin 云端 E2E

测试环境在云端从当前 checkout 编译生产后端，使用独立 PostgreSQL、Redis 和临时应用配置。通过正式 HTTP API 创建管理员测试会话、Devin 分组与账号、普通用户及该分组的 API Key，再访问 `/v1/*` 公开接口。不会连接已有 Sub2API 实例，也不使用 `SUB2API_API_KEY` / `SUB2API_USER_TOKEN`。

## 两种验证边界

| 模式 | 实际运行 | 能证明什么 |
| --- | --- | --- |
| `mock` | 当前生产后端 + PostgreSQL + Redis + 原生 Connect/protobuf HTTP fixture | 当前代码的认证、分组权限、公开 Key、模型同步、调度、协议转换、用量持久化及重启路径可协同工作 |
| `live` | 相同隔离后端与数据库，直连独立 Devin 测试账号 | 所选模型与账号在本次执行时能经过公开网关完成真实 Devin 推理 |

Mock 不证明真实 Devin 可用；测试已有远端 Sub2API 只能证明该远端版本，不能验证当前 checkout。管理员账号探测成功也不能替代公开 Key 和分组调度测试。

## 启动与测试

需要 Linux、Bash、Docker Engine（本地 `/var/run/docker.sock`）、Docker Compose v2，以及 `backend/go.mod` 要求的 Go 版本。测试不要求前端、额外 Python 包或真实账号。Docker 可拉取 `postgres:18-alpine`、`redis:8-alpine`；Go 模块需可下载。

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

- 管理 API 同步 Devin 模型后，普通 Key 的 `/v1/models` 可发现模型。
- `/v1/responses`、`/v1/chat/completions`、`/v1/messages` 的 JSON 和 SSE 结果；要求生成文本、完成事件及 JSON 用量有效。
- 公开 WebSocket Responses 预热不发起推理；function 工具调用、工具结果与 `previous_response_id` 续接保留原生会话。
- 长期 Key 在输出前收到 401 时只恢复一次；无效公开 Key 和无权绑定的独占分组被拒绝。
- 公开请求用量最终持久化到该用户和 Key；重启后依然能从数据库和调度快照处理公开请求。
- 自定义与推断 Fusion 配对经过公开 Key 调度可完成 Lead 委派、Sidekick 报告、Lead 审查三次原生调用，两个角色使用独立上下文；使用显式合成价格验证计费路径。
- 原生 `invalid_argument` 在输出前返回 HTTP 错误，在部分流式输出后返回终止失败，不会报告成功或重放推理。

合成 fixture 只存在于 `devine2e` build tag 的测试包，不进入发布二进制。它使用仓库的 protobuf schema，因此不能独立发现该 schema 与真实上游同时发生的偏差；需要 live 验证补足。此套测试尚不覆盖浏览器 OAuth 登录、图片、Fusion 故障恢复与后台压缩、完整的 OMO/OMP 客户端或所有客户端 UI。

## 真实 Devin 上游

为云端测试准备独立的 Devin 测试账号及可撤销凭据、选择有权限的真实模型 UID，并明确允许使用其额度。不要复用用户侧服务或从现有服务导出账号凭据。

需要将 `DEVIN_E2E_CREDENTIAL` 通过秘密管理注入测试进程；`DEVIN_E2E_CREDENTIAL_TYPE` 为 `apikey`（默认，长期 Key）或 `oauth`（导入会话）。`DEVIN_E2E_MODEL` 必须明确设置，测试不猜测模型。`DEVIN_E2E_UPSTREAM` 可覆盖默认的 `https://server.codeium.com`。

```bash
# 凭据由 Secret 注入；不要把真实值写进命令历史、仓库或报告。
export DEVIN_E2E_MODEL='<测试账号目录中的模型 UID>'
export DEVIN_E2E_CREDENTIAL_TYPE=apikey
make test-devin-live
```

Live 执行模型同步与六次推理请求（三种 HTTP 协议 × JSON/SSE），正常网关恢复机制可能重试失败请求。每次默认限制 1024 个输出 token，可通过 `DEVIN_E2E_MAX_OUTPUT_TOKENS` 在 1–32768 范围调整，以适配所选模型的输出要求。会消耗真实上游额度；无需为每次 PR 自动调用真实账号。使用独立低额度账号控制成本。模型必须支持三种网关适配需要的基础文本能力。

托管云环境应允许访问 `server.codeium.com`，以及上游实际返回的 API server 域名。仅放行 `static.devin.ai` 不能访问原生 RPC。浏览器授权额外涉及 `api.devin.ai`，不属于本套测试。保留平台提供的代理、凭据域名绑定与 CA 验证；域名被策略拒绝时需要通过环境配置流程更新，不能通过关闭代理绕过。凭据若是代理注入占位符，必须能在正确目标域名的请求中注入，不能把限定到远端 Sub2API 的 Key 当作 Devin 凭据。

## CI

`Devin E2E` workflow 对后端改动的 PR 和 main push 自动运行 mock；不向 PR 提供真实凭据。

如需 live，在 GitHub Environment `devin-e2e-live` 中配置 secret `DEVIN_E2E_CREDENTIAL` 和 variable `DEVIN_E2E_MODEL`（可选 `DEVIN_E2E_CREDENTIAL_TYPE`），按团队要求配置审批人与分支限制，然后在 main 手动选择 workflow 的 `live` 模式。本次代码添加不会自行创建这些持久配置或触发付费测试。

## 结果与停止

默认输出目录为 `.cache/devin-e2e/<UTC 时间>-<进程号>/`，可通过 `DEVIN_E2E_ARTIFACTS` 指定绝对路径：

- `results.jsonl`：Go 测试事件，明确包含 mock/live 模式；测试失败会返回非零退出码。
- `provenance.txt`：checkout commit、未提交文件数、Go 版本和后端二进制 SHA-256。
- `backend-private.log`：仅本地排查的私有日志。不要上传或分享；可能包含应用运行诊断。
- `server`：本次从源码构建的后端。

CI 只上传测试结果和 provenance，不上传后端日志、二进制、应用配置或数据库。临时管理员密码、JWT 和用户 Key 不打印；临时应用目录由 Go 测试清理。该目录和本地构建产物默认不纳入 Git。

正常退出或 Ctrl-C 会停止后端并删除本次 Compose 容器和网络。若进程被强制杀死，先用 `docker compose ls` 确认本次 `devin-e2e-<进程号>` project，再清理该 project：

```bash
DEVIN_E2E_DB_PASSWORD=unused docker compose -p devin-e2e-<进程号> -f deploy/docker-compose.devin-e2e.yml down --volumes --remove-orphans
```

清理命令只针对确认的测试 project。可按需删除本次私有结果目录。
