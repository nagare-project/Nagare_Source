# Animeko importer（M1）

转换核心位于 `internal/importer`，目前支持把 Animeko subscription 中的 `factoryId: rss` v1 和 `factoryId: web-selector` v2 转为 Source Spec v1，并保留 tier、channel tier、CSS/JSONPath、集号正则、matchVideo、Referer 与 User-Agent 语义。

CLI 入口为 `nagare-source import animeko`，支持单个导出对象、subscription 的 `mediaSources` 数组和递归目录。仓库 fixture 覆盖 RSS、indexed selector、多线路、Cookie/Referer、命名捕获组及非 RE2 正则的显式降级诊断。

未知 factory 必须产生 `unsupported_factory`；已知 factory 中无法表达的字段产生 `unsupported_rule`，不得输出部分规则后静默成功。
