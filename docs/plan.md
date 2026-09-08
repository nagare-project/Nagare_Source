# Nagare Source 统一来源仓库与插件计划

## 1. 目标

Nagare Source 建立一个与客户端语言、播放器和原始规则生态无关的来源标准。Animeko、Kazumi、Nagare 旧规则以及社区规则先被转换成规范化来源，再通过同一个插件接口向 Nagare 提供播放候选。

核心关系如下：

```text
Animeko subscription ─┐
KazumiRules ──────────┼─> importer + overlay ─> Source Spec v1 ─> resolver runtime
Nagare schema 1 ──────┤                                      │
community source ─────┘                                      └─> Candidate API ─> Nagare
```

原始生态只是规则的输入来源。运行时不存在 `animeko source`、`kazumi source` 和 `native source` 三套分支；所有来源都必须通过同一个 schema、测试流程和候选接口。

## 2. 已确认的上游能力

### Animeko

Animeko 的订阅文件以 `factoryId + version + arguments` 描述来源，常见工厂为：

- `rss`：BT/RSS 来源，例如 AnimeGarden。
- `web-selector`：使用 CSS Selector、JSONPath、正则和 WebView 解析在线源。

Animeko 已有 `MediaSourceTier` 和 channel tier。数值越小优先级越高；高质量 WEB 来源可以在查询完成后立即选中。其媒体位置模型能表达磁力、HTTP `.torrent`、HTTP/HLS 文件和需要 WebView 解析的播放页面。

这些类型属于 Animeko 的 Kotlin 内部接口，订阅 JSON 可以跨语言读取，但不能把 Kotlin `MediaSource` 当作 Nagare 的运行时插件直接调用。

### Kazumi

KazumiRules 支持 XPath 和 API 两种搜索/选集方式：

- XPath：从 HTML 提取作品、线路、剧集和播放页面。
- API：使用 GET/POST、请求头、查询参数、请求体和受限 JSONPath 提取相同信息。

Kazumi 规则通常只解析到剧集播放页，最终 HLS/MP4 地址由 Kazumi 的 WebView 视频嗅探器获得。因此导入 Kazumi 规则时，规则转换和浏览器解析能力必须同时实现。

### 结论

两者没有共享的运行时 API，也不能直接互相加载规则。它们存在足够大的公共语义，可以通过统一中间模型整合：请求、列表提取、字段提取、线路、剧集、播放页、视频嗅探、请求头、来源优先级和播放传输类型。

## 3. 仓库结构

```text
Nagare_Source/
├── README.md
├── LICENSE
├── docs/
│   ├── plan.md
│   ├── source-spec-v1.md
│   ├── plugin-api-v1.md
│   └── contributing-sources.md
├── schema/
│   ├── source-v1.schema.json
│   ├── resolve-request-v1.schema.json
│   ├── candidate-v1.schema.json
│   └── repository-index-v1.schema.json
├── sources/
│   ├── web/
│   └── bt/
├── importers/
│   ├── animeko/
│   ├── kazumi/
│   └── nagare-v1/
├── overlays/
├── fixtures/
├── tests/
├── reports/
│   └── health.json
└── dist/
    ├── index.json
    ├── sources/
    └── bt-index.sqlite.zst
```

`sources/` 是唯一正式规则集合。`importers/` 只负责读取上游格式并产生统一模型；`overlays/` 保存无法自动推导的修补，例如线路优先级、Referer、集号正则和浏览器嗅探条件。

`dist/` 完全由 CI 生成，不接受直接修改。

## 4. Source Spec v1

每个来源由一个声明式 YAML 文件描述。第一版禁止来源携带任意脚本，运行时只开放已审核的请求、提取、转换和浏览器嗅探原语。

```yaml
schema: nagare-source/v1
id: tvtfun
name: TvTFun
kind: web
tier: 0

origin:
  ecosystem: animeko
  upstream: https://example.invalid/source.json
  version: "2"
  license: MIT

search:
  request:
    method: GET
    url: https://example.invalid/api/search
    query:
      q: "{{title}}"
  response: json
  items: $.data.items[*]
  fields:
    title: $.name
    subject_key: $.id

episodes:
  request:
    method: GET
    url: https://example.invalid/api/subjects/{{subject_key}}
  response: json
  # 线路和剧集的提取定义由 source-spec-v1 完整文档规定。

resolve:
  mode: browser_sniff
  match:
    include:
      - "\\.m3u8(?:\\?|$)"
      - "\\.mp4(?:\\?|$)"
  request_headers:
    Referer: https://example.invalid/

defaults:
  resolution: 1080P
  subtitle_languages: [zh-Hans]
```

