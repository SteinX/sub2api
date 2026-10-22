# Devin 客户端信息的独立更新

在管理后台 **系统设置 → 网关 → Devin 客户端信息** 中配置。此面板单独保存，不依赖页面底部的全局设置保存按钮。

支持三种来源，优先级为：**手动固定 → 同一发布地址最近成功同步的配置 → 内置配置**。

- 手动固定：启用后修改 JSON 并点击“保存 Devin 配置”。本实例的新请求立即生效，其他实例最多五秒后读取到新设置，无需重启或发布后端。可以固定较早版本进行回退。
- 独立发布：将完整配置 JSON 发布到你维护的公共 HTTPS 地址（例如独立仓库的 raw JSON、静态站点或对象存储），在面板保存该 URL 并开启自动同步。配置包与 sub2api 程序可以独立发布。
- 自动同步在请求到达时触发后台检查，检查间隔为一分钟；请求继续使用当前有效配置，不等待远程读取，没有流量时不轮询。点击“立即同步”可以等待已保存地址的同步结果。手动固定期间暂停自动检查，“立即同步”仍可更新备用远程配置。同步返回时重新核对发布地址和手动固定配置，丢弃已过时的结果。
- 远程读取失败、文档不完整、字段不合法、schema 不兼容或保存失败时，继续使用最近有效配置。该配置保存在数据库，重启后仍可使用。更换地址后不会复用旧地址的配置。
- 关闭自动同步保留已同步配置；关闭手动固定恢复远程配置；同时清空发布地址则恢复内置配置。

## 配置包

内置配置文件是 [`backend/internal/pkg/devin/client_identity.default.json`](../backend/internal/pkg/devin/client_identity.default.json)。复制该文件作为第一份独立发布的 JSON，在确认对应 CLI 的实际标识后调整字段并更新 `revision`。此文件记录兼容回退值，不代表官方最新版本。

```json
{
  "schema_version": 1,
  "revision": "my-profile-2026-10-22.1",
  "chat": {
    "ide_name": "devin-cli",
    "ide_type": "chisel",
    "ide_version": "3000.6.2",
    "extension_name": "chisel",
    "extension_version": "3000.6.2",
    "os": "linux",
    "locale": "en"
  },
  "discovery": {
    "ide_name": "chisel",
    "ide_version": "0.0.0-dev",
    "extension_name": "chisel",
    "extension_version": "0.0.0-dev",
    "os": "linux",
    "locale": "en"
  },
  "token_exchange": {
    "ide_name": "windsurf",
    "ide_version": "1.48.2",
    "extension_name": "windsurf",
    "extension_version": "1.48.2",
    "os": "windows",
    "locale": "en"
  }
}
```

三组标识分别用于：

| 分组 | 请求 |
| --- | --- |
| `chat` | `GetUserJwt`、`AssignModel`、`GetChatMessage` |
| `discovery` | `GetCliModelConfigs` |
| `token_exchange` | `GetSelfDevinSessionToken` |

只更新一个版本字符串并不能证明协议兼容。发布前应核对这些字段及接口行为，聊天与模型发现的标识不必相同。配置变化会使旧模型与会话令牌缓存失效，进行中的客户端继续使用原配置快照，新客户端使用新配置。

文档只允许客户端名称、版本、系统和语言等声明，最大 32 KiB；不接受 token、API 地址、任意请求头、协议字段或可执行代码。版本为 `x.y.z` 或 `x.y.z-suffix`。发布地址需要公共 HTTPS，不允许 URL 凭据、查询参数或片段；启用服务端 URL 白名单时，发布域名也须加入 `security.url_allowlist.upstream_hosts`。建议使用直接提供 JSON 的稳定 URL，不使用需要签名查询参数的下载跳转。

## 管理 API

沿用现有管理员认证、限流和审计：

- `GET /api/v1/admin/settings/devin-client-identity`：查看配置、当前生效值、来源及最近同步状态。
- `PUT /api/v1/admin/settings/devin-client-identity`：完整保存 `{ "manual": null, "source_url": "https://example.com/profile.json", "auto_sync": true }`。可由独立配置发布流水线调用；发布单个实例后其他实例最多五秒后读取。
- `POST /api/v1/admin/settings/devin-client-identity/sync`：立即同步已保存的地址；检查响应中的 `sync_error` 确认结果。此操作不会覆盖手动固定值。

这项配置只改变客户端声明，不能增加账号或 service user 的上游权限。
