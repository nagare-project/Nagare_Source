# Source Spec v1

Source Spec v1 是 Nagare 的声明式来源格式。Animeko、Kazumi、Nagare 旧规则和社区规则在进入运行时前都必须转换成该格式。规范文件使用 YAML，发布时规范化为 JSON。

规范标识固定为 `nagare-source/v1`，权威机器定义位于 [`schema/source-v1.schema.json`](../schema/source-v1.schema.json)。本文件描述字段语义；Schema 决定字段是否合法。

## 1. 顶层结构

```yaml
schema: nagare-source/v1
id: example-http
name: Example HTTP media
kind: web
tier: 1
enabled: true
origin: { ... }
search: { ... }
episodes: { ... }
resolve: { ... }
defaults: { ... }
limits: { ... }
selftest: { ... }
```

| 字段 | 语义 |
| --- | --- |
| `id` | 仓库内永久且唯一的来源 ID，同时必须等于文件名。只允许小写字母、数字和连字符。 |
| `kind` | `web` 或 `bt`。v1 不接受其他类型。 |
| `tier` | `0..9`，数值越小优先级越高。它是静态基线，不代表本次请求一定成功。 |
| `channel_tiers` | 按解析出的线路名覆盖 tier；用于保留 Animeko channel tiers。 |
| `enabled` | 可选，默认为 `true`。停用规则仍可保留以供诊断和迁移。 |
| `origin` | 上游生态、地址、版本、许可证和可选内容摘要。 |
| `search` | 用标题查询条目或 BT 发布项。 |
| `episodes` | WEB 来源的选集阶段；BT 来源不需要。 |
| `resolve` | 把播放页或发布项转换为传输候选。 |
| `defaults` | 规则无法提取时采用的分辨率、字幕语言和线路 tier。 |
| `matching` | 标题预处理、别名数量以及条目/集号强制匹配策略。 |
| `ranking` | 上游的次级排序权重和 seeders 能力，不取代来源 tier。 |
| `cache` | 来源搜索缓存 TTL；`0` 表示禁用。 |
| `limits` | 来源级并发、频率、超时、响应大小和跳转限制；BT 来源可用 `max_pages`（配合 URL 里的 `{{page}}`）多翻几页，老番的整季合集常不在第一页。 |
| `selftest` | 定时健康检查使用的稳定标题、集号和最低期望。 |

`origin.ecosystem` 只能是 `community`、`animeko`、`kazumi` 或 `nagare-v1`。自动导入器应把上游原始内容的 SHA-256 写入 `origin.digest`，便于判断 overlay 是否已过期。

## 2. 请求

所有请求必须显式提供 `method` 和 HTTP(S) `url`。后续阶段可以用单个模板变量引用前一阶段产生的绝对 URL，例如 `{{subject_url}}`：

```yaml
request:
  method: GET
  url: https://media.example.invalid/api/search
  query:
    q: "{{title}}"
  headers:
    Accept: application/json
  allowed_hosts:
    - media.example.invalid
  timeout_ms: 10000
  max_bytes: 2097152
  max_redirects: 3
```

v1 只允许 `GET` 和 `POST`。每个请求都必须给出非空 `allowed_hosts`。GET 不能携带 `body` 或 `form`；POST 可选择文本 `body` 或键值 `form`，但不能同时使用。模板替换发生在 URL、query、header、body 和 form 的字符串值中，然后再进行 URL 编码。

`allowed_hosts` 同时约束初始请求和重定向。`*.example.org` 只匹配其子域，不匹配根域。运行时还必须拒绝回环、私网、链路本地、Unix socket、`file:` 以及 DNS 重绑定到这些地址的结果。静态校验器只能检查字面 IP，运行时必须在每次连接前再次检查解析结果。

规则中禁止提交 `Authorization` 和 `Cookie` 固定值。需要 Cookie 的来源使用 `resolve.cookie_policy: source`，其 jar 只属于当前来源和当前隔离会话。

## 3. 响应和提取器

`response` 可为 `json`、`html`、`xml`、`rss` 或 `text`。`items` 选出列表，`fields` 从每个列表项提取命名字段：

