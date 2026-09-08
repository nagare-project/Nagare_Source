# Nagare schema 1 importer（M1）

转换核心位于 `internal/importer`，目前可以把旧磁力 YAML 转为 `kind: bt`，保留请求模板、XML/JSON 字段、transforms、seeders、priority 和 selftest。CLI 入口为 `nagare-source import nagare-v1`，生成结果仍需通过 Source Spec Schema 和静态安全检查。

仓库 fixture 和回归测试覆盖 XML namespace、`any` 回退、字段引用、时间/体积/magnet transform、seeders、priority 和 selftest。单文件与递归目录使用同一入口。