### 4.1 允许的来源类型

- `web`：在线网页、JSON API、HLS、MP4 或其他可由播放器直接读取的 HTTP 媒体。
- `bt`：RSS/API 搜索得到的磁力或 HTTP `.torrent`。
- `library`：Jellyfin、Emby 等用户自有媒体库，进入后续版本。

### 4.2 允许的解析原语

- HTTP `GET`、`POST`。
- JSON、XML、RSS 和 HTML 响应。
- JSONPath、CSS Selector、XPath。
- 正则提取和预定义字符串转换。
- 模板变量替换。
- 受控浏览器网络嗅探。
- 固定请求头、来源 Cookie jar 和 Referer 传递。

浏览器嗅探在独立运行时中执行。规则不能访问本机文件、Nagare token、其他来源 Cookie 或任意内网地址。

## 5. 统一查询模型

来源查询不能只依赖一个生态的 ID。请求同时携带外部 ID、名称别名和明确的剧集坐标：

```json
{
  "schema": "nagare-resolve-request/v1",
  "subject": {
    "ids": {
      "bangumi": "400602",
      "anilist": "154587"
    },
    "titles": [
      "葬送的芙莉莲",
      "葬送のフリーレン",
      "Sousou no Frieren"
    ],
    "season": 1
  },
  "episode": {
    "id": "bgm:1172765",
    "number": "3",
    "absolute": 3,
    "airDate": "2023-09-29"
  },
  "preferences": {
    "subtitleLanguages": ["zh-Hans", "zh-Hant"],
    "maxResolution": "2160P"
  }
}
```

匹配顺序为精确剧集 ID、精确条目 ID、标题别名与集号、发布日期辅助校验。来源必须返回匹配置信度和采用的依据，不能在无法识别集号时默认猜第一集。

## 6. 统一候选模型

所有来源最终输出 `Candidate`，传输方式使用联合类型。

### 6.1 HLS/HTTP

```json
{
  "schema": "nagare-candidate/v1",
  "id": "tvtfun:400602:3:line-b",
  "sourceId": "tvtfun",
  "tier": 0,
  "matchConfidence": 1.0,
  "transport": {
    "type": "hls",
    "url": "https://cdn.example.invalid/episode-03.m3u8",
    "headers": {
      "Referer": "https://example.invalid/"
    },
    "expiresAt": 1788691200000
  },
  "metadata": {
    "resolution": "1080P",
    "subtitleLanguages": ["zh-Hans"],
    "channel": "线路B",
    "episode": 3
  }
}
```

### 6.2 BitTorrent

```json
{
  "schema": "nagare-candidate/v1",
  "id": "animegarden:0123456789abcdef0123456789abcdef01234567",
  "sourceId": "animegarden",
  "tier": 3,
  "matchConfidence": 0.95,
  "transport": {
    "type": "torrent",
    "magnet": "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
    "infoHash": "0123456789abcdef0123456789abcdef01234567",
    "fileIndex": 0
  },
  "metadata": {
    "resolution": "1080P",
    "subtitleLanguages": ["zh-Hans"],
    "fansub": "示例字幕组",
    "sizeBytes": 1073741824,
    "episode": 3
  }
}
```

传输字段参考 Stremio Stream Object 已验证的通用表达方式：直接 URL，或 `infoHash + fileIdx`。Nagare 扩展来源层级、番剧 ID、集号、匹配置信度和过期时间。

## 7. Plugin API v1

插件协议使用 HTTP/JSON，默认只监听回环地址。它既可以由 Nagare 启动为本地子进程，也可以由用户配置为远程服务。

### 7.1 端点

- `GET /v1/manifest`：插件身份、协议版本和能力。
- `GET /v1/sources`：可用来源、tier、状态和版本。
- `POST /v1/candidates`：查询一集的播放候选。
- `POST /v1/selfcheck`：运行指定来源的自检。
- `GET /v1/health`：运行时健康状态。

`POST /v1/candidates` 默认返回 `application/x-ndjson`。每找到一个候选就立即输出一行，不等待所有来源结束：

```jsonl
{"event":"candidate","candidate":{"sourceId":"tvtfun","tier":0,"transport":{"type":"hls","url":"https://cdn.example.invalid/e3.m3u8"}}}
{"event":"candidate","candidate":{"sourceId":"animegarden","tier":3,"transport":{"type":"torrent","infoHash":"0123456789abcdef0123456789abcdef01234567"}}}
{"event":"source_error","sourceId":"example","category":"resolve_timeout"}
{"event":"done","queried":18,"succeeded":12,"durationMs":1380}
```

