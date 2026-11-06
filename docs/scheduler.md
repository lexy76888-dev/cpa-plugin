# 调度器

构建与安装见[README](../README.md)。本页介绍账号调度、设置和管理接口。

## 调度逻辑

调度器依次执行四层判断。每一层都会筛选或排序账号，再把结果交给下一层。

### 1. 只接管当前 CPA 最高优先级层

- 只考虑 provider 为 `codex` 的候选账号，忽略其他 provider。
- 没有显式 CPA 账号优先级的 Codex 账号按优先级 `0` 处理。
- 插件接管当前已确认的最高 CPA 账号优先级层中的全部 Codex 账号。较低 CPA
  优先级层仍由 CPA 自己的 fallback 逻辑处理，不会进入插件队列。
- 如果希望所有 Codex 账号一起参与调度，应为它们设置相同的 CPA 账号优先级。
  最简单的推荐配置是全部使用优先级 `0`。

CPA 账号优先级与插件自己的账号优先级是两个独立设置。插件不会从 CPA 读取
插件优先级，也不会把插件优先级写回 CPA。

### 2. 先判断账号是否真的可用

进入调度范围的账号会被分成三个实际类别：

1. **可直接使用：**额度信息新鲜或仍在允许范围内，而且账号当前可用。
2. **可安全试用：**额度证据未知或已经过期，但能够在使用前进行安全验证。
3. **不可用：**长周期额度已耗尽、认证被阻止、熔断器已打开、临时耗尽反馈仍
   有效，或当前无法安全验证账号。

可直接使用的账号先于可安全试用的账号。插件不会选择不可用账号。

账号必须拥有有效的长周期额度（周额度或月额度）。五小时额度窗口是可选的：
如果 OpenAI 暂时不返回五小时额度，只要长周期额度有效，账号仍然可以使用。
如果长周期额度缺失或无效，账号保持未知/不可用，由 CPA fallback 接管。

如果账号同时存在长周期额度耗尽和较早记录的临时耗尽反馈，周额度或月额度
耗尽是权威原因，管理界面也会显示这个真实原因。

### 3. 对可用账号排序

只有完成可用性分类之后，插件优先级才参与排序，而且只在同一个可选择类别
内部生效。插件优先级较高的账号会先被考虑，但插件优先级不能把不可用账号
移动到可用账号前面。

插件优先级相同时：

1. 如果 `monthly_mode` 为 `priority`，月度账号排在周度账号前面。
2. 否则，或者账号属于同一额度类型时，账号到期时间更早的排在前面。
3. 到期时间相同后，剩余额度更高的排在前面。
4. 最后使用稳定的账号 ID 顺序打破平局。

默认的 `monthly_mode: expiry_order` 会让周度和月度账号共同按到期时间执行
Fill First 排序。可选的 `priority` 模式会明确优先使用月度账号。

### 4. 对不可用账号排序

账号不可用后会忽略插件优先级，因为优先级无法让一个已耗尽的账号恢复可用。

管理界面把不可用账号按预计恢复时间从早到晚排列。无法确定恢复时间的账号
放在最后。这样，队列反映的是哪个账号真正可能最先恢复，而不是保留已经没有
调度意义的优先级顺序。

如果“可直接使用”和“可安全试用”两组中都没有安全选择，插件不会返回账号，
并允许 CPA fallback。

### 排序示例

假设当前最高 CPA 优先级层中有四个 Codex 账号，实际显示和调度顺序如下：

| 顺序 | 状态         | 插件优先级 | 恢复时间   | 排序原因                                           |
| ---- | ------------ | ---------: | ---------- | -------------------------------------------------- |
| 1    | 可直接使用   |         10 | —          | 在可直接使用的账号中优先级最高                     |
| 2    | 可直接使用   |          0 | —          | 账号仍然可用，因此排在所有不可用账号之前           |
| 3    | 周额度耗尽   |        100 | 2 小时后   | 账号不可用，忽略高优先级；它是最早恢复的不可用账号 |
| 4    | 已耗尽或未知 |       1000 | 更晚或未知 | 账号不可用，而且预计最后恢复或无法确定恢复时间     |

## 额度刷新与新周期激活

### 额度刷新

额度刷新从 ChatGPT 读取当前 Codex 额度状态。它不会发送普通模型请求，也不需要
一直打开管理页面。

