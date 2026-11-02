# macOS 本机 Fusion smoke

当前已改为 [Linux 远端执行](LINUX_REMOTE.md)，本页保留为备用操作说明。

本轮继续交付本机运行器，验证两个开发用小任务。运行顺序为：准备独立目录 → 检查客户端和专用 Key → 启动一次客户端 → 独立验收文件 → 核对费用。这里的 smoke 不属于 8 题冻结评测，也不产生质量或性价比结论。

`prepare` 和 `inspect` 不调用模型。`run` 会在你的本机调用所配置的网关并产生费用；云端仅做离线测试。运行器不会自动重跑或启动下一题。

先把本次工作区改动同步到本机仓库和测试网关。若使用随任务提供的 `fusion-personal-local.patch`，在匹配基线的仓库根目录先检查再应用；已同步改动的仓库无需重复应用：

```bash
git apply --check /PATH/TO/fusion-personal-local.patch
git apply /PATH/TO/fusion-personal-local.patch
```

补丁基线为 Sub2API `abd27dfce`。检查有冲突时先解决本机版本差异，不使用强制覆盖。

## 1. 安装锁定版本

已有相同版本时可直接使用。需要独立安装时，以下命令使用 npm 的平台安装逻辑，适用于 macOS 的 Intel / Apple Silicon；无需复制 Linux 二进制路径：

```bash
export FUSION_TOOLS_DIR="$HOME/.local/share/sub2api-fusion-clients"
npm install --prefix "$FUSION_TOOLS_DIR" --cache "$FUSION_TOOLS_DIR/npm-cache" \
  --no-audit --no-fund \
  opencode-ai@1.19.4 oh-my-opencode@5.1.44 \
  @oh-my-pi/pi-coding-agent@18.12.6 bun@1.4.4
export PATH="$FUSION_TOOLS_DIR/node_modules/.bin:$PATH"
opencode --version
omp --version
bun --version
```

版本来自上一轮已核验的 [versions.json](versions.json)。macOS 安装与真实上游运行需在本机确认；云端的实际客户端探针运行于 Linux x64。

## 2. 准备测试网关和凭据

