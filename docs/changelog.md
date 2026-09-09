# Nagare Source 开发日志

---

## [Unreleased] - 2026-09-09

### 原生 WebView 解析实验与续作匹配修复

新增 macOS WKWebView helper 与 Go Browser 适配器，使用独立非持久化会话、跨 iframe 媒体探测、最多两个同时解析的会话，以及经安全代理执行的有界 Range 可读性验证。只有显式设置 `NAGARE_SOURCE_NATIVE_WEBVIEW=1` 才启用；构建入口为 `make native-helper`。macOS 14.5 上 HTTPS 已观测到 CONNECT 代理请求，普通 HTTP 页面未可靠经过该配置，因此实验通道拒绝 HTTP 入口，并在页面加载前安装 HTTP/WebSocket 等网络请求阻断规则。正式默认仍为 Chrome。

新增 `resolve.match.allow_verified_media`：经实际响应确认的 video/HLS 可以接受无扩展名或未列入通用 URL 正则的 CDN 地址，仍执行 exclude 与公网校验。Kazumi 新导入结果保留此能力，当前生产快照只更新已经实测的 7sefun。播放器 Referer 仅采用来源明确配置，避免把 iframe 地址误作为 Referer 触发 CDN 的 HTTP 404；Cookie 按目标域名、路径、有效期和 Secure 属性筛选，跨 host 重定向清除凭据。

续作匹配忽略中日韩标题中的排版空格，并识别中文/日文季期及英文 Season；明确季数或年份冲突时拒绝匹配，后续季不再通过基础标题的子串匹配回退到第一季。真实搜索已选择《关于我转生变成史莱姆这档事第四季》第 1 集，原生插件流程返回 `206 video/mp4` 的 HTTP 候选，一次测量约 10.8 秒。此结果证明搜索、选集、解析和入口可读性；Nagare 内实际起播及完整播放尚未验收。

代理修复 CONNECT 后已被 HTTP 服务缓冲的隧道数据丢失问题，增加管线化 CONNECT 回归。探索结论、限制与后续验收见 [原生解析方案](native-webview-resolver.md)。

### Kazumi 来源的跨域媒体 CDN

浏览器解析现在把来源声明的 `allowed_hosts` 用于入口与顶层页面导航，同时允许隔离 profile 访问通过公网 DNS 校验的页面依赖、嵌套播放器 iframe 和媒体 CDN；最终仍只返回匹配规则、HTTP 2xx、具有媒体路径或 MIME 类型且不指向回环、私网、链路本地或 RFC 6598 地址的响应。此前导入的 Kazumi 规则只知道播放站域名，独立播放器和 CDN 不在其中，这是跨域来源超时的原因之一；搜索失败、页面加载及请求头错误还需分别诊断。新增回归覆盖跨域公网 CDN、跨域顶层导航拒绝、私网媒体拒绝和 HTML 播放器误判。

### 浏览器临时 profile 清理确定性

Chrome 上下文与分配器退出后，解析器会确认临时 profile 已消失，并对仍在落盘的子进程写入执行有界重试；最终无法清理时返回明确错误，不再忽略 `RemoveAll` 失败。真实 Chrome 隔离测试连续运行五次通过，修复提交为 `b9766fa`。

### M5 首个获准生产上游与定时同步

新增 `nagare-upstream-approvals/v1` 清单与仓库校验。每个批准条目固定上游仓库、分支、已审核 revision、输入与专属输出目录、稳定 ID 策略、许可证路径和 SHA-256，以及规则、规范化来源和 fixture 各自的再分发范围。本地保存许可证全文，release 同步携带对应 notice；仓库绑定、路径穿越、notice 篡改或未列入清单的上游都会失败关闭。实现提交为 `9c6a70c`。

KazumiRules 已依据上游 MIT License 成为第一个批准条目。`sync-approved` 只接受 remote 匹配、工作区干净、HEAD 为已审核 revision 后代且许可证摘要未变化的 Git checkout；导入结果先在临时目录生成，和全部现有来源共同校验后才原子替换专属目录，失败时恢复旧快照。每周 workflow 只使用 `contents: read`，执行同步、fixture 健康报告、完整测试和仓库校验后上传 14 天审核产物，不推分支或创建 PR。实现提交为 `c52e7e0`。

首次快照固定到 KazumiRules `5fea5eb84768a2290ddedb156583ef20d9b43561`，确定性转换 84 条 Source Spec；12 条保持启用，72 条因上游弃用、交互验证、固定凭据或其他保守诊断保持禁用。健康报告现在覆盖仓库全部 86 条来源。生成快照提交为 `8b61abd`。Animeko `ani-subs` 根仓库截至本次核对仍未声明许可证，因此不进入批准清单，也不会被同步或再分发。

### M5 来源健康报告与静态页

