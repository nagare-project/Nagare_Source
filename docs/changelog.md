# Nagare Source 开发日志

---

## [0.1.0] - 2026-09-06

### 先确定统一接口，再接具体来源

Nagare Source 的第一步不是再造一套 Animeko 或 Kazumi 规则，而是确定两者进入 Nagare 后共同遵守的接口。Animeko subscription、KazumiRules、Nagare 旧规则和社区新增规则都是输入；经过 importer 与人工 overlay 后，它们必须变成同一种 `Source Spec v1`。

这条边界解决的是长期维护问题：Nagare 客户端只实现一次来源发现和候选消费逻辑，不需要永久保留 Animeko、Kazumi、`native` 三套运行时分支。原始生态仍保留在 `origin` 中，用于追踪版本、许可证和内容摘要，但不再决定客户端怎样播放。

### WEB 和 BT 共用 Candidate

统一输出模型叫 Candidate。WEB 来源可以返回 HLS 或普通 HTTP 媒体，BT 来源可以返回 magnet、infohash 或 `.torrent`；标题匹配依据、集号、清晰度、字幕、请求头和过期时间都使用同一个结构表达。

播放策略也在协议层写清楚：WEB 与 BT 并发查询，验证通过的高优先级在线候选可以立即起播，BT 继续查询并进入换源列表或作为回退。仓库保存规则、可复现的 BT 索引和健康报告，不保存会过期的 HLS 签名地址、Cookie 或用户令牌。

### Source Spec v1 已有机器约束

完成了四份 JSON Schema：

| Schema | 约束内容 |
| --- | --- |
| `source-v1.schema.json` | 来源、搜索、选集、解析、限制和自检定义。 |
| `resolve-request-v1.schema.json` | 条目 ID、标题别名、季度和集号请求。 |
| `candidate-v1.schema.json` | HLS、HTTP 和 torrent 播放候选。 |
| `repository-index-v1.schema.json` | 发布版本、来源摘要、能力和产物路径。 |

Source Spec v1 只允许声明式请求、提取和转换，不允许来源携带任意 JavaScript、Shell 或宿主语言代码。请求必须声明 `allowed_hosts`；静态检查会拒绝字面私网/回环地址、未知模板变量、固定 `Authorization` 和 `Cookie`。浏览器嗅探只能在隔离会话里工作，最终媒体地址仍要经过域名和传输类型检查。

仓库内加入了 WEB 与 BT 示例规则，以及请求、Candidate 和 NDJSON 流 fixture。示例全部使用 `example.invalid`，用于离线验证协议，不伪装成可以播放的生产来源。

### Plugin API v1 采用 HTTP/JSON 与 NDJSON

插件接口不绑定 Go、Kotlin、Dart 或具体播放器。普通端点使用 HTTP/JSON，`POST /v1/candidates` 使用 NDJSON 流式返回三个事件：

- `candidate`：找到一个候选就立即发送，不等待其余来源。
- `source_error`：单个来源失败时返回稳定的错误分类，不让整次查询失败。
- `done`：最后发送一次查询统计，并保证是流中的最后一个事件。

Manifest 会声明插件版本、协议版本、Source Schema 版本和能力。`/v1/health` 只表示插件进程可服务，来源健康度由 `/v1/sources` 与 `/v1/selfcheck` 单独表达，避免“一个站点失效”等同于“整个插件宕机”。

### M0 已从文档变成可执行基线

新增 Go CLI：

```sh
go run ./cmd/nagare-source validate
go run ./cmd/nagare-source build --version dev
```

`validate` 会编译 Schema，校验全部来源和 fixture，并执行来源 ID、模板、请求头和公共网络边界的语义检查。`build` 把 YAML 规范化为 JSON，按来源 ID 排序，为每份产物计算 SHA-256，并生成 `dist/index.json`。

构建时间优先读取显式参数或 `SOURCE_DATE_EPOCH`，否则取当前 Git commit 时间。相同输入和时间会生成逐字节一致的产物；输出先写入临时目录再整体替换，并拒绝把仓库根目录、用户目录或它们的危险祖先当成输出目录。

GitHub Actions 目前执行三道检查：Go 测试、仓库校验、两次独立构建后的逐文件比较。本次本地执行 `go test ./...` 已通过。

### 两类导入器和统一 CLI 已经建立

