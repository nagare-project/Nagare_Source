# BT Index v1

`bt-index.sqlite.zst` 是仓库发布给客户端的可复现 BT 快照。[BT Source Crawler](bt-crawler.md) 负责把允许缓存的 RSS/API 项目转换为 JSONL；仓库构建器负责严格校验、规范化、去重、生成 SQLite，再以 Zstandard 压缩。抓取动作和发布动作因此可以独立重试，错误输入不会覆盖已有发布目录。

## 构建输入

每个非空行是一个 `nagare-bt-record/v1` 对象：

```json
{"schema":"nagare-bt-record/v1","sourceId":"example-rss","infoHash":"0123456789abcdef0123456789abcdef01234567","magnet":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567","title":"[Example Fansub] Example Show - 01 [1080P]","episode":1,"publishedAt":"2026-01-02T03:04:05Z","sizeBytes":1073741824,"fansub":"Example Fansub","seeders":12,"resolution":"1080P","subtitleLanguages":["zh-Hans","zh-Hant"]}
```

字段约束：

- `schema`、`sourceId`、`infoHash`、`title` 必填；`sourceId` 必须指向本次仓库中的 BT 来源。
- `infoHash` 接受 40 位十六进制或 32 位 Base32，入库时统一为小写十六进制。
- `magnet` 与 `torrentUrl` 至少提供一个；magnet 的 `xt=urn:btih` 必须和 `infoHash` 一致。
- `episode` 必须大于 0；`sizeBytes` 和 `seeders` 不得为负数；`publishedAt` 使用 RFC 3339。
- `resolution` 与 Candidate v1 使用同一组枚举；字幕语言使用 BCP 47 风格标签且不可重复。
- 未声明字段、同一 `sourceId + infoHash` 的重复记录和超过 1 MiB 的单行都会让整个构建失败。

示例输入位于 [`fixtures/bt-index/releases.jsonl`](../fixtures/bt-index/releases.jsonl)。

## SQLite 布局

解压后的数据库把格式版本写入 `PRAGMA user_version = 1`，并包含两张表：

```sql
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE releases (
  source_id TEXT NOT NULL,
  info_hash TEXT NOT NULL,
  magnet TEXT,
  torrent_url TEXT,
  title TEXT NOT NULL,
  normalized_title TEXT NOT NULL,
  episode REAL,
  published_at INTEGER,
  size_bytes INTEGER,
  fansub TEXT,
  seeders INTEGER,
  resolution TEXT,
  subtitle_languages TEXT NOT NULL,
  PRIMARY KEY (source_id, info_hash)
) WITHOUT ROWID;
```

`published_at` 是 Unix 毫秒；`subtitle_languages` 是已排序的 JSON 数组。`normalized_title` 对标题做 Unicode 小写化，并把连续标点和空白折叠为一个空格。客户端可以用 `normalized_title + episode` 索引先查询本地快照，再向实时来源补查：

```sql
SELECT *
FROM releases
WHERE normalized_title = ? AND episode = ?
ORDER BY seeders DESC, published_at DESC;
```

## 发布与完整性

```sh
SOURCE_DATE_EPOCH=1767323045 \
  go run ./cmd/nagare-source build \
  --version 0.1.0 \
  --bt-records fixtures/bt-index/releases.jsonl
```

记录会按 `source_id + info_hash` 排序，数据库使用固定页大小和 schema，Zstandard 编码器固定级别与单线程。相同来源、BT 输入、版本和构建时间会产生逐字节一致的发布目录。

`index.json` 的 `artifacts.btIndex` 给出路径、格式、数据库 schema 版本、记录数和压缩文件的 SHA-256。客户端必须在解压或打开数据库前校验摘要，并拒绝未知 schema 版本。
