package parse

import (
	"bytes"
	"fmt"
	"strings"

	"golang.org/x/net/html"

	"github.com/Aravindh97/granth/internal/doc"
)

// parseHTML keeps headings, paragraphs, list items and tables and drops
// navigation, scripts and styles.
func parseHTML(data []byte, fallbackTitle string) ([]doc.Page, string, error) {
	if err := requireUTF8(data); err != nil {
		return nil, "", err
	}
	root, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	title := fallbackTitle
	var blocks []doc.Block
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "footer", "noscript", "template":
				return
			case "title":
				if t := textOf(n); t != "" {
					title = t
				}
				return
			case "h1", "h2", "h3", "h4", "h5", "h6":
				if t := textOf(n); t != "" {
					blocks = append(blocks, doc.Block{Kind: doc.Heading, Level: int(n.Data[1] - '0'), Text: t})
				}
				return
			case "p", "li", "pre", "blockquote", "dd", "dt":
				if t := textOf(n); t != "" {
					blocks = append(blocks, doc.Block{Kind: doc.Paragraph, Text: t})
				}
				return
			case "table":
				if rows := tableRows(n); len(rows) > 0 {
					blocks = append(blocks, tableBlock(rows))
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return []doc.Page{{Number: 1, Blocks: blocks}}, title, nil
}

func textOf(n *html.Node) string {
	var sb strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

func tableRows(t *html.Node) [][]string {
	var rows [][]string
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			var row []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
					row = append(row, textOf(c))
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(t)
	return rows
}
