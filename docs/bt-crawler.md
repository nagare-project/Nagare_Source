# BT Source Crawler

`crawl-bt` 执行 Source Spec v1 的 BT Search 阶段，把 RSS/XML 或 JSON 响应转换成 `nagare-bt-record/v1` JSONL。它与索引构建器分离：抓取失败不会触碰已有发布目录，只有通过记录校验的输出才能交给 `build --bt-records`。

## 离线自检

每条可发布 BT 来源应提交最小、脱敏且获得许可的响应 fixture，并用来源里的 `selftest` 做离线回归：

```sh
go run ./cmd/nagare-source crawl-bt \
  --source sources/bt/example-rss.yaml \
  --response-file fixtures/responses/example-rss.xml \
  --selftest \
  --out /tmp/bt-records.jsonl
```

`--selftest` 使用 `selftest.request` 的首个标题或 query，并检查 `min_candidates` 与 torrent 传输类型。fixture 仍走正常的模板渲染、响应大小限制、解析、transform、匹配和记录校验，只替换网络读取步骤。自检失败时不会改写 `--out`。

## 实时抓取

单来源查询：

```sh
go run ./cmd/nagare-source crawl-bt \
  --source sources/bt/example-rss.yaml \
  --title 'Example Animation' \
  --episode 3 \
  --out /tmp/bt-records.jsonl
```

`--source` 也可以指向目录。目录内的 YAML 会按路径稳定排序，分别执行各自的 selftest 或同一个显式查询，最终 JSONL 再按 `sourceId + infoHash` 排序。离线 `--response-file` 只接受单个来源文件，避免把一份响应错误套用到多个来源。

当前 BT 运行时覆盖：

- RSS/XML 的受限 XPath：绝对/相对元素路径、首段 `//`、`text()`、属性和显式 namespace。
- JSON 的受限 JSONPath：属性、数组下标、`[*]` 以及顶层数组 `$`。
- `field` 依赖、`any` 回退、required/optional 字段和 Source Spec v1 的无副作用 transform。
- 标题/集号强制匹配、默认分辨率和字幕、infohash/magnet 一致性、稳定去重与结构化诊断。

单个坏发布项会产生 `item_extraction_failed` 或 `item_rejected` warning 并被跳过；响应本身无法解析、Source Spec 非法或 selftest 不达标会让整次命令失败。目前只有 HTTP `.torrent`、且响应或 URL 中完全没有 infohash 的项目会被拒绝；后续 torrent 元信息读取器应从 bencoded `info` 字典计算哈希后再纳入索引。

## 网络边界

实时抓取器不读取环境代理，避免代理绕过目标检查。每次请求和重定向都必须满足 `allowed_hosts`，每次新连接都会重新解析 DNS；任何回环、私网、链路本地或未指定地址都会终止请求。请求同时执行：

- Source Spec 的总超时、响应头超时和 TLS 握手超时。
- 请求级或来源级 `max_response_bytes` 与 `max_redirects`。
- 固定 `Authorization`、`Cookie` 和 URL credentials 拒绝。
- HTTP 非 2xx 拒绝，以及压缩响应解码后的大小限制。
- 错误信息中的 URL query 整体脱敏。

发布前仍需由调度环境执行频率与并发控制；单次 `crawl-bt` 只发起每个来源的一次 Search 请求。