```yaml
items: $.items[*]
fields:
  title: $.name
  subject_key: $.id
  number:
    type: jsonpath
    expression: $.episode
    transforms:
      - trim
      - parse_episode
```

字符串提取器是与响应类型对应的简写：JSON 使用 JSONPath，HTML 使用 CSS Selector，XML/RSS 使用 XPath。对象形式可以明确指定：

- `jsonpath`：受限 JSONPath，不执行表达式或脚本。
- `css`：CSS Selector；`attribute` 缺省时读取规范化文本。
- `xpath`：XPath 1.0 子集，不开放扩展函数。
- `regex`：RE2 语法；`group` 默认为 `0`。
- `field`：引用同一条记录中已经提取的字段，用于继续执行 transform。
- `template`：用已提取字段和受控索引变量构造字符串；用于 API 规则生成最终播放页，不执行表达式。

`any` 按顺序选择第一个非空提取结果，用于上游同一值可能出现在多个位置的情况：

```yaml
magnet:
  any:
    - { type: xpath, expression: ./enclosure/@url }
    - { type: xpath, expression: ./link/text() }
  transforms: [trim]
```

若两个独立 selector/JSONPath 返回平行数组，collection 使用 `mode: zipped`，各字段提取器使用 `scope: document`。结果按下标合并，以最短数组长度为准；普通 `items` 模式则相对每个 item 提取字段。

所有实现至少支持以下无副作用 transform：

| Transform | 结果 |
| --- | --- |
| `trim`、`lowercase`、`uppercase` | 基础字符串规范化。 |
| `html_decode`、`url_decode` | 解码 HTML entity 或百分号编码。 |
| `absolute_url` | 相对于产生当前值的响应 URL 解析。 |
| `parse_episode` | 提取十进制集号；无法确定时返回错误，不得回退为第一集。 |
| `parse_size`、`parse_datetime` | 规范化为 bytes 或 RFC 3339。`parse_datetime` 接受 RFC 1123/822/3339 与 `2006-01-02 15:04:05 -0700`；不带时区的串按 `assume_offset`（如 `"+08:00"`，Mikan 的 pubDate 是北京时间裸串）解释，缺省当 UTC。 |
| `parse_fansub` | 从规范发布标题中提取字幕组。 |
| `magnet` | 从 infohash 创建 magnet，可引用标题字段并附加 tracker。 |
| `normalize_infohash` | 规范化 40 位十六进制或 32 位 base32 infohash。 |
| `regex`、`replace`、`prepend`、`append`、`default` | 带参数的受控字符串转换。 |

正则统一采用 RE2，禁止回溯断言和反向引用，保证不同语言运行时结果和资源上限一致。

## 4. 阶段与变量

### 4.1 Search

WEB `search.fields` 至少应产生 `title` 和稳定的 `subject_key`。BT 搜索直接产生候选所需字段，通常包括 `title`、`magnet` 或 `torrent_url`、`episode`、`info_hash`、`published_at`、`size` 和 `seeders`。

Search 可使用请求变量：

- `title`：当前尝试的标题别名。
- `keyword`：`title` 的兼容别名，供 Kazumi importer 映射。
- `episode` / `episode_number`：请求中的明确集号。
- `source`：来源 ID。

运行时应依次尝试标题别名、去重结果，并受来源频率限制约束。

### 4.2 Episodes

WEB 来源必须有 `episodes`。单线路页面直接使用 `items + fields`；多线路页面使用 `lines`：

```yaml
episodes:
  request:
    method: GET
    url: https://example.invalid/show/{{subject_key}}
  response: html
  lines:
    items: .playlist
    fields:
      channel: .name
      line_key:
        type: css
        expression: a
        attribute: data-line
    episodes:
      items: a.episode
      fields:
        number:
          type: regex
          expression: "第([0-9]+)集"
          group: 1
          transforms: [parse_episode]
        play_url:
          type: css
          expression: :scope
          attribute: href
          transforms: [absolute_url]
```

