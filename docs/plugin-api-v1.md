# Plugin API v1

Plugin API v1 是 Nagare 与来源运行时之间的本机 HTTP/JSON 协议。当前进程端和 Nagare 客户端都只接受显式回环 IP，不支持远程插件地址。协议主版本为 `1`。

## 0. 启动实现

仓库自带的 Go 进程端可直接执行：

```sh
go run ./cmd/nagare-source serve \
  --root . \
  --listen 127.0.0.1:7788 \
  --version dev
```

`serve` 在开始监听前校验全部来源，只接受 `127.0.0.0/8` 或 `::1` 的显式 IP；不能用 `localhost`、通配地址或外网地址绕过本地边界。端口设为 `0` 时由系统分配空闲端口。监听成功后，stdout 恰好写一行 UTF-8 JSON，然后不再承载其他输出：

```json
{"event":"ready","protocol":"nagare-plugin-launch/v1","url":"http://127.0.0.1:43127"}
```

Nagare 对 readiness 使用 10 秒超时和 16 KiB 单行上限，严格校验字段与显式回环 URL，再请求 manifest 协商 Plugin API v1。插件的运行诊断应写 stderr，并自行脱敏；Nagare 不把子进程 stderr 复制到普通日志。禁用插件、重新配置或退出 Nagare 时，宿主会取消请求并终止子进程。需要指定浏览器时使用 `--chrome PATH`。

可用 `make selftest-plugin` 执行离线运行时/API 回归。`example-http` 与 `example-rss` 使用占位域名，默认禁用，不参与生产找源；插件 selfcheck 对禁用来源返回 `disabled`。需要校验 BT 示例时，可显式使用 `crawl-bt --response-file` 运行离线 fixture。普通启动读取仓库中已启用的真实来源。

## 1. 通用规则

- 基础路径为 `/v1`，请求和普通响应使用 UTF-8 JSON。
- `POST /v1/candidates` 与 `POST /v1/releases` 使用 `application/x-ndjson` 流式返回。
- POST 请求要求 `Content-Type: application/json`，请求体上限为 1 MiB，开始流之前拒绝未知字段和非法集号。
- 未识别字段按对应 JSON Schema 的 `additionalProperties` 规则处理；请求 v1 默认拒绝未知字段。
- 每个请求可携带 `X-Request-ID`；插件应原样返回，日志也使用该 ID 关联，不能记录用户令牌。
- 已完成协商的客户端可发送 `X-Nagare-Protocol-Version: 1`；不支持或无法解析的版本返回 HTTP `426`。
- 客户端断开连接或取消请求时，插件必须取消尚未完成的来源任务和浏览器会话。

## 2. Manifest 与版本协商

`GET /v1/manifest`

```json
{
  "id": "org.nagare.source.community",
  "name": "Nagare Community Sources",
  "version": "0.1.0",
  "protocolVersions": [1],
  "sourceSchemaVersions": [1],
  "capabilities": ["web", "browser_sniff", "bt", "bt_releases", "ndjson"]
}
```

`bt_releases` 表示插件提供 `POST /v1/releases`（整部作品 BT 搜索，见第 5 节）；没有这个能力的旧插件只能逐集调用 `/v1/candidates`。客户端取自身与插件 `protocolVersions` 的最高交集。没有交集时停止调用并显示 `unsupported_protocol`。v1 新增可选字段不提升主版本；删除字段、修改既有语义或默认行为必须发布新的协议主版本。

## 3. Sources

`GET /v1/sources`

```json
{
  "sources": [
    {
      "id": "example-http",
      "name": "Example HTTP media",
      "kind": "web",
      "tier": 1,
      "version": "1",
      "enabled": true,
      "status": "healthy",
      "capabilities": ["direct", "web"]
    }
  ]
}
```

`status` 为 `healthy`、`degraded`、`unavailable`、`interactive_required` 或 `disabled`。状态影响排序和 UI，不得覆盖本次调用产生的明确错误。

## 4. Candidates

`POST /v1/candidates` 的请求体必须通过 [`resolve-request-v1.schema.json`](../schema/resolve-request-v1.schema.json)。成功响应立即返回：

```http
HTTP/1.1 200 OK
Content-Type: application/x-ndjson
Cache-Control: no-store
```