导入层已经具备稳定 ID、上游地址与许可证校验、SHA-256 来源摘要、结构化诊断、确定性排序和原子写入。Overlay 支持深层合并，但禁止修改 `schema`、`id`、`kind` 和 `origin`；它还可以绑定原始内容摘要，在上游规则变化后阻止过期修补继续静默生效。

Animeko 转换核心能够读取 subscription 根对象，把 `rss` v1 转为 BT 来源、把 `web-selector` v2 转为 WEB 来源，并保留 tier、线路优先级、选择器、集号正则、Referer、User-Agent、Cookie 会话策略和浏览器嗅探条件。Nagare v1 转换器能够读取旧磁力 YAML，迁移 XML/JSON 提取路径、字段名、transform、请求限制、排名能力和 selftest。无法无损表示的 factory、版本、字段或 transform 会产生结构化诊断，不会静默输出残缺来源。

CLI 已增加 `import animeko` 和 `import nagare-v1`，统一执行转换、overlay、Source Spec 校验、JSONL 诊断和原子写入；只要出现错误诊断，就不写出任何来源。契约测试覆盖 Animeko RSS、Web Selector、未知 factory、Nagare v1 成功转换和不受支持的 transform，并把成功结果再次送入 Source Spec v1 Schema 校验。

导入器 fixture 现已覆盖真实上游结构，并完成单文件、subscription 与递归目录端到端验收。本地兼容性冒烟测试成功转换 Animeko 官方订阅仓库的 20 条规则，以及 Nagare 旧测试集的 6 条 schema 1 规则；导入结果均再次通过统一 Schema 和安全检查。真实来源网络自检及许可证逐项核对仍待完成，Kazumi 转换器属于 M2。

### BT SQLite 发布产物已经可复现

新增 `nagare-bt-record/v1` JSONL 输入契约与纯 Go 构建器。构建器会验证来源引用、infohash、magnet 一致性、发布时间、大小、字幕和清晰度，拒绝未知字段与重复 `sourceId + infoHash`，再按稳定顺序写入 SQLite。数据库包含规范化标题/集号查询索引和版本元数据，随后以固定参数压缩为 `bt-index.sqlite.zst`。

仓库 `index.json` 现在记录 BT 产物的格式、schema 版本、记录数和压缩文件 SHA-256；即使没有抓取记录也会发布可打开的空索引。测试会对相同输入独立构建两次并逐字节比较，同时解压数据库执行真实 SQL 查询。CI 的复现检查已经带入 BT fixture，避免只验证来源 JSON 而漏掉二进制产物。

### Source Spec BT 抓取与离线自检已接通

新增 `crawl-bt` 命令，直接执行统一规则的 BT Search 阶段。RSS/XML 路径支持受限 XPath、属性与 namespace；JSON 路径支持属性、数组和通配选择；两者共用 `field`、`any`、required/optional 语义与受控 transform。输出在写盘前再次通过 BT record 规范化、infohash/magnet 一致性检查、稳定去重和排序。

网络读取执行 `allowed_hosts`、重定向次数、超时、响应体大小和 DNS 公网地址检查，不使用环境代理；固定认证头、Cookie、URL credentials 和私网目标会被拒绝。错误日志只保留脱敏后的请求 URL。离线 fixture 使用同一解析链，只替换网络响应，因此 CI 可以稳定执行来源 selftest，再把其 JSONL 结果送进两次独立 SQLite 构建比较。

真实只读冒烟测试也验证了安全边界和兼容性：临时导入的 AnimeGarden 入口首先因跳转目标不在 `allowed_hosts` 而被阻止；把实际 API 主机显式加入临时 allowlist 后，同一规则成功规范化 47 条记录，并稳定折叠一条重复 infohash。测试规则、响应和结果只保存在临时目录，未在许可证尚未核对时写入仓库。

### 仓库与计划

建立了公开仓库 `nagare-project/Nagare_Source`，默认分支为 `main`，采用 MIT License。完整路线记录在 `docs/plan.md`，包含来源导入、BT 索引、resolver、浏览器嗅探、Nagare 接入、健康检查和社区发布流程。

下一阶段需要把 BT SQLite 接入定期抓取与发布并完成来源网络自检，再进入 Kazumi 导入、运行时 resolver、浏览器嗅探和 Nagare 客户端接入。当前版本完成的是统一接头和可验收的仓库/导入基线，还没有交付“点击某一集即可播放”的完整链路。