Episodes 请求可使用 search 的全部输出字段；多线路的剧集提取还可引用线路字段。

API 选集可以在 `episodes.variables` 中声明相对于整份响应提取的共享字段。嵌套线路/剧集还提供从零开始的 `line_index`、`episode_index`，以及从一开始的 `line_number`、`episode_number`。这些值可以由 `template` extractor 构造播放页：

```yaml
episodes:
  variables:
    slug: {type: jsonpath, expression: $.data.slug, scope: document}
  lines:
    # ...
    episodes:
      fields:
        play_url:
          type: template
          expression: https://example.invalid/{{slug}}?line={{line_index}}&episode={{episode_index}}
```

### 4.3 Resolve

`mode: direct` 表示 URL 已在字段中，`mode: browser_sniff` 表示先访问播放页，再从隔离浏览器的网络请求中选择媒体地址。

```yaml
resolve:
  mode: browser_sniff
  transport: hls
  url: "{{play_url}}"
  match:
    include:
      - "\\.m3u8(?:\\?|$)"
    exclude:
      - "/advertising/"
    nested_include:
      - "/(?:player|embed)/"
  request_headers:
    Referer: https://example.invalid/
  cookie_policy: source
  allowed_hosts:
    - example.invalid
    - "*.cdn.example.invalid"
  timeout_ms: 15000
```

`browser_sniff` 必须给出 `match.include` 和 `allowed_hosts`。匹配第一个候选并不代表成功：运行时仍要确认传输类型、允许域名、集号依据，并在返回前做轻量可读性验证。

可选 `match.allow_verified_media: true` 允许已通过实际 HTTP 2xx 与 video/HLS 内容验证的地址绕过 include URL 模式，适用于无文件扩展名的 CDN。exclude 始终生效；只有明确提供验证证据的运行时支持该能力，普通页面 URL 或未经验证的 DOM 提示不能使用它。入口和顶层页面受 `allowed_hosts` 约束，嵌套页面依赖及最终媒体仍需通过运行时的公网地址校验。

传输映射为：

- `hls` → Candidate `transport.type: hls`。
- `http` → Candidate `transport.type: http`。
- `auto` → 根据嗅探结果在 HLS 与普通 HTTP 间判断。
- `torrent` → 根据字段值在 magnet 与 HTTP `.torrent` 间判断。
- `magnet` / `torrent_file` → Candidate `transport.type: torrent` 的明确变体。

`cookies` 只允许非敏感的固定来源偏好 cookie，每项为一个 `name=value`。会话、认证或验证码 cookie 必须由隔离浏览器在当前解析会话中产生，不能进入规则或日志。

## 5. 匹配要求

候选必须按以下优先顺序建立依据：精确剧集 ID、精确条目 ID、标题别名与集号、发布日期辅助校验。每个 Candidate 都必须返回 `matchConfidence` 和 `match.basis`。发布日期只能辅助，不能单独证明标题和集号。

若集号缺失、冲突或无法解析，来源应返回 `episode_not_found` 或 `episode_ambiguous`，不得猜测第一集。

## 6. 安全与持久化

- 规则不执行 JavaScript、Shell 或宿主语言代码。
- 临时媒体 URL、URL query、Cookie 和授权头不得写回来源、索引、普通日志或 fixture。
- 日志可以保留 scheme、host 和脱敏后的 path；query 默认整体替换为 `[REDACTED]`。
- HLS/HTTP Candidate 可携带 `expiresAt`，过期后必须重新解析。
- 私有种子的 tracker 策略必须在读取 torrent `private` 标记后决定。

完整示例见 [`sources/web/example-http.yaml`](../sources/web/example-http.yaml) 和 [`sources/bt/example-rss.yaml`](../sources/bt/example-rss.yaml)。

仓库提供的统一执行器会把 WEB direct、WEB browser-sniff 与 BT 结果全部转换为 Candidate，并通过 Plugin API 并发输出。运行时入口和 NDJSON 行为见 [Plugin API v1](plugin-api-v1.md)；独立 BT 抓取、离线自检与索引输入见 [BT Source Crawler](bt-crawler.md)。
