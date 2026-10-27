# Devin 用户旅程与发布验收

此前协议测试、手写客户端配置和内部工具执行器的成功，未覆盖用户从页面复制配置到实际客户端完成任务的路径。旧文件名和 `devine2e` build tag 为兼容保留；名称不再代表验收级别。真实上游可用也不等于产品用户旅程已通过。

| 层级 | 入口和实际执行 | 可以证明的范围 |
| --- | --- | --- |
| 协议合同 | 编译生产后端；临时 PostgreSQL/Redis；手写公共 HTTP/WebSocket 请求 | 认证、分组、调度、协议、预留和计费合同。`live` 仍不是客户端 E2E |
| 客户端合同 | 固定版本 CLI；运行器编写配置；公共 Key；模拟或真实上游 | 指定 CLI 的短协议行为，不覆盖产品配置交付 |
| `client_mock` | 完整产品应用：登录表单、Dashboard、Key 列表、选择 Key、Use Key 发现模型与复制；同一配置交给真实 CLI；公共 Key、分组、模拟模型；独立校验项目产物 | 完整本地产品页面到客户端的回归；上游仍为模拟，不证明真实模型行为 |
| `user_e2e` | 部署产品入口及其配置；真实公共 Key 和实际 group/channel；真实 CLI 和获授权真实模型；项目产物、协作及会话证据 | 只有这一层可签发真实用户 E2E 验收凭证。当前自动 runner 不产生这种凭证 |

## 可重复的项目旅程

```bash
npm ci --prefix tools/devin-e2e-clients --ignore-scripts --omit=optional --no-audit --no-fund
pnpm --dir frontend install --frozen-lockfile
make test-devin-user-journeys-mock
make test-devin-acceptance
```

需要 Linux x64、Node 24、Python 3、Go、Docker Compose、pnpm 9 和已安装的 Chromium/Chrome。Playwright 只驱动已有浏览器；可显式设置 `CHROMIUM_EXECUTABLE_PATH`，不下载或放宽浏览器安全设置。工作区及配置均为临时目录。root 运行时项目代码、客户端和浏览器使用低权限用户；普通用户使用下述受限容器，个人 HOME 不挂载且不可写。保留环境代理、CA 和网络限制。

浏览器加载正常 Vite 构建的完整应用，提交真实登录表单，经正常路由进入 Key 列表并选择 Key，再由生产 `UseKeyModal` 调用公共模型目录 API。没有组件挂载 harness、fetch mock、注入 Pinia 或注入登录态。它选择模型、环境变量 Key、项目工作区、精确测试命令和 OMP 模式，然后点击真实复制按钮，核对剪贴板与页面内容相同。运行器只落盘这些文件；不会修补配置。模型上游为脚本化决策，工具由真实客户端执行。

目录诊断只匹配同源 GET `/v1/models` 且带 `client_version` 的请求；Dashboard 的 `/api/v1/usage/dashboard/models` 不能作为目录响应证据。失败阶段区分弹窗、模型选择框、目标 option 和配置复制。仅保存请求、失败和取消计数、响应结构类型、DOM 存在/可见布尔值、选项数量和复制文件数，不保存任意错误消息、响应正文、请求头、完整 URL、HTML 或截图。没有这些观察项时，单个 HTTP 200 不能确定目录请求是否发出，更不能将缺少选项归因为模型授权或目录体积。合成 239 项目录和关闭后选 Key 打开弹窗的组件回归不证明真实失败根因，也不替代完整产品旅程。

合成工程有随机需求及 nonce、一个错误的折扣函数、受保护的测试和测试入口。每个用例先证明原工程测试失败，然后要求客户端读取、修复、运行测试和读取产物。独立 oracle 使用新的随机输入验证功能，不替客户端执行任务或生成验收产物。再次打开客户端必须有相同会话哈希，实际读取产物、正常终止；结束后重新核对保护文件、源文件及最终产物。OMP 的工具成功须绑定实际调用 ID、精确命令及新鲜的测试退出凭据；仅进程 exit 0、文本包含正确值或服务端 HTTP 200 均不够。

当前矩阵为 OpenCode/OMO × direct/Fusion，加 OMP × direct/Fusion × default/compatibility，共八个配置组合。OMP 默认 Fusion 的历史裁剪会破坏续接；页面默认提供显式兼容模式，选择不兼容组合会显示说明并停止生成配置。该项报告为 `unsupported_by_product` 且要求零推理；不是第八个任务通过。原默认配置的真实 CLI 失败保留在 [冻结的模拟上游证据](../tools/devin-acceptance/fixtures/omp-default-fusion-before.json)，绑定干净提交 `d44d08fd7ed794262638cb71de8e0b24edc45506`，任务成功但同一会话续接发生 409。

