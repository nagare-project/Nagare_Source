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
