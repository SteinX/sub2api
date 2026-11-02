# Linux 远端 Fusion smoke

当前执行地点为本任务的 Linux 远端环境。OpenCode `1.19.4`、OMP `18.12.6`、Bun `1.4.4` 的实际版本和运行器参数已确认；真实模型测试等待测试网关与凭据配置。

## 连接配置

需要提供以下非秘密参数：

- 已部署本次变更的 Sub2API 测试网关 URL，以 `/v1` 结尾。
- 外层使用的具体模型 UID，以及 Fusion Worker 的公开模型别名。
- 专用测试 Key 的数字 ID。

在云环境 Secrets 中绑定 `SUB2API_API_KEY` 和 `SUB2API_USER_TOKEN`。后者是 Key 所属面板用户的 JWT，仅用于只读账务查询。不要将密钥填入版本化配置或聊天。若没有现成测试网关，需先建立隔离测试部署并配置 Devin 上游账号。

测试 Key 应属于标准计费组、没有其他流量，且剩余额度大于零、不高于本次 `$0.75` 预留。首轮关闭保温；保持已交付模板中的自动压缩、promotion、fallback 关闭。运行器会在模型启动前核对 Key 身份、状态、并发和额度。

远端出站受环境网络策略控制。确定网关地址后验证连通性；若策略拒绝该域名，使用环境配置增加允许的目标，不绕过代理或关闭 TLS 校验。

## 首次运行

从仓库根目录执行；用实际连接参数替换占位值。`prepare` / `inspect` 不调用模型。

```bash
mkdir -p /workspace/fusion-runs
python3 tools/fusion-eval/local_runner.py prepare \
  --directory /workspace/fusion-runs/omp-short-01 \
  --client omp --case short-repair \
  --base-url https://YOUR-TEST-GATEWAY.example/v1 \
  --direct-model REPLACE_WITH_CONCRETE_MODEL_UID \
  --fusion-model fusion-personal-worker \
  --api-key-id 123 --budget-usd 0.75 \
  --cli-json '["/tmp/fusion-clients/node_modules/@oven/bun-linux-x64/bin/bun","/tmp/fusion-clients/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js"]'
python3 tools/fusion-eval/local_runner.py inspect /workspace/fusion-runs/omp-short-01
```

凭据与连接预检通过后，启动一次真实调用：

```bash
python3 tools/fusion-eval/local_runner.py run /workspace/fusion-runs/omp-short-01
python3 tools/fusion-eval/local_runner.py inspect /workspace/fusion-runs/omp-short-01
```

后续 OMO 使用新的运行目录和 `--client omo`，CLI 数组为 `["/tmp/fusion-clients/node_modules/opencode-linux-x64/bin/opencode"]`。这些 `/tmp` 路径属于当前环境的安装；环境重建后先检查版本或按 [探针安装说明](README.md#1-重现不收费的-loopback-探针) 恢复安装。

## 结果边界

先执行一个短任务，再核对派发、文件断言和已观察结算。启动标记持久保存后不重发同一 run；中断或缺少证据保留未知预留。费用范围始终为 `partial`，不能据此声称所有迟到费用和在途预留均已完整核清。退出码 `3` 表示已有记录、仍待人工核对，实际任务结果见 `result.json`。

运行器只导出脱敏结构证据；客户端自身的数据目录可能保存原始会话。完整验收矩阵和冻结 A/B 仍按 [接入计划](../../docs/DEVIN_FUSION_OMO_OMP_PLAN.md) 执行；本短任务属于开发 smoke。
