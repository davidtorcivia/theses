// Package markdown renders block text the way the app shows it: GitHub tables,
// strikethrough, footnotes and bare URLs, plus the two inline elements the
// mockup uses, @handle mentions and bracketed notes. Raw HTML in the source is
// dropped, so a block is safe to write into a page.
package markdown

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Typographer is off because the content already uses real quotes and dashes,
// and WithUnsafe is off so raw HTML never reaches the page.
var md = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Footnote, extension.Linkify),
	goldmark.WithParserOptions(parser.WithInlineParsers(
		util.Prioritized(mentionParser{}, 150),
		util.Prioritized(noteParser{}, 150),
	)),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(nodeRenderer{}, 500))),
)

// RenderBlock renders one block of markdown.
func RenderBlock(source string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(source), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(source))
	}
	return template.HTML(buf.String())
}

// RenderDocument renders a whole document. The blocks are joined and converted
// in one pass so that a footnote defined in one block can be referenced from
// another and the ids still line up.
func RenderDocument(blocks []string) template.HTML {
	return RenderBlock(strings.Join(blocks, "\n\n"))
}

// Plain strips the markup and returns the words, for the search index. Block
// text is separated by blank lines; link targets and footnote markers are left
// out, mentions read as @handle and notes as their text.
func Plain(source string) string {
	src := []byte(source)
	var b strings.Builder
	doc := md.Parser().Parse(text.NewReader(src))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch n := n.(type) {
			case *mention:
				b.WriteString("@" + n.handle)
				return ast.WalkSkipChildren, nil
			case *note:
				b.WriteString(n.body)
				return ast.WalkSkipChildren, nil
			case *ast.FencedCodeBlock:
				// A code block keeps its text in line segments rather than in
				// child nodes, so the walk would otherwise pass over it.
				writeLines(&b, src, n.Lines())
			case *ast.CodeBlock:
				writeLines(&b, src, n.Lines())
			case *ast.Text:
				b.Write(n.Segment.Value(src))
				if n.SoftLineBreak() || n.HardLineBreak() {
					b.WriteByte('\n')
				}
			case *ast.String:
				b.Write(n.Value)
			case *ast.AutoLink:
				b.Write(n.URL(src))
			}
		} else if n.Type() == ast.TypeBlock && n.NextSibling() != nil {
			b.WriteString("\n\n")
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(blankLines.ReplaceAllString(b.String(), "\n\n"))
}

var blankLines = regexp.MustCompile(`\n{3,}`)

func writeLines(b *strings.Builder, src []byte, lines *text.Segments) {
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		b.Write(line.Value(src))
	}
}

// A mention is @handle written outside a word.
type mention struct {
	ast.BaseInline
	handle string
}

var kindMention = ast.NewNodeKind("Mention")

func (n *mention) Kind() ast.NodeKind { return kindMention }

func (n *mention) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"handle": n.handle}, nil)
}

// A note is an aside in square brackets, either addressed to someone by their
// initials, [DF: ask about this], or a check, [check the date].
type note struct {
	ast.BaseInline
	by   string
	body string
}

var kindNote = ast.NewNodeKind("Note")

func (n *note) Kind() ast.NodeKind { return kindNote }

func (n *note) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"by": n.by, "body": n.body}, nil)
}

var (
	mentionPattern = regexp.MustCompile(`^@([A-Za-z0-9_][A-Za-z0-9_-]*)`)
	notePattern    = regexp.MustCompile(`^\[(?:([A-Z]{2,4}): ([^\]\n]+)|(check[^\]\n]*))\]`)
)

type mentionParser struct{}

func (mentionParser) Trigger() []byte { return []byte{'@'} }

func (mentionParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	// An @ inside a word is an address or a handle someone typed mid-word,
	// not a mention.
	if r := block.PrecendingCharacter(); unicode.IsLetter(r) || unicode.IsDigit(r) {
		return nil
	}
	line, _ := block.PeekLine()
	m := mentionPattern.FindSubmatchIndex(line)
	if m == nil {
		return nil
	}
	block.Advance(m[1])
	return &mention{handle: string(line[m[2]:m[3]])}
}

type noteParser struct{}

func (noteParser) Trigger() []byte { return []byte{'['} }

func (noteParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	m := notePattern.FindSubmatchIndex(line)
	if m == nil {
		return nil
	}
	// [DF: see this](url) is a link with an odd label, not a note.
	if len(line) > m[1] && (line[m[1]] == '(' || line[m[1]] == '[') {
		return nil
	}
	block.Advance(m[1])
	if m[6] >= 0 {
		return &note{body: string(line[m[6]:m[7]])}
	}
	by := string(line[m[2]:m[3]])
	return &note{by: by, body: by + ": " + string(line[m[4]:m[5]])}
}

type nodeRenderer struct{}

func (r nodeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindMention, r.renderMention)
	reg.Register(kindNote, r.renderNote)
}

func (nodeRenderer) renderMention(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		handle := util.EscapeHTML([]byte(n.(*mention).handle))
		_, _ = w.WriteString(`<b class="mention" data-handle="`)
		_, _ = w.Write(handle)
		_, _ = w.WriteString(`">@`)
		_, _ = w.Write(handle)
		_, _ = w.WriteString(`</b>`)
	}
	return ast.WalkSkipChildren, nil
}

// The body of a note is written as text: markdown inside a note is not
// processed, so an aside cannot smuggle markup into the page.
func (nodeRenderer) renderNote(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		note := n.(*note)
		_, _ = w.WriteString(`<mark class="note"`)
		if note.by != "" {
			_, _ = w.WriteString(` data-by="`)
			_, _ = w.Write(util.EscapeHTML([]byte(note.by)))
			_, _ = w.WriteString(`"`)
		}
		_, _ = w.WriteString(`>`)
		_, _ = w.Write(util.EscapeHTML([]byte(note.body)))
		_, _ = w.WriteString(`</mark>`)
	}
	return ast.WalkSkipChildren, nil
}
