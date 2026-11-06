# 额度快照 API

额度快照通过 CPAMP 汇总 Codex 账号、额度、冷却、近期请求和价格。只使用调度器时可以不配置；未配置的快照接口返回 `503`。

## 调用方与权限

适用于外部客户端、状态面板或服务端程序，一次调用取得多类额度和用量信息。该接口只读，不提供探针启停、账号调度设置或任意账号查询；这些控制能力使用 [Management API](scheduler.md#management-api)。浏览器网页不能直接凭令牌跨域读取，建议通过自己的后端接入。

## 配置

配置文件默认位于 CPA 服务用户的 `~/.config/CLIProxyAPI/cpa-quota/config.json`。也可用 `CPA_QUOTA_CONFIG` 指定绝对路径。容器内路径以容器中的服务用户为准。

以下是合成示例，请替换地址、路径和客户端 Key 哈希：

```json
{
  "manager_url": "http://127.0.0.1:8090",
  "manager_key_file": "/absolute/private/cpamp-admin",
  "read_token_file": "/absolute/private/snapshot-token",
  "client_key_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "sync_quota_snapshots": false,
  "estimate_state_file": "/absolute/private/estimate-references.json"
}
```

| 字段                   | 用途                                                                    |
| ---------------------- | ----------------------------------------------------------------------- |
| `manager_url`          | CPAMP 地址。使用有效 HTTPS；HTTP 只允许数字回环地址，例如 `127.0.0.1`。 |
| `manager_key_file`     | 保存 CPAMP 管理 Key 的私有文件。                                        |
| `read_token_file`      | 保存独立随机只读令牌的私有文件，不能与管理 Key 相同。                   |
| `client_key_sha256`    | 需要汇总近期请求的 CPA 客户端 API Key 的 SHA-256，小写十六进制。        |
| `sync_quota_snapshots` | 默认关闭。开启后将真实主额度观测保存到 CPAMP，用于周期费用估值。        |
| `estimate_state_file`  | 可选的匿名总量参考文件；只有开启同步时才写入。                          |

配置文件和两个密钥文件必须是服务用户或 root 所有的普通文件，权限设为 `0600`，不能使用符号链接；父目录也应由服务用户控制。密钥只放文件中，不写进 URL、启动参数或日志。修改配置后重新加载插件或重启 CPA；无效配置会撤销旧读取授权。

CPAMP 需提供账号、模型、额度快照、冷却、价格、近期请求和周期费用接口。数据依赖详见源码 `internal/status/`；缺失数据按分区报错，不补造额度或金额。

## 读取

```http
GET /v0/resource/plugins/cpa-quota/snapshot
Authorization: Bearer <snapshot-read-token>
```

每次请求使用 `Authorization: Bearer <snapshot-read-token>`，令牌由服务端配置的 `read_token_file` 提供。

手动刷新使用 `?refresh=1`。只允许此参数，不接受客户端指定账号、地址或历史范围。响应禁止缓存，并拒绝带浏览器 `Origin` 的请求；适合服务端或原生客户端读取。

外层响应为 `served_at_ms` 和 `snapshot`。快照版本为 `schema_version: 2`，主要分区：

| 分区        | 内容                                                                    |
| ----------- | ----------------------------------------------------------------------- |
| `accounts`  | 匿名账号 ID、遮罩邮箱、状态、模型、主额度窗口及可选周期估值。           |
| `cooldowns` | 账号冷却原因及恢复时间。                                                |
| `recent`    | 绑定客户端最近 24 小时内至多 100 条请求；`truncated` 表示仍有更多数据。 |
| `prices`    | CPAMP 模型价格及阶梯规则。                                              |

各分区带 `observed_at_ms`、`stale`、`error` 和 `data`。未知数据为 null 或明确错误，不能当成零；`served_at_ms` 也不能当作额度更新时间。完整结果缓存最多 60 秒，不完整结果最多 5 秒；并发读取复用同一次采集。手动刷新距上次完成不足 5 秒返回 `429`。

额度范围固定为 `codex_main`，不覆盖 Spark 独立额度。周期费用依据 CPAMP 同账号、同窗口的计价记录估算；它不是现金余额或实测套餐容量。缺价时可能偏低，数据不足则不输出总量估值。近期请求只用于即时展示，不能代替完整历史账本。

## 常见返回

- `401`：读取令牌缺失或不匹配。
- `403`：请求包含浏览器 Origin。
- `400`：请求方法、路径、正文或查询参数不受支持。
- `409`：读取期间配置发生变化，应按当前配置重新读取。
- `429`：手动刷新过于频繁。
- `503`：配置缺失、无效或密钥文件不可读。

读取令牌只允许读取快照，不授予调度设置、账号修改或额度激活权限。管理功能仍由 CPA 管理 Key 保护。