所有启用来源并发启动；请求带 `preferences.transports`（允许列表，如 `["torrent"]`）时只启动能产出这些 transport 的来源，客户端用它做「只要 BT」的快路径，不必等整队浏览器嗅探超时。每行是一个完整 JSON 事件，以换行结束：

进程端先把请求交给统一 Source Spec 运行时：WEB 来源执行搜索、条目匹配、选集、线路与 direct/browser resolve；BT 来源复用同一安全抓取器并转换为 torrent Candidate。CSS、XPath、受限 JSONPath、正则、模板和无副作用 transform 都在声明式运行时执行。每个来源的并发数、请求频率、总 deadline、响应大小、跳转和 `allowed_hosts` 独立生效。

### `candidate`

```json
{"event":"candidate","candidate":{"schema":"nagare-candidate/v1","id":"example-http:400602:3:main","sourceId":"example-http","tier":1,"matchConfidence":1,"match":{"basis":["subject_id","title_episode"],"episodeNumber":3},"transport":{"type":"hls","url":"https://cdn.example.invalid/e3.m3u8"},"metadata":{"resolution":"1080P","episode":3}}}
```

`candidate` 必须通过 [`candidate-v1.schema.json`](../schema/candidate-v1.schema.json)。同一请求内 `candidate.id` 必须唯一。插件发现候选后立即发送，不等待其他来源。BT 候选的 `metadata.title` 是来源上的原始发布标题（字幕组、季数、清晰度都在里面），客户端用它展示和做本机解析；`match.subjectTitle` 只是请求里的作品名，不能替代它。

### `source_error`

```json
{"event":"source_error","sourceId":"example","category":"resolve_timeout","message":"source exceeded its configured deadline","retryable":true}
```

固定错误分类如下：

| 分类 | 含义 |
| --- | --- |
| `invalid_request` | 请求未通过 Schema 或语义检查。 |
| `unsupported_protocol` | 没有共同协议版本。 |
| `source_disabled` | 来源被用户或仓库禁用。 |
| `unsupported_factory` | Animeko factory 不受导入器支持。 |
| `unsupported_rule` | 上游字段或规则无法无损表示。 |
| `interactive_required` | 需要验证码或人工交互。 |
| `search_timeout` / `search_failed` | 搜索超时或失败。 |
| `no_subject_match` | 没有可靠条目匹配。 |
| `episode_not_found` / `episode_ambiguous` | 集号缺失或存在冲突。 |
| `resolve_timeout` / `resolve_failed` | 最终解析超时或失败。 |
| `browser_blocked` | 浏览器策略、验证码或站点行为阻止解析。 |
| `unsafe_redirect` | 请求或跳转越过网络安全边界。 |
| `invalid_candidate` | 来源输出无法满足 Candidate Schema。 |
| `cancelled` | 客户端取消了工作。 |

单个来源失败不改变 HTTP 状态，仍通过流内 `source_error` 报告。只有在开始流之前即可确认的整体错误（例如非法请求）使用 HTTP `400`；协议不兼容使用 `426`；运行时不可用使用 `503`。

### `done`

```json
{"event":"done","queried":18,"succeeded":12,"failed":6,"durationMs":1380}
```

`done` 恰好出现一次并且是最后一个事件。`queried = succeeded + failed`；一个来源产生多个 Candidate 仍只计作一次成功。若客户端已断开，则无需尝试写入 `done`。

完整流 fixture 位于 [`fixtures/ndjson/candidates.jsonl`](../fixtures/ndjson/candidates.jsonl)。

## 5. Releases（整部作品 BT 搜索）

`/v1/candidates` 一次只解一集；BT 选集窗口要的是另一种模型：**按作品标题把所有 BT 来源搜一遍、拿回全部发布，选集交给客户端在本机完成**（与 animego 网站的磁力搜索同一模型）。`/v1/releases` 就是这个入口，两者并存：在线来源和逐集 BT 仍走 `/v1/candidates`，行为不变。manifest 带 `bt_releases` 时才可调用。

### 请求

`POST /v1/releases`，请求体必须通过 [`release-search-request-v1.schema.json`](../schema/release-search-request-v1.schema.json)；协议头、`Content-Type`、1 MiB 上限、单一 JSON 对象、拒绝未知字段等开流前检查与 `/v1/candidates` 相同。

