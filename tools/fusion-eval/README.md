# Fusion 客户端探针与个人评测准备

本目录区分三步：**本地 stub 合同验证 → 用户本机真实 Fusion smoke → 冻结 A/B 评测**。已完成本地客户端探针，继续提供本机 smoke 运行器和离线费用工具；没有在云端启动收费评测。部署、恢复合同及后续验收矩阵见 [Fusion 说明](../../docs/DEVIN_FUSION.md) 和 [个人接入计划](../../docs/DEVIN_FUSION_OMO_OMP_PLAN.md)。

当前选择 **Linux 远端执行**，从 [远端 smoke 操作说明](LINUX_REMOTE.md) 开始；[macOS 操作说明](LOCAL_SMOKE.md) 保留为备用。`prepare` / `inspect` 不调用模型；`run` 启动一次真实客户端，并保存独立文件验收、派发和已观察费用的证据。运行结果仍保留费用完整性未知，不能自动作为冻结 A/B 成绩或释放未知预留。

## 固定版本与已有证据

2026-10-23 从官方 npm `latest` 解析并安装以下版本；完整 tarball URL、integrity 和结果范围在 [versions.json](versions.json)：

| 组件 | 固定版本 |
| --- | --- |
| OpenCode `opencode-ai` | 1.19.4 |
| OMO `oh-my-opencode` | 5.1.44 |
| OMP `@oh-my-pi/pi-coding-agent` | 18.12.6 |
| Bun | 1.4.4 |

`oh-my-openagent` 的 npm `latest` 同为 5.1.44，但本次加载的是 `oh-my-opencode` 包，不混称两种安装形态。探针运行环境为 Linux x64。

| 实际运行的客户端 | 已验证 | 结构证据 |
| --- | --- | --- |
| 纯 OpenCode，`--pure` | SSE → 本地 read → 工具结果回传 → 最终文本；同次工具续接的 `x-session-id` 一致 | [pure fixture](fixtures/opencode-1.19.4-pure.jsonl) |
| OMO 插件 + OpenCode | 同上；`--continue` 保留会话身份，新会话身份不同 | [OMO fixture](fixtures/omo-5.1.44-opencode-1.19.4.jsonl) |
| 原版 OMP，无扩展 | SSE / read 闭环通过；`models.yml compat.sessionHeader` 没有产生会话头 | [OMP baseline](fixtures/omp-18.12.6-baseline.jsonl) |
| OMP + 本目录公开扩展 | read 闭环、`--continue`、新会话；动态 `x-litellm-session-id` 同会话稳定、新会话不同 | [OMP extension fixture](fixtures/omp-18.12.6-extension.jsonl) |

这些是**真实客户端连接假 API**的证据；stub 没有 Fusion 编排、真实模型或真实计费。自定义 probe agent 只声明 read，不能据此声称真实 OMO 派发、编辑/命令工具、动态 MCP、compact、fork、双 Worker 或工具执行去重已验收。OMP 注入的必填 `i` 字段也已由实际出站 schema 记录。

补充重采样还记录了 system/instructions 和 tool choice 的摘要：[OMO contract](fixtures/omo-5.1.44-opencode-1.19.4-contract.jsonl)、[OMP extension contract](fixtures/omp-18.12.6-extension-contract.jsonl)。两客户端各一次 read 往返的 instructions / tools / tool choice 摘要均相同；早期 fixture 没有 instructions / tool choice 摘要，不代表完整合同稳定。真实 Worker 的动态系统提示仍需单独观察。

OMO 的启动迁移会无条件申请 `HOME/.omo` 下的锁；本环境用户目录只读，产生 EROFS 警告，插件仍继续运行。其 Claude transcript 目录另需公开的 `CLAUDE_CONFIG_DIR`。本次没有重设 HOME、修改第三方包或写用户配置。**仅设置 XDG 不能保证 OMO 在本机完全隔离**；复现插件探针应使用独立系统账户/隔离环境，或保持用户目录只读。配置迁移本身未验收。OpenCode 模型目录的后台网络读取在本环境被拒绝，不影响已显式配置的 loopback 模型。

## 1. 重现不收费的 loopback 探针

