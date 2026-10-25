# Devin 账号与用量

## 账号管理

Devin 支持长期 API Key、浏览器登录和导入会话。添加时保存名称、备注、分组、并发、优先级、倍率、负载系数、到期时间和到期暂停设置。

两种账号都可以编辑上游地址与模型白名单／映射；默认地址为 `https://server.codeium.com`。模型选项来自账号授权时发现的目录，“同步上游模型”会重新拉取并保存目录。Devin 不预填 Claude 模型或映射预设，也不展示未接入其原生协议的号池和自定义错误码设置。更换登录身份或 Key 应使用账号菜单中的“重新授权”，以便重新校验凭据、检查重复账号并同步模型。

重授权使用原账号的上游地址和代理进行校验，保留分组、倍率、到期设置及其他管理配置。通用 OAuth 刷新接口对 Devin 返回明确提示，不会调用其他平台的刷新服务。

账号测试、模型查询、转发和用量查询共用 Devin 会话服务。长期 Key 的测试在输出开始前遇到 401 时恢复会话并重试一次；已输出部分内容后不重放请求。仅会话凭据失效时需要重新授权。测试成功后沿用系统统一的账号状态恢复逻辑，定时测试也使用同一条链路。

## 创建 Key 与客户端接入

创建 Key 时 Devin 归入“其他”，不按它承载的模型厂商拆分类目。标准模式仍需要管理员将 Devin 账号绑定到可用的 Devin 平台分组，或配置了 Devin 路由的组合分组，并让用户能够使用该分组；添加账号本身不会自动授予分组权限。“其他”没有可用分组时仍可选择，页面会给出配置提示。

“使用密钥”和 CC Switch 导入先读取当前 Key 可用的真实模型目录，再选择模型。Codex、Claude Code 和 OpenCode 配置使用同一个选择，包括 Fusion 的上游 UID；目录获取失败或为空时不会生成猜测的 GPT / Claude 配置。可先检查分组权限、模型白名单，再同步上游模型并刷新目录。

在“密钥 → 使用 Key”中选择 Devin 模型后，切换客户端标签并复制生成配置。客户端使用本站公开 Key 和本站地址，原生 PAT 只由管理员在账号管理中配置。直接模型与已启用的 `fusion-*` 使用相同的客户端接入流程；更换模型时新建会话，续接时保持原有消息和工具结果不变。

- OpenCode 使用 OpenAI-compatible Chat provider；可用 `--pure` 进行无插件检查。OMO 模板固定插件版本并提供只读 `devin-check` primary agent，关闭模型和运行时 fallback。先填写项目绝对路径，生成的 `external_directory` 仅允许该工作目录，不使用全域许可。模板供接入检查使用，不将外层 Worker 派发视为已验证能力。
- OMP 标签生成用户目录下 `models.yml` 和 `config.yml`。JSON 也是合法的 YAML 1.2；配置使用 Chat Completions，关闭自动 compact、`supersedeReads`、`dropUseless` 及自动重试，并把模型角色指向选择的同一模型。读取裁剪的默认行为可能改写 Fusion 历史而触发冲突，不能把兼容配置的结果当作默认配置通过。保留更多历史会增加上下文占用，长会话需单独验证。
- Codex 的 Responses 与 Claude Code 的 Messages 标签继续提供相应协议配置；不要仅依据协议适配成功声称对应 CLI 版本的全部会话和工具能力通过。非 Fusion 的 HTTP Responses 应关闭存储，由客户端发送完整历史。

将配置与已有用户配置合并后，新建会话先测试简短回复，再在工作目录放置 `probe.txt`，检查真实读取与结果续接。预算或历史冲突应先检查本地用量和客户端配置，不反复自动发送请求。验证边界及固定客户端版本见[客户端进程验收](DEVIN_AGENT_E2E.md)。

Devin 上游使用 Cascade 的 Connect/protobuf 接口。Sub2API 对外提供以下适配：

