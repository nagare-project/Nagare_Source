# 导入器与 Overlay

导入器把上游规则转成 Source Spec v1，随后应用人工 overlay，再执行与正式来源相同的 Schema 和安全校验。任何 error 诊断都会阻止全部输出，避免批量导入产生难以察觉的部分成功。

实现参考了 [Animeko 官方源码](https://github.com/open-ani/animeko) 的 codec/config 定义和 [Animeko 官方订阅仓库](https://github.com/creamycake-anime/ani-subs) 的实际规则结构。

## Animeko

```sh
go run ./cmd/nagare-source import animeko \
  --input upstream/ani-subs \
  --upstream 'https://github.com/owner/repo/blob/main/{path}' \
  --license AGPL-3.0-only \
  --out imported \
  --overlays overlays \
  --diagnostics reports/import-animeko.jsonl
```

支持单个 `{factoryId, version, arguments}`、`{mediaSources: [...]}` subscription，以及递归包含 `.json` 的目录。目录模式按相对路径稳定排序；`{path}` 会替换成经过 URL 转义的相对路径。未包含 `{path}` 时，相对路径追加到 upstream URL。

### RSS v1

- `{keyword}`、`{page}` 转为 `{{title}}`、`{{page}}`。
- RSS item 的 enclosure/link 映射为 `download_url` 回退。
- 标题、体积、发布时间、字幕组、集号和 infohash 进入统一字段。
- `filterByEpisodeSort`、`filterBySubjectName`、tier 与速率限制被保留。
- magnet 与 HTTP `.torrent` 统一使用 `transport: torrent`，运行时按值判断。

### web-selector v2

- 支持 `a`、`indexed`、`json-path-indexed` 三种条目格式。
- 支持 `no-channel` 和 `index-grouped` 两种剧集/线路格式，包括独立链接 selector。
- 保留 channel tiers、标题/集号过滤、缓存 TTL、默认清晰度与字幕语言。
- `matchVideo` 映射为 `browser_sniff`，保留嵌套 URL、捕获组、Referer、User-Agent 与非敏感偏好 cookie。
- Kotlin regex 会转换为 RE2；无法等价编译时采用保守媒体/集号模式并输出 `regex_rewritten` warning。
- 上游通常不能静态给出 CDN 域名，因此默认 allowlist 只有站点 host，并输出 `overlay_required`。

`onlySupportsPlayers` 是 Animeko 播放器实现 ID，不具备跨客户端语义；导入器明确输出 `player_restriction_dropped`，由传输验证替代。

## Kazumi

```sh
go run ./cmd/nagare-source import kazumi \
  --input upstream/KazumiRules \
  --upstream 'https://github.com/Predidit/KazumiRules/blob/main/{path}' \
  --license MIT \
  --out imported
```

目录模式递归读取 `.json`，并忽略官方仓库用于展示元数据而不是规则正文的 `index.json`。支持对象或对象数组根节点。

XPath 模式会转换搜索 URL、GET/表单 POST、作品列表、作品名称与链接、多线路和剧集链接；`@keyword` 映射为 `{{title}}`，相对链接通过 `base_url` 规范化。API 模式支持 GET/POST、query、headers、JSON/form body、受限 JSONPath、嵌套线路、整份章节响应变量和播放页模板。`@source`、`@episodeUrl`、从零开始的线路/剧集索引和从一开始的序号都转换为 Source Spec 的显式模板变量。

Kazumi 通常把剧集地址解析到播放页，因此统一映射为 `browser_sniff`。规则只提供站点与 API host，不能证明最终 CDN 范围，导入结果会输出 `overlay_required`，由审核后的 overlay 扩充 `resolve.allowed_hosts`。Referer、User-Agent 和来源 Cookie 会话语义被保留。

安全相关行为采用保守策略：

- `deprecated` 规则导入但默认禁用。
- 启用验证码/反爬配置的规则不复制脚本，输出 `interactive_required` 并禁用。
- 固定 `Authorization`、Cookie、API key 等凭据头会删除，输出 `sensitive_header_dropped` 并禁用。
- `adBlocker` 与 `useLegacyParser` 是 Kazumi WebView 实现细节，只输出明确 warning。
- `delimited` API 章节格式目前无法由 Source Spec v1 collection 无损表示，输出 `unsupported_rule` error；当前官方规则使用的 API 章节格式为 `nested`。

合成 fixture 覆盖 XPath 与 API 两条路径；兼容性验收还会对公开 KazumiRules 工作副本执行两次离线转换并比较产物。该检查不请求来源站点，也不把上游规则正文复制进本仓库。

## Nagare schema 1

```sh
go run ./cmd/nagare-source import nagare-v1 \
  --input legacy-rules \
  --upstream 'https://github.com/owner/repo/blob/main/{path}' \
  --license MIT \
  --out imported
```

映射内容包括：

- `{{query}}` 请求模板、headers 和 timeout。
- XML/RSS 路径、JSONPath、namespace 与 `any` 回退。
- `$field` 引用。
- `trim`、`lower`、`format_bytes`、`format_kb`、`unix_rfc3339`、`parse_fansub`、regex 和 magnet transform。
- seeders、priority 与 search-only selftest。

旧 ID 中的下划线会转为连字符；发生非 ASCII 损失时添加输入摘要后缀，并输出 `id_normalized`。未知 transform 或无法表达的格式产生 `unsupported_rule` error。

## 结构化诊断

诊断是每行一个 JSON 对象：

```json
{"severity":"warning","category":"overlay_required","source":"example","path":"rules.json:mediaSources[0].arguments.searchConfig.matchVideo","message":"browser_sniff allowlist contains only the site host"}
```

常见 category：

- `unsupported_factory`、`unsupported_rule`、`unsupported_field`
- `invalid_upstream`、`invalid_source`、`invalid_generated_source`
- `id_normalized`、`tier_clamped`、`limit_clamped`
- `regex_rewritten`、`overlay_required`、`selftest_missing`
- `deprecated_source`、`interactive_required`、`sensitive_header_dropped`
- `selector_inferred`、`variable_normalized`、`legacy_type_normalized`
- `invalid_overlay`、`overlay_applied`、`duplicate_source`

## Overlay

文件名固定为 `<source_id>.yaml`：

```yaml
schema: nagare-source-overlay/v1
source_id: example
origin_digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
reason: Browser self-check observed a stable dedicated CDN.
patch:
  resolve:
    allowed_hosts:
      - video.example.invalid
      - "*.cdn.example.invalid"
  selftest:
    request:
      titles: [Example Animation]
      episode: 3
    expect:
      min_candidates: 1
      transport: hls
```

`origin_digest` 可省略，但正式社区 overlay 应始终填写。它绑定导入前的单条上游对象；摘要变化会产生错误而不是继续应用旧修补。Patch 递归合并 object，替换数组/标量，`null` 删除字段；身份和来源记录不可修改。
