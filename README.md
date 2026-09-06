# Nagare Source

Nagare Source 是面向番剧资源发现与播放解析的统一来源协议和社区规则仓库。

仓库把 Animeko、Kazumi、Nagare 现有规则以及社区新增规则规范化为同一种来源描述。客户端只需要实现一次 Nagare Source 协议，即可接收 HTTP/HLS、磁力和 `.torrent` 候选，并按来源质量、实时健康度与用户偏好完成选择。

当前阶段先确定协议和实施边界，详见 [docs/plan.md](docs/plan.md)。

## 设计原则

- `sources/` 是唯一正式来源格式；Animeko 与 Kazumi 通过导入器进入这一格式。
- 在线源优先返回，BT 查询同时进行并随时作为回退候选。
- 仓库保存规则、BT 索引快照和健康报告，不保存会过期的 HLS 签名地址或 Cookie。
- 协议使用 HTTP/JSON 与 NDJSON，避免绑定 Go、Kotlin、Dart 或特定播放器。
- 每条规则保留来源、版本、许可证与校验信息，便于社区维护和回溯。

## 计划中的仓库结构

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

## 状态

协议设计阶段。仓库暂不发布可直接播放的来源包。

