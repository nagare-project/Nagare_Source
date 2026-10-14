// Package marketplace 生成社区来源市场的静态站点：把已启用来源按类型整合成两个来源包
// （BT 磁力、在线播放），每个来源包一份可下载文档，外加机器可读索引和带“安装”按钮的页面。
//
// 安装按钮只是打开用户本机 Nagare 的安装确认页（http://127.0.0.1:<端口>/sources/install），
// 由 Nagare 自己下载、校验 SHA-256 并在用户确认后保存；页面本身不向本机发任何请求。
package marketplace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nagare-project/Nagare_Source/internal/repository"
)

// Schema 是 index.json 的格式标识。
const Schema = "nagare-source-marketplace/v1"

// PackSchema 是来源包文档的格式标识。
const PackSchema = "nagare-source-pack/v1"

// MaxPackBytes 与 Nagare 下载安装文件的上限一致。
const MaxPackBytes = 1 << 20

// DefaultNagarePort 与 Nagare 的默认本机端口一致；页面允许用户改。
const DefaultNagarePort = 8590

// Pack 是来源包文档 packs/<kind>.json：一次下载、一次确认、一次插件重启装上一整类来源。
// Sources 的每一项都是完整的独立来源文档；安装端逐个交给插件校验，再分别保存成 <id>.json。
type Pack struct {
	Schema      string           `json:"schema"`
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Version     string           `json:"version"`
	Sources     []map[string]any `json:"sources"`
}

// Member 是索引里来源包的一个成员。
type Member struct {
	repository.SourceSummary
	Status string `json:"status,omitempty"`
}

// PackEntry 是索引里的一个来源包。Path 相对于 index.json；SHA256 覆盖 Path 处文件的原始字节。
type PackEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Path        string   `json:"path"`
	SHA256      string   `json:"sha256"`
	Bytes       int      `json:"bytes"`
	Sources     []Member `json:"sources"`
}

// Index 是 index.json 的内容。
type Index struct {
	Schema      string      `json:"schema"`
	GeneratedAt string      `json:"generatedAt"`
	Packs       []PackEntry `json:"packs"`
}

// SourceCount 是全部来源包的成员总数。
func (index Index) SourceCount() int {
	count := 0
	for _, pack := range index.Packs {
		count += len(pack.Sources)
	}
	return count
}

// Options 控制生成内容。Health 为来源 id → 健康状态，可为空。
// 健康状态只写进索引和页面，不进来源包，因此不会改变来源包的摘要。
type Options struct {
	GeneratedAt string
	Health      map[string]string
}

// Site 是生成结果；Documents 的键是相对输出目录的斜杠路径。
type Site struct {
	Index     Index
	IndexJSON []byte
	HTML      []byte
	Documents map[string][]byte
}

type packDefinition struct {
	kind, id, name, description string
}

// packDefinitions 决定市场里有哪些来源包以及展示顺序；没有成员的来源包不发布。
var packDefinitions = []packDefinition{
	{kind: "bt", id: "nagare-source-bt", name: "BT 磁力",
		description: "社区维护的 BT 磁力来源合集：按作品标题搜索各站点的种子与磁力链接。"},
	{kind: "web", id: "nagare-source-web", name: "在线播放",
		description: "社区维护的在线播放来源合集：由插件在本机浏览器里解析播放地址，需要安装 Chrome 或 Chromium。"},
}

type packMember struct {
	member   Member
	document map[string]any
}

// Build 读取并校验仓库来源，把已启用来源按类型整合成来源包。
// 已禁用的来源（占位示例、上游弃用规则等）不进入市场。
func Build(root string, options Options) (Site, error) {
	validator, err := repository.NewValidator(root)
	if err != nil {
		return Site{}, err
	}
	sources, err := repository.LoadSources(root, validator)
	if err != nil {
		return Site{}, err
	}
	byKind := map[string][]packMember{}
	seen := map[string]bool{}
	for _, source := range sources {
		summary := repository.SummarizeSource(source.Document)
		if !summary.Enabled {
			continue
		}
		if seen[summary.ID] {
			return Site{}, fmt.Errorf("duplicate source id %q", summary.ID)
		}
		seen[summary.ID] = true
		byKind[summary.Kind] = append(byKind[summary.Kind], packMember{
			member:   Member{SourceSummary: summary, Status: options.Health[summary.ID]},
			document: source.Document,
		})
	}
	site := Site{
		Index:     Index{Schema: Schema, GeneratedAt: options.GeneratedAt, Packs: []PackEntry{}},
		Documents: map[string][]byte{},
	}
	for _, definition := range packDefinitions {
		members := byKind[definition.kind]
		delete(byKind, definition.kind)
		if len(members) == 0 {
			continue
		}
		entry, data, err := buildPack(definition, members, validator)
		if err != nil {
			return Site{}, fmt.Errorf("pack %s: %w", definition.id, err)
		}
		site.Documents[entry.Path] = data
		site.Index.Packs = append(site.Index.Packs, entry)
	}
	for kind := range byKind {
		return Site{}, fmt.Errorf("no marketplace pack for source kind %q", kind)
	}
	indexJSON, err := json.MarshalIndent(site.Index, "", "  ")
	if err != nil {
		return Site{}, err
	}
	site.IndexJSON = append(indexJSON, '\n')
	var page bytes.Buffer
	if err := marketplacePage.Execute(&page, pageData{Index: site.Index, DefaultPort: DefaultNagarePort}); err != nil {
		return Site{}, err
	}
	site.HTML = page.Bytes()
	return site, nil
}