固定版本：OpenCode `1.19.4`、oh-my-opencode 插件 `5.1.44`（宿主 OpenCode）、OMP `18.12.6`、Bun `1.4.4`。这不是用户反馈里的 Senpi engine OMO；Senpi 的结果不可互相替代。模拟成员为 `swe-2-max` / `e2e-sidekick`，不是对线上 `swe-2-high` 或所有 effort 档位的验收。报告记录实际物理 UID、原生角色调用和隔离上下文哈希。

基础项目旅程证明正常关闭后重开同一会话。另可用 `DEVIN_JOURNEY_CAPABILITIES=true` 启动实际 OMP direct/compatibility RPC 探测：16 个多行合成文件，要求至少 128 KiB 实际进入原生模型历史，再检查 compact、空闲客户端 SIGKILL 后恢复和 fork。合成 token 用量随实际输入字节增长，并非供应商用量。失败阶段及未执行的后续能力分别保留；开启该探测时未完成必需能力会使测试失败。其他客户端、Fusion 的这些能力、worker 和在途断线仍不得用内部执行器或空闲进程恢复代替。关闭 OMP pruning 会增加历史占用，不能据此承诺长会话兼容。

OMP compact 的两项旧失败已定位到测试侧：OMP 18.12.6 对本地 `/compact soft` 返回 `response` 而不发送 `prompt_result`；旧探测等待了错误终态。默认 compact 同时生成主摘要和短摘要，旧测试准入层把第二个并行意图直接拒为 409，客户端显示 fetch 失败。项目准入现把并行意图排队，实际转发仍一次一个，请求和金额上限不增加。受控回归要求新压缩记录、摘要保留隐藏 nonce、重启恢复检查点、无工具回忆 nonce；默认 RPC 已通过，显式本地命令另作回归。不是改用内部 compact 执行器。

`DEVIN_JOURNEY_CAPABILITIES=true` 运行完整 OMP direct/compatibility 能力探测。`independent` 仅作有意的部分诊断，不可替代完整能力验收。诊断 fetch 观测只允许配置的 loopback 原点，保留原 fetch/代理/CA，对其他目标直接拒绝，且不读取请求头或提示正文。真实目标不会使用该 mock 诊断器。


## 价格与配置合同

以下是零付费合同，不归入 `user_e2e`：

- 动态价格目录缺失、为空、exact/canonical 分别存在、同时冲突以及更新后生效；明确物理 exact 条目优先。缺失 `swe-2-high` 价格仍拒绝，不创建其价格。
- 真实 Lead/Sidekick UID 的分组价格、渠道价格、错误 group、错误 platform、仅给虚拟 Fusion UID 定价；缺失成员返回具体物理 UID。
- 先预热公开目录，再添加、禁用、移除和恢复成员/配对；下一次用户发现读取当前授权快照，不等待第二层列表 TTL。
- 生产后端经真实管理配置路径更新 group 价格，再以已预热公共 Key 请求；价格失败必须在原生推理前发生。负例保留失败码，不能将普通模型系列回退价当 Fusion 的明确价格。

现有相关测试位于 `devin_fusion_pricing_test.go`、`devin_pricing_precedence_test.go`、`devin_model_discovery_test.go` 和 `internal/devine2e/gateway_test.go`。测试率仅作用于隔离数据，不是供应商 SWE 报价。

## 旧证据重评

`tools/devin-acceptance/reassess_legacy.py` 只读取已保存的脱敏 Go JSON 测试事件，不访问网络。原 42 个短用例均有旧合同通过记录，但旧运行器没有保留客户端正常停止、错误终态及产品配置证据，因此 **0 项可直接认证为用户 E2E**。六个发送前取消合同只有在同源父组最终通过、从而覆盖零转发断言时可复用；其余 36 项任务完成需要新证据。失败尝试和为恢复会话做的重复准备单独保留，不增加覆盖计数。缺证据不是已观察到功能失败，也不是通过。

## 发布门槛

`client-journeys.json` 绑定 commit/tree、编译后端 SHA-256、浏览器产物和依赖锁 SHA-256、客户端版本/可执行文件、规范化配置指纹、产物指纹，以及每个终态和实际工具证据。模拟 CI 只上传结构化 JSON，不上传配置、剪贴板、日志、截图、秘密或真实账号记录。

