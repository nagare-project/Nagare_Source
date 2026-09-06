# Fixtures

- `requests/`：通过 Resolve Request Schema 的客户端请求。
- `candidates/`：WEB 与 BT 联合类型的期望结果。
- `ndjson/`：完整插件事件流；必须以唯一 `done` 事件结束。
- `importers/`：Animeko 与 Nagare schema 1 的上游结构回归样例。
- `bt-index/`：爬取器交给发布构建器的规范化 BT JSONL 记录。
- `responses/`：Source Spec 抓取器的脱敏上游响应，用于离线自检。

Fixture 必须离线、最小化、可重复并完成脱敏。Importer fixture 只复现配置结构，不复制站点页面；随 resolver 加入的站点 fixture 还需要保存获准使用的最小上游响应和对应提取期望。