// buildPack 编码一个来源包，并按安装端的路径（取出每个成员的原始 JSON，逐个做独立来源校验）
// 再走一遍，保证发布出去的来源包整个装得上。
func buildPack(definition packDefinition, members []packMember, validator *repository.Validator) (PackEntry, []byte, error) {
	sort.Slice(members, func(i, j int) bool {
		left, right := members[i].member, members[j].member
		if left.Tier != right.Tier {
			return left.Tier > right.Tier
		}
		return left.ID < right.ID
	})
	documents := make([]map[string]any, len(members))
	summaries := make([]Member, len(members))
	for index, member := range members {
		documents[index], summaries[index] = member.document, member.member
	}
	encodedSources, err := encodeJSON(documents)
	if err != nil {
		return PackEntry{}, nil, err
	}
	// 版本只取决于成员内容：来源不变时来源包逐字节不变，已发出的安装链接不会无故失效。
	contentDigest := sha256.Sum256(encodedSources)
	pack := Pack{
		Schema: PackSchema, ID: definition.id, Name: definition.name, Description: definition.description,
		Version: hex.EncodeToString(contentDigest[:])[:12], Sources: documents,
	}
	data, err := encodeJSON(pack)
	if err != nil {
		return PackEntry{}, nil, err
	}
	if len(data) > MaxPackBytes {
		return PackEntry{}, nil, fmt.Errorf("encoded pack exceeds %d bytes", MaxPackBytes)
	}
	if err := verifyPack(data, summaries, validator); err != nil {
		return PackEntry{}, nil, err
	}
	digest := sha256.Sum256(data)
	return PackEntry{
		ID: pack.ID, Name: pack.Name, Kind: definition.kind, Description: pack.Description, Version: pack.Version,
		Path: "packs/" + definition.kind + ".json", SHA256: hex.EncodeToString(digest[:]), Bytes: len(data),
		Sources: summaries,
	}, data, nil
}

func verifyPack(data []byte, members []Member, validator *repository.Validator) error {
	var decoded struct {
		Schema  string            `json:"schema"`
		Sources []json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode pack: %w", err)
	}
	if decoded.Schema != PackSchema || len(decoded.Sources) != len(members) {
		return errors.New("encoded pack does not round-trip")
	}
	for index, raw := range decoded.Sources {
		value, err := repository.DecodeDocument(raw, "json")
		if err != nil {
			return fmt.Errorf("%s: %w", members[index].ID, err)
		}
		document, err := validator.ValidateStandaloneSource(value)
		if err != nil {
			return fmt.Errorf("%s: standalone validation: %w", members[index].ID, err)
		}
		if id := repository.SummarizeSource(document).ID; id != members[index].ID {
			return fmt.Errorf("%s: pack member decoded as %q", members[index].ID, id)
		}
	}
	return nil
}

// LoadHealth 读取健康报告（nagare-source-health/v1），返回来源 id → 状态。
func LoadHealth(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var report struct {
		Sources []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("decode health report: %w", err)
	}
	statuses := make(map[string]string, len(report.Sources))
	for _, source := range report.Sources {
		statuses[source.ID] = source.Status
	}
	return statuses, nil
}

