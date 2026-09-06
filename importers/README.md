# Importers

导入器只把上游格式转换成 `nagare-source/v1`，不参与运行时解析。每个导入结果仍须通过仓库校验器和 fixture 测试。

- `animeko/`：M1 支持 `rss` 与 `web-selector` v2。
- `nagare-v1/`：M1 支持旧磁力 YAML。
- `kazumi/`：M2 支持 XPath 与 API 规则。

无法表达的字段必须产生结构化诊断，禁止静默丢弃。
