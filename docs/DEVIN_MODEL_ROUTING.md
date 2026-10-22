# Devin 模型路由与 Fusion 语义保护

本次实现基于 `1623bd7ce96502f71e919e130362065956cb5ba6`。
交接文件的审计基线为 `402b62ac784acedd5bdb7007a7d40b79c22d3b73`；两者之间的改动是 CI/上游同步维护，没有改变 Devin 传输路径。
开发环境最初停留在 `d0bb23820b1ce6d6c6ab43931e64cb96a59102bc`，实施前已快进到上述主线，保留客户端身份快照和账号用量功能。

本文件说明上游目录的路由语义保护。另已实现默认开启、按 model ID 推断配对的网关 Lead/Sidekick 状态机，配置、三协议工具往返、持久会话和逐次计费见 [网关 Fusion](DEVIN_FUSION.md)。无法解析成员的 Fusion 条目及成员自身的未知 harness 继续拒绝；没有验证官方私有 harness 或与官方 CLI 的等价性。

## 路由规则

`internal/pkg/devin/model_route.go` 是分类的共同入口，转发、账号调度、模型列表、账号连接测试和目录同步均使用它。

| 目录条目 | 路线 | 默认行为 |
| --- | --- | --- |
| 可从 ID 唯一解析出成员的 Fusion 配对，或启用中的显式覆盖 | `gateway_fusion` | 默认开启；验证两个具体成员后执行双角色编排 |
| 普通模型，包括没有完整元数据的普通 UID | `direct_chat` | 保持 UID，发送聊天请求 |
| `is_model_router=true` 或 display 为 `MODEL_ROUTER`，没有特殊执行要求 | `assign_then_chat` | 分配一次，检查结果后聊天 |
| 配对成员缺失、歧义或不可直调的 `fusion-<lead>-sidekick-<worker>` | `blocked` | 明确拒绝，不抽取 Lead |
| 裸 `fusion`、`fusion-sidekick-*`、其他以 `fusion` 开头的特殊 UID，或明确标为 Fusion 的目录项 | `blocked` | 执行语义未验证 |
| 具体成员的非空 `harness_uids` 或无法解析配对的 opaque harness 条目 | `blocked` | 不执行未知 harness；完整公开配对 ID 可走独立的网关编排 |

名称匹配只用于识别候选；不会把配对推断、目录可见性或 HTTP 200 标成官方 harness 已验证状态。普通模型的新 UID 不会因没有独立目录元数据而全部被拒绝。

没有 Lead-only 开关或实验白名单。需要单模型时直接选择它的普通模型 ID。把 Fusion 名称通过渠道映射、账号映射或别名改为普通 Lead，仍会拒绝。

## 两次检查和请求快照

1. 在 HTTP 入口记录映射前的模型。已识别的特殊 UID 在账号选择之前返回策略错误。
2. 转发前检查入口模型、调度提示模型、账号映射结果和当前账号目录。
3. `Prepare` 接收路由计划，校验后取得账号 JWT；普通 router 才执行 `AssignModel`。
4. 分配结果存在 harness、缺少 UID/JWT、指向特殊项、被禁用条目或另一个 router 时终止；不递归分配，不安装被拒绝的 UID/JWT。
5. 最终模型有独立目录行时，重新校验输出上限、图片和工具支持。默认输出上限可以缩小，显式超过最终上限则拒绝。
6. 普通动态分配目标没有独立目录行时，保持既有 router 合同，在诊断中记录能力来源为 `router_catalog`，不虚构目标能力证明。

以上 `AssignModel` 规则适用于普通 router。2026-10-22 新版 Fusion 补充第 24.5 节报告，官方 Fusion 专用调用路径把返回的 `assignment.model_uid` 当作 Sidekick 使用；不能据此把 Fusion 分配结果安装成 Lead，也不能把 `harness_uids` 当成模型 UID。网关 Fusion 从公开配对 ID 或显式 profile 固定两个具体成员，只给具体成员调用 `Prepare`，不调用 Fusion 专用 assignment。当前不需要重写普通 router 的分配语义；未来若支持专用 Fusion assignment，必须另建配对解析合同。