// Write 把站点写入 dir。dir 必须不存在或为空，避免残留已下架来源包的旧文件。
func (site Site) Write(dir string) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("output directory %s is not empty", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	files := map[string][]byte{"index.json": site.IndexJSON, "index.html": site.HTML}
	for path, data := range site.Documents {
		files[path] = data
	}
	for path, data := range files {
		target := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

type pageData struct {
	Index       Index
	DefaultPort int
}

var marketplacePage = template.Must(template.New("marketplace").Funcs(template.FuncMap{
	"join":  strings.Join,
	"lower": strings.ToLower,
	"kb": func(bytes int) string {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	},
}).Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <meta name="referrer" content="no-referrer">
  <title>Nagare 来源市场</title>
  <style>
    :root { font-family: Inter, ui-sans-serif, system-ui, sans-serif; color: #172033; background: #f5f7fb; }
    body { margin: 0; }
    main { width: min(1120px, calc(100% - 32px)); margin: 48px auto; }
    header { display: flex; flex-wrap: wrap; align-items: end; justify-content: space-between; gap: 16px 24px; margin-bottom: 20px; }
    h1 { margin: 0 0 8px; font-size: clamp(28px, 5vw, 44px); letter-spacing: -0.04em; }
    p { margin: 0; color: #61708a; line-height: 1.6; }
    a { color: #315ee7; }
    .notice { margin-bottom: 20px; padding: 14px 18px; border: 1px solid #dfe5ef; border-radius: 14px; background: #fff; }
    .toolbar { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; margin-bottom: 20px; }
    .toolbar input { font: inherit; padding: 8px 12px; border: 1px solid #cfd7e4; border-radius: 10px; background: #fff; color: inherit; }
    .toolbar input[type=search] { flex: 1 1 240px; }
    .toolbar label { display: inline-flex; gap: 8px; align-items: center; color: #61708a; }
    .toolbar input[type=number] { width: 96px; }
    .packs { display: grid; gap: 18px; }
    .pack { padding: 22px; border: 1px solid #dfe5ef; border-radius: 18px; background: #fff; box-shadow: 0 8px 30px rgba(38, 55, 86, .06); }
    .pack-head { display: flex; flex-wrap: wrap; gap: 16px 24px; align-items: start; justify-content: space-between; margin-bottom: 14px; }
    .pack-head > div:first-child { flex: 1 1 360px; display: grid; gap: 8px; }
    .pack h2 { margin: 0; font-size: 24px; letter-spacing: -0.02em; }
    .meta { display: flex; flex-wrap: wrap; gap: 6px; }
    .tag { display: inline-block; padding: 3px 9px; border-radius: 999px; font-size: 12px; font-weight: 700; color: #3c4a63; background: #e9edf3; white-space: nowrap; }
    .tag.bt { color: #2747a8; background: #e3eaff; }
    .tag.web { color: #6b2fa3; background: #f1e5ff; }
    .tag.healthy { color: #096b45; background: #dff7ec; }
    .tag.degraded { color: #855500; background: #fff0c2; }
    .tag.unavailable, .tag.interactive_required { color: #a12828; background: #ffe1e1; }
    .actions { display: flex; gap: 8px; }
    .actions a, .actions button { font: inherit; font-size: 15px; padding: 10px 18px; border-radius: 10px; border: 1px solid #cfd7e4; background: transparent; color: inherit; cursor: pointer; text-decoration: none; white-space: nowrap; }
    .actions a.install { border-color: #315ee7; background: #315ee7; color: #fff; font-weight: 600; }
    .table-wrap { overflow-x: auto; }
    table { width: 100%; border-collapse: collapse; font-size: 14px; }
    th, td { padding: 9px 10px; border-top: 1px solid #e7ecf3; text-align: left; vertical-align: top; }
    th { color: #61708a; font-size: 12px; font-weight: 600; }
    tr[hidden] { display: none; }
    td small { display: block; color: #61708a; }
    .hosts { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; color: #61708a; word-break: break-all; }
    .empty { padding: 32px; text-align: center; color: #61708a; }
    footer { margin-top: 24px; font-size: 13px; }
    @media (prefers-color-scheme: dark) {
      :root { color: #edf3ff; background: #101521; }
      p, .hosts, .toolbar label, .empty, th, td small { color: #9eacc2; }
      .pack, .notice { border-color: #2c3547; background: #171e2c; box-shadow: none; }
      th, td { border-color: #2c3547; }
      .toolbar input { border-color: #2c3547; background: #171e2c; }
      .tag { color: #c5d0e3; background: #263047; }
      .actions a, .actions button { border-color: #2c3547; }
      a { color: #91adff; }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <div>
        <h1>Nagare 来源市场</h1>
        <p>{{len .Index.Packs}} 个来源包 · 共 {{.Index.SourceCount}} 个来源{{if .Index.GeneratedAt}} · 生成于 {{.Index.GeneratedAt}}{{end}}</p>
      </div>
      <p><a href="index.json">index.json</a> · <a href="../">健康报告</a></p>
    </header>
    <div class="notice">
      <p>每个来源包点一次“安装”就能装上整组来源。安装会在<strong>你本机的 Nagare</strong> 里打开确认页：Nagare 自己下载来源包、校验 SHA-256、列出包内每个来源会访问的站点，经你确认后才保存。本页不会向你的电脑发送任何请求。来源由社区维护，请只安装你有权访问的站点。</p>
    </div>
    <div class="toolbar">
      <input id="query" type="search" placeholder="在来源包里搜索名称、id 或站点" aria-label="搜索来源">
      <label>Nagare 端口 <input id="port" type="number" min="1" max="65535" value="{{.DefaultPort}}"></label>
    </div>
    <div class="packs">
      {{range .Index.Packs}}<section class="pack" data-path="{{.Path}}" data-sha256="{{.SHA256}}">
        <div class="pack-head">
          <div>
            <h2>{{.Name}}</h2>
            <div class="meta">
              <span class="tag {{.Kind}}">{{len .Sources}} 个来源</span>
              <span class="tag">{{.ID}}</span>
              <span class="tag">v{{.Version}}</span>
              <span class="tag">{{kb .Bytes}}</span>
            </div>
            <p>{{.Description}}</p>
          </div>
          <div class="actions">
            <a class="install" href="#" target="_blank" rel="noopener noreferrer">安装{{.Name}}</a>
            <button type="button" class="copy">复制地址</button>
          </div>
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>来源</th><th>访问站点</th><th>等级</th><th>状态</th></tr></thead>
            <tbody>
              {{range .Sources}}<tr data-search="{{lower .ID}} {{lower .Name}} {{join .AllowedHosts " "}} {{lower .Description}}">
                <td><strong>{{.Name}}</strong>{{if .Homepage}} · <a href="{{.Homepage}}" rel="noopener noreferrer" target="_blank">主页</a>{{end}}<small>{{.ID}} · v{{.Version}}{{if .Description}} · {{.Description}}{{end}}</small></td>
                <td class="hosts">{{join .AllowedHosts ", "}}</td>
                <td>T{{.Tier}}</td>
                <td>{{if .Status}}<span class="tag {{.Status}}">{{.Status}}</span>{{else}}—{{end}}</td>
              </tr>{{end}}
            </tbody>
          </table>
        </div>
      </section>{{end}}
    </div>
    <p class="empty" id="empty" hidden>没有匹配的来源。</p>
    <footer><p>“复制地址”得到来源包的直链，可粘贴到 Nagare 设置里的“从地址安装”。重复安装同一来源包会更新到最新版本。Nagare 未运行或端口不对时，安装页会打不开。</p></footer>
  </main>
  <script>
  (function () {
    var defaultPort = {{.DefaultPort}};
    var packs = Array.prototype.slice.call(document.querySelectorAll(".pack"));
    var rows = Array.prototype.slice.call(document.querySelectorAll(".pack tbody tr"));
    var query = document.getElementById("query");
    var port = document.getElementById("port");
    var empty = document.getElementById("empty");
    try {
      var saved = parseInt(window.localStorage.getItem("nagare-port") || "", 10);
      if (saved > 0 && saved < 65536) { port.value = saved; }
    } catch (error) {}
    function currentPort() {
      var value = parseInt(port.value, 10);
      return value > 0 && value < 65536 ? value : defaultPort;
    }
    function packURL(pack) { return new URL(pack.dataset.path, window.location.href).href; }
    function refreshLinks() {
      packs.forEach(function (pack) {
        pack.querySelector(".install").href = "http://127.0.0.1:" + currentPort() + "/sources/install?url=" +
          encodeURIComponent(packURL(pack)) + "&sha256=" + encodeURIComponent(pack.dataset.sha256);
      });
    }
    function applyFilter() {
      var text = query.value.trim().toLowerCase();
      var visible = 0;
      rows.forEach(function (row) {
        row.hidden = !!text && row.dataset.search.indexOf(text) === -1;
        if (!row.hidden) { visible++; }
      });
      empty.hidden = visible !== 0;
    }
    port.addEventListener("change", function () {
      port.value = currentPort();
      try { window.localStorage.setItem("nagare-port", String(currentPort())); } catch (error) {}
      refreshLinks();
    });
    query.addEventListener("input", applyFilter);
    packs.forEach(function (pack) {
      pack.querySelector(".copy").addEventListener("click", function (event) {
        var button = event.currentTarget;
        navigator.clipboard.writeText(packURL(pack)).then(function () {
          button.textContent = "已复制";
          setTimeout(function () { button.textContent = "复制地址"; }, 1500);
        });
      });
    });
    refreshLinks();
    applyFilter();
  })();
  </script>
</body>
</html>
`))
