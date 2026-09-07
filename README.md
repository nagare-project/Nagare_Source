# Nagare Source

Nagare Source 是面向番剧资源发现与播放解析的统一来源协议和社区规则仓库。

仓库把 Animeko、Kazumi、Nagare 现有规则以及社区新增规则规范化为同一种来源描述。客户端只需要实现一次 Nagare Source 协议，即可接收 HTTP/HLS、磁力和 `.torrent` 候选，并按来源质量、实时健康度与用户偏好完成选择。

当前已完成 M0–M3 的协议基线、来源导入、确定性 BT SQLite 发布产物与隔离浏览器解析内核，并交付 M4 在本仓库负责的 Source Spec 运行时与 Plugin API 进程端。完整路线见 [docs/plan.md](docs/plan.md)。

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
go run ./cmd/nagare-source serve --listen 127.0.0.1:7788 --version dev
go run ./cmd/nagare-source crawl-bt \
  --source sources/bt/example-rss.yaml \
  --response-file fixtures/responses/example-rss.xml \
  --selftest \
  --out /tmp/nagare-bt-records.jsonl
go run ./cmd/nagare-source build \
  --version dev \
  --bt-records /tmp/nagare-bt-records.jsonl
```

`serve` 先校验仓库中的全部 Source Spec，再在显式回环 IP 上提供 Plugin API v1。可用 `GET /v1/manifest`、`GET /v1/sources`、`POST /v1/candidates`、`POST /v1/selfcheck` 和 `GET /v1/health`；`POST /v1/candidates` 会并发执行所有启用来源并逐行发送 NDJSON Candidate。离线插件链路可用 `make selftest-plugin` 验证。

`build` 生成被 Git 忽略的 `dist/index.json`、规范化 JSON 来源和 `dist/bt-index.sqlite.zst`。默认时间戳取 `SOURCE_DATE_EPOCH`，未设置时取当前 Git commit 时间，因此固定输入可以得到逐字节一致的发布产物。省略 `--bt-records` 时仍生成合法的空 BT 索引。

导入命令支持单文件或递归目录：

```sh
go run ./cmd/nagare-source import animeko \
  --input upstream/ani-subs \
  --upstream 'https://github.com/example/rules/blob/main/{path}' \
  --license AGPL-3.0-only \
  --out imported

go run ./cmd/nagare-source import nagare-v1 \
  --input upstream/legacy-rules \
  --upstream 'https://github.com/example/rules/blob/main/{path}' \
  --license MIT \
  --out imported

go run ./cmd/nagare-source import kazumi \
  --input upstream/KazumiRules \
  --upstream 'https://github.com/Predidit/KazumiRules/blob/main/{path}' \
  --license MIT \
  --out imported
```

导入器会输出 JSONL 结构化诊断；存在任何错误时不写出部分结果。人工修补通过带上游摘要锁定的 overlay 应用。

## 文档

- [Source Spec v1](docs/source-spec-v1.md)
- [Plugin API v1](docs/plugin-api-v1.md)
- [贡献来源](docs/contributing-sources.md)
- [导入器与 Overlay](docs/importers.md)
- [BT Index v1](docs/bt-index-v1.md)
- [BT Source Crawler](docs/bt-crawler.md)
- [Browser Resolver Runtime](docs/browser-runtime.md)
- [开发日志](docs/changelog.md)
- [完整实施计划](docs/plan.md)

## 当前范围

M0–M3 与 M4 插件进程端已实现并可验收。当前支持 Animeko RSS/web-selector v2、Kazumi XPath/API、Nagare schema 1、受网络边界约束的 BT 抓取与离线自检、确定性 repository index 与 `bt-index.sqlite.zst`、隔离浏览器嗅探，以及把 WEB/BT 统一为 Candidate 的并发 NDJSON 服务。Kazumi 的验证码脚本不会被导入；弃用、需要交互或携带固定凭据的规则默认禁用。Nagare 主程序中的插件生命周期、候选 UI、播放器快速选择与自动换源不在本仓库中，仍需在 Nagare 客户端仓库完成；生产来源清单、定时网络运行与逐项许可证核对继续按 M4–M5 推进。仓库中的 `example.invalid` 规则仅用于离线协议示例，不是可播放的生产来源。
