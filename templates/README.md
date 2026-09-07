# Source templates

复制与目标最接近的模板，再替换所有 `template-*`、`replace-me.invalid`、名称、自检标题、来源版本和许可证。模板使用保留的 `.invalid` 域名，不能直接作为生产来源发布。

- [`web.yaml`](web.yaml)：搜索、选集并直接返回 HLS/HTTP 的 WEB 来源。若最终地址只能从播放页获得，把 `resolve.mode` 改为 `browser_sniff`，并按 [Browser Resolver Runtime](../docs/browser-runtime.md) 补充 `match`、`cookie_policy` 和最小 `allowed_hosts`。
- [`bt.yaml`](bt.yaml)：从 RSS/XML 提取 magnet 的 BT 来源；JSON API 可把 `response`、`items` 和字段 extractor 改为受限 JSONPath。
- [`overlay.yaml`](overlay.yaml)：绑定上游摘要的最小人工修补。不要复制整份来源。

复制后至少提交脱敏 fixture，并执行：

```sh
go run ./cmd/nagare-source validate
go test ./...
go run ./cmd/nagare-source health --mode fixture --out /tmp/health.json --html /tmp/health.html
```

模板本身由仓库测试送入 Source Spec v1 Schema 与安全 lint，模板演进不会绕过正式来源规则。
