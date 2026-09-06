# Plugin API v1

Plugin API v1 是 Nagare 与来源运行时之间的 HTTP/JSON 协议。默认监听回环地址；远程部署必须使用 TLS 和部署方提供的认证。协议主版本为 `1`。

## 1. 通用规则

- 基础路径为 `/v1`，请求和普通响应使用 UTF-8 JSON。
- `POST /v1/candidates` 使用 `application/x-ndjson` 流式返回。
- 未识别字段按对应 JSON Schema 的 `additionalProperties` 规则处理；请求 v1 默认拒绝未知字段。
- 每个请求可携带 `X-Request-ID`；插件应原样返回，日志也使用该 ID 关联，不能记录用户令牌。
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

所有启用来源并发启动。每行是一个完整 JSON 事件，以换行结束：

### `candidate`

```json
{"event":"candidate","candidate":{"schema":"nagare-candidate/v1","id":"example-http:400602:3:main","sourceId":"example-http","tier":1,"matchConfidence":1,"match":{"basis":["subject_id","title_episode"],"episodeNumber":3},"transport":{"type":"hls","url":"https://cdn.example.invalid/e3.m3u8"},"metadata":{"resolution":"1080P","episode":3}}}
```

`candidate` 必须通过 [`candidate-v1.schema.json`](../schema/candidate-v1.schema.json)。同一请求内 `candidate.id` 必须唯一。插件发现候选后立即发送，不等待其他来源。

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

`mode` 为 `fixture` 或 `network`。fixture 模式不得访问网络。响应为普通 JSON，逐来源返回 `healthy`、`degraded`、`unavailable` 或 `interactive_required`，以及分阶段耗时和结构化错误分类。自检不返回或持久化最终签名媒体 URL。

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