最近观察到 Codex 活动时，各账号只在自己的刷新期限到达后才会刷新；后台不会
按固定全局间隔反复扫描全部账号。活跃窗口结束并进入空闲后，普通后台刷新会
休眠，直到 Codex 请求、管理操作或到期的新周期操作将其唤醒。

收到 `usage_limit_reached` 响应后，插件会立即把刚才选择的账号标记为临时耗尽，
直到响应中的重置时间；如果响应没有重置时间，则临时阻止两分钟。额度耗尽不会
计入熔断失败。重复出现的非额度错误才由熔断器处理。

### 自动小请求与额度周期

上游可能已经到达重置时间，却要等下一次 Codex 请求才真正启动新周期。插件先只读检查额度；确认需要激活后，使用账号自己的订阅凭据向 Codex Responses 端点发送一次小请求，再读取额度核验结果。当前请求固定使用 `gpt-6-luna`、输入 `hi`、`store: false` 和流式响应；模型由源码中的 `codexResetProbeModel` 指定，当前没有页面选模型的设置。

一次请求可能同时带动周窗口和 5h 窗口，插件共享同次操作，不为两个窗口各发一遍。账号身份、凭据变化、并发探测和重启恢复沿持久化操作记录核对；未知发送结果先验证，不直接重复发送。上游拒绝、周期未变化或没有可靠额度证据都不能显示为已激活。

#### 周窗口维护

- 已确认的周窗口保存其上游重置时间，在该时间后约 1 分钟唤醒并重新检查。
- 若新鲜额度证明确需激活周窗口，不等待 `probe_trigger_times` 中的 5h 排程；仍受 `enable_reset_probe` 和 `probe_send_windows` 控制。
- 周窗口激活失败走 15 分钟后的重试判断，不能因为 5h 排程而推迟到下一固定时刻。
- 这用于及时衔接周周期、减少首次业务请求延后造成的时间漂移。实际周期及重置时间仍以上游为准，没有“指定星期几强制重置”或增加周额度的能力。

#### 5h 排程与允许发送时段

| 设置                     | 作用                                                   | 默认                             |
| ------------------------ | ------------------------------------------------------ | -------------------------------- |
| `enable_reset_probe`     | 周窗口和 5h 自动激活的共同开关                         | `false`                          |
| `probe_trigger_times`    | 每天启动 5h 检查的时刻列表，格式 `HH:MM`               | 空列表，按窗口状态和重置时间处理 |
| `probe_send_timezone`    | 排程与允许发送时段的 IANA 时区                         | 空字符串，使用服务器本地时区     |
| `probe_send_windows`     | 允许发出激活请求的时间段，格式 `HH:MM-HH:MM`，可跨午夜 | 空列表，不限制发送时段           |
| `quota_refresh_interval` | 额度刷新间隔；探针只读观察间隔不小于 30 分钟           | `30m`                            |

`probe_trigger_times` 是触发时刻，`probe_send_windows` 是发请求的时间限制，两者不同。定时触发后先检查额度：窗口仍有效时不会为了“到点”而强发请求。排程允许约 10 分钟的触发容差；普通 Codex 业务请求不受这些时段限制。

下面是按北京时间每天三个时刻检查的配置示例，**不是出厂默认值**。首次配置时可放在 `plugins.configs.cpa-quota` 下：

```yaml
enable_reset_probe: true
probe_send_timezone: Asia/Shanghai
probe_trigger_times: ["07:00", "12:00", "17:00"]
probe_send_windows: ["07:00-22:00"]
```

配置了发送时段后，周窗口激活也不能在时段之外发送。已有安装优先使用插件已持久化的设置，修改宿主 YAML 不保证覆盖页面保存值；可以通过管理 API 核对当前实际设置。

#### 启用、停止与立即探测

- **启用自动激活**：页面“调度设置”勾选“自动新周期探针”并保存，或通过设置 API 将 `enable_reset_probe` 设为 `true`。
- **停止自动激活**：取消勾选并保存。该开关同时停止周窗口与 5h 的新激活，不停止普通业务请求，不清除已有窗口记录；已发送的上游请求无法撤回。
- **立即探测 5h**：页面按钮或 `POST /probe/trigger` 发起一轮手动探测，可越过每日触发时刻限制，但不能越过允许发送时段、总开关、账号身份或并发保护。探针关闭时返回 `409`，不会自动替你开启。
- **停止账号接管**：设置 `handle_enabled: false`，让 CPA 自身调度继续处理。这与自动探针开关分开，不能将关闭接管当作停止小请求。

