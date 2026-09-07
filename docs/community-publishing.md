# 社区发布与来源健康

本仓库把来源规则、BT 快照和健康报告作为一组带摘要的发布产物。线上探测与可复现 release 分开运行：健康页可以反映即时网络状态，tag release 只打包 Git 中已经审阅和校验的输入。

## 健康报告

离线 fixture 模式用于 PR 与本地开发，不访问第三方站点：

```sh
make health
```

真实网络模式只访问每条 Source Spec 明确允许的主机，并继续执行 DNS 公网地址、重定向、响应体、超时和并发限制：

```sh
go run ./cmd/nagare-source health \
  --mode network \
  --concurrency 6 \
  --out /tmp/nagare-health.json \
  --html /tmp/nagare-health.html
```

JSON 遵守 `nagare-source-health/v1`，按来源 ID 稳定排序，并汇总以下状态：

- `healthy`：自检成功。
- `degraded`：来源可用，但 fixture、覆盖范围或结果不完整。
- `unavailable`：自检失败或没有可用结果。
- `interactive_required`：来源需要验证码或人工交互，自动任务不会绕过。
- `disabled`：规则明确停用。

每个来源只保留状态、耗时、稳定错误分类和经过长度限制的消息；报告不保存 Cookie、授权头或临时媒体 URL。生成器隔离单来源 panic，并通过临时文件与备份原子替换已有报告。

## GitHub Pages 健康页

`.github/workflows/health.yml` 每六小时、手动触发或 `main` 上相关文件变化时执行网络健康检查，上传 `_site/health.json` 与 `_site/index.html`，再通过 GitHub 官方 Pages actions 部署。首次使用前，仓库管理员需要在 Pages 设置中选择 **GitHub Actions** 作为构建与部署来源；仅合并 workflow 不代表站点已经启用。

健康 workflow 不修改来源规则，也不会根据一次网络失败自动提交 tier 变化。维护者应根据连续报告判断是否修复、降级或停用来源。

## 版本化 release

向仓库推送形如 `v1.2.3` 的 tag 会触发 `.github/workflows/release.yml`：

1. 执行全部 Go 测试和仓库校验。
2. 使用 tag commit 时间作为 `SOURCE_DATE_EPOCH` 构建 `dist/`。
3. 把规范化来源、BT SQLite 和审阅过的 `health.json` 打包为固定顺序、固定所有者和无 gzip 时间戳的 tarball。
4. 生成 `SHA256SUMS`，创建或更新同名 GitHub Release。

`dist/index.json` 的每个来源条目都有 SHA-256；`artifacts.btIndex` 和 `artifacts.health` 也分别记录格式、schema 版本和摘要。客户端必须先验证摘要，再读取压缩数据库或健康报告。

## 上游同步与许可证门禁

只有明确获准使用和再分发的上游规则才能进入自动同步清单。批准至少需要记录：上游仓库与具体路径、许可证表达式、可再分发范围、稳定规则 ID，以及导入结果允许提交哪些 fixture 或衍生文件。

当前仓库没有已批准的生产上游清单，因此不会加入一个能够下载并自动提交第三方规则正文的空壳定时任务。现有 importer 和 overlay 已能在批准后执行确定性转换；在此之前，兼容性测试结果不能代替许可证批准。

## 破坏性 schema 迁移

Source Spec 当前只有 v1。破坏性变更必须先提交新 schema、兼容性说明和确定的 `v1 -> vN` 字段映射，再实现可重复执行的迁移命令与 golden fixtures。没有目标版本时，不提供会猜测未来语义的伪迁移器；v1 产物继续通过 `sourceSchemaVersions: [1]` 明确声明兼容范围。
