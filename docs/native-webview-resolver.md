# Nagare 原生播放解析方案

状态：实验实现，2026-09-10。已在 Nagare 验证一集真实 HTTP 视频起播；全来源兼容、整集播放与弹幕仍未全部验收。

## 已验证的故障

1. 请求标题“关于我转生变成史莱姆这档事 第四季”与站内标题的空格不同，之前可能通过基础标题子串选到第一季。现在优先匹配规范化标题，并拒绝季数/年份冲突。
2. Kazumi 的规则定位的是播放页。独立播放器 iframe 及实际 CDN 需要页面执行后才能得到。仅导入 XPath 规则不能继承 Kazumi 的 WebView 能力。
3. 视频地址可能没有扩展名。本次第四季第 1 集返回的 CDN 不在旧默认正则覆盖范围，实际内容为 MP4。
4. 把 iframe 地址推断成 Referer 会让该 CDN 返回 404；使用来源声明的请求头后 Range 读取返回 206。

## 已实现与实测

调用链为：Source Spec 搜索 → 校验作品/集数 → WKWebView 加载播放页及 iframe → 获取 video/source 或 HLS 提示 → Go 安全代理 Range 验证 → Candidate v1。

`native/macos/main.m` 是独立 helper。主进程通过 stdin 发送单次 JSON 请求，通过 stdout 消费结构化事件。它不读取用户浏览器数据，使用非持久化 WKWebsiteDataStore，不显示操作窗口。Go 适配器使用原有 Browser 接口，支持取消、会话并发限制、错误分类、域名筛选和请求头清理。

真实 7sefun 流程选中第四季第 1 集，返回 `transport=http`、HTTP 206、`video/mp4`；一次完整测量约 10.8 秒，加入网络限制后使用来源默认 15 秒期限的回归约 14.0 秒。只读取媒体前几个 KiB，不保存临时签名地址到本仓库。入口可读性不等于完整播放验收，也不能证明所有来源都可用。

## macOS 网络限制

本机为 macOS 14.5。WKWebsiteDataStore 的代理接口存在，HTTPS 页面实测产生 CONNECT 请求。使用普通 HTTP 的独立测试未经过代理：不存在的测试域名返回 DNS 错误，另一个 HTTP 测试超时且代理计数为零。因此当前原生通道仅接受 HTTPS 页面，并在加载前用 WKContentRuleList 阻断 HTTP、WebSocket、file 和 FTP 请求。需要这些依赖的来源暂不适用。

这个限制尚不足以宣布原生运行时达到生产验收：还需验证 HTTPS 重定向、各类子资源、WebRTC 和新窗口请求的完整边界，以及不同 macOS 版本的行为。不能仅以系统 API 存在推断全部流量已受代理约束。

## 本地验证方式

在来源仓库中执行：

```sh
make native-helper
go test ./...
go run ./cmd/nagare-source validate
NAGARE_SOURCE_LIVE_TEST=1 go test ./internal/sourceruntime -run TestNativeLiveSeasonFour -v -count=1
```

最后一项会访问真实第三方来源，普通测试不会运行它；它使用来源默认超时，只报告作品、集号、媒体类型、host 和耗时。

实验接入需要显式设置 `NAGARE_SOURCE_NATIVE_WEBVIEW=1`。helper 可以放在插件可执行文件同目录、仓库 `bin/nagare-source-wk`，或通过 `NAGARE_SOURCE_WK_HELPER` 指定。默认继续使用 Chrome。此次没有修改 Nagare 的已保存配置。

## 下一阶段

2026-09-10 补充：修复中日韩标题搜索的排版空格后，《幼女战记 第二季》第 5 集在 Nagare 全来源查询中约 14.5 秒返回 206 video/mp4。mpv 报告时长 1537.045 秒，进度持续前进超过 17 秒。浏览器来源已按 tier/ID 在 Runner 外排队，最多两路并发，排队不再消耗来源 deadline；直接在线与 BT 查询仍独立进行。该视频的弹幕指纹匹配失败，需要单独处理。

1. 完成网络边界及 Cookie 隔离的真实 WKWebView 回归后，再决定是否默认启用。
2. 扩大 Nagare 起播验收，继续验证拖动进度、完整播放及真实失败自动换源。HLS 还需验证分片和密钥请求。
3. 在已有的两路排队调度上增加真实健康反馈；排队不计入 Runner deadline，但慢来源仍可能延迟后续来源，需要继续衡量总耗时。
4. 保留 Kazumi legacy parser 标记并实现对应行为；当前实验重点是 video/source 与 HLS 提示，不宣称所有 Kazumi 规则等价。
5. Windows 研究 WebView2，Linux 研究 WebKitGTK。打包平台 helper、签名和发布自动化尚未实现。

## 参考

- [Kazumi 解析架构](https://kazumi.app/docs/architecture/video-parser/)：平台 WebView 与播放器分工。
- [Kazumi Apple 实现](https://github.com/Predidit/Kazumi/blob/fd7b2bb00acf700fd6c2bf0d7c508ee43d09aad3/lib/webview/video/impl/video_webview_apple_impl.dart)：本次核对的源代码版本。
- [WebKit WKWebsiteDataStore](https://github.com/WebKit/WebKit/blob/main/Source/WebKit/UIProcess/API/Cocoa/WKWebsiteDataStore.h)：公开代理 API 的平台版本声明。
- [Apple 代理 failover 设置](https://developer.apple.com/documentation/network/nw_proxy_config_set_failover_allowed(_:_:))：配置禁止非代理回退；实际覆盖范围仍需平台测试。
