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
- `POST /v1/candidates` 使用 `application/x-ndjson` 流式返回。
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
  "capabilities": ["web", "browser_sniff", "bt", "ndjson"]
}
```

客户端取自身与插件 `protocolVersions` 的最高交集。没有交集时停止调用并显示 `unsupported_protocol`。v1 新增可选字段不提升主版本；删除字段、修改既有语义或默认行为必须发布新的协议主版本。

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

## 5. Self-check

`POST /v1/selfcheck`

```json
{
  "sourceIds": ["example-http"],
  "mode": "network"
}
```

`mode` 为 `fixture` 或 `network`。fixture 模式不得访问网络。响应为普通 JSON，逐来源返回 `healthy`、`degraded`、`unavailable` 或 `interactive_required`、总耗时和结构化错误分类；实现可以追加分阶段耗时。自检不返回或持久化最终签名媒体 URL。

当前 CLI 会自动连接 `fixtures/responses/<sourceId>.xml|rss|json|txt` 中存在的 BT fixture；缺少 fixture 的来源在 fixture 模式返回结构化 degraded 结果，不会退回网络请求。重复或未知 source ID 在执行任何自检前返回 HTTP `400`。

## 6. Health

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

## 7. 选择与并发约定

插件流只负责尽快交付候选，不决定播放器最终选择。Nagare 同时启动 WEB 与 BT 工作；验证通过的高优先级 WEB 候选可以立即起播，BT 继续运行并进入换源列表。客户端排序至少考虑 tier、channel tier、匹配置信度、滚动成功率、解析延迟、分辨率、字幕偏好，以及 BT 的做种数、发布时间和体积。

Nagare 客户端只启动用户显式配置的本地插件，不接受任意远程 URL。客户端边读 NDJSON 边更新候选列表；来源 tier 不高于 1 且匹配置信度不低于 0.8 的在线候选可以立即起播，无需等待 `done` 或 BT 查询完成。其余候选按来源 tier、channel tier、匹配置信度、在线/BT、分辨率和 BT 做种数稳定排序。

当前候选同步启动失败，或 mpv 对对应播放 `fileId` 报告 `end-file: error` / 进程异常时，客户端尝试下一在线候选，在线耗尽后进入最佳 BT。正常 EOF、用户停止、播放器退出和手动换源都不触发自动回退。查询可由用户取消，已到达的候选继续保留为手动换源与重试入口；单来源错误按固定分类显示，不阻塞其他来源。

HLS/HTTP URL 与请求头只保存在当前内存会话并直接交给 mpv。候选流响应使用 `Cache-Control: no-store`；播放器状态只暴露不含 URL/请求头的 `fileId` 和错误原因，界面与普通日志不得显示临时凭据。