部署本次改动，按 [Fusion 配置](../../docs/DEVIN_FUSION.md#默认行为与覆盖) 建立 `fusion-personal-worker`，固定两个具体成员、价卡和 profile 限制。首轮关闭网关保温，避免试验结束后继续产生维护调用。客户端自动压缩、模型 promotion 和模板支持的 fallback 也关闭，分别留到后续验收。

每次运行使用一个独占的、**有限 USD 额度**的测试 Key，并使用标准计费组；运行期间不要让其他客户端使用它。外层和 Worker 共用这个 Key，各 Worker 保留自己的动态会话身份。无限额 Key 不累计可用于核对的 `quota_used`。

运行器需要两个环境变量，均只在本机注入，不写进配置或结果：

- `SUB2API_API_KEY`：用于模型调用的专用测试 Key。
- `SUB2API_USER_TOKEN`：该 Key 所属面板用户的 JWT，用于只读查询 Key 和 usage。它不会传给客户端子进程。

记录面板中这个 Key 的数字 ID，供 `--api-key-id` 使用。运行器会在内存核对数字 ID 对应的 Key 与 `SUB2API_API_KEY` 是否相同，不保存接口返回的明文 Key。

`--budget-usd` 表示这次运行保留的本地预算。Key 额度、profile 预算与本地预留共同约束试点；这些都不是供应商实际收费的绝对上限。普通模型账务可能延迟，未知或迟到费用需要继续核对。

运行器要求 **Key 剩余额度不大于 `--budget-usd`**，否则在调用模型前拒绝启动。下例使用 `$0.75` 预留，对应新建测试 Key 的额度也应不大于 `$0.75`；不要把 `$4` 的整个 smoke 资金池直接作为这次 Key 的额度。若最低完整工具链装不进该额度，先调整试验范围与预算，再建立新的运行目录。

## 3. 准备并运行一个任务

从仓库根目录执行。替换 endpoint、具体模型 UID 和 Key ID，模型必须已经出现在你的 `/v1/models` 中。输出根目录建议使用实际的、非符号链接目录；运行器拒绝覆盖已有的 run 目录。macOS 的 `/tmp`、`/var` 常为符号链接，可使用下例 HOME 下的真实目录，或 `/private/tmp`。

```bash
export FUSION_RUNS_DIR="$HOME/sub2api-fusion-runs"
mkdir -p "$FUSION_RUNS_DIR"
python3 tools/fusion-eval/local_runner.py prepare \
  --directory "$FUSION_RUNS_DIR/omp-short-01" \
  --client omp --case short-repair \
  --base-url https://YOUR-SUB2API.example/v1 \
  --direct-model REPLACE_WITH_CONCRETE_MODEL_UID \
  --fusion-model fusion-personal-worker \
  --api-key-id 123 --budget-usd 0.75
python3 tools/fusion-eval/local_runner.py inspect "$FUSION_RUNS_DIR/omp-short-01"
```

准备步骤只生成工作区、任务说明、配置和运行快照。确认本机已注入两个环境变量后，手动启动一次：

```bash
python3 tools/fusion-eval/local_runner.py run "$FUSION_RUNS_DIR/omp-short-01"
python3 tools/fusion-eval/local_runner.py inspect "$FUSION_RUNS_DIR/omp-short-01"
```

默认使用 PATH 中的 `omp` / `opencode`。需要指定启动命令时，在 `prepare` 加 `--cli-json '["/absolute/path/to/omp"]'`，也可提供 Bun 与 `dist/cli.js` 两个数组元素；运行器不经 shell 拼接这条命令。

为 OMO 使用新的 run 目录并改为 `--client omo`；第二个任务改为 `--case tool-roundtrip`。两个客户端各做这两个短任务，共四次 smoke，每次准备独立 Key 和目录，不自动扩大费用额度。

## 4. 如何阅读结果

两个任务都要求外层派发到 `fusion-worker`，由 Worker 实际修复文件，再由外层核对结果。独立 checker 位于被测工作区外，验证源码确实改变、断言通过和产物一致；不会采信模型声称“测试已通过”。checker 会运行工作区代码，因此应只在合成测试项目里使用。

结果分开记录：

- 文件与断言是否通过，以及独立 checker 退出码。
- CLI 是否正常退出、是否观察到向指定 Worker 的派发，以及可取得的实际模型证据。
- 已观察的 Key 结算增量、usage 明细合计及二者是否匹配。
- 未知预算占用和仍缺少的证据。

客户端 JSON 流可能含正文、思考和工具结果；运行器只保存白名单结构元数据与摘要，不把原始 stdout / stderr 作为评测轨迹落盘。客户端自身仍可能在 run 的隔离数据目录保存会话；对外分享时仅导出运行器的结果和结构证据。OMP 父进程的 task 结果不包含全部子工具事件或子 session ID，不能由一次 task 成功推导子 Worker 的全部行为已验收。

当前用户 API 无法查询全部未知 / 在途预留，usage 日志也可能缺失或延迟。因此即便 Key 增量与 usage 合计相同，结果仍保留 `cost_scope: partial` 和本次预算占用。它可以证明“已观察到这些结算”，不能证明所有费用已完整落账，也不能直接充当 A/B 成绩。

`prepare` / `inspect` 成功退出码为 0；`run` 保存结果后用退出码 3 表示仍待人工核对费用，即使文件任务已经完成。退出码 2 表示启动校验或任务失败。客户端自身的退出码另见 `evidence.json` 的 `process` 字段。

如果进程中断，`inspect` 会显示启动记录与未知状态。超时会终止客户端进程组，但不证明已派发的上游推理或脱离该组的本地命令全部停止。不要重新运行同一个目录或换会话自动重做已有副作用；先核对原客户端、Fusion 会话、工具产物和账务。运行器不会因超时而把预留释放为零。

## 5. 后续验收边界

先完成两个客户端的真实外层 → Fusion Worker → Lead / Sidekick → 本地工具闭环，再验证两 Worker 交错、取消、compact / fork、长工具流程和工具执行去重。通过后冻结新任务与隐藏断言，按 [完整计划](../../docs/DEVIN_FUSION_OMO_OMP_PLAN.md) 做 A/B。

OMP Worker 放在项目 `.omp/agents/fusion-worker.md`；单独设置 `PI_CODING_AGENT_DIR/agents` 不足以保证发现。OMO 仍可能尝试用户目录 `.omo` 的启动迁移；XDG 配置隔离不等于整个用户目录隔离。运行器保留原 HOME 值，实际迁移和日常使用配置需在本机单独验收。

## 离线自检

```bash
python3 -m unittest discover -s tools/fusion-eval -p 'test_*.py' -v
```

这些测试使用合成工作区、假进程和本地响应，不发送真实模型请求。