以下安装命令对应本次 Linux x64 收据，所有安装和缓存位于 `/tmp/fusion-clients`；不会升级全局 CLI。其他平台需使用官方对应平台包，并重新记录版本和 probe 结果。

```bash
mkdir -p /tmp/fusion-clients/workspace
npm install --prefix /tmp/fusion-clients --cache /tmp/fusion-clients/npm-cache \
  --ignore-scripts --omit=optional --no-audit --no-fund \
  opencode-ai@1.19.4 opencode-linux-x64@1.19.4 \
  oh-my-opencode@5.1.44 oh-my-opencode-linux-x64@5.1.44 \
  @oh-my-pi/pi-coding-agent@18.12.6 @oh-my-pi/pi-natives-linux-x64@18.12.6 \
  bun@1.4.4 @oven/bun-linux-x64@1.4.4
```

从仓库根目录复制 **[templates/probe](templates/probe/)**，其中全部模型映射都指向 stub，Key 为 `local-probe-only`。这些文件不能用于日常 Fusion 配置。

```bash
mkdir -p /tmp/fusion-clients/config/opencode /tmp/fusion-clients/workspace/.omo
mkdir -p /tmp/fusion-clients/omp-agent /tmp/fusion-clients/omp-root
mkdir -p /tmp/fusion-clients/data/omp /tmp/fusion-clients/state/omp /tmp/fusion-clients/cache/omp
cp tools/fusion-eval/templates/probe/opencode.json /tmp/fusion-clients/config/opencode/opencode.json
cp tools/fusion-eval/templates/probe/omo.jsonc /tmp/fusion-clients/workspace/.omo/omo.jsonc
cp tools/fusion-eval/templates/probe/omp-models.yml /tmp/fusion-clients/omp-agent/models.yml
cp tools/fusion-eval/templates/probe/omp-config.yml /tmp/fusion-clients/omp-agent/config.yml
printf 'LOCAL_PROBE_FIXTURE\n' > /tmp/fusion-clients/workspace/probe.txt
```

先在一个终端从合成工作区启动 stub；它只监听 `127.0.0.1`，默认最多接受 64 个请求，每个请求最多 2 MiB：

```bash
cd /tmp/fusion-clients/workspace
python3 /ABS/PATH/TO/sub2api/tools/fusion-eval/probe_server.py \
  --output /tmp/fusion-clients/requests.jsonl
```

在另一个终端设置隔离路径。`PI_CONFIG_DIR` 是 OMP 官方支持的、相对现有用户目录的配置根覆盖；此处计算其相对路径，没有改写 HOME：

```bash
export XDG_CONFIG_HOME=/tmp/fusion-clients/config
export XDG_DATA_HOME=/tmp/fusion-clients/data
export XDG_STATE_HOME=/tmp/fusion-clients/state
export XDG_CACHE_HOME=/tmp/fusion-clients/cache
export CLAUDE_CONFIG_DIR=/tmp/fusion-clients/claude
export npm_config_cache=/tmp/fusion-clients/npm-cache
export OPENCODE_DISABLE_AUTOUPDATE=true
export PI_CODING_AGENT_DIR=/tmp/fusion-clients/omp-agent
export PI_CONFIG_DIR="$(python3 -c 'import os; print(os.path.relpath("/tmp/fusion-clients/omp-root", os.path.expanduser("~")))')"
cd /tmp/fusion-clients/workspace
```

纯 OpenCode 基线：

```bash
/tmp/fusion-clients/node_modules/opencode-linux-x64/bin/opencode run \
  --pure --agent probe --model local-fusion/fusion-probe --format json \
  'Read probe.txt and return LOCAL_PROBE_COMPLETE.'
```

OMO 插件测试使用相同命令去掉 `--pure`；**先满足上述用户目录隔离条件**。再次运行加 `--continue` 验证续接；去掉该参数应产生不同会话头。

OMP 原版基线：

```bash
/tmp/fusion-clients/node_modules/@oven/bun-linux-x64/bin/bun \
  /tmp/fusion-clients/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js \
  --model local-fusion/fusion-probe --tools read --no-lsp --no-pty \
  --no-extensions --no-skills --no-rules --no-title --thinking off \
  --max-time 45 --mode json -p 'Read probe.txt and return LOCAL_PROBE_COMPLETE.'
```