`acceptance.py verify --mode client_mock` 只检查模拟回归证据。`user_e2e` 还要求真实服务、部署产品入口、全部必需能力及价格证据；存在未验收或不支持的必需组合时不得宣称完整验收。默认 Fusion 的页面拦截也不能签发真实任务通过。

发布含 Devin 功能的版本时，`Release.prepare` 要求带验收注释的 annotated tag。`release_guard.py` 将测试 SHA/tree 与待发布源核对，重新读取全部分页的 exact-SHA Actions/check runs、commit statuses、PR reviews 和未解决 review threads。必需集合显式包含：

- `backend-ci.yml`：shell、test、frontend、golangci-lint、release-helpers。
- `security-scan.yml`：backend-security、frontend-security。
- `devin-e2e.yml`：mock、clients-mock、user-journeys-mock。

缺失检查、仍运行、失败、未解决评审、安全扫描未完成均阻止发布。准备阶段及每个外部发布步骤前都重新查询。只排除当前 Release 自身以避免循环等待；必需安全检查不能跳过。独立评审必须指向不可变 base/head，补丁哈希采用 `git diff --binary --full-index BASE HEAD | sha256sum`，发布时重新计算。发布注释是维护者的可审计声明，不是密码学证明；它不能代替运行证据、分支权限和独立评审。

使用已有 `gh` 认证读取快照：

```bash
python3 tools/devin-acceptance/github_snapshot.py --repository OWNER/REPO --pr NUMBER \
  --commit SHA --review independent-review.json --output private-current-state.json
python3 tools/devin-acceptance/acceptance.py tag-note --mode user_e2e \
  --report sanitized-real-journeys.json --state private-current-state.json --commit SHA --tree TREE
```

只有真实 E2E 完成并审核的结构化报告可用于该命令。当前没有生成器会把模拟报告升级成真实报告。历史上尚无 Devin 功能的版本继续沿用旧发布流程；dry run 可显示阻碍并继续构建，但不会发布。脚本不修改安全设置、权限、secrets 或 branch protection；读取权限不足即停止相关验收。

## 非 root 客户端隔离

UID 0 的既有路径通过把浏览器、CLI、工程代码降为 UID 65534 来隔离原生凭据和账本。普通用户不能仅取消这项检查：相同 UID 的未隔离进程可读取项目外文件和父进程环境。user namespace 或 Landlock 不可用时，不修改系统设置或请求提权来绕过限制。

非 root 路径使用环境已有的本地 Docker 权限运行受限客户端子容器。Daemon 没有改配置，也不声称它是 rootless Docker daemon。容器使用调用者 UID、`cap-drop=ALL`、`no-new-privileges`、只读根、独立 PID namespace 和 `network=none`，限制内存、CPU、PID 和输出。工程 oracle 的 stdout/stderr 在宿主共享 2 MiB 上限，容器原始日志禁用；超限终止并验证 CID 清理。只挂载当前合成工程及客户端状态目录为可写；锁定客户端、系统库、公开 CA 和 Chromium 配置只读。个人 HOME、Docker socket、原生 PAT、数据库／Redis 密码、批准文件、账本和私有 registry 不挂载或传给模型。

浏览器在独立容器内用 stdin 收到临时 UI 登录信息，其 profile 留在容器临时存储，退出时删除。CLI 不获得 UI 密码或浏览器认证状态；工程测试和 oracle 也在隔离容器运行。容器没有普通网络出口，只经唯一 Unix socket 连接可信父进程的固定产品 UI relay；容器内仍使用产品复制出来的原始 localhost 配置，不手写或修补它。原生服务继续由宿主后端通过原代理／CA 及 HTTPS 单域限制访问。

准备过程为 `python3 tools/devin-e2e-clients/container_sandbox.py prepare`。它使用固定公开镜像 digest，验证实际命名空间、私有文件／父进程不可见、HOME 不可写和 capabilities 边界。真实入口在接触原生凭据前要求这项探测通过；只准备沙箱不授权认证或生成。镜像缺失、Docker 不可用或探测失败时停止，不回退到同 UID 裸进程。需要 Linux x64、同文件系统的本地 Docker daemon 和已有 Node/Chromium。

公开客户端依赖通过 `npm ci --ignore-scripts --omit=optional` 预先安装，临时 OpenCode 目录复用锁定的只读包缓存，不改写产品复制的配置，也不允许模型进程联网下载。OMO 还必须观察本次进程的插件启动记录；仅安装包、配置含插件名或宿主完成任务不能算插件通过。

