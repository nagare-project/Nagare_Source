# Kazumi importer（M2）

该目录将实现 Kazumi XPath 与 API GET/POST 规则导入，并把 `@keyword`、`@source`、线路/剧集变量、Referer、User-Agent、legacy parser 和 native player 标记映射到统一模型。播放页统一映射为 `browser_sniff`。

验证码和人机验证产生 `interactive_required`，CI 不尝试绕过。