每个事件都必须是一行完整 JSON。客户端断开请求后，插件取消仍在进行的解析任务。

### 7.2 版本协商

插件 manifest 声明：

```json
{
  "id": "org.nagare.source.community",
  "version": "0.1.0",
  "protocolVersions": [1],
  "sourceSchemaVersions": [1],
  "capabilities": ["web", "browser_sniff", "bt", "ndjson"]
}
```

协议采用主版本兼容规则。新增可选字段不提升主版本；修改字段语义、删除字段或改变默认行为必须发布新主版本。

## 8. 在线优先与 BT 回退

协调器同时开始在线源和 BT 来源查询。这里的“在线优先”只影响验证和播放选择，不应推迟 BT 搜索，否则在线失败后还要重新等待 BT 查询。

推荐流程：

1. 本地缓存由 Nagare 主程序优先检查。
2. 并发查询所有已启用来源，按域名执行并发和速率限制。
3. T0/T1 在线候选进入快速验证：获取 HLS manifest，或对 HTTP 文件执行小范围 Range 请求。
4. 第一个通过验证且集号匹配可靠的高优先级在线候选可立即起播。
5. 其他在线与 BT 候选继续进入列表，供用户换源。
6. 当前在线候选解析失败、验证失败或播放器加载失败时，协调器按排序选择下一在线候选，再回退到最佳 BT。

候选排序至少考虑：

- 来源 tier 与 channel tier。
- 集号匹配置信度。
- 最近滚动成功率。
- 解析延迟和首字节时间。
- 分辨率与字幕语言偏好。
- BT 做种数、发布时间和资源体积。

来源健康分只能影响排序，不能掩盖本次请求的明确错误。

## 9. 存储与更新策略

### 9.1 Git 仓库保存

- 规范化来源规则。
- 上游来源、版本、许可证和内容摘要。
- 离线解析 fixture。
- 人工 overlay。
- 聚合健康报告。
- Schema 与文档。

### 9.2 BT 索引发布

CI 定期抓取允许缓存的 BT RSS/API，规范化标题、infohash、发布时间、大小、字幕组和初步集号，生成 `bt-index.sqlite.zst` 发布产物。

生成数据库采用滚动保留策略，避免把每次抓取产生的大量变更写入 Git 历史。客户端可以先查本地索引，再向实时来源补查最新结果。

### 9.3 在线地址

仓库只保存在线来源规则和健康数据。最终 HLS/HTTP 地址、Cookie、授权头和短期签名只能存在于单次解析会话中：

- 日志默认脱敏 URL query 和敏感请求头。
- 候选可携带 `expiresAt`。
- 过期候选必须重新解析，不能写入长期缓存。

## 10. 上游导入器

### 10.1 Animeko importer

第一阶段支持：

- `factoryId: rss`。
- `factoryId: web-selector` version 2。
- `tier`、`channelTiers`。
- CSS Selector、JSONPath、集号正则。
- `matchVideo`、Cookie、Referer 和 User-Agent。
- `MagnetLink`、`HttpTorrentFile`、`HttpStreamingFile`、`WebVideo` 的语义映射。

无法识别的 factory 必须报告 `unsupported_factory`，不得静默丢弃。

### 10.2 Kazumi importer

第一阶段支持：

- XPath 搜索与选集。
- API GET/POST 搜索与选集。
- `@keyword`、`@source`、线路与剧集序号变量。
- Referer、User-Agent、legacy parser 和 native player 标记。
- 播放页到 `browser_sniff` 的映射。

验证码和需要交互的人机验证标记为 `interactive_required`。CI 不尝试绕过交互验证。

### 10.3 Nagare schema 1 importer

Nagare 现有磁力 YAML 映射为 `kind: bt`，保留请求模板、XML/JSON 字段提取、transforms、seeders 能力、priority 和 selftest。

## 11. 社区维护流程

社区可以提交统一 Source Spec，也可以提交上游导入映射或 overlay。每个 PR 必须通过：

1. JSON Schema 与 YAML 语法校验。
2. URL 模板、选择器和正则静态检查。
3. fixture 离线解析测试。
4. 固定自检标题的搜索与集号测试。
5. 播放页解析和候选类型检查。
6. 请求域名、跳转域名和浏览器网络范围审计。
7. 来源、许可证和版本字段检查。

合并后的定时任务执行在线健康检查并更新 `reports/health.json`。连续失败的来源先自动降级 tier 并在报告中标红，由维护者决定修复或停用；CI 不直接删除社区规则。

