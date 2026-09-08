# Browser Resolver Runtime

Browser Resolver Runtime 执行 Source Spec v1 的 `resolve.mode: browser_sniff`。它只接收已经由搜索和选集阶段生成的 `play_url` 等变量，返回一个经网络响应确认可读的 HLS/HTTP 媒体入口；不把最终 URL、Cookie 或浏览器 profile 写入仓库和长期缓存。

## 执行流程

1. 严格展开 `resolve.url`，缺少变量或残留模板时立即失败。
2. 校验入口 URL 的协议、host、DNS 地址和 `allowed_hosts`。
3. 为单次解析创建独立的 Chrome profile 与本地出站代理。
4. 注入规则允许的 Referer、User-Agent 和固定偏好 Cookie。
5. 监听 Chrome DevTools Network 事件；`nested_include` 命中时在同一浏览器会话和总 deadline 内继续解析嵌套播放页。
6. 只接受 HTTP 2xx 响应且匹配 `include`、不匹配 `exclude` 的 URL；规则声明 `capture_group` 时，以对应的命名或数字分组作为媒体 URL 并再次校验边界。
7. 使用扩展名与 MIME 类型确认 HLS/HTTP transport；配置类型与实际响应冲突时继续等待其他候选。
8. 取消浏览器上下文，关闭代理连接并删除临时 profile，然后才返回媒体入口。

## 网络边界

Chrome 的全部 HTTP/HTTPS 流量被强制送入仅监听回环地址的临时代理。代理不使用环境代理，并执行以下检查：

- 仅允许 `http` 与 `https`，拒绝 URL credentials。
- host 必须匹配精确或 `*.` 子域 allowlist；通配符不匹配根域。
- DNS 结果在代理侧校验并按已验证 IP 直接拨号，避免浏览器二次解析造成 DNS rebinding。
- 拒绝回环、私网、链路本地、组播、未指定地址和 RFC 6598 shared address space。
- HTTPS 使用 CONNECT 到已固定的允许 IP；会话结束时主动关闭所有 tunnel。
- 页面跳转仍由 CDP 事件再次校验，并受 `limits.max_redirects` 限制。

浏览器禁用后台联网、扩展、同步、组件更新、应用缓存、非代理 WebRTC UDP，并把磁盘/媒体缓存限制到最小值。测试可显式允许回环地址，但生产构造函数没有公开绕过私网策略的选项。

## 会话数据与日志

`cookie_policy: source` 允许当前隔离会话使用规则偏好 Cookie 和页面产生的临时 Cookie。可用于播放器的 Referer、Origin、User-Agent、Cookie 或会话 Authorization 只存在于返回对象内；其他浏览器请求头不会透传。

运行时日志只记录去除 credentials、fragment 和 query 的页面 URL，接受媒体时只记录 host 与 transport。底层 CDP 错误日志被关闭，避免第三方库直接输出带签名的地址；对外错误使用稳定分类：`resolve_timeout`、`resolve_failed`、`browser_blocked`、`unsafe_redirect` 和 `cancelled`。

## 测试

普通 `go test ./...` 使用确定性浏览器替身覆盖嵌套页面、响应状态、正则匹配、transport、取消、超时、跳转边界、DNS 策略和日志脱敏。真实 Chrome 测试使用只监听回环地址的临时页面，不访问第三方站点：

```sh
make selftest-browser
```

测试页面产生带短期 token 的 HLS 请求，并验证 Referer、固定 Cookie、页面会话 Cookie、媒体响应、日志脱敏以及 profile 删除。未安装 Chrome/Chromium 时该项跳过；CI 仍执行不依赖浏览器安装的全部核心测试。

## 当前边界

- 验证码与交互式登录仍返回 `interactive_required`，运行时不自动绕过。
- HTTPS tunnel 无法在代理层按解密后的响应体计数；解析器在媒体响应出现后立即取消会话，并用总 deadline 限制暴露窗口。
- M3 的安全媒体解析内核现已接入 Source Spec 执行器；Candidate 组装、跨来源并发和 Plugin API NDJSON 服务由 M4 进程端提供。Nagare 客户端的播放器选择与自动换源仍在客户端仓库实现。
