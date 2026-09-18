package markdown

import (
	"strings"
	"testing"
)

func TestRenderBlock(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "emphasis",
			in:   "A claim with *stress* and **weight**.",
			want: "<p>A claim with <em>stress</em> and <strong>weight</strong>.</p>\n",
		},
		{
			name: "strikethrough",
			in:   "~~withdrawn~~ now",
			want: "<p><del>withdrawn</del> now</p>\n",
		},
		{
			name: "table",
			in:   "| year | source |\n| --- | --- |\n| 1974 | Ford |",
			want: "<table>\n<thead>\n<tr>\n<th>year</th>\n<th>source</th>\n</tr>\n</thead>\n<tbody>\n<tr>\n<td>1974</td>\n<td>Ford</td>\n</tr>\n</tbody>\n</table>\n",
		},
		{
			name: "footnote",
			in:   "A claim.[^1]\n\n[^1]: The source.",
			want: "<p>A claim.<sup id=\"fnref:1\"><a href=\"#fn:1\" class=\"footnote-ref\" role=\"doc-noteref\">1</a></sup></p>\n" +
				"<div class=\"footnotes\" role=\"doc-endnotes\">\n<hr>\n<ol>\n<li id=\"fn:1\">\n" +
				"<p>The source.&#160;<a href=\"#fnref:1\" class=\"footnote-backref\" role=\"doc-backlink\">&#x21a9;&#xfe0e;</a></p>\n</li>\n</ol>\n</div>\n",
		},
		{
			name: "autolinks",
			in:   "See https://example.com/x and www.example.com",
			want: "<p>See <a href=\"https://example.com/x\">https://example.com/x</a> and <a href=\"http://www.example.com\">www.example.com</a></p>\n",
		},
		{
			name: "mention",
			in:   "Ask @ada about it.",
			want: "<p>Ask <b class=\"mention\" data-handle=\"ada\">@ada</b> about it.</p>\n",
		},
		{
			name: "mention after punctuation",
			in:   "(@ada-two)",
			want: "<p>(<b class=\"mention\" data-handle=\"ada-two\">@ada-two</b>)</p>\n",
		},
		{
			name: "address is not a mention",
			in:   "Write to ada@example.com.",
			want: "<p>Write to <a href=\"mailto:ada@example.com\">ada@example.com</a>.</p>\n",
		},
		{
			name: "note addressed to someone",
			in:   "The date is wrong. [AL: check the transcript]",
			want: "<p>The date is wrong. <mark class=\"note\" data-by=\"AL\">AL: check the transcript</mark></p>\n",
		},
		{
			name: "check note",
			in:   "[check the date] before the edit",
			want: "<p><mark class=\"note\">check the date</mark> before the edit</p>\n",
		},
		{
			name: "a link that reads like a note stays a link",
			in:   "[AL: the paper](https://example.com)",
			want: "<p><a href=\"https://example.com\">AL: the paper</a></p>\n",
		},
		{
			name: "heading and list",
			in:   "# Findings\n\n- one\n- two",
			want: "<h1>Findings</h1>\n<ul>\n<li>one</li>\n<li>two</li>\n</ul>\n",
		},
		{
			name: "typographer is off",
			in:   `He said "quotes" -- and left...`,
			want: "<p>He said &quot;quotes&quot; -- and left...</p>\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(RenderBlock(tt.in)); got != tt.want {
				t.Errorf("RenderBlock(%q) =\n%q\nwant\n%q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRenderBlockEscapesHostileInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "script block",
			in:   "<script>alert(1)</script>",
			want: "<!-- raw HTML omitted -->\n",
		},
		{
			name: "inline html",
			in:   "before <img src=x onerror=alert(1)> after",
			want: "<p>before <!-- raw HTML omitted --> after</p>\n",
		},
		{
			name: "javascript link",
			in:   "[click](javascript:alert(1))",
			want: "<p><a href=\"\">click</a></p>\n",
		},
		{
			name: "data image",
			in:   "![x](data:text/html;base64,PHNjcmlwdD4=)",
			want: "<p><img src=\"\" alt=\"x\"></p>\n",
		},
		{
			name: "quote after a mention",
			in:   `@ada" onclick="steal()`,
			want: "<p><b class=\"mention\" data-handle=\"ada\">@ada</b>&quot; onclick=&quot;steal()</p>\n",
		},
		{
			name: "markup inside a note",
			in:   `[AL: <script>x</script> & "q"]`,
			want: "<p><mark class=\"note\" data-by=\"AL\">AL: &lt;script&gt;x&lt;/script&gt; &amp; &quot;q&quot;</mark></p>\n",
		},
		{
			name: "angle brackets in text",
			in:   "5 < 6 & 7 > 2",
			want: "<p>5 &lt; 6 &amp; 7 &gt; 2</p>\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(RenderBlock(tt.in))
			if got != tt.want {
				t.Errorf("RenderBlock(%q) =\n%q\nwant\n%q", tt.in, got, tt.want)
			}
			if strings.Contains(got, "<script") || strings.Contains(got, "onerror") || strings.Contains(got, "javascript:") {
				t.Errorf("RenderBlock(%q) let markup through: %q", tt.in, got)
			}
		})
	}
}