新增 `nagare-source health` 和 Source Health v1 Schema。fixture 模式复用离线响应，network 模式执行受既有公网边界约束的真实自检；单来源并发受限、panic 相互隔离，结果按 ID 稳定排序并原子写入 JSON 与经过 HTML 转义的静态页。仓库校验现在要求健康报告完整覆盖全部来源、汇总计数准确，Plugin API `/v1/sources` 同时读取已发布状态。

新增每六小时运行的 GitHub Pages workflow，发布网络健康 JSON 与静态页。workflow 使用 GitHub 官方 Pages actions；仓库仍需在 Pages 设置中选择 GitHub Actions 后才会真正上线。实现提交为 `2725d2f`。

### M5 可复现 release 与健康产物

仓库 index 现在要求 `artifacts.health`，构建会把经过 Schema 与语义校验的 `reports/health.json` 复制进 `dist/health.json` 并记录生成时间和 SHA-256。相同来源、BT 输入、健康报告、版本与时间戳的两次完整构建已经逐字节一致。

新增语义化 tag release workflow。`vX.Y.Z` tag 使用对应 commit 时间构建发布目录，再以固定排序、时间、所有者和无 gzip 时间戳的方式生成 tarball 与 `SHA256SUMS`；重复运行会更新同名 release 产物。实现提交为 `70c82d5`。

### M5 贡献模板与门禁基础

新增通过 Source Schema、安全 lint 和测试持续验证的 WEB、BT 与 overlay 模板，以及 PR 模板中的许可证、fixture、网络边界和健康报告检查项。实现提交为 `9698f1b`。

本阶段先建立模板与门禁，没有用兼容性测试代替许可证批准。后续 `9c6a70c`、`c52e7e0` 与 `8b61abd` 已把首个明确采用 MIT 的 KazumiRules 上游纳入机器清单、只读定时同步和生产快照；没有许可证的上游仍然失败关闭。Source Spec 目前只有 v1，破坏性迁移器继续等待真实目标 schema 和确定字段映射，避免生成会猜测未来语义的空壳工具。

### Source Spec v1 已成为统一运行时

新增 `internal/sourceruntime`，直接执行规范化来源，不为 Animeko、Kazumi 或社区规则保留生态专用分支。WEB 流程覆盖标题别名搜索、条目匹配、单/多线路选集、集号校验、direct HLS/HTTP 轻量可读性检查和 M3 browser-sniff；BT 流程复用安全抓取器并把规范化记录转换成同一个 Candidate v1。

提取层实现 CSS、XPath、受限 JSONPath、正则、字段依赖、模板、`any`、items/zipped collection、响应级变量和 Source Spec 的无副作用 transform。HTML 字符串简写按 CSS 解释，XML/RSS 按 XPath 解释；zipped collection 按最短必填列稳定合并。运行时测试覆盖 Animeko CSS、Kazumi XPath/API、JSON WEB、BT、direct HLS 和 browser-sniff 端到端形状。

每个来源共享并发 semaphore 与速率门，来源总 deadline 覆盖排队、搜索、选集和最终解析。所有 HTTP 请求继续执行 allowed-host、DNS 公网地址、跳转、响应大小和凭据边界。请求 URL 参数按 query 编码，JSON body 变量按 JSON 字符串规则转义；模板替换只扫描规则原文一次，不会把用户输入中的 `{{...}}` 再解释为模板。

### Plugin API v1 进程端完成

新增 `nagare-source serve`，在加载时校验仓库来源，并只允许显式回环 IP。进程提供 `/v1/manifest`、`/v1/sources`、`/v1/candidates`、`/v1/selfcheck` 和 `/v1/health`，支持 `X-Nagare-Protocol-Version` 协商、`X-Request-ID` 回显、1 MiB 严格 JSON 请求边界、无缓存响应和信号关闭。关闭或客户端断开会沿请求 context 取消来源工作与浏览器会话。

Candidate 协调器并发启动所有启用来源，谁先通过验证就先写 NDJSON；单来源错误独立输出 `source_error`，不会阻塞 WEB/BT 的其他结果。输出 Candidate 在发送前再次通过 Candidate v1 Schema，检查来源归属与请求内唯一 ID；最后恰好写一条 `done`，统计成功/失败来源。离线 fixture selfcheck 与真实回环 HTTP 冒烟已经验证 `serve` 到 BT runtime 的完整链路。

这次按边界拆成两笔实现提交：`579166a` 交付 Source Spec 执行器与请求模板安全，`be6f88f` 交付 Plugin API 和 `serve`。本日志与相关说明单独提交。

### M4 Nagare 客户端端到端接入完成

`nagare-source serve` 现在用 `nagare-plugin-launch/v1` 的单行 JSON readiness 报告系统分配的回环端口，诊断保留在 stderr。Nagare 客户端只接受用户显式配置的绝对可执行文件与仓库目录，启动本地子进程、校验显式回环 URL、协商 Plugin API v1，并在禁用或退出时终止子进程。readiness 实现提交为 `ea56679`。