| 接口 | 行为 |
| --- | --- |
| `POST /v1/responses` | JSON / SSE、function / custom 工具、namespace、工具结果及思考签名回放 |
| `POST /v1/chat/completions` | JSON / SSE、工具调用、`reasoning_content` / `reasoning_signature`、可选流式 usage |
| `POST /v1/messages` | Anthropic Messages JSON / SSE，复用现有协议转换 |
| WebSocket `/v1/responses` | 同一连接内保存完整历史与工具声明，支持工具结果续接和上一轮 `previous_response_id`；`generate:false` 只确认预热，不发起生成 |

普通模型的 HTTP Responses 不保存服务端会话，需要发送完整历史，`store:true`、HTTP `previous_response_id` 和结构化输出格式会明确返回错误。普通模型的 WebSocket `previous_response_id` 仅能引用当前连接的上一轮响应；持久 Fusion 可在首帧通过经过当前用户和 Key 验证的会话或响应关联恢复原账号，后续帧仍校验上一轮响应。涉及 Fusion 的模型切换需要新建连接。显式替换 `tools` 而省略 `tool_choice` 时恢复自动选择，避免残留已移除的指定工具。图片输入要求内联 base64 data URL；图片能力、工具能力和输出限制来自账号模型目录，其他未实现的内置工具不会被当作已支持。原生路由模型先调用 `AssignModel` 获取具体模型与 assignment JWT，再按实际模型处理工具 schema；路由到 Gemini 时会转换 nullable 类型数组。Lead/Sidekick 组合的持久 Fusion 执行独立维护会话与计费，见 [Fusion 接入说明](DEVIN_FUSION.md)。

## 连接测试与 `invalid_argument`

连接测试与正式转发共用原生请求默认值、模型能力和输出上限，测试不再固定限制到 128 token。原生采样默认值对齐已核查的 CLI 请求：temperature 1、top-k 40、top-p 为 float32 的 0.95、max-newlines 400；停止序列仅使用调用方提供的值。

失败时会保留 RPC 阶段（例如 `GetUserJwt`、`AssignModel`、`GetChatMessage`），以及上游提供的合法 trace ID。`invalid_argument` 本身无法证明是模型 UID、请求字段还是上游内部故障；重试仍失败时应记录这些信息定位，不会把失败显示成测试成功。上游原始错误正文和凭据不直接透出。