func TestRenderDocument(t *testing.T) {
	got := string(RenderDocument([]string{"First block.", "Second block.[^a]", "[^a]: The source."}))
	want := "<p>First block.</p>\n<p>Second block.<sup id=\"fnref:1\"><a href=\"#fn:1\" class=\"footnote-ref\" role=\"doc-noteref\">1</a></sup></p>\n" +
		"<div class=\"footnotes\" role=\"doc-endnotes\">\n<hr>\n<ol>\n<li id=\"fn:1\">\n" +
		"<p>The source.&#160;<a href=\"#fnref:1\" class=\"footnote-backref\" role=\"doc-backlink\">&#x21a9;&#xfe0e;</a></p>\n</li>\n</ol>\n</div>\n"
	if got != want {
		t.Errorf("RenderDocument =\n%q\nwant\n%q", got, want)
	}
}

func TestPlain(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"emphasis", "A *stressed* word and **weight**.", "A stressed word and weight."},
		{"heading and list", "# Findings\n\n- one\n- two", "Findings\n\none\n\ntwo"},
		{"link text without the target", "See [the paper](https://example.com/x).", "See the paper."},
		{"autolink keeps the url", "See https://example.com/x", "See https://example.com/x"},
		{"mention", "Ask @ada about it.", "Ask @ada about it."},
		{"notes", "[AL: check this] and [check the date]", "AL: check this and check the date"},
		{"footnote marker dropped", "A claim.[^1]\n\n[^1]: The source.", "A claim.\n\nThe source."},
		{"code span", "Run `go test ./...` first.", "Run go test ./... first."},
		{"table cells", "| a | b |\n| --- | --- |\n| 1 | 2 |", "a\n\nb\n\n1\n\n2"},
		{"raw html dropped", "<script>alert(1)</script>", ""},
		{"fenced code block", "Prose.\n\n```go\nfmt.Println(1)\n```", "Prose.\n\nfmt.Println(1)"},
		{"indented code block", "Prose.\n\n    go test ./...\n    go vet ./...", "Prose.\n\ngo test ./...\ngo vet ./..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Plain(tt.in); got != tt.want {
				t.Errorf("Plain(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMentions(t *testing.T) {
	tests := []struct {
		source string
		want   []string
	}{
		{"hello @ada", []string{"ada"}},
		{"@ada and @grace and @ada again", []string{"ada", "grace"}},
		{"ada@example.com is an address", nil},
		{"nothing here", nil},
		{"```\n@ada\n```", nil},
		{"`@ada`", nil},
		{"**@ada**", []string{"ada"}},
		{"(@ada)", []string{"ada"}},
	}
	for _, tt := range tests {
		got := Mentions(tt.source)
		if len(got) != len(tt.want) {
			t.Errorf("Mentions(%q) = %v, want %v", tt.source, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("Mentions(%q) = %v, want %v", tt.source, got, tt.want)
				break
			}
		}
	}
}
