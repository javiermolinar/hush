package tui

import (
	"strings"

	ast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"
)

const menuPreviewBytes = 100
const menuMarkdownBytes = 4 << 10

// Menus need a small sample, not a full document layout. Bound the parser
// input separately from the output so markdown syntax can extend past the
// displayed text without parsing entire answers for every picker row.
func menuPreview(source string, markdownSource bool) string {
	source = limitText(source, menuMarkdownBytes)
	if markdownSource {
		source = menuMarkdownText(source)
	} else {
		source = safeText(source)
	}
	return limitText(strings.Join(strings.Fields(source), " "), menuPreviewBytes)
}

// menuMarkdownText strips decoration and link destinations, preserving block
// breaks for wrapped previews. Callers bound the source before parsing it.
func menuMarkdownText(source string) string {
	if source == "" {
		return ""
	}
	src := []byte(safeText(source))
	root := markdown.Parser().Parse(text.NewReader(src))
	var plain strings.Builder
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			if n.Type() == ast.TypeBlock {
				plain.WriteByte('\n')
			}
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Text:
			b := v.Segment.Value(src)
			if !v.IsRaw() {
				b = util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(b)))
			}
			plain.Write(b)
			if v.HardLineBreak() || v.SoftLineBreak() {
				plain.WriteByte(' ')
			}
		case *ast.String:
			plain.Write(v.Value)
		case *ast.AutoLink:
			plain.Write(v.Label(src))
		case *ast.HTMLBlock:
			appendHTMLPreview(&plain, v, src)
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			for i := 0; i < v.Lines().Len(); i++ {
				line := v.Lines().At(i)
				plain.Write(line.Value(src))
			}
		}
		// Links and images contribute their children (visible labels),
		// not their destinations. Inline HTML tags and comments have no text.
		return ast.WalkContinue, nil
	})
	// A character reference can decode to a control even if the source had none.
	return safeText(plain.String())
}

// Goldmark keeps HTML blocks as raw source, not a tree with text children.
// Tokenize the bounded source to keep text without attributes or comments.
func appendHTMLPreview(out *strings.Builder, block *ast.HTMLBlock, src []byte) {
	var raw strings.Builder
	for i := 0; i < block.Lines().Len(); i++ {
		line := block.Lines().At(i)
		raw.Write(line.Value(src))
	}
	if block.HasClosure() {
		raw.Write(block.ClosureLine.Value(src))
	}
	z := html.NewTokenizer(strings.NewReader(raw.String()))
	var hidden []string
	for {
		switch token := z.Next(); token {
		case html.ErrorToken:
			return
		case html.TextToken:
			if len(hidden) == 0 {
				out.Write(z.Text())
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			name, _ := z.TagName()
			switch tag := string(name); tag {
			case "head", "script", "style", "template", "title":
				if token == html.StartTagToken {
					hidden = append(hidden, tag)
				} else if token == html.EndTagToken && len(hidden) > 0 && hidden[len(hidden)-1] == tag {
					hidden = hidden[:len(hidden)-1]
				}
			case "address", "article", "aside", "blockquote", "br", "caption", "dd", "details", "div", "dl", "dt",
				"fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header",
				"hr", "li", "main", "nav", "ol", "p", "pre", "section", "summary", "table", "td", "th", "tr", "ul":
				if len(hidden) == 0 {
					out.WriteByte(' ')
				}
			}
		}
	}
}