## 12. 安全边界

- 声明式规则不执行任意 JavaScript、Shell 或宿主语言代码。
- 普通 HTTP 来源拒绝回环、链路本地、私网、Unix socket 和 `file:` 地址。
- 浏览器解析器使用独立临时 profile，来源之间不共享 Cookie。
- 插件只能获得解析请求，不能获得 Nagare 登录 token、animego token 或用户媒体库凭据。
- 响应头、Cookie 和 URL query 在日志中按字段脱敏。
- BT 私有种子不能自动补公共 tracker；HTTP `.torrent` 在读取 private 标记后再决定 peer 发现策略。
- 每个来源设置并发、超时、响应体大小和跳转次数上限。

## 13. 实施阶段

### M0：协议定稿

- 完成四份 JSON Schema。
- 写 Source Spec 和 Plugin API 文档。
- 建立规范化测试工具和最小示例来源。
- 固定错误分类、版本协商和 NDJSON 事件语义。

验收：示例 WEB 与 BT 来源可以通过 schema 校验并生成候选 fixture。

### M1：BT 与 Animeko 订阅导入

- 导入 Nagare 现有 schema 1。
- 支持 Animeko `rss`。
- 支持 Animeko `web-selector` 的纯 HTTP/HTML/API 部分。
- 生成仓库 index 和 BT SQLite 发布产物。

验收：AnimeGarden 与至少一个公开测试 HTTP 媒体来源经过统一协议返回结果。

### M2：Kazumi 规则导入

- 支持 Kazumi XPath 与 API 规则。
- 建立公共字段转换和 overlay 机制。
- 对同一站点的 Animeko/Kazumi 规则做差异测试，避免重复来源。

验收：至少各一条 XPath、API 规则完成搜索、线路和集号解析。

### M3：浏览器解析运行时

- 实现隔离浏览器会话。
- 实现网络请求嗅探、URL 匹配、请求头与 Cookie 传递。
- 实现取消、超时、域名限制和日志脱敏。

验收：测试站点能从播放页解析出短期 HLS 地址，地址不写入磁盘和日志。

### M4：Nagare 插件接入

- Nagare 管理插件生命周期与版本协商。
- 实现 `/v1/candidates` NDJSON 消费。
- 统一本地、在线、BT 候选 UI。
- 实现在线快速选择、错误换源与 BT 回退。

验收：点击一集后可先播放在线候选，在线失败时自动切换下一来源或 BT，并保留手动换源入口。

当前状态：已完成。`nagare-source serve` 通过 `nagare-plugin-launch/v1` readiness 把系统分配的回环端口交给 Nagare；客户端只启动用户显式配置的本地子进程，完成显式回环校验、Plugin API v1 协商、增量 NDJSON 消费和退出清理。作品页在同一播放区域提供本地播放与来源入口，插件候选列表统一承载在线和 BT；可靠的高优先级在线候选可立即起播，同步启动失败或 mpv 异步播放错误会依次换源并最终复用 BT 管线，正常播完、用户停止和手动切换不会误触发回退。短期 URL、Cookie 和请求头不进入长期缓存、状态、界面或普通日志。真实子进程回环冒烟与前后端自动化测试均已通过，M4 端到端验收完成。

### M5：社区发布

- 完成贡献指南和规则模板。
- 定时同步获准使用的上游规则。
- 发布来源健康页和版本化 release。
- 为破坏性 schema 变更建立迁移工具。

验收：社区只维护一份 Source Spec，发布流程自动产生 Nagare 可用产物。

当前状态：贡献指南与持续校验模板、Source Health v1、定时网络健康页 workflow、带健康摘要的确定性仓库产物和语义化 tag release workflow 已完成。生产上游自动同步等待逐项许可证与再分发批准；破坏性迁移工具等待新 schema 版本及确定的字段映射，不能在只有 v1 时猜测未来迁移语义。

## 14. 完成标准

- Nagare 主程序不含任何站点专用解析代码。
- 所有来源经过同一候选模型进入播放器。
- Animeko 与 Kazumi 的受支持规则可以自动导入，无法导入的字段有明确诊断。
- 在线候选无需等待 BT 查询完成即可起播。
- BT 查询与在线解析并行，在线失败时无需重新搜索。
- HLS 临时凭据不进入 Git、长期缓存或普通日志。
- 来源失效、集号不确定、解析超时和播放器失败都能在 UI 中区分。
- 仓库产物可复现、带版本、带来源记录并能由社区通过 PR 维护。
