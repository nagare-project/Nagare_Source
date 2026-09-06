# Importers

导入器只把上游格式转换成 `nagare-source/v1`，不参与运行时解析。每个导入结果仍须通过仓库校验器和 fixture 测试。

- `animeko/`：已支持 `rss` v1 与 `web-selector` v2。
- `nagare-v1/`：已支持旧磁力 YAML schema 1。
- `kazumi/`：M2 支持 XPath 与 API 规则。

无法表达的字段必须产生结构化诊断，禁止静默丢弃。

实现位于 `internal/importer/`，入口为 `nagare-source import`。详细用法见 `docs/importers.md`。