协议对照来源：[OMP Devin provider](https://github.com/can1357/oh-my-pi/blob/0b5c27ab24719c86a5986b95ee55f4e75bc1aaa0/packages/ai/src/providers/devin.ts)、[wire metadata](https://github.com/can1357/oh-my-pi/blob/0b5c27ab24719c86a5986b95ee55f4e75bc1aaa0/packages/catalog/src/wire/devin.ts)、[protobuf 字段](https://github.com/can1357/oh-my-pi/blob/0b5c27ab24719c86a5986b95ee55f4e75bc1aaa0/packages/catalog/src/discovery/devin-proto.ts)。可在发布前使用[云端 Devin E2E 环境](DEVIN_E2E.md)，通过当前后端的公开网关执行确定性协议回归，并用独立测试账号验证真实上游。模拟结果不代表真实账号或上游协议当前可用。

## 账号用量

账号列表的用量列支持 Devin 浏览器登录、导入会话及长期 API Key。首次加载通过现有 `/api/v1/admin/accounts/usage/batch` 查询，移动端按可见区域加载；“主动查询”会绕过缓存。

展示内容：

- 日用量、周用量：上游剩余百分比转换为已用百分比（`100 - remaining`），分别使用上游返回的重置时间。
- 套餐名称，以及上游提供的 Prompt / Flow / Flex credits 余额、ACU 已用量和上限。
- 整个窗口缺失或百分比显式为 null 时显示“上游未提供数据”。若提供有效重置时间但省略百分比，则按 ProtoJSON 默认零值解析为已耗尽。日额度使用独立的 `daily` 字段，周额度复用 `seven_day`，不套用 Claude 的五小时窗口。

后端通过 JSON Connect 调用 `/exa.seat_management_pb.SeatManagementService/GetUserStatus`，使用 Bearer 会话凭据和 `metadata.api_key`。支持 camelCase / snake_case 响应字段及字符串形式的数字。协议参考为 [devin2api 的 GetUserStatus 实现](https://github.com/WncFht/devin2api/blob/e8547b6d60cb09fddc6debe44bafdfa69867a963/internal/ccpanel/upstream.go)。

查询复用 Devin 的账号代理、出站地址校验和会话恢复逻辑；JSON 元数据沿用 `token_exchange` 客户端身份配置，同一次查询固定身份快照，身份配置更新会切换用量缓存。长期 API Key 遇到 401 时恢复会话并重试一次；仅会话凭据的账号提示重新授权。403 提示账号或套餐不支持查询，429 提示稍后重试。查询错误不修改账号调度状态，也不向前端透出上游原始诊断。

前后端成功结果最多缓存三分钟（若配额先重置则提前过期），失败结果缓存一分钟；前端也保留后端观测时间，不延长已有快照寿命。同一凭据的并发查询合并，调用方取消不会中断共享查询。凭据、上游地址或代理 ID 改变会使用新的后端缓存；重新授权或切换代理也会触发前端刷新。模型同步返回空目录时保留原有模型，避免意外清空调度条件。

协议测试使用模拟响应，覆盖真实零值、缺失字段、两种 JSON 字段命名、认证恢复、缓存和批量查询；前端回归覆盖展示、刷新及批量加载。真实套餐的返回字段与权限仍由 Devin 上游决定。

## Fusion 协作

网关 Fusion 默认开启，按 `fusion-<lead>-sidekick-<sidekick>` model ID 和账号目录推断配对，账号编辑支持覆盖或关闭配对，为 Responses、Chat Completions、Anthropic Messages 提供统一的 Lead 委派、Sidekick 工具执行和 Lead 审查。会话固定账号和角色模型，工具由接入客户端执行，两个模型按各次实际用量计费。配置、会话关联、取消与验收边界见 [网关 Fusion](DEVIN_FUSION.md)。

遇到 `devin_fusion_pricing_unavailable` 时，错误会指出未通过检查的真实成员 UID。管理员需分别核对 Lead 和 Sidekick：价格按当前 Key 分组的模型定价、关联渠道中 Devin 平台的模型定价、全局已识别价卡依次解析；仅给虚拟 `fusion-*` 名称配价不够。渠道中给其他分组或其他平台配置的同名规则不会生效。

普通模型请求成功、模型出现在目录中，都不能证明它满足 Fusion 的价格准入。`GET /api/v1/admin/channels/model-pricing?model=<真实 UID>` 只查询全局默认价，不能据其 `identified` 判断分组或渠道覆盖是否生效。没有已发布 token 费率的模型，应先由管理员确认并配置真实费率；不能从 Devin credits 倍率推导美元 token 单价，也不能用另一模型的价格或系列猜测价放行。修改分组／渠道定价应走管理接口，以执行正常的缓存失效流程。

新版支持使用同一 Devin 公共 Key 调用 `GET /v1/models?pricing_models=<Lead UID>,<Sidekick UID>`，分别查看当前授权目录中物理模型的 `identified`、`calculable` 和价格来源。该只读检查沿用 Fusion 的有效分组／渠道和明确价格判定，不生成模型回答、不预留或扣费，也不返回费率、账号或分组标识。`not_advertised` 表示该 UID 不在此 Key 当前可见目录中；`unidentified` 仍应停止在推理前。零有效价格只有在明确规则已识别且可计算时才有意义，不能用它推断供应商免费。