OMP 扩展把每次请求的运行时 session ID 传到原生 Chat serializer，详细 API、限制和测试见 [扩展说明](omp-fusion-session.md)。不要把静态会话头写入 provider：

扩展模型目录中的零 cost 只是未填写的客户端展示占位值，**不代表 Fusion 免费**；真实费用须用网关实际价卡和完整任务账务核对。

```bash
export SUB2API_FUSION_BASE_URL=http://127.0.0.1:8765/v1
export SUB2API_FUSION_MODEL=fusion-probe
export SUB2API_FUSION_API_KEY=local-probe-only
/tmp/fusion-clients/node_modules/@oven/bun-linux-x64/bin/bun \
  /tmp/fusion-clients/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js \
  -e /ABS/PATH/TO/sub2api/tools/fusion-eval/omp-fusion-session.ts \
  --model sub2api-fusion/fusion-probe --tools read --no-lsp --no-pty \
  --no-skills --no-rules --no-title --thinking off --max-time 45 --mode json \
  -p 'Read probe.txt and return LOCAL_PROBE_COMPLETE.'
```

同一配置加 `--continue`，然后再运行一次 fresh，比较结构日志。日志仅保留角色序列、system/instructions 摘要、工具名称/schema 形状及摘要、tool choice 摘要、头名称和会话头 SHA-256；不保存 Authorization 值、正文、工具参数或原始 session ID。stub 的固定 8 tokens usage 是合成数据，不能进入真实费用成绩。

## 2. 用户本机真实 Fusion smoke

使用 **[templates/personal](templates/personal/)** 作为待填写候选配置，不要复用 probe 的全角色 stub 映射。这些模板尚未经过真实 Worker 派发验收，`.invalid` endpoint 和 `REPLACE_WITH_CONCRETE_MODEL_UID` 会阻止直接使用。

配置层已检查稳定版 OMO 包内 JSON schema、OpenCode `agent list` 和 OMP 的 agent/settings/model 解析器，结果见 [validation.json](templates/personal/validation.json)。这些检查确认字段和 Worker 定义被读取，不替代实际派发和权限执行验收。

