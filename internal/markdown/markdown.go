// Package markdown renders block text the way the app shows it: GitHub tables,
// strikethrough, footnotes and bare URLs, plus the custom inline elements the
// mockup uses, @handle mentions, proposition references and bracketed notes.
// Raw HTML in the source is dropped, so a block is safe to write into a page.
package markdown

import (
	"bytes"
	"html/template"
	"regexp"
	"strconv"
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
		util.Prioritized(propositionParser{}, 149),
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
// out, mentions read as @handle, propositions by id and notes as their text.
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
			case *proposition:
				b.WriteString("Proposition " + strconv.FormatInt(n.id, 10))
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

// A proposition is a stable reference whose visible label is resolved by the
// reader. The stored markdown contains only the proposition id.
type proposition struct {
	ast.BaseInline
	id int64
}

var kindProposition = ast.NewNodeKind("Proposition")

func (n *proposition) Kind() ast.NodeKind { return kindProposition }

func (n *proposition) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"id": strconv.FormatInt(n.id, 10)}, nil)
}

var kindMention = ast.NewNodeKind("Mention")

func (n *mention) Kind() ast.NodeKind { return kindMention }

func (n *mention) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"handle": n.handle}, nil)
}

// A note is an aside in square brackets, either addressed to someone by their
// initials, [AL: ask about this], or a check, [check the date].
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
	mentionPattern     = regexp.MustCompile(`^@([A-Za-z0-9_][A-Za-z0-9_-]*)`)
	propositionPattern = regexp.MustCompile(`^@\[p:([1-9][0-9]*)\]`)
	notePattern        = regexp.MustCompile(`^\[(?:([A-Z]{2,4}): ([^\]\n]+)|(check[^\]\n]*))\]`)
)

type propositionParser struct{}

func (propositionParser) Trigger() []byte { return []byte{'@'} }

func (propositionParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	if r := block.PrecendingCharacter(); unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return nil
	}
	line, _ := block.PeekLine()
	m := propositionPattern.FindSubmatchIndex(line)
	if m == nil {
		return nil
	}
	id, err := strconv.ParseInt(string(line[m[2]:m[3]]), 10, 64)
	if err != nil {
		return nil
	}
	block.Advance(m[1])
	return &proposition{id: id}
}

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
	// [AL: see this](url) is a link with an odd label, not a note.
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
	reg.Register(kindProposition, r.renderProposition)
	reg.Register(kindNote, r.renderNote)
	reg.Register(ast.KindLink, r.renderLink)
	reg.Register(ast.KindAutoLink, r.renderAutoLink)
}

// Written links allow http, https and the two internal workspace paths.
// Other targets stay literal so user-authored labels cannot conceal unsafe URLs.
func (nodeRenderer) renderLink(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	link := n.(*ast.Link)
	internal := internalURL.Match(link.Destination)
	if !httpURL.Match(link.Destination) && !internal {
		if entering {
			_ = w.WriteByte('[')
		} else {
			_, _ = w.WriteString("](")
			_, _ = w.Write(util.EscapeHTML(link.Destination))
			_ = w.WriteByte(')')
		}
		return ast.WalkContinue, nil
	}
	if entering {
		_, _ = w.WriteString(`<a href="`)
		_, _ = w.Write(util.EscapeHTML(util.URLEscape(link.Destination, true)))
		if internal {
			_, _ = w.WriteString(`">`)
		} else {
			_, _ = w.WriteString(`" rel="noopener">`)
		}
	} else {
		_, _ = w.WriteString("</a>")
	}
	return ast.WalkContinue, nil
}

// Written links may point to the web or directly to a workspace proposition.
var httpURL = regexp.MustCompile(`^(?i:https?)://`)

var internalURL = regexp.MustCompile(`^(?:/show|/p/[1-9][0-9]*)$`)

// An autolink is a URL Linkify found in the running text, an address, or one
// somebody wrote in angle brackets. The last of those carries whatever scheme
// was typed, so the rule is the written link's rule: a target is followed only
// when it is http, https or an email address, and anything else is written as
// the words it was, with no href for the page to offer. rel="noopener" goes on
// the ones that are followed for the same reason it goes on a written link. The
// browser's renderer leaves every autolink as text, which is the one difference
// between the two that is on purpose.
func (nodeRenderer) renderAutoLink(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	link := n.(*ast.AutoLink)
	url := link.URL(source)
	email := link.AutoLinkType == ast.AutoLinkEmail
	if !email && !httpURL.Match(url) {
		_, _ = w.Write(util.EscapeHTML(link.Label(source)))
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString(`<a href="`)
	if email && !bytes.HasPrefix(bytes.ToLower(url), []byte("mailto:")) {
		_, _ = w.WriteString("mailto:")
	}
	_, _ = w.Write(util.EscapeHTML(util.URLEscape(url, false)))
	_, _ = w.WriteString(`" rel="noopener">`)
	_, _ = w.Write(util.EscapeHTML(link.Label(source)))
	_, _ = w.WriteString(`</a>`)
	return ast.WalkContinue, nil
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

func (nodeRenderer) renderProposition(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		id := strconv.FormatInt(n.(*proposition).id, 10)
		for parent := n.Parent(); parent != nil; parent = parent.Parent() {
			if parent.Kind() == ast.KindLink {
				_, _ = w.WriteString("Proposition " + id)
				return ast.WalkSkipChildren, nil
			}
		}
		_, _ = w.WriteString(`<a class="proposition-ref" data-proposition="`)
		_, _ = w.WriteString(id)
		_, _ = w.WriteString(`" href="/p/`)
		_, _ = w.WriteString(id)
		_, _ = w.WriteString(`">Proposition `)
		_, _ = w.WriteString(id)
		_, _ = w.WriteString(`</a>`)
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

// Mentions returns the handles named in source, once each, in the order they
// appear. It is the parser the renderer uses, so an @ inside a word or inside a
// code fence is not a mention here either, and what notifies somebody is
// exactly what the page shows as a mention.
func Mentions(source string) []string {
	src := []byte(source)
	var out []string
	seen := map[string]bool{}
	doc := md.Parser().Parse(text.NewReader(src))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if m, ok := n.(*mention); ok && entering && !seen[m.handle] {
			seen[m.handle] = true
			out = append(out, m.handle)
		}
		return ast.WalkContinue, nil
	})
	return out
}
