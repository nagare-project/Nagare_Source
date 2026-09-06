# Importer fixtures

这些文件使用无网络、无版权页面内容的合成数据复现真实上游结构：

- `animeko/subscription.json` 同时覆盖 `rss` v1、`web-selector` v2、indexed search、多线路、Kotlin 命名捕获组和非 RE2 lookahead 的诊断改写。
- `nagare-v1/source.yaml` 覆盖 schema 1 的 XML namespace、`any` 回退、字段引用、transform、priority、seeders 和 selftest。
- `kazumi/xpath.json` 覆盖 HTML XPath 搜索、多线路剧集和浏览器嗅探入口。
- `kazumi/api.json` 覆盖 POST JSON、受限 JSONPath、响应变量、嵌套线路和播放页模板。

真实上游兼容性通过相同结构的公开规则进行本地验收，但上游规则正文及站点响应不会复制进本仓库。
