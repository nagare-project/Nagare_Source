# Animeko importer（M1）

转换核心位于 `internal/importer`，目前支持把 Animeko subscription 中的 `factoryId: rss` v1 和 `factoryId: web-selector` v2 转为 Source Spec v1，并保留 tier、channel tier、CSS/JSONPath、集号正则、matchVideo、Referer 与 User-Agent 语义。

M1 仍需补齐 CLI 入口、真实上游 fixture 和端到端导入验收。

未知 factory 必须产生 `unsupported_factory`；已知 factory 中无法表达的字段产生 `unsupported_rule`，不得输出部分规则后静默成功。