```json
{
  "schema": "nagare-release-search/v1",
  "subject": {
    "ids": {"anilist": "188525"},
    "titles": ["描绘直至生命尽头", "Kore Kaite Shine", "これ描いて死ね", "Draw This, Then Die!"]
  }
}
```

- `subject.titles` 必填，1–4 个非空字符串。服务端去首尾空白、丢空串、按大小写不敏感去重（保留第一次出现的写法），最多 4 个；什么都不剩时返回 HTTP `400 invalid_request`。
- 标题**原样**作为搜索关键词，不做 `/v1/candidates` 那种紧凑写法 / 数字季的变体扩展。
- `subject.ids` 可选（键名规则同 resolve 请求），只用于关联与诊断，不参与搜索。

### 行为

- 所有已启用的 `kind: bt` 来源并发启动，每个来源一个任务。
- 每个标题一个请求，只取第 1 页（`{{page}}` 恒为 1，与 `limits.max_pages` 无关），集号变量为空；同一来源内的请求仍由 `limits.requests_per_minute` 隔开，并占用来源的 `limits.concurrency` 槽位。
- **不应用** `matching.require_episode` / `require_subject`：抽取与记录规范化之外不再筛任何条目，没有集号的合集、繁体或别名标题都会返回。
- 每个来源的 deadline 为 `min(limits.timeout_ms, 8 秒)`，从进入来源算起（含排队等并发槽位的时间）。到点时**保留已经返回的标题的结果**作为部分结果交出，不丢弃。
- 某个标题的响应里有条目、却一条记录都抽不出来（规则已经对不上站点）时，这个标题按 `search_failed` 失败处理，而不是当成零结果——「零 ≠ 死」。
- 同一来源内按 sourceId/infoHash（只有种子地址时按地址）去重：做种数已知且更高的优先，否则保留第一次出现的（按标题顺序）。不跨来源去重——同一 infohash 在不同来源各出现一次，由客户端合并。
- 排序：做种数降序（未知排后）→ 发布时间降序（未知排后）→ 标题升序。
- 某个来源结束就立即写出它的全部 `release`，紧跟它的 `source_result`，不等其他来源；写入由一个写者串行完成，同一来源的事件不会与别的来源交错。
- 客户端断开或取消请求时，插件取消全部来源工作。

### 缓存

每个来源在插件进程内存里缓存整部作品搜索结果，键为 `sourceId` + 小写后排序的标题集合（标题顺序、大小写不同的同一请求共用一条）。只缓存**完整成功**的结果（所有标题请求都成功、未撞 deadline）：有发布缓存 1 小时，零结果缓存 5 分钟；规则声明了 `cache.ttl_ms` 时以它为上限，`0` 表示该来源不缓存。失败与部分结果不缓存。每个来源最多 256 条，按最近最少使用淘汰。命中时立即重放并在 `source_result` 里标 `"cached": true`；排队等并发槽位的重复请求拿到槽位时会再查一次缓存。插件重启即清空。

### 事件

```json
{"event":"release","release":{"id":"garden:<infohash>","sourceId":"garden","title":"[Group] Title - 01 [1080p]","transport":{"type":"torrent","magnet":"magnet:?xt=urn:btih:...","infoHash":"<40 位小写十六进制>","torrentUrl":"https://..."},"fansub":"Group","sizeBytes":123,"seeders":5,"publishedAt":"2026-07-04T12:00:00Z","episode":"1"}}
{"event":"source_result","sourceId":"garden","state":"ok","count":42,"partial":false,"cached":false,"durationMs":812}
{"event":"source_result","sourceId":"nyaa","state":"failed","count":0,"category":"search_failed","message":"source request failed","retryable":true,"durationMs":1033}
{"event":"done","queried":6,"succeeded":5,"failed":1,"durationMs":8012}
```

`release` 必须通过 [`release-v1.schema.json`](../schema/release-v1.schema.json)：

