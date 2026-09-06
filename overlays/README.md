# Overlays

这里保存无法从上游规则可靠推导的最小修补。overlay 必须引用来源 ID，并建议绑定 `origin_digest`；不要复制整份来源，也不要在这里保存认证 Cookie、令牌或临时媒体地址。

```yaml
schema: nagare-source-overlay/v1
source_id: example-http
origin_digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
reason: CDN host is only visible during an isolated browser self-check.
patch:
  resolve:
    allowed_hosts:
      - media.example.invalid
      - "*.cdn.example.invalid"
```

Patch 对 object 做递归合并，对数组和标量做整体替换，`null` 删除字段。overlay 禁止修改 `schema`、`id`、`kind` 和 `origin`。摘要不匹配时导入失败，强制维护者在上游变更后重新审核。
