# Fixtures

- `requests/`：通过 Resolve Request Schema 的客户端请求。
- `candidates/`：WEB 与 BT 联合类型的期望结果。
- `ndjson/`：完整插件事件流；必须以唯一 `done` 事件结束。

Fixture 必须离线、最小化、可重复并完成脱敏。当前 M0 校验协议边界；随 M1/M2 导入器加入的站点 fixture 还需要保存最小上游响应和对应的提取期望。