页面可查看探针开关、排程和结果，支持启停与立即探测；目前没有排程时刻或发送时段的专用编辑控件，这些设置可通过管理 API 修改。当前也没有“仅停 5h、保留周窗口激活”的独立开关，不应将总开关描述为只影响 5h。

### CPA 暂时无法确认账号列表时

CPA 无法确认当前 Codex 账号列表和优先级时，普通刷新和新周期激活会停止。插件
可以继续提供安全的管理信息，同时重试账号列表同步。

高风险设置 `probe_on_provisional_roster` 允许在这种情况下，使用最近一次保存的
账号列表尝试新周期激活。每次尝试前都会重新验证账号凭据，但插件仍无法保证
账号没有被删除，也无法保证账号没有被移动到其他 CPA 优先级层。除非明确理解并
接受该风险，否则应保持关闭。

## 功能

- 面向 CPA Codex 账号的优化版 Fill First 调度。
- 生产选择与管理队列都按真实可用性优先排序。
- 支持周额度和月额度，五小时额度窗口可选。
- 处理 `usage_limit_reached` 额度耗尽反馈。
- 账号级故障熔断器。
- 按期限驱动的额度刷新，以及可选的新周期激活。
- 支持浏览器语言检测的中英文管理界面。
- 账号别名、备注、标签、分组和插件优先级。
- 调度设置与账号标注的 JSON 导入和导出。
- Linux/amd64 发布包。

## CPA 配置

全局启用插件，并启用本插件：

```yaml
plugins:
  enabled: true
  configs:
    cpa-quota:
      enabled: true
      priority: 1 # CPA plugin registration/load priority
```

这里的注册/加载 `priority` 不是 CPA 账号优先级，也不是插件自己的单账号调度
优先级。账号标注和调度设置通过插件页面管理，不使用 CPA 的通用插件表单。

默认调度设置：

```yaml
handle_enabled: true
quota_refresh_interval: 30m
stale_after: 5h
refresh_active_window: 1h
refresh_after_reset_delay: 1m
refresh_retry_delays: 1m,5m,15m
refresh_on_startup: false
monthly_mode: expiry_order
fallback: fill-first
enable_usage_feedback: true
enable_reset_probe: false
probe_on_provisional_roster: false
# 默认不固定每日时刻；排程示例见上文
probe_send_timezone: ""
probe_trigger_times: []
probe_send_windows: []
log_scheduler_decisions: false
max_refresh_concurrency: 1
quota_endpoint: https://chatgpt.com/backend-api/wham/usage
circuit_failure_threshold: 5
circuit_open_duration: 30m
circuit_half_open_success_threshold: 2
max_log_entries: 200
log_retention: 24h
```

`monthly_mode` 可选值：

- `expiry_order`：周度账号和月度账号共同按到期时间排序。
- `priority`：在同一个可选择类别和插件优先级中，月度账号排在周度账号前面。

`quota_endpoint` 被限制为预期的 ChatGPT 额度端点，不能改为任意主机。

## 管理界面

从 CPA Management Center 打开 **Codex 调度器**，或者访问：

```text
/v0/resource/plugins/cpa-quota/status
```

页面提供：

- 与生产调度一致的账号队列和下一账号预览；
- 分别显示 CPA 优先级和插件优先级；
- 额度条、重置时间、不可用原因和熔断状态；
- 带有通俗安全说明的调度设置；
- 别名、备注、标签、分组和单账号插件优先级编辑；
- 额度刷新、日志查看/导出以及配置导入/导出；
- 中英文界面切换。

受保护的数据和操作需要 CPA 管理密钥。密钥只保留在当前浏览器页面会话中，
不会写入插件状态、`localStorage`、`sessionStorage`、导出文件或日志。

## 隐私与数据说明

插件在 CPA 进程内部运行，使用 CPA host callback 和插件自己的 CPA Management
API 路由。插件不运行外部服务，也不会向插件作者发送数据。

插件可能使用已经配置在 CPA 中的 Codex 凭据，向以下端点发送认证请求：

```text
GET https://chatgpt.com/backend-api/wham/usage
GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits
```

开启新周期激活后，插件还可能发送前文说明的极小 Codex 激活请求。

本地插件状态可能包含调度设置、额度快照、操作状态、日志、别名、备注、标签和
分组名称。不要在备注、别名、标签或分组标注中填写秘密。管理界面避免渲染
access token、Authorization header、Cookie 和其他凭据字段。

