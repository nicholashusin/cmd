package ui

import (
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderMarkdown(t *testing.T) {
	// 1. Basic formatting: bold, italic, strikethrough, inline code
	input := "Hello **world** and *italic* and ~~strike~~ with `fmt.Println(\"hi\")`"
	out := string(RenderMarkdown(input))
	if !strings.Contains(out, "<strong>world</strong>") {
		t.Errorf("expected <strong>world</strong>, got: %s", out)
	}
	if !strings.Contains(out, "<em>italic</em>") {
		t.Errorf("expected <em>italic</em>, got: %s", out)
	}
	if !strings.Contains(out, "<del>strike</del>") {
		t.Errorf("expected <del>strike</del>, got: %s", out)
	}
	if !strings.Contains(out, "<code class=\"md-inline-code\">fmt.Println(&#34;hi&#34;)</code>") {
		t.Errorf("expected escaped inline code, got: %s", out)
	}

	// 2. Fenced Go code block with syntax highlighting
	codeBlock := "```go\nfunc Sum(a, b int) int {\n\treturn a + b\n}\n```"
	codeOut := string(RenderMarkdown(codeBlock))
	if !strings.Contains(codeOut, "<pre class=\"md-code-block\"><code class=\"lang-go\">") {
		t.Errorf("expected pre and code with lang-go, got: %s", codeOut)
	}
	if !strings.Contains(codeOut, "<span class=\"tok-kw\">func</span>") {
		t.Errorf("expected highlighted 'func' keyword, got: %s", codeOut)
	}

	// 3. Headings, lists, blockquotes
	structDoc := "# Title 1\n## Subtitle\n\n> This is a quote\n\n- item 1\n- item 2\n\n1. first\n2. second"
	structOut := string(RenderMarkdown(structDoc))
	if !strings.Contains(structOut, "<h1 class=\"md-h1\">Title 1</h1>") {
		t.Errorf("expected h1, got: %s", structOut)
	}
	if !strings.Contains(structOut, "<h2 class=\"md-h2\">Subtitle</h2>") {
		t.Errorf("expected h2, got: %s", structOut)
	}
	if !strings.Contains(structOut, "<blockquote class=\"md-quote\">") {
		t.Errorf("expected blockquote, got: %s", structOut)
	}
	if !strings.Contains(structOut, "<ul class=\"md-ul\">\n<li>item 1</li>\n<li>item 2</li>\n</ul>") {
		t.Errorf("expected ul list, got: %s", structOut)
	}
	if !strings.Contains(structOut, "<ol class=\"md-ol\">\n<li>first</li>\n<li>second</li>\n</ol>") {
		t.Errorf("expected ol list, got: %s", structOut)
	}

	// 4. Links and bare URLs
	linkDoc := "See [Example](https://example.com) or bare https://example.org"
	linkOut := string(RenderMarkdown(linkDoc))
	if !strings.Contains(linkOut, "<a href=\"https://example.com\" target=\"_blank\" rel=\"noopener noreferrer\">Example</a>") {
		t.Errorf("expected markdown link, got: %s", linkOut)
	}
	if !strings.Contains(linkOut, "<a href=\"https://example.org\" target=\"_blank\" rel=\"noopener noreferrer\">https://example.org</a>") {
		t.Errorf("expected auto URL, got: %s", linkOut)
	}

	// 5. XSS Prevention: javascript: links and <script> tags
	xssDoc := "[evil](javascript:alert(1)) and <script>evil()</script>"
	xssOut := string(RenderMarkdown(xssDoc))
	if strings.Contains(xssOut, "<script>") || strings.Contains(xssOut, "javascript:alert(1)") && strings.Contains(xssOut, "href=") {
		t.Errorf("XSS vulnerability detected in output: %s", xssOut)
	}
	if !strings.Contains(xssOut, "&lt;script&gt;evil()&lt;/script&gt;") {
		t.Errorf("expected escaped script tag, got: %s", xssOut)
	}

	// 6. Paragraph with line breaks
	paraDoc := "Line 1\nLine 2"
	paraOut := string(RenderMarkdown(paraDoc))
	if !strings.Contains(paraOut, "<p class=\"md-p\">Line 1<br>\nLine 2</p>") {
		t.Errorf("expected paragraph with <br>, got: %s", paraOut)
	}

	// 7. Single-escaping of & in URL query parameters
	queryDoc := "[search](https://example.com/search?q=foo&lang=go&sort=desc)"
	queryOut := string(RenderMarkdown(queryDoc))
	if strings.Contains(queryOut, "&amp;amp;") {
		t.Errorf("double escaping detected in markdown link href: %s", queryOut)
	}
	if !strings.Contains(queryOut, `href="https://example.com/search?q=foo&amp;lang=go&amp;sort=desc"`) {
		t.Errorf("expected single-escaped query params in href, got: %s", queryOut)
	}

	// 8. Snake case preservation vs explicit underscore emphasis
	snakeDoc := "use foo_bar_baz or my_func_name, but _do_ emphasize (_this_) and _that_!"
	snakeOut := string(RenderMarkdown(snakeDoc))
	if strings.Contains(snakeOut, "foo<em>bar</em>baz") || strings.Contains(snakeOut, "my<em>func</em>name") {
		t.Errorf("snake_case was incorrectly italicized: %s", snakeOut)
	}
	if !strings.Contains(snakeOut, "<em>do</em>") || !strings.Contains(snakeOut, "(<em>this</em>)") || !strings.Contains(snakeOut, "<em>that</em>") {
		t.Errorf("expected proper italicization of _do_, (_this_), and _that_, got: %s", snakeOut)
	}

	// 9. Trailing punctuation stripping on bare auto-URLs
	punctDoc := "check (https://example.org/path). and https://example.org/docs/Spec_(v2)!"
	punctOut := string(RenderMarkdown(punctDoc))
	if !strings.Contains(punctOut, `(<a href="https://example.org/path" target="_blank" rel="noopener noreferrer">https://example.org/path</a>).`) {
		t.Errorf("expected trailing ').' stripped from bare URL, got: %s", punctOut)
	}
	if !strings.Contains(punctOut, `<a href="https://example.org/docs/Spec_(v2)" target="_blank" rel="noopener noreferrer">https://example.org/docs/Spec_(v2)</a>!`) {
		t.Errorf("expected URL with balanced parens kept and trailing '!' stripped, got: %s", punctOut)
	}

	// 10. Protocol-relative URL rejection
	protoRelDoc := "[evil](//evil.com/hack)"
	protoRelOut := string(RenderMarkdown(protoRelDoc))
	if strings.Contains(protoRelOut, "href=") {
		t.Errorf("protocol-relative URL should not be linked: %s", protoRelOut)
	}

	// 11. Fenced code block without lang, indented up to 3 spaces, trailing whitespace not getting class="ws"
	indentedFence := "   ```\nline with trailing spaces   \n   ```"
	indentedOut := string(RenderMarkdown(indentedFence))
	if !strings.Contains(indentedOut, "<pre class=\"md-code-block\"><code>line with trailing spaces   </code></pre>") {
		t.Errorf("expected clean pre/code without class ws for indented fence, got: %s", indentedOut)
	}
	if strings.Contains(indentedOut, "class=\"ws\"") {
		t.Errorf("trailing whitespace in code block should not have class ws: %s", indentedOut)
	}

	// 12. Single-line ```triple``` and ``double ` tick``
	singleTriple := "test with ```inlineTriple()``` code"
	tripleOut := string(RenderMarkdown(singleTriple))
	if !strings.Contains(tripleOut, "<code class=\"md-inline-code\">inlineTriple()</code>") {
		t.Errorf("expected inline code for single line triple backticks, got: %s", tripleOut)
	}
	if strings.Contains(tripleOut, "`") {
		t.Errorf("no leftover backticks expected in tripleOut: %s", tripleOut)
	}

	doubleTick := "test with ``double ` tick`` code"
	doubleOut := string(RenderMarkdown(doubleTick))
	if !strings.Contains(doubleOut, "<code class=\"md-inline-code\">double ` tick</code>") {
		t.Errorf("expected inline code with embedded backtick, got: %s", doubleOut)
	}

	// 13. NUL-byte input stripped
	nulDoc := "foo\x00bar `code\x00span`"
	nulOut := string(RenderMarkdown(nulDoc))
	if strings.Contains(nulOut, "\x00") {
		t.Errorf("NUL byte should be stripped from output: %s", nulOut)
	}
	if !strings.Contains(nulOut, "foobar") || !strings.Contains(nulOut, "<code class=\"md-inline-code\">codespan</code>") {
		t.Errorf("expected foobar and codespan, got: %s", nulOut)
	}

	// 14. Thematic break / horizontal rule
	thematicTests := []struct {
		input  string
		wantHR bool
	}{
		{"---", true},
		{"***", true},
		{"___", true},
		{"- - -", true},
		{"* * *", true},
		{"   ---", true},
		{"--", false},
		{"--- not hr", false},
	}
	for _, tc := range thematicTests {
		res := string(RenderMarkdown(tc.input))
		hasHR := strings.Contains(res, `<hr class="md-hr">`)
		if hasHR != tc.wantHR {
			t.Errorf("RenderMarkdown(%q) hr=%v, want %v (got: %s)", tc.input, hasHR, tc.wantHR, res)
		}
	}

	// Paragraph 1\n---\nParagraph 2 without blank lines
	paraBreak := "Paragraph 1\n---\nParagraph 2"
	paraBreakOut := string(RenderMarkdown(paraBreak))
	if !strings.Contains(paraBreakOut, "<p class=\"md-p\">Paragraph 1</p>\n<hr class=\"md-hr\">\n<p class=\"md-p\">Paragraph 2</p>") {
		t.Errorf("expected paragraph break with hr, got: %s", paraBreakOut)
	}

	// Fenced code containing --- does not become <hr>
	fencedHR := "```\n---\n```"
	fencedHROut := string(RenderMarkdown(fencedHR))
	if strings.Contains(fencedHROut, "<hr") {
		t.Errorf("fenced code block containing --- should not become hr, got: %s", fencedHROut)
	}

	// 15. Heading levels H1 through H6 and non-heading 7 hashes
	headingDoc := "# H1\n## H2\n### H3\n#### H4\n##### H5\n###### H6\n####### Seven\n# Section #2"
	headingOut := string(RenderMarkdown(headingDoc))
	for level := 1; level <= 6; level++ {
		tag := fmt.Sprintf("<h%d class=\"md-h%d\">H%d</h%d>", level, level, level, level)
		if !strings.Contains(headingOut, tag) {
			t.Errorf("expected heading %s, got: %s", tag, headingOut)
		}
	}
	if strings.Contains(headingOut, "<h7") {
		t.Errorf("####### should not produce <h7>, got: %s", headingOut)
	}
	if !strings.Contains(headingOut, "<p class=\"md-p\">####### Seven</p>") {
		t.Errorf("expected ####### Seven to render as paragraph, got: %s", headingOut)
	}
	if !strings.Contains(headingOut, "<h1 class=\"md-h1\">Section #2</h1>") {
		t.Errorf("expected # Section #2 to keep interior hash in heading text, got: %s", headingOut)
	}

	// 16. Verify static/style.css heading scale without heading bottom borders, direct-child selectors, hr, and table rules
	cssBytes, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("failed to read static/style.css: %v", err)
	}
	cssStr := string(cssBytes)
	for _, sel := range []string{".markdown-body h1", ".markdown-body h2", ".markdown-body h3", ".markdown-body h4", ".markdown-body h5", ".markdown-body h6", ".markdown-body > :first-child", ".markdown-body > :last-child", ".markdown-body hr", ".markdown-body table"} {
		if !strings.Contains(cssStr, sel) {
			t.Errorf("expected style.css to contain selector %q", sel)
		}
	}
	for _, sz := range []string{"1.45em", "1.25em", "1.12em", "1em", "0.92em", "0.86em"} {
		if !strings.Contains(cssStr, sz) {
			t.Errorf("expected heading font-size %q in style.css", sz)
		}
	}
	for _, hSel := range []string{".markdown-body h1 {", ".markdown-body h2 {"} {
		idx := strings.Index(cssStr, hSel)
		if idx == -1 {
			t.Errorf("expected %q in style.css", hSel)
			continue
		}
		blockEnd := strings.IndexByte(cssStr[idx:], '}')
		if blockEnd == -1 {
			t.Errorf("unclosed rule for %q in style.css", hSel)
			continue
		}
		ruleBody := cssStr[idx : idx+blockEnd]
		if strings.Contains(ruleBody, "border-bottom") || strings.Contains(ruleBody, "padding-bottom") {
			t.Errorf("%q must not have border-bottom or padding-bottom: %s", hSel, ruleBody)
		}
	}

	// 17. Empty headings, closing hashes, and indented blockquotes
	extraHeadOut := string(RenderMarkdown("#\n##   \n# ###\n   ### Foo ###\n   > quoted line"))
	for _, want := range []string{
		"<h1 class=\"md-h1\"></h1>",
		"<h2 class=\"md-h2\"></h2>",
		"<h1 class=\"md-h1\"></h1>",
		"<h3 class=\"md-h3\">Foo</h3>",
		"<blockquote class=\"md-quote\"><p class=\"md-p\">quoted line</p>\n</blockquote>",
	} {
		if !strings.Contains(extraHeadOut, want) {
			t.Errorf("expected %q in %s", want, extraHeadOut)
		}
	}

	// 18. Backslash escapes at block start and inline, while keeping code spans literal
	escDoc := "\\# not heading\n\\- not list\n\\> not quote\n1\\. not list\n\\*not italic\\* and \\`not code\\` and \\[not link\\](https://example.com) and `\\*literal\\*` and \\<script\\>"
	escOut := string(RenderMarkdown(escDoc))
	for _, unexpected := range []string{"<h1", "<ul", "<blockquote", "<ol", "<em>", "<code class=\"md-inline-code\">not code", "<a href=\"https://example.com\">not link</a>", "<script>"} {
		if strings.Contains(escOut, unexpected) {
			t.Errorf("backslash escape failure (%q found) in %s", unexpected, escOut)
		}
	}
	if !strings.Contains(escOut, `<code class="md-inline-code">\*literal\*</code>`) {
		t.Errorf("expected literal backslash inside inline code, got: %s", escOut)
	}
	if !strings.Contains(escOut, "*not italic* and `not code`") {
		t.Errorf("expected unescaped literal characters, got: %s", escOut)
	}
	if !strings.Contains(escOut, `<a href="https://example.com"`) {
		t.Errorf("expected bare https://example.com after \\[not link\\] still auto-linked, got: %s", escOut)
	}

	// 19. Go-safe CommonMark flanking emphasis & >20 matches & nested ***
	goProse := "*p = *q\nif *a != nil && *b != nil\n2 * 3 * 4\n***bold italic***\n**bold and *inner*!**\n___under bold italic___\n__under bold__"
	goOut := string(RenderMarkdown(goProse))
	if strings.Contains(goOut, "<em>p") || strings.Contains(goOut, "<em>a") || strings.Contains(goOut, "<em> 3") || strings.Contains(goOut, "<em>3") {
		t.Errorf("Go pointer/math expression wrongly italicized: %s", goOut)
	}
	if !strings.Contains(goOut, "<em><strong>bold italic</strong></em>") && !strings.Contains(goOut, "<strong><em>bold italic</em></strong>") {
		t.Errorf("expected ***bold italic*** nested strong/em, got: %s", goOut)
	}
	if !strings.Contains(goOut, "<strong>bold and <em>inner</em>!</strong>") {
		t.Errorf("expected nested strong+em, got: %s", goOut)
	}
	if !strings.Contains(goOut, "<em><strong>under bold italic</strong></em>") && !strings.Contains(goOut, "<strong><em>under bold italic</em></strong>") {
		t.Errorf("expected ___under bold italic___ nested strong/em, got: %s", goOut)
	}
	// > 25 _x_ matches without 20-match cap
	var manyUnder []string
	for k := 0; k < 25; k++ {
		manyUnder = append(manyUnder, fmt.Sprintf("_w%d_", k))
	}
	manyOut := string(RenderMarkdown(strings.Join(manyUnder, " ")))
	if !strings.Contains(manyOut, "<em>w24</em>") {
		t.Errorf("expected >20 underscore italics without cap, got: %s", manyOut)
	}
	// Unicode underscore boundaries
	uniOut := string(RenderMarkdown("café_latte vs _café_"))
	if strings.Contains(uniOut, "caf<em>") || !strings.Contains(uniOut, "<em>café</em>") {
		t.Errorf("Unicode underscore boundary handling failed: %s", uniOut)
	}

	// 20. Balanced parens in [label](url), formatted label, and bare URL stopping before *
	advLinks := "[Docs (**lang** `spec`)](https://example.org/docs/Spec_(v2)) and *https://example.com*"
	advLinksOut := string(RenderMarkdown(advLinks))
	if !strings.Contains(advLinksOut, `<a href="https://example.org/docs/Spec_(v2)" target="_blank" rel="noopener noreferrer">Docs (<strong>lang</strong> <code class="md-inline-code">spec</code>)</a>`) {
		t.Errorf("balanced-paren + formatted link label failed: %s", advLinksOut)
	}
	if !strings.Contains(advLinksOut, `<em><a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a></em>`) {
		t.Errorf("expected *https://example.com* to italicize without including * in URL: %s", advLinksOut)
	}

	// 21. +, N), start="N", and nested lists
	listDoc := "+ plus one\n+ plus two\n\n5) fifth\n6) sixth\n   - nested a\n   - nested b\n7) seventh"
	listOut := string(RenderMarkdown(listDoc))
	if !strings.Contains(listOut, "<ul class=\"md-ul\">\n<li>plus one</li>\n<li>plus two</li>\n</ul>") {
		t.Errorf("expected + bullet list, got: %s", listOut)
	}
	if !strings.Contains(listOut, "<ol class=\"md-ol\" start=\"5\">\n<li>fifth</li>\n<li>sixth\n<ul class=\"md-ul\">\n<li>nested a</li>\n<li>nested b</li>\n</ul>\n</li>\n<li>seventh</li>\n</ol>") {
		t.Errorf("expected nested list with start=5, got: %s", listOut)
	}

	// 22. GFM pipe tables including alignments, escaped \|, inline code `a|b`, and XSS
	tableDoc := "| Left | Center | Right | Expr |\n| :--- | :---: | ---: | --- |\n| `a|b` | a \\| b | <script>x</script> | **ok** |"
	tableOut := string(RenderMarkdown(tableDoc))
	for _, want := range []string{
		"<table class=\"md-table\">",
		"<th align=\"left\">Left</th>",
		"<th align=\"center\">Center</th>",
		"<th align=\"right\">Right</th>",
		"<th>Expr</th>",
		"<td align=\"left\"><code class=\"md-inline-code\">a|b</code></td>",
		"<td align=\"center\">a | b</td>",
		"<td align=\"right\">&lt;script&gt;x&lt;/script&gt;</td>",
		"<td><strong>ok</strong></td>",
	} {
		if !strings.Contains(tableOut, want) {
			t.Errorf("expected %q in table output:\n%s", want, tableOut)
		}
	}

	// 23. Emphasis openersBottom boundary when an intraword opener fails rule-of-3
	emphCases := []struct {
		input string
		want  string
	}{
		{"a**b*c*d", `<p class="md-p">a**b<em>c</em>d</p>`},
		{"**foo*bar*baz*", `<p class="md-p">*<em>foo<em>bar</em>baz</em></p>`},
	}
	for _, tc := range emphCases {
		got := string(RenderMarkdown(tc.input))
		if !strings.Contains(got, tc.want) {
			t.Errorf("RenderMarkdown(%q) = %s, want %s", tc.input, got, tc.want)
		}
	}

	// 24. Quoted and angle-bracketed URLs / mailto autolinks
	urlCases := []struct {
		name      string
		input     string
		want      string
		forbidSub string
	}{
		{
			name:  "double-quoted URL",
			input: `"https://example.com"`,
			want:  `&#34;<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>&#34;`,
		},
		{
			name:  "double-quoted URL with trailing dot",
			input: `"https://example.com."`,
			want:  `&#34;<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>.&#34;`,
		},
		{
			name:  "single-quoted URL",
			input: `'https://example.com'`,
			want:  `&#39;<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>&#39;`,
		},
		{
			name:  "angle autolink",
			input: `<https://example.com>`,
			want:  `<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>`,
		},
		{
			name:  "angle autolink with query amp",
			input: `<https://example.com/a?x=1&y=2>`,
			want:  `<a href="https://example.com/a?x=1&amp;y=2" target="_blank" rel="noopener noreferrer">https://example.com/a?x=1&amp;y=2</a>`,
		},
		{
			name:  "angle mailto autolink",
			input: `<mailto:user@example.com>`,
			want:  `<a href="mailto:user@example.com" target="_blank" rel="noopener noreferrer">user@example.com</a>`,
		},
		{
			name:  "escaped opening angle bracket leaves bare URL clean",
			input: `\<https://example.com>`,
			want:  `&lt;<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>&gt;`,
		},
		{
			name:  "markdown link with angle-bracketed destination",
			input: `[Docs](<https://example.com>)`,
			want:  `<a href="https://example.com" target="_blank" rel="noopener noreferrer">Docs</a>`,
		},
		{
			name:  "angle link inside brackets does not nest anchors",
			input: `[<https://a.com>](https://b.com)`,
			want:  `[<a href="https://a.com" target="_blank" rel="noopener noreferrer">https://a.com</a>](<a href="https://b.com" target="_blank" rel="noopener noreferrer">https://b.com</a>)`,
		},
		{
			name:      "empty host URL not linked",
			input:     `"https://"`,
			want:      `<p class="md-p">&#34;https://&#34;</p>`,
			forbidSub: `href=`,
		},
	}
	for _, tc := range urlCases {
		got := string(RenderMarkdown(tc.input))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: RenderMarkdown(%q) = %s, want %s", tc.name, tc.input, got, tc.want)
		}
		if tc.forbidSub != "" && strings.Contains(got, tc.forbidSub) {
			t.Errorf("%s: unexpected %q in %s", tc.name, tc.forbidSub, got)
		}
	}
	nestedAngleLink := string(RenderMarkdown(`[<https://a.com>](https://b.com)`))
	if strings.Contains(nestedAngleLink, `>https://a.com</a>`) && strings.Count(nestedAngleLink, `<a `) != 2 {
		t.Errorf("expected two sibling links without nesting, got: %s", nestedAngleLink)
	}
	if strings.Contains(nestedAngleLink, `<a href="https://b.com" target="_blank" rel="noopener noreferrer">[<a`) {
		t.Errorf("unexpected nested anchor tag in: %s", nestedAngleLink)
	}

	// 25. Escaped pipes in table inline code vs non-pipe backslashes
	tablePipeDoc := "| Code | Esc | Double |\n| --- | --- | --- |\n| `a \\| b` | `path\\to\\file` | `a \\\\| b` |"
	tablePipeOut := string(RenderMarkdown(tablePipeDoc))
	for _, want := range []string{
		`<td><code class="md-inline-code">a | b</code></td>`,
		`<td><code class="md-inline-code">path\to\file</code></td>`,
		`<td><code class="md-inline-code">a \\| b</code></td>`,
	} {
		if !strings.Contains(tablePipeOut, want) {
			t.Errorf("expected %q in pipe table output:\n%s", want, tablePipeOut)
		}
	}
	escapedSlashTable := "| Left | Right |\n| --- | --- |\n| a \\\\| b | c |"
	escapedSlashOut := string(RenderMarkdown(escapedSlashTable))
	if !strings.Contains(escapedSlashOut, "<td>a \\</td>\n<td>b</td>") {
		t.Errorf("expected escaped backslash before cell delimiter pipe to split cells: %s", escapedSlashOut)
	}

	// 26. Explicit Markdown links, angle autolinks, and URL fragments
	linkCases := []struct {
		name      string
		input     string
		want      string
		forbidSub string
	}{
		{
			name:  "markdown link",
			input: `[spec](https://example.com/spec/1)`,
			want:  `<a href="https://example.com/spec/1" target="_blank" rel="noopener noreferrer">spec</a>`,
		},
		{
			name:  "angle URL and mailto autolink",
			input: `See <https://example.com/a?x=1&y=2> or <mailto:user@example.com>.`,
			want:  `<a href="https://example.com/a?x=1&amp;y=2" target="_blank" rel="noopener noreferrer">https://example.com/a?x=1&amp;y=2</a> or <a href="mailto:user@example.com" target="_blank" rel="noopener noreferrer">user@example.com</a>.`,
		},
		{
			name:  "bare URL with fragment preserved",
			input: `Check https://example.com/docs/page#details for details.`,
			want:  `<a href="https://example.com/docs/page#details" target="_blank" rel="noopener noreferrer">https://example.com/docs/page#details</a> for details.`,
		},
	}
	for _, tc := range linkCases {
		got := string(RenderMarkdown(tc.input))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: RenderMarkdown(%q) = %s, want %s", tc.name, tc.input, got, tc.want)
		}
		if tc.forbidSub != "" && strings.Contains(got, tc.forbidSub) {
			t.Errorf("%s: unexpected %q in %s", tc.name, tc.forbidSub, got)
		}
	}
}