每个请求使用固定客户端身份和目录副本。运行时缓存键包含账号/凭据/代理作用域、身份指纹、schema 和策略版本；返回目录时深拷贝 protobuf，调用方不能修改其他请求共享的缓存。失败或空目录刷新沿用既有的“不覆盖最后有效目录”行为。

Cascade 的会话范围仍由 API Key、账号和映射后的请求模型构成。router 分配的物理模型不改变跨轮历史所属范围；execution ID 每次生成独立。完整历史、工具归属、`previous_response_id` 和 `store=true` 的原有约束不变。

## 协议来源

`ModelInfo.harness_uids` 已补齐为 **field 20 / repeated string**，来源为：

[oh-my-pi 固定提交的完整 codeium_common.proto](https://github.com/can1357/oh-my-pi/blob/3a2758b68f7c8e6bdf22c3a76b86c8675e5e0c9e/packages/ai/src/providers/devin/proto/exa/codeium_common_pb/codeium_common.proto)。

已有的 `ModelAssignment.harness_uids` 保持 field 3。`cascade.pb.go` 由 protoc 33.0 和 protoc-gen-go v1.36.11 重新生成，没有手工修改生成代码。protoc 的 Go 生成版本头显示为 `v6.33.0`。

`internal/pkg/devin/testdata/model_routing/` 保存独立来源的最小 descriptor、synthetic wire fixture 和重生成命令。它们不使用应用的 `cascade.proto` 生成，测试可发现应用字段号、类型或重复性声明错误。fixture 不包含真实账号数据。

应用生成命令：

```sh
cd backend
go generate ./internal/pkg/devin/pb
```

## 错误合同与调度影响

| 稳定业务码 | HTTP | 归因 |
| --- | --- | --- |
| `devin_fusion_orchestration_unsupported` | 400 | 无法从 ID 解析可用配对 |
| `devin_model_route_unverified` | 400 | 特殊执行语义未验证 |
| `devin_assignment_harness_unsupported` | 400 | 分配要求了未支持 harness |
| `devin_model_capability_unsupported` | 400 | 所选或最终模型的能力不足 |
| `devin_model_unavailable` | 400 | 当前账号未广告可用条目 |
| `devin_upstream_route_mismatch` | 502 | 分配/响应与获准的执行合同冲突 |

上述 typed error 均为网关内不可重试的终止决定。它们不会刷新 token、设置账号错误或限流、进入账号故障转移，也不会计入账号调度健康失败。真实认证、限流和传输错误仍使用既有恢复逻辑。

OpenAI Responses/Chat 使用现有 `error` envelope，附稳定 `code` 和 `param=model`。Anthropic 保留其标准 envelope 和兼容 `error.type`，稳定业务码放在 message 前缀。Devin 的 Messages 入口与其他原生兼容 Provider 一样直接可用，不依赖只适用于 OpenAI/composite 分组的 dispatch 开关。

正常预检在 SSE 内容和成功 header 之前返回 JSON 错误。如果账号排队的既有 keepalive 已提交 SSE header，则发送对应协议的终止错误事件，并记录 Ops 的逻辑错误状态，不追加普通 JSON 或成功结束标记。生成过程中发现特殊执行身份时同样终止，保留已经观察到的实际 usage。

## 目录、迁移与管理界面

账号 `extra.devin_models` 继续保存上游广告的 ID，包含被策略阻止的特殊项，不自动改写用户映射。
新增 `extra.devin_model_catalog` 保存已知 protobuf 元数据、目录摘要、身份/schema/策略版本和供管理界面显示的 routes。
这里沿用现有 discovery 对 disabled、重复和内部 display 条目的过滤；没有新增保存完整原始 RPC 报文的功能。

调度重新使用元数据分类，不把保存的 routes 当作永久授权。公开模型列表、上游同步返回的 `models` 和账号调度均排除被阻止条目。同步响应额外返回 `devin_routes`，特殊条目仍可在管理界面查看原因。

已有账号只有 ID 列表时，已识别的 Fusion 名称立即受保护；不根据缺失元数据下线所有普通模型。管理员应同步一次上游模型或重新授权，补齐不含 Fusion 名称的 opaque harness 元数据。转发始终会查询当前账号目录进行预检，旧 ID 缓存不能让未支持 harness 获准生成。

编辑账号、连接账号和上游同步界面分别展示普通模型、运行时分配、未验证特殊项和未实现配对，并对现有 Fusion→单模型映射提示其会被拒绝。同步不会重写或删除映射。

无需数据库结构迁移。回滚应用可能恢复旧的错误路由；回滚期间应继续隔离 Fusion 和未支持 harness 条目。新加的 extra 元数据可保留，不清空账单或已有用量。

## 诊断与计费

生成前返回 `X-Sub2API-Request-Id`，复用服务端请求 ID；没有服务端 ID 时生成独立 ID。响应中的 `model` 保持既有兼容含义，可能是入口别名，不作为物理执行证据。

默认输出并索引精简的 `devin.route_decision`。临时将现有 `log.level` 配置为 `debug` 后，还会输出 `devin.route_trace`。二者使用 `audit.devin.routing` component，进入已有受保护的系统日志索引，服从现有采样、队列和保留配置。恢复默认日志级别即可停止详细记录；这里没有新增独立的按账号诊断采样开关或自动过期授权。

管理员可使用系统日志界面，或现有接口：

```text
GET /api/v1/admin/ops/system-logs?component=audit.devin.routing&request_id=<ID>
```

详细记录包括入口/映射/分配/wire 模型、目录与身份版本、决策阶段和归因、实际模型观测、usage 自报模型和计费来源。HTTP 入口没有传递原始模型上下文时，明确标记 `gateway_entry_fallback`，不把 fallback 伪称为真实入站捕获。处理器/渠道已有映射汇总为 `handler_channel`，显式调度提示和账号别名另记步骤。

诊断不记录 token、JWT、账号主机、请求正文、工具参数/结果、thinking 或签名。上游 message ID 只保留摘要；任意异常/超长模型 ID 转为摘要。最多保留 16 个不同实际模型、32 个 usage 观测和各 16 个 harness 标识；usage 达上限时保留初始历史和最后一帧，并标记截断。这些诊断属于管理员，不新增租户跨账号查询接口。

usage 仍按原有终态快照规则结算。观测层不累加累计帧，不推断角色费用；后续 usage 帧缺少 billing model 时仍保留此前上游声明。缺少计费模型时继续使用既有映射请求模型 fallback，并单独标注来源。流中错误和断连排水的实际用量仍交给原有 usage 链；本地预检拒绝不产生账单结果。

## 验证范围

新增测试覆盖独立 schema fixture、分类、完整配对及特殊变体、display-only router、分配后 harness/特殊 UID/缺失 JWT、最终能力、映射绕过、两个调度器、有效目录、缓存副本并发、诊断索引和脱敏，以及三种协议的流式/非流式拒绝矩阵。既有认证恢复、身份快照、工具历史、断连排水和账号隔离测试继续运行。

可复现命令（需要 Go 1.27.3；race 需要 CGO 工具链）：

```sh
cd backend
GOMAXPROCS=2 go test -mod=readonly -p 2 -tags=unit ./...
GOMAXPROCS=2 go test -race -mod=readonly -p 2 -tags=unit ./internal/pkg/devin/... ./internal/service ./internal/handler -run 'Test(Devin|InboundModel|ModelRoute|ModelInfoIndependent|ModelEvidence|PrepareChecks|FinalModelCapabilities|UnexpectedSpecial)' -count=1
CGO_ENABLED=0 go build -mod=readonly -trimpath -o /tmp/sub2api-fusion ./cmd/server
cd ..
make test-frontend
pnpm --dir frontend run build
```

本环境带 HTTP 代理，完整测试发现阿里云 SDK 把本地 mock 的 `host:port` 与 `NO_PROXY` 严格比较。测试夹具已补充该随机回环端点的代理例外，外部代理配置保持继承。

真实 Devin 文本调用、真实工具多轮、Lead/Sidekick 协作和官方 CLI 等价性均未验证；没有使用真实凭据、发起付费推理或部署服务。详细执行结果见本次任务交回说明。