Nagare 已增量消费 Candidate NDJSON，在作品页持续展示在线与 BT 候选。可靠的 T0/T1 在线候选可以在其他来源仍搜索时立即起播；同步启动失败或 mpv 报告的异步播放错误会按来源 tier、线路 tier、匹配置信度、传输类型、分辨率和做种数尝试下一候选，在线耗尽后复用原有 BT 播放管线。正常播完、用户停止或手动换源不会触发自动回退，用户也可以取消后续搜索并保留手动重试入口。

短期 URL、Cookie 和请求头只在当前候选流与播放会话中传递；候选代理响应使用 `Cache-Control: no-store`，状态、界面与普通日志不显示这些敏感值。客户端基础接入和完整故障转移分别提交为 `97cf311` 与 `2d89540`。使用真实 `nagare-source` 子进程完成了启动、来源状态、增量候选、取消和随 Nagare 退出清理的回环冒烟验收，M4 验收条件已满足。

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

API 选集规则可以在 `episodes.variables` 提取整份响应共享字段，并通过受控 `template` extractor 结合 `line_index`、`line_number`、`episode_index` 与 `episode_number` 构造播放页。Schema 与静态模板检查共用同一组变量；回归测试确保已声明变量不会被 lint 误判为未知字段。

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

### Kazumi XPath/API 导入链路完成

新增 `import kazumi`，统一处理单文件与递归规则目录，并跳过官方仓库中仅用于展示索引的 `index.json`。XPath 路径覆盖 GET/表单 POST、作品搜索、相对链接、多线路和集号；API 路径覆盖 GET/POST、query、headers、JSON/form body、受限 JSONPath、响应级变量、嵌套线路，以及 `@source`、`@episodeUrl` 和线路/剧集索引组成的最终播放页模板。转换结果与 Animeko 一样进入 `browser_sniff`，没有新增 Kazumi 专用运行时分支。

安全策略不会复制验证码脚本或长期凭据：弃用规则、需要交互验证的规则和携带固定认证/API key 的规则均可审计地输出为禁用状态，相关字段产生结构化诊断。浏览器 CDN 域名无法由静态规则证明时继续要求 overlay；`delimited` API 章节格式则明确报错，不静默丢失语义。

本地只读兼容性验收对官方 KazumiRules 当前 84 条规则进行了两次离线转换，84 条均通过 Source Spec 与安全 lint，且两次来源 YAML 和诊断逐字节一致。12 条规则保持启用，其他规则因上游弃用、验证码交互或固定凭据而禁用。合成测试另外固定了 XPath/API 转换、POST JSON、变量模板、凭据删除、同站点 Animeko/Kazumi 身份收敛和 CLI 目录导入行为；没有访问第三方资源站点或提交其规则正文。

### M3 隔离浏览器解析内核完成

新增 Go 浏览器 resolver 与 chromedp 适配器。每次解析建立临时 Chrome profile 和仅监听回环地址的出站代理；浏览器 HTTP/HTTPS 请求必须经过代理的 allowlist、DNS 公网地址筛选与固定 IP 拨号，HTTPS CONNECT 同样不能绕过边界。浏览器还关闭后台联网、扩展、同步、组件更新、应用缓存和非代理 WebRTC UDP，并在解析结束后关闭 tunnel、删除 profile。

网络事件解析支持 `include`、`exclude`、命名/数字 `capture_group`、同会话 `nested_include`、跳转上限、总 deadline 和调用方取消。只有收到 HTTP 2xx 响应且扩展名/MIME 与 transport 一致的媒体才会返回；Referer、Origin、User-Agent 和当前会话 Cookie 可以临时交给播放器，但不会持久化。运行时 URL 日志删除 credentials、fragment 与 query，底层 CDP 日志也被关闭，避免短期签名出现在普通日志中。

确定性替身测试覆盖嵌套导航、HLS/HTTP 判断、失败响应、跨边界跳转、私网与 RFC 6598 地址、取消、超时和脱敏。真实 headless Chrome 冒烟测试使用本地临时页面触发带短期 token 的 HLS 请求，验证固定 Referer、偏好 Cookie、页面会话 Cookie、媒体可读响应、日志无 token，以及返回前 profile 已删除。该测试不访问第三方站点，可通过 `make selftest-browser` 运行。

### 仓库与计划

建立了公开仓库 `nagare-project/Nagare_Source`，默认分支为 `main`，采用 MIT License。完整路线记录在 `docs/plan.md`，包含来源导入、BT 索引、resolver、浏览器嗅探、Nagare 接入、健康检查和社区发布流程。

下一阶段需要把 BT SQLite 接入定期抓取与发布并完成来源网络自检，再实现 Plugin API、跨来源并发和 Nagare 客户端接入。当前版本完成的是统一接头和可验收的 M0–M3 协议、导入及浏览器解析基线，还没有交付“点击某一集即可播放”的完整链路。