func TestFormatCommitMessage(t *testing.T) {
	msg := "Fix parser bug (see https://example.com/docs/42)."
	out := string(FormatCommitMessage(msg))
	if !strings.Contains(out, `<a href="https://example.com/docs/42" target="_blank" rel="noopener noreferrer">https://example.com/docs/42</a>).`) {
		t.Errorf("expected trailing ').' outside link in commit message, got: %s", out)
	}

	// HTML entities and apostrophes remain safely escaped
	entityMsg := `don't panic in Server's "quoted" path or literal &#42; & <tag>`
	entityOut := string(FormatCommitMessage(entityMsg))
	if strings.Contains(entityOut, `href=`) {
		t.Errorf("unexpected link in plain commit message: %s", entityOut)
	}
	if !strings.Contains(entityOut, `don&#39;t panic in Server&#39;s &#34;quoted&#34; path or literal &amp;#42; &amp; &lt;tag&gt;`) {
		t.Errorf("expected properly escaped entities, got: %s", entityOut)
	}

	// Bare URL with fragment
	anchorMsg := "See https://example.com/page#section for details"
	anchorOut := string(FormatCommitMessage(anchorMsg))
	wantAnchorLink := `See <a href="https://example.com/page#section" target="_blank" rel="noopener noreferrer">https://example.com/page#section</a> for details`
	if anchorOut != wantAnchorLink {
		t.Errorf("FormatCommitMessage(%q) = %q, want %q", anchorMsg, anchorOut, wantAnchorLink)
	}

	// Quoted & angle-bracketed URLs in commit messages must not swallow delimiters into href
	quotedMsg := `Check <https://example.com>, "https://example.com.", 'https://example.com', and "https://".`
	quotedOut := string(FormatCommitMessage(quotedMsg))
	for _, want := range []string{
		`&lt;<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>&gt;`,
		`&#34;<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>.&#34;`,
		`&#39;<a href="https://example.com" target="_blank" rel="noopener noreferrer">https://example.com</a>&#39;`,
		`&#34;https://&#34;`,
	} {
		if !strings.Contains(quotedOut, want) {
			t.Errorf("FormatCommitMessage(%q) missing %q in: %s", quotedMsg, want, quotedOut)
		}
	}
}

