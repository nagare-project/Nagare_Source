# 贡献来源

本仓库只接受 Source Spec、导入映射、最小 overlay 和不含凭据的离线 fixture。不要提交抓取到的临时 HLS 地址、Cookie、授权头或大体积站点镜像。

## 新增规则

1. 从 [`templates/web.yaml`](../templates/web.yaml) 或 [`templates/bt.yaml`](../templates/bt.yaml) 复制最接近的模板；需要修补导入结果时使用 [`templates/overlay.yaml`](../templates/overlay.yaml)。`sources/` 下的 example 文件是离线协议 fixture，不应当作生产来源改名提交。
2. 选择稳定的 `id`，保存为 `sources/<kind>/<id>.yaml`。ID 和文件名一经发布不应更改。
3. 填写真实 `origin.upstream`、上游版本和许可证。无法确认再分发权限时不要提交规则正文。
4. 设置最小的 `allowed_hosts`、请求限制和解析正则。不要用通配符允许无关域名。
5. 在 `fixtures/` 加入脱敏后的最小请求、响应和期望 Candidate。fixture 必须可离线测试且不得过期。
6. 运行校验和测试。

```sh
go run ./cmd/nagare-source validate
go test ./...
```

也可以使用：

```sh
make check
make test
make selftest-bt
make selftest-plugin
make health
```

## 校验内容

当前工具会执行：

- 五份 JSON Schema 的语法和数据校验，包括来源健康报告。
- 来源 ID 唯一性、ID/文件名一致性和 kind/目录一致性。
- 模板语法、变量存在性和 RE2 正则编译。
- HTTP(S) URL、字面私网/回环地址、`allowed_hosts` 和敏感固定 header 检查。
- Candidate、resolve request 与 NDJSON fixture 校验。
- BT JSONL 记录与其 BT 来源引用校验。
- 构建后的仓库 index 自校验。

DNS 解析结果、重定向目标和响应体大小必须由运行时再次检查；静态校验不是网络沙箱的替代品。

## Fixture 要求

Fixture 只保留证明提取逻辑所需的节点。删除广告、统计脚本、用户标识、query 签名和无关正文。涉及上游页面内容时同时遵守其许可证与引用要求。

每条规则最终至少覆盖：

- 搜索结果能提取规范标题和稳定 key，或 BT 发布项。
- 指定集号能精确定位，不会在失败时选择第一集。
- 多线路规则能保留 channel 和 channel tier。
- resolve 结果能转换为通过 Candidate Schema 的 `hls`、`http` 或 `torrent` 联合类型。
- 失败路径产生 [`plugin-api-v1.md`](plugin-api-v1.md) 中的固定错误分类。

网络自检使用稳定、获准访问的测试标题。验证码或需要交互的人机验证必须报告 `interactive_required`，CI 不绕过。

BT 来源应把脱敏响应放入 `fixtures/responses/`，通过 [`crawl-bt`](bt-crawler.md) 执行离线 selftest。真实网络运行只用于定时健康与发布任务，PR 校验不得依赖第三方站点可用性。

## Overlay

自动导入结果应优先由 importer 修复。只有无法从上游可靠推导的事实才进入 overlay，例如线路 tier、必须的 Referer、集号正则或浏览器嗅探 allowlist。overlay 格式、摘要锁定和导入命令见 [导入器与 Overlay](importers.md)。

## 生成发布产物

`dist/` 完全由工具生成并被 Git 忽略：

```sh
SOURCE_DATE_EPOCH=1767323045 \
  go run ./cmd/nagare-source build --version 0.1.0 \
  --bt-records fixtures/bt-index/releases.jsonl
```

构建会把 YAML 规范化为 `dist/sources/<id>.json`，按 ID 排序生成 `dist/index.json`，并把规范化 BT JSONL 生成 `dist/bt-index.sqlite.zst`。每份来源和压缩 BT 索引都在 index 中带 SHA-256。相同输入、版本和时间戳必须产生逐字节一致的输出；BT 输入契约和数据库布局见 [BT Index v1](bt-index-v1.md)。

## PR 检查表

- [ ] 来源、版本、许可证和上游地址准确。
- [ ] 没有脚本、密钥、Cookie 或临时 URL。
- [ ] `allowed_hosts` 和资源限制足够小。
- [ ] 正常、无匹配和集号不确定路径都有 fixture。
- [ ] `validate`、`go test ./...` 和可复现构建通过。
- [ ] 与已有 Animeko/Kazumi 来源对照过，未创建同站点重复来源。