1. 按 [网关 Fusion 配置](../../docs/DEVIN_FUSION.md#默认行为与覆盖) 在本机测试网关部署本次变更，创建独立测试 Key 和 `fusion-personal-worker` profile，填入真实 Lead / Sidekick UID、价卡和预算。先核对 `/v1/models` 的实际模型，确保测试 Key 能访问明确的具体模型和 Fusion 别名。不要改生产配置来完成 smoke。
2. OpenCode provider 用 `@ai-sdk/openai-compatible` / Chat Completions。填入自己的 Sub2API endpoint 和环境变量 Key；`model`、`small_model`、OMO 所有默认 agent/category 保持具体模型，只有显式 `fusion-worker` 使用 Fusion。OMP 配置的所有 modelRoles 和普通 agent overrides 保持具体模型，只有 `fusion-worker` 使用扩展 provider；Key 通过本机环境变量提供，不写进仓库。
3. OMP 将 `fusion-worker.md` 放入项目 `.omp/agents/`，加载 session 扩展，并设置 `SUB2API_FUSION_ENDPOINT_KIND=sub2api`、真实 HTTPS `SUB2API_FUSION_BASE_URL` 和 `SUB2API_FUSION_MODEL=fusion-personal-worker`。单独使用 `PI_CODING_AGENT_DIR/agents` 不保证 discovery 能找到它。角色覆盖与 precedence 说明见 [OMP 角色模板](templates/omp-fusion-session-roles.yml)。实际请求中的 `request.model` 可以覆盖角色映射，必须观察最终派发。
4. 每个客户端先在全新合成工作区执行两个短 smoke：真实外层派发 → Fusion Worker → 读写/命令验证 → Worker 报告 → 外层核对文件与退出码。保存客户端实际工具执行次数与网关物理调用次数、session/epoch 指纹、模型 UID 和完整任务费用；不要导出私有推理正文。
5. personal 模板的权限限制仍是**候选配置**。OMP Worker 显式列出 `read/write/edit/bash`，没有 `task` 或 `spawns`；OMO Worker 关闭 agent 派发工具并拒绝 task 权限。必须抓取实际工具列表并尝试递归派发，才能确认限制生效。若客户端仍暴露其他派发工具，先修配置，不凭提示词视为隔离完成。

默认只验证一个显式 Worker。OMP 先设置 `compaction.enabled: false`，保持 smoke 短小；仅 pin `smol` 不能保证摘要绕过 Fusion。**真实外层/子 Worker 身份、交错两个 Worker、compact、fork、取消、重试及有副作用工具去重仍待本机验收**。contract conflict 后不能换个 session 自动重发；先停止并查明账务和工具执行状态。通过这些边界前，不建议把 Fusion 设为长期默认 Worker。

当前模板关闭 OMP 自动 retry、retry 模型 fallback，以及 OMO 插件的 model/runtime fallback。这些设置已通过对应发布版的配置解析；OMO 插件设置不代表 OpenCode 内核和 SDK 的所有重试都已关闭，实际发送次数仍需观察。

## 3. 冻结 A/B 和预算账本

完整方案以 [个人评测计划](../../docs/DEVIN_FUSION_OMO_OMP_PLAN.md#7-质量时间与费用的冻结评测) 为准。先冻结题目/隐藏测试、工作区、版本/SHA、外层映射、两臂模型与 effort、工具、价卡、预算和运行顺序：A 为同一 Worker 直连具体 Lead，B 为固定 Lead + Sidekick。除了 Worker 两种变体，其余条件保持相同；失败、超时和未知费用都保留在结果中。

[cases-manifest.example.json](cases-manifest.example.json) 的 8 题只是规格草案，未编写/冻结隐藏测试，也未执行。以下 Python 工具是离线参考账本和汇总器，不发送请求，**不是完整 runner、生产支付门控或远程发送拦截器**。真实运行器仍需在所有 Agent 的每次发送前落实预算与停止规则，并补齐丢失 run 的 UNKNOWN 证据。

```bash
python3 tools/fusion-eval/ledger.py reserve-pair budget.jsonl \
  --cohort-id pilot --case-id c1 --client omp --snapshot frozen-comparison.json \
  --lead-cap-usd 0.75 --fusion-cap-usd 0.75 --total-budget-usd 12
python3 tools/fusion-eval/ledger.py settle-run budget.jsonl --record one-run.json
python3 tools/fusion-eval/ledger.py budget-status budget.jsonl
python3 tools/fusion-eval/ledger.py summarize results.jsonl \
  --manifest tools/fusion-eval/cases-manifest.example.json
```

`frozen-comparison.json` 是两臂共享的 `comparison_snapshot` 对象，包含两种 Worker 配置及共同冻结条件。`one-run.json` 格式示例（仅合成格式，不能当作成绩）：

预算 journal 命令依赖 Unix `flock`；`summarize` 可跨平台运行。Windows 本机应在兼容的 Linux 环境执行预算 journal 命令。

```json
{"schema_version":1,"record_id":"format-c1-lead","cohort_id":"format-example","case_id":"c1","client":"omp","arm":"lead","comparison_snapshot":{"fixture":"synthetic-format-only","worker_variants":{"lead":"lead-uid","fusion":["lead-uid","sidekick-uid"]}},"outcome":"completed","completed":true,"quality_passed":true,"cost_scope":"full_task","confirmed_usd":"0.20","reserved_unknown_usd":"0","inflight_reserve_usd":"0"}
```

两臂 `comparison_snapshot` 必须完全相同；每个 `record_id` 唯一。金额可用十进制字符串。账本按 pair 预留，未结算的另一臂保留完整 cap；UNKNOWN / partial 不释放资金占用。汇总费用包含失败；缺失或 partial 的全任务费用、零成功数都会使 `cost_per_success` 为 null。有未知/在途金额时只显示带标记的已知费用下界；manifest 缺失记录显示 `missing_run_records`，不猜测是否曾启动。`$12` 只是计划里的 paired-runs 子池，另外 smoke `$4` / cache `$4` 不由这个 journal 自动控制。

## 离线检查

```bash
python3 -m unittest discover -s tools/fusion-eval -p 'test_*.py' -v
/tmp/fusion-clients/node_modules/@oven/bun-linux-x64/bin/bun test \
  tools/fusion-eval/omp-fusion-session.test.ts
```