func FuzzRenderMarkdownAndCommitMessage(f *testing.F) {
	seeds := []string{
		"# Heading 1\nSome text with *italic* and **bold** and `code`.",
		"```go\nfunc main() {\n\tprintln(\"hello\")\n}\n```",
		"> Blockquote line 1\n> Blockquote line 2",
		"* item 1\n* item 2\n  * nested item",
		"1. first\n2. second\n3. third",
		"[label](https://example.com/path/42)",
		"[evil](javascript:alert(1))",
		"[evil]( javascript:alert(1) )",
		"[evil](java\tscript:alert(1))",
		"[evil](javascript&#x3A;alert(1))",
		"[evil](data:text/html;base64,PHNjcmlwdD4=)",
		"[evil](vbscript:msgbox)",
		"[safe relative](docs/readme.md)",
		"[safe anchor](#section)",
		"[safe abs](/overview)",
		"[safe mailto](mailto:user@example.com)",
		"<https://example.com>",
		`"https://example.com."`,
		"[Docs](<https://example.com>)",
		"[<https://a.com>](https://b.com)",
		"a**b*c*d",
		"| `a \\| b` | `a \\\\| b` |\n| --- | --- |\n| x | y |",
		"<script>alert('xss')</script>",
		"<iframe src=\"javascript:alert(1)\"></iframe>",
		"<img src=x onerror=alert(1)>",
		"See https://example.com/docs/42 and <mailto:user@example.com>.",
		`Plain text \[escaped\] &#42; <https://example.org/page#sec>`,
		strings.Repeat(">", 100) + " deep quote",
		strings.Repeat("*", 200),
		strings.Repeat("_", 200),
		strings.Repeat("`", 200),
		strings.Repeat("[x](http://a.com)", 50),
		"\x00\r\n\r\nhello \x00 world",
		"---\n***\n___\n- - -\n##### H5\n###### H6",
	}

	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 8192 {
			return
		}

		// 1. RenderMarkdown
		outMD := string(RenderMarkdown(input))
		if utf8.ValidString(input) && !utf8.ValidString(outMD) {
			t.Fatalf("RenderMarkdown produced invalid UTF-8 for valid UTF-8 input")
		}
		assertStrictXSSSafety(t, outMD, "RenderMarkdown")

		// 2. FormatCommitMessage
		outCM := string(FormatCommitMessage(input))
		if utf8.ValidString(input) && !utf8.ValidString(outCM) {
			t.Fatalf("FormatCommitMessage produced invalid UTF-8 for valid UTF-8 input")
		}
		assertStrictXSSSafety(t, outCM, "FormatCommitMessage")
	})
}

