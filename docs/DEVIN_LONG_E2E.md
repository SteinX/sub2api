# Devin 长任务 E2E

## 用例与验证边界

`backend/internal/devine2e/long_workflow_test.go` 从当前 checkout 编译生产后端，接入一次性的 PostgreSQL、Redis 和普通用户 Key。Lead 分两次委派 Sidekick 读取 16 个合成文件，Sidekick 报告两组数值之和，Lead 审查后给出结果。每次生成随机三位数，正确答案由测试端独立计算且不放入提示。工具执行器实际读取文件并独立核对唯一调用 ID、文件覆盖和最终数值，不以模型声称完成作为通过条件。

第八次读取后重启后端，工具结果提交到原有持久会话。Sidekick 使用较低的上下文阈值以实际触发压缩；最终检查原始目标、两个完整报告、独立角色上下文、压缩归档，以及每个普通推理和摘要各自唯一的 SQL 计费记录。

这是公开 Responses API 的工具执行器。它不启动 OpenCode、OMO 或 OMP，不能替代[真实客户端进程矩阵](DEVIN_AGENT_E2E.md)。`mock` 只模拟原生模型决策；调度、持久化、协议转换、重启和工具文件读取仍经过实际代码。真实模型是否遵循任务及摘要约束，须由独立授权的 `live` 运行验证。

## 运行

依赖、凭据目标、代理及清理行为与[协议套件](DEVIN_E2E.md)相同。

```bash
make test-devin-long-e2e  # 合成上游，不消耗真实模型额度
make test-devin-long-plan  # 真实账号接入与价格核验，零生成
```

计划选择有确定价格、支持工具的具体模型，按 18000 输入和 1024 输出 token 的报价估算排序。真实长任务的两个角色使用同一个候选模型、独立会话，以控制反复读取的成本；mock 使用两个不同的合成模型核对角色路由。计划记录选择和价格，不使用模型系列回退价格放行。

一次运行最多 24 个公开请求，无自动重跑；并发为 1。Fusion 单 turn 最多 64 次内部推理、20 次外部工具调用、两次委派、300 秒，每次最多输出 1024 token；关闭缓存保活。账户仅保留一个并发槽，使摘要也串行执行。每个角色每 turn 最多四次摘要，单次 30 秒、最多 1024 输出 token。

单 turn 金额默认保留 `$0.10`，`DEVIN_E2E_LONG_MAX_TURN_USD` 可在明确批准后设为不大于 `$0.50` 的正有限数。临时 Key 默认额度 `$1`；`DEVIN_E2E_AGENT_KEY_QUOTA` 只能降低额度。多批测试须在创建新 Key 前扣除既有用量，不能把每次新 Key 的额度当作累计上限。上述值均为本地保护，不是供应商账单硬上限；达到保护、首个失败或发送结果不明即停止，不自动提高限制。

`DEVIN_E2E_LONG_MAX_PUBLIC_REQUESTS` 只能把单批默认 24 降到 1–24，供已审阅的续跑扣除既有请求。错误响应的会话头在断言失败前保存，清理前仅向 `private-long-failure-state.json` 写入角色、尝试状态、用量是否已观察及本地成本，不写入凭据、会话 ID 或工具正文。尝试审计的 `failure_kind` 只使用固定枚举，不暴露原始上游错误。合成故障 `DEVIN_E2E_LONG_MOCK_FAILURE=sidekick_incomplete_tool` 仅允许 mock，用于确认首个失败也能保留诊断；它不代表任何真实故障的根因。

真实运行需要同时设置 `DEVIN_E2E_ALLOW_PAID=true` 和 `DEVIN_E2E_LONG_ALLOW_PAID=true`，将 `DEVIN_E2E_LONG_EXPECT_MODEL` 固定为已审阅计划的模型，并使用已经批准的预算与额度。选模结果变化时在生成前停止。短客户端矩阵的开关不单独授权新的长任务限额。GitHub 自动作业只运行 mock；本套件不增加自动 live 作业。

结果默认写入被 Git 忽略的 `.cache/devin-e2e/`。`long-plan.json` 记录计划，`long-workflow.json` 记录请求和真实文件读取数量，`long-context.json` 记录结构性上下文检查，`private-usage-long-protocol-fusion.json` 记录本地累计计费。真实账号计划、用量和诊断仅留私有记录；自动 CI 只归档合成运行的结构结果。

## 现有深层回归

可先运行以下确定性用例，不需要真实凭据：

```bash
cd backend
go test ./internal/pkg/devin/fusion -run 'TestFusionLongToolWorkflowSurvivesIndependentRoleCompaction|TestFusionPersistentLanesToolsReworkAndPerRequestUsage|TestFusionQueuedClientToolsPersistSerializeAndBillOnce|TestFusionDisconnectStopsBeforeNextRoleAndCanResume'
go test ./internal/pkg/managedcontext -run '^TestLongToolSessionBeyondLegacyLimits$'
go test -race -count=1 -timeout=3m ./internal/pkg/managedcontext -run '^TestLongToolSessionBeyondLegacyLimits$'
```

Fusion 用例覆盖 16 次工具往返、独立角色压缩、报告与返工、排队工具的串行执行和重启恢复；managed context 用例覆盖 270 轮工具往返、超过 12 MiB 的历史，以及多次压缩后保留原目标。竞态检测显著增加长历史用例耗时，应按单轮设置超时，不能把整个十轮测试的超时当作单轮功能失败或通过。