| 字段 | 说明 |
| --- | --- |
| `id` | 与 BT Candidate 相同：`<sourceId>:<infohash>`，只有种子地址时 `<sourceId>:url:<16 位十六进制地址摘要>`。 |
| `title` | 来源上的原始发布标题，超过 512 个字符时在字符边界截断。 |
| `transport` | `type` 恒为 `torrent`；`magnet` / `infoHash`（40 位小写十六进制）/ `torrentUrl` 为空时省略，至少有一个。 |
| `fansub`、`sizeBytes`、`seeders`、`publishedAt` | 来源给了才有；`publishedAt` 为 RFC 3339 UTC。 |
| `episode` | 从标题解析出的单集号（十进制字符串，如 `"1"`、`"12.5"`）；合集或解不出时省略。 |

不合 Schema 的单条发布被跳过（插件 stderr 记来源 id 与条数），不连坐整个来源；若一个来源的发布全部无效，该来源报 `failed` / `invalid_candidate`。

每个被查询的来源**恰好一条** `source_result`：

| `state` | 含义 | 附带字段 |
| --- | --- | --- |
| `ok` | 至少一条发布。`partial: true` 表示撞到 deadline 或有标题请求失败，结果不完整。 | `count`、`partial`、`cached`、`durationMs` |
| `zero` | 所有请求都成功，但没有任何发布（正常的「没有资源」）。 | `count: 0`、`partial: false`、`cached`、`durationMs` |
| `failed` | 没有发布，且至少一个请求失败或超时。 | `count: 0`、`category`、`message`、`retryable`、`durationMs` |

`category` 沿用第 4 节的固定分类（`search_failed`、`search_timeout`、`unsafe_redirect`、`cancelled`、`invalid_candidate` 等）。多个标题都失败时报告标题顺序上的第一个失败。

`done` 恰好一次且最后出现：`queried` 为已启用 BT 来源数，`succeeded` 为 `ok` 与 `zero` 的来源数，`failed` 为其余。

## 6. Self-check

`POST /v1/selfcheck`

```json
{
  "sourceIds": ["example-http"],
  "mode": "network"
}
```

`mode` 为 `fixture` 或 `network`。fixture 模式不得访问网络。响应为普通 JSON，逐来源返回 `healthy`、`degraded`、`unavailable` 或 `interactive_required`、总耗时和结构化错误分类；实现可以追加分阶段耗时。自检不返回或持久化最终签名媒体 URL。

当前 CLI 会自动连接 `fixtures/responses/<sourceId>.xml|rss|json|txt` 中存在的 BT fixture；缺少 fixture 的来源在 fixture 模式返回结构化 degraded 结果，不会退回网络请求。重复或未知 source ID 在执行任何自检前返回 HTTP `400`。

## 7. Health

`GET /v1/health` 仅表示插件进程是否可服务，不聚合站点是否全部正常：

```json
{
  "status": "ok",
  "version": "0.1.0",
  "protocolVersions": [1],
  "uptimeSeconds": 3600
}
```

站点级状态由 `/v1/sources` 和 `/v1/selfcheck` 提供。

## 8. 选择与并发约定

插件流只负责尽快交付候选，不决定播放器最终选择。Nagare 同时启动 WEB 与 BT 工作；验证通过的高优先级 WEB 候选可以立即起播，BT 继续运行并进入换源列表。客户端排序至少考虑 tier、channel tier、匹配置信度、滚动成功率、解析延迟、分辨率、字幕偏好，以及 BT 的做种数、发布时间和体积。

Nagare 客户端只启动用户显式配置的本地插件，不接受任意远程 URL。客户端边读 NDJSON 边更新候选列表；来源 tier 不高于 1 且匹配置信度不低于 0.8 的在线候选可以立即起播，无需等待 `done` 或 BT 查询完成。其余候选按来源 tier、channel tier、匹配置信度、在线/BT、分辨率和 BT 做种数稳定排序。

当前候选同步启动失败，或 mpv 对对应播放 `fileId` 报告 `end-file: error` / 进程异常时，客户端尝试下一在线候选，在线耗尽后进入最佳 BT。正常 EOF、用户停止、播放器退出和手动换源都不触发自动回退。查询可由用户取消，已到达的候选继续保留为手动换源与重试入口；单来源错误按固定分类显示，不阻塞其他来源。

HLS/HTTP URL 与请求头只保存在当前内存会话并直接交给 mpv。候选流响应使用 `Cache-Control: no-store`；播放器状态只暴露不含 URL/请求头的 `fileId` 和错误原因，界面与普通日志不得显示临时凭据。