容器先 `create`，持久登记完整 CID 和随机 scope、检查限制后才 `start`。进程退出／超时的清理核对容器真实状态，父进程异常时仍按未挂载的私有 registry 清理，仅操作匹配 scope 的对象。后台工具不能靠 Docker 客户端退出逃逸。宿主读写项目采用有界、不跟随链接的目录句柄；尤其压力文件创建也不能通过模型留下的 symlink 越出工程。

## 明确的用户确认零价

用户于 2026-10-25 确认 SWE-2 免费；此为 `user_confirmed` 事实，不是官方价卡已核实。当前映射依据是已认证目录中明确出现 `swe-2-high`。`price_facts.py` 只为这个精确 UID 生成临时分组 token 零价规则及 provenance，不给未知 UID、相邻型号或付费 Lead 继承零价，也不修改全局／生产定价。

后续计划可附 `user_price_facts`，须与明确零价快照及其 `group_pricing_sha256` 一起绑定批准；执行时仍重新验证实际授权目录。缺失价格保持 fail-closed，显式零价则应 `identified=true`、`calculable=true` 且金额为零。零价不取消请求数、时间、工具、交接、用量与预算保护；Fusion 的付费 Lead 必须继续单独计算。新的代码 SHA 和快照均需新批准，不能沿用绑定 b19b6dd 的旧预检批准。

## 可执行的真实上游入口（需要逐次批准）

入口为 `bash deploy/devin-user-journeys.sh plan|live`。`authorized_journey.py` 先验证批准、计划哈希、干净的最终 SHA/tree、精确目标域名、模型和预算，再构建产品、启动 Docker 或接触原生服务。`plan` 也会传输原生凭据并在临时数据库保存派生凭据，因此同样需要单独批准；它仅做授权、目录、实际价格检查及产品页面配置交付，账本禁止任何生成。每次批准只对绑定的源码和快照有效；失败的 plan 不算通过，旧批准不能用于新提交的重跑。边界回归要求缺少批准、凭据或精确域名时没有启动子进程。

先在私有文件准备以下计划。数值是 runner 支持的上界示例，不是已经批准的额度，应按实际用例减少。物理 UID 必须来自实际授权目录。`reference_pricing_sha256` 绑定预先审阅的公共价卡；若需要当前真实分组价格，另加 `group_pricing_sha256`，并提供仅含价格规则数组的对应快照。缺价则停，不创建猜测价格。

```json
{
  "schema_version": 1,
  "source": {"commit": "FINAL_COMMIT", "tree": "FINAL_TREE"},
  "product": "isolated_complete_preview", "dataset": "synthetic_price_project_v1",
  "upstream_origins": ["https://server.codeium.com"],
  "models": {"direct": "swe-2-high", "lead": "claude-opus-5-5-medium", "sidekick": "swe-2-high"},
  "clients": ["opencode", "omo", "omp"], "omp_capabilities": "true",
  "reference_pricing_sha256": "REVIEWED_SHA256",
  "stop_on_failure": true, "retry_after_uncertain_send": false,
  "limits": {"public_requests": 144, "output_tokens": 1024, "wall_seconds": 3600,
    "turn_seconds": 300, "internal_calls_per_turn": 64, "tools_per_turn": 20,
    "handoffs_per_turn": 2, "parallelism": 1, "local_usd": 3, "turn_usd": 0.5}
}
```

明确取得批准后，维护者记录批准文件：`schema_version:1`、`approved:true`、唯一 `approval_id`、`mode`、24 小时内的 `expires_at`、规范化计划 `plan_sha256`，以及 `native_auth`、`temporary_credential_storage`、`local_preview_accounts` scopes；`live` 另需 `model_generation`。这是维护者可审计的操作联锁，不是授权签名。每个批准 ID 固定使用 `.cache/devin-approved-runs/<ID>.json`，独占创建，不能更换产物目录重置。新 ID 需要新的批准，不能用于重试不明请求。

批准后才可安全注入原生凭据引用，设置 `DEVIN_JOURNEY_PLAN_FILE`、`DEVIN_JOURNEY_APPROVAL_FILE` 和 `DEVIN_E2E_ARTIFACTS`（新的私有目录）；有批准快照时再设置 `DEVIN_JOURNEY_GROUP_PRICING_FILE`。真实值不放到命令行或配置文件。启动器保护原生凭据和账本；root 路径降为低权限用户，普通用户路径使用受限容器，保留平台代理和 CA。`--validate-only` 不会启动服务或生成验收证据。

所有用例共用同一持久账本，以及一个临时公共 Key 的真实本地配额预留。HTTP/SSE 错误、缺正常终态、发送不明、客户端失败、用量读取失败或预算耗尽均停止，不自动重试；SDK 后续请求也无法越过已停止账本。主摘要和短摘要意图排队，但合计计数，实际转发并发仍为一。退出清理客户端、后端及临时 SQL/Redis；结构化记录仅留私有目录。本地用量不是供应商扣费硬上限。

