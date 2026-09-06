# Nagare Source

Nagare Source 是面向番剧资源发现与播放解析的统一来源协议和社区规则仓库。

仓库把 Animeko、Kazumi、Nagare 现有规则以及社区新增规则规范化为同一种来源描述。客户端只需要实现一次 Nagare Source 协议，即可接收 HTTP/HLS、磁力和 `.torrent` 候选，并按来源质量、实时健康度与用户偏好完成选择。

当前已完成 M0 的可执行基线：四份 JSON Schema、协议文档、WEB/BT 示例、fixture 校验、静态安全检查和可复现的仓库索引构建。完整路线见 [docs/plan.md](docs/plan.md)。

## 设计原则

- `sources/` 是唯一正式来源格式；Animeko 与 Kazumi 通过导入器进入这一格式。
- 在线源优先返回，BT 查询同时进行并随时作为回退候选。
- 仓库保存规则、BT 索引快照和健康报告，不保存会过期的 HLS 签名地址或 Cookie。
- 协议使用 HTTP/JSON 与 NDJSON，避免绑定 Go、Kotlin、Dart 或特定播放器。
- 每条规则保留来源、版本、许可证与校验信息，便于社区维护和回溯。

## 仓库结构

```text
schema/       统一规则、请求和候选结果的 JSON Schema
sources/      社区维护的规范化来源
importers/    Animeko、Kazumi 与 Nagare 旧格式导入器
overlays/     对自动导入结果的人工修补
fixtures/     离线解析样本
reports/      来源健康与性能报告
dist/         CI 生成的版本化发布产物
docs/         协议与实施文档
```

## 快速开始

需要 Go 1.23 或更新版本：

```sh
go run ./cmd/nagare-source validate
go test ./...
go run ./cmd/nagare-source build --version dev
```

`build` 生成被 Git 忽略的 `dist/index.json` 和规范化 JSON 来源。默认时间戳取 `SOURCE_DATE_EPOCH`，未设置时取当前 Git commit 时间，因此固定输入可以得到逐字节一致的发布产物。

## 文档

- [Source Spec v1](docs/source-spec-v1.md)
- [Plugin API v1](docs/plugin-api-v1.md)
- [贡献来源](docs/contributing-sources.md)
- [开发日志](docs/changelog.md)
- [完整实施计划](docs/plan.md)

## 当前范围

M0 已实现并可由 CI 验收。M1 的确定性 index 生成基础也已完成；Animeko/Nagare v1 导入器、BT SQLite 抓取、网络 resolver、浏览器嗅探和 Nagare 客户端接入仍按 M1–M4 继续实现。仓库中的 `example.invalid` 规则仅用于离线协议示例，不是可播放的生产来源。