var reHTMLTag = regexp.MustCompile(`(?s)<(/?[a-zA-Z0-9]+)([^>]*)>`)
var reTagHref = regexp.MustCompile(`href="([^"]*)"`)

func assertStrictXSSSafety(t *testing.T, htmlOut, origin string) {
	lower := strings.ToLower(htmlOut)
	if strings.Contains(lower, "<script") || strings.Contains(lower, "<iframe") || strings.Contains(lower, "<object") {
		t.Fatalf("%s contains unescaped dangerous tag: %s", origin, htmlOut)
	}

	matches := reHTMLTag.FindAllStringSubmatch(htmlOut, -1)
	for _, m := range matches {
		tagName := strings.ToLower(strings.TrimPrefix(m[1], "/"))
		attrs := strings.ToLower(m[2])

		// No event handlers in any unescaped tag
		if strings.Contains(attrs, "onerror=") || strings.Contains(attrs, "onload=") || strings.Contains(attrs, "onclick=") {
			t.Fatalf("%s contains unescaped tag with event handler: %s", origin, m[0])
		}

		// If it's an <a> tag, verify href is safe
		if tagName == "a" {
			if hm := reTagHref.FindStringSubmatch(m[2]); len(hm) > 1 {
				hrefVal := hm[1]
				decoded := hrefVal
				for {
					next := html.UnescapeString(decoded)
					if next == decoded {
						break
					}
					decoded = next
				}
				var clean strings.Builder
				for i := 0; i < len(decoded); i++ {
					if decoded[i] > ' ' && decoded[i] != 0x7f {
						clean.WriteByte(decoded[i])
					}
				}
				hLower := strings.ToLower(clean.String())
				if strings.HasPrefix(hLower, "javascript:") || strings.HasPrefix(hLower, "vbscript:") || strings.HasPrefix(hLower, "data:") {
					t.Fatalf("%s contains dangerous href scheme %q in %s", origin, hrefVal, m[0])
				}
			}
		}
	}
}