direct 和 Fusion 转发都安装批准的单 turn deadline。direct 每次转发前使用相同公共 Key 查询当前有效价，保守地分别按全部授权上下文计入 input/cache-read/cache-write 并加最大输出；估价超过单 turn 或累计剩余额度时不发推理，结算增量异常也停止。这种估价可能提前拒绝实际较短的请求，不能为凑覆盖放宽保护。临时 Redis 使用随机认证；密码不进入浏览器、CLI 或工程环境。父进程记录客户端进程组及启动身份，超时先发送 TERM 让 Python 清理，再处理可核实的进程组；身份不明时报告清理失败，不能当验收成功。

当前真实 runner 实现基础项目、同会话续接及 OMP direct 的 fork、压力、compact、空闲进程恢复；三客户端 Fusion 高级能力、worker、在途断线和 Senpi 尚未实现。输出明确为 `product_live_partial`，不会自动签发 `user_e2e` 或放行发布。隔离预览中的真实价格也不能证明现有部署的 group/channel 正确。后续新增能力应先完成可复现实现，再单独申请真实请求预算。

本地 `/compact soft` 的 ACK 只证明入队，与同步 compact RPC 的终态不同。探测在原时间边界内查询同一次操作是否结束，再验证新检查点，不重复发送压缩请求。诊断文件损坏不会跳过进程清理；mock fetch 诊断禁止自动跟随重定向。

## 待批准的真实用户链路计划

当前阶段未授权新一轮真实生成或线上写入。部署管理 Token 已过期，实际 group/channel 的 SWE 规则仍未核明；这些步骤暂停，不能以模拟价格替代。应先安全恢复只读访问并核实两个真实成员的价格来源、group/channel 生效范围和服务版本；不输出或复制秘密。

所需访问与凭据一次汇总如下；值只由秘密管理注入，不写入聊天、CLI 参数、配置文件或日志：

| 用途 | 必需访问/凭据 | 传输及持久化边界 |
| --- | --- | --- |
| 部署现状和真实价格前置核验 | 刷新的 `SUB2API_USER_TOKEN`；沿用现有权限，只做获准只读操作 | 仅现有 Sub2API 管理服务；不改价格、分组或权限 |
| 现有公开调用方对照 | 现有 Devin 分组 `SUB2API_API_KEY` | 仅其绑定的 Sub2API 服务；客户端使用环境变量引用 |
| 最终代码的真实用户 E2E | 经审阅的临时完整产品预览实例；`DEVIN_E2E_PAT` 安全引用；一次性 UI 测试用户及公共 Key | 需另行批准在 loopback 临时实例添加本地账号、分组和真实价格配置；PAT/派生会话只在临时 SQL/Redis，真实上游仍仅 `server.codeium.com`；不把现有线上 Key/Token 转发到预览实例 |
| 发布前当前检查和评审 | Actions 已有 GitHub token 的读能力 | 只读 checks、Actions、statuses、PR reviews/review threads。权限不足时拒绝验收，不能改为过滤后的快照或复制其他凭据 |

预览使用完整产品门户入口；实际业务路由与价格规则须在开始前核实。若改在已有线上实例验收最终代码，还需要独立部署/线上写入授权，本次不包含该授权。Senpi engine 的 OMO 二进制/分发来源尚待核实，不可用 oh-my-opencode 插件的结果替代。

后续需单独审阅的批次建议：七个已支持配置各至多十个公开请求，三客户端 Fusion 长旅程各至多二十四个，共最多 142；如两个普通 HTTP 对照也需要重验，合计上限为 144。单并发、每次最多输出 1024 token、整批 60 分钟、本地累计金额最多 $3，单 turn 保留 64 内部调用/20 工具/2 交接/300 秒/$0.50 的上界。只向批准的 Sub2API 实例及 `server.codeium.com` 发送合成工程。目标优先锁定反馈中的 `claude-opus-5-5-medium` / `swe-2-high`，但必须先核实真实授权和明确价格；缺价或需换模型即停在零生成前置阶段。尚未取得 Senpi 的覆盖不能列为通过，若新增其独立矩阵要另列请求数后批准。首次失败、预算拒绝或发送结果不明即停，不自动重跑，不重置累计预算。已有有效证据先复用，不为凑覆盖重发。上述是未来批准提案，不是现有授权，也不是供应商扣费硬上限。