调度页面 Resource 路由只提供界面外壳，调度数据与操作走 Management 认证。
另有独立认证的 `/snapshot` Resource API，使用专用只读令牌，不能执行管理操作。

## 响应额度与冷却恢复

插件接入当前 CPA 的请求生命周期、普通响应、流式响应和 WebSocket 观察回调，按请求开始时冻结的账号绑定接收额度观测。额度观测与探测验证结果回写同一调度状态；旧观测不得覆盖较新的耗尽结果。正文、请求头和凭据只在有界内存中处理，不落盘。

CPAMP 提供 `/usage-service/quota-cooldowns/recover-verified` 时，插件只恢复由管理器持有且已核对原记录时间、账号绑定和新额度观测的额度冷却，不改手动禁用或其他故障状态。恢复请求结果未知时核对原记录，不盲目重复提交。连接配置使用状态目录内权限为 `0600` 的 `cpamp-recovery.json`，字段为 `url` 和 `key`；只允许 HTTPS 或数字回环 HTTP。缺配置或接口不可用时保留冷却并明确记录失败。

## Management API

下面路径均以 `/v0/management/plugins/cpa-quota` 为前缀，使用 `Authorization: Bearer <CPA管理Key>`。`Content-Type: application/json` 用于带 JSON 正文的写入。Resource 页面 `/v0/resource/plugins/cpa-quota/status` 只提供外壳，不携带受保护账号数据。

| 方法与路径                             | 用途                                                 |
| -------------------------------------- | ---------------------------------------------------- |
| `GET /status?format=json`              | 当前调度队列、额度窗口、探针结果、设置和运行状态。   |
| `GET /settings`                        | 读取完整设置。                                       |
| `PUT /settings`                        | 保存完整设置，包含接管、探针启停与排程。             |
| `POST /probe/trigger`                  | 立即执行一轮 5h 探测，无需正文。                     |
| `POST /refresh`                        | 请求后台刷新额度；不等于强制发送激活请求。           |
| `POST /refresh/account`                | 刷新单账号，正文为 `{"auth_id":"example-account"}`。 |
| `GET /logs`                            | 读取调度和探测日志。                                 |
| `GET /export`、`POST /import`          | 导出／导入调度设置及账号标注。                       |
| `GET /annotations`、`PUT /annotations` | 读取／整体更新账号和分组标注。                       |
| `PATCH /annotations/account`           | 更新单账号别名、备注、分组、标签和插件优先级。       |
| `PATCH /annotations/group`             | 更新分组名称、备注、标签等信息。                     |
| `POST /credentials/resolve`            | 处理当前账号凭据结果不确定状态；只在明确核对后操作。 |

### 从其他程序启停与修改排程

1. `GET /settings` 取得当前完整 JSON。
2. 只修改所需字段，例如 `enable_reset_probe` 或 `probe_trigger_times`。
3. 将修改后的**完整对象**提交到 `PUT /settings`，核对返回的实际设置。

设置接口不是 PATCH。仅提交 `{"enable_reset_probe":true}` 会遗漏其他布尔值和必要参数，不能作为安全的启用示例。管理 API 中 `probe_trigger_times` 和 `probe_send_windows` 是逗号分隔字符串；宿主 YAML 中则是数组。例如 API 值分别为 `"07:00,12:00,17:00"`、`"07:00-22:00"`。当前空字符串表示保留原排程／时段，不能用它清空已有设置。

### 手动触发与结果核对

```http
POST /v0/management/plugins/cpa-quota/probe/trigger
Authorization: Bearer <CPA管理Key>
```

接口复用自动探测的执行路径。成功返回 `202` 和 `trigger` 信息，包括触发时间、涉及账号数量、时段是否允许及是否越过每日时刻限制；这不表示每个账号都已生成新周期，须继续读取状态与日志核对。

- `409 reset_probe_disabled`：自动探针未开启。
- `409 probe_trigger_in_flight`：已有手动探测正在执行。
- `503`：运行时不可用或账号后台操作未获授权。
- 其他执行失败返回明确错误类别；上游 HTTP 状态在存在时通过 `upstream_status` 提供，不返回账号凭据。

供客户端汇总展示的只读接口是独立的 `GET /v0/resource/plugins/cpa-quota/snapshot`，使用专用读取令牌，详见[额度快照 API](status-snapshot.md)。不要把 CPA 管理 Key 交给只需读取快照的调用方。

## 许可证

MIT License。参见 [LICENSE](../LICENSE)。
