package btcrawler

import (
	"bytes"
	"errors"
	"strings"

	"github.com/andybalholm/cascadia"
	nethtml "golang.org/x/net/html"
)

// HTML 响应只开放 CSS Selector：有些站点的 RSS 只给最新一页，完整结果只在搜索网页里
// （Mikan 的 RSS 搜索固定 100 条，网页搜索给到 1000 条）。entity 在解析时已经解码，
// 规则不需要再加 html_decode。

func parseHTML(data []byte) (*nethtml.Node, error) {
	return nethtml.Parse(bytes.NewReader(data))
}

// selectCSS 在 scope 节点的后代里选元素；":scope" 表示 scope 自身。
func selectCSS(scope any, expression string) ([]*nethtml.Node, error) {
	node, ok := scope.(*nethtml.Node)
	if !ok || node == nil {
		return nil, errors.New("CSS context is not an HTML node")
	}
	if strings.TrimSpace(expression) == ":scope" {
		return []*nethtml.Node{node}, nil
	}
	matcher, err := cascadia.Compile(expression)
	if err != nil {
		return nil, err
	}
	return cascadia.QueryAll(node, matcher), nil
}

// htmlValues 读 attribute 指定的属性（缺失的节点跳过），否则读规范化文本。
func htmlValues(nodes []*nethtml.Node, attribute string) []string {
	values := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if attribute == "" {
			values = append(values, htmlText(node))
			continue
		}
		for _, candidate := range node.Attr {
			if candidate.Namespace == "" && strings.EqualFold(candidate.Key, attribute) {
				values = append(values, candidate.Val)
				break
			}
		}
	}
	return values
}

// htmlText 是节点内全部文本，空白折叠成单个空格：表格单元格里的缩进和换行不进字段值。
func htmlText(node *nethtml.Node) string {
	var builder strings.Builder
	var walk func(*nethtml.Node)
	walk = func(current *nethtml.Node) {
		if current.Type == nethtml.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(builder.String()), " ")
}
