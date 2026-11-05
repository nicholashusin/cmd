package diff

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitLinesAndIsBinary(t *testing.T) {
	data := []byte("hello\r\nworld\nfoo")
	lines := SplitLines(data)
	if len(lines) != 3 || lines[0] != "hello" || lines[1] != "world" || lines[2] != "foo" {
		t.Fatalf("unexpected SplitLines output: %v", lines)
	}

	if len(SplitLines(nil)) != 0 {
		t.Errorf("expected empty lines for nil")
	}

	if IsBinary([]byte("normal text")) {
		t.Errorf("plain text marked as binary")
	}
	if !IsBinary([]byte("some\x00binary")) {
		t.Errorf("binary with NUL byte not detected")
	}
}

func TestMyersEdits(t *testing.T) {
	a := []string{"A", "B", "C", "A", "B", "B", "A"}
	b := []string{"C", "B", "A", "B", "A", "C"}

	edits := Edits(a, b)
	if len(edits) == 0 {
		t.Fatalf("expected non-empty edits")
	}

	// Verify that applying edits transforms a into b
	var recon []string
	for _, e := range edits {
		switch e.Op {
		case OpEqual:
			recon = append(recon, a[e.AIndex])
		case OpInsert:
			recon = append(recon, b[e.BIndex])
		}
	}
	if len(recon) != len(b) {
		t.Fatalf("reconstructed length %d != len(b) %d", len(recon), len(b))
	}
	for i := range recon {
		if recon[i] != b[i] {
			t.Fatalf("mismatch at %d: %s != %s", i, recon[i], b[i])
		}
	}
}

func TestBuildRowsAndCollapse(t *testing.T) {
	left := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
	right := []string{"1", "2", "3", "4", "FIVE", "6", "7", "8", "9", "10"}

	edits := Edits(left, right)
	rows := BuildRows(left, right, edits)
	if len(rows) != 10 {
		t.Fatalf("expected 10 rows, got %d", len(rows))
	}

	collapsed := Collapse(rows, 1, nil)
	// Should have skip at top (AtTop: true), row 4, row 5 (change), row 6, skip at bottom (AtBottom: true)
	hasTopSkip := false
	hasBottomSkip := false
	for _, r := range collapsed {
		if r.Type == RowSkip {
			if r.AtTop {
				hasTopSkip = true
			}
			if r.AtBottom {
				hasBottomSkip = true
			}
		}
	}
	if !hasTopSkip || !hasBottomSkip {
		t.Errorf("expected both AtTop and AtBottom RowSkips, got top=%v bottom=%v", hasTopSkip, hasBottomSkip)
	}

	uni := Unified(collapsed)
	if len(uni) == 0 {
		t.Errorf("expected non-empty unified rows")
	}
}

func TestIntraline(t *testing.T) {
	l := "func Hello(name string)"
	r := "func Hello(user string)"
	lSpans, rSpans, ok := Intraline(l, r)
	if !ok {
		t.Fatalf("expected intraline to succeed")
	}
	if len(lSpans) != 1 || len(rSpans) != 1 {
		t.Fatalf("expected 1 span each, got %d and %d", len(lSpans), len(rSpans))
	}
	if l[lSpans[0].Start:lSpans[0].End] != "name" {
		t.Errorf("expected 'name', got '%s'", l[lSpans[0].Start:lSpans[0].End])
	}
	if r[rSpans[0].Start:rSpans[0].End] != "user" {
		t.Errorf("expected 'user', got '%s'", r[rSpans[0].Start:rSpans[0].End])
	}

	// Test UTF-8 multibyte characters
	lMB := "fmt.Println(\"世界\")"
	rMB := "fmt.Println(\"地球\")"
	lSpansMB, rSpansMB, okMB := Intraline(lMB, rMB)
	if !okMB {
		t.Fatalf("expected intraline to succeed with multibyte")
	}
	if lMB[lSpansMB[0].Start:lSpansMB[0].End] != "世界" {
		t.Errorf("expected '世界', got '%s'", lMB[lSpansMB[0].Start:lSpansMB[0].End])
	}
	if rMB[rSpansMB[0].Start:rSpansMB[0].End] != "地球" {
		t.Errorf("expected '地球', got '%s'", rMB[rSpansMB[0].Start:rSpansMB[0].End])
	}

	// Test > 50% change cutoff
	lDiff := "abcdefghij"
	rDiff := "1234567890"
	_, _, okDiff := Intraline(lDiff, rDiff)
	if okDiff {
		t.Errorf("expected intraline to abandon when > 50%% changed")
	}
}

func TestLargeFileMyersMiddleSnake(t *testing.T) {
	const totalLines = 5000
	a := make([]string, totalLines)
	for i := 0; i < totalLines; i++ {
		a[i] = fmt.Sprintf("line %d of content", i+1)
	}

	var b []string
	b = append(b, a[:2098]...)
	for j := 0; j < 6; j++ {
		b = append(b, fmt.Sprintf("inserted around 2099 - line %d", j+1))
	}
	b = append(b, a[2108:4408]...)
	for j := 0; j < 6; j++ {
		b = append(b, fmt.Sprintf("inserted around 4409 - line %d", j+1))
	}
	b = append(b, a[4418:]...)

	edits := Edits(a, b)
	rows := BuildRows(a, b, edits)
	collapsed := Collapse(rows, 3, nil)

	changedRows := 0
	for _, r := range collapsed {
		if r.Type != RowEqual && r.Type != RowSkip {
			changedRows++
		}
	}

	if changedRows > 35 {
		t.Fatalf("expected <= 35 changed rows, got %d (Myers middle snake failed to isolate edits!)", changedRows)
	}

	hasLargeSkip := false
	for _, r := range collapsed {
		if r.Type == RowSkip && r.SkipCount > 2000 {
			hasLargeSkip = true
		}
	}
	if !hasLargeSkip {
		t.Errorf("expected a large RowSkip (> 2000 lines) between the two edits")
	}
}

func TestRebaseAttribution(t *testing.T) {
	oldParent := []byte("line1\nline2\n")
	old := []byte("line1\nline2_changed\n")

	newParent := []byte("line1\nupstream_add\nline2\n")
	newFile := []byte("line1\nupstream_add\nline2_changed\n")

	edits := Edits(SplitLines(old), SplitLines(newFile))
	rows := BuildRows(SplitLines(old), SplitLines(newFile), edits)
	MarkRebased(rows, old, newFile, oldParent, newParent)

	// In newFile, "upstream_add" came from newParent, so it's rebase-inherited!
	hasRebasedR := false
	for _, r := range rows {
		if r.RightText == "upstream_add" && r.RebasedR {
			hasRebasedR = true
		}
	}
	if !hasRebasedR {
		t.Errorf("expected upstream_add to be marked RebasedR")
	}
}

func TestSyntaxHighlighting(t *testing.T) {
	code := []string{
		"package main",
		"func add(a, b int) int {",
		"\t// add two numbers <&>",
		"\tmsg := `multi",
		"\tline string`",
		"\treturn a + b",
		"}",
	}

	spans := HighlightLines(code, LangGo)
	if len(spans) != len(code) {
		t.Fatalf("expected %d span sets, got %d", len(code), len(spans))
	}

	// Check package keyword in line 0
	if len(spans[0]) == 0 || spans[0][0].Class != TokenKeyword {
		t.Errorf("expected package keyword in line 0")
	}

	// Check multi-line raw string across line 3 and line 4
	if len(spans[4]) == 0 || spans[4][0].Class != TokenString {
		t.Errorf("expected string class in line 4 continuation of raw string")
	}

	// Test RenderLineHTML with tokens, intraline spans, trailing whitespace, and HTML escaping
	rawLine := "\t// add two numbers <&>   "
	htmlRender := RenderLineHTML(rawLine, LangGo, []Span{{Start: 4, End: 7}})
	str := string(htmlRender)

	if !strings.Contains(str, "&lt;&amp;&gt;") {
		t.Errorf("expected HTML escaping for <&>, got: %s", str)
	}
	if !strings.Contains(str, `class="intra"`) {
		t.Errorf("expected intra mark, got: %s", str)
	}
	if !strings.Contains(str, `class="ws"`) {
		t.Errorf("expected ws span, got: %s", str)
	}
	if !strings.Contains(str, `class="tok-com"`) {
		t.Errorf("expected tok-com span, got: %s", str)
	}

	// Verify template.HTML type
	var _ template.HTML = htmlRender
}

func TestCollapseContextExpansion(t *testing.T) {
	var left, right []string
	for i := 1; i <= 20; i++ {
		left = append(left, "line")
		if i == 10 {
			right = append(right, "changed")
		} else {
			right = append(right, "line")
		}
	}

	edits := Edits(left, right)
	rows := BuildRows(left, right, edits)
	collapsed := Collapse(rows, 1, nil)

	hasSkip := false
	for _, r := range collapsed {
		if r.Type == RowSkip {
			hasSkip = true
			if r.SkipCount == 0 || r.SkipGroup == 0 {
				t.Errorf("expected positive SkipCount and SkipGroup on RowSkip, got count=%d, group=%d", r.SkipCount, r.SkipGroup)
			}
		}
	}

	if !hasSkip {
		t.Errorf("expected RowSkip in collapsed rows")
	}
}

func TestUnifiedRowsSplit(t *testing.T) {
	left := []string{"keep1", "old1", "old2", "keep2"}
	right := []string{"keep1", "new1", "new2", "keep2"}

	edits := Edits(left, right)
	rows := BuildRows(left, right, edits)
	collapsed := Collapse(rows, 1, nil)
	uni := Unified(collapsed)

	hasDel := false
	hasAdd := false
	for _, r := range uni {
		if r.Type == RowDel {
			hasDel = true
		}
		if r.Type == RowAdd {
			hasAdd = true
		}
		if r.Type == RowChange {
			t.Errorf("unified rows should not contain RowChange, got one: %+v", r)
		}
	}

	if !hasDel || !hasAdd {
		t.Errorf("expected unified rows to contain both RowDel and RowAdd")
	}
}

func TestMultiLineRawStringAndBlockComments(t *testing.T) {
	code := []string{
		"func f() {",
		"\t/* block comment",
		"\t   spanning",
		"\t   three lines */",
		"\ts := `raw string",
		"\t   spanning",
		"\t   three lines`",
		"\tprintln(s)",
		"}",
	}

	tokens := HighlightLines(code, LangGo)
	if len(tokens) != len(code) {
		t.Fatalf("expected %d token sets, got %d", len(code), len(tokens))
	}

	// Line 1, 2, 3 should all have TokenComment
	for line := 1; line <= 3; line++ {
		hasCom := false
		for _, tok := range tokens[line] {
			if tok.Class == TokenComment {
				hasCom = true
			}
		}
		if !hasCom {
			t.Errorf("expected line %d to have TokenComment", line)
		}
	}

	// Line 4, 5, 6 should all have TokenString
	for line := 4; line <= 6; line++ {
		hasStr := false
		for _, tok := range tokens[line] {
			if tok.Class == TokenString {
				hasStr = true
			}
		}
		if !hasStr {
			t.Errorf("expected line %d to have TokenString", line)
		}
	}
}

func TestCollapsePositionsAndBounds(t *testing.T) {
	rows := make([]Row, 100)
	for i := range rows {
		rows[i] = Row{Type: RowEqual, LeftLine: i + 1, RightLine: i + 1}
	}
	rows[50] = Row{Type: RowChange, LeftLine: 51, RightLine: 51}

	collapsed := Collapse(rows, 3, nil)
	// With ctx=3 around line 51 (0-indexed 50), kept lines are 48..54 (1-indexed)
	// Top skip should be lines 1..47
	// Bottom skip should be lines 55..100
	if len(collapsed) == 0 {
		t.Fatalf("expected collapsed rows")
	}
	topSkip := collapsed[0]
	if topSkip.Type != RowSkip || topSkip.LeftStart != 1 || topSkip.LeftEnd != 47 || topSkip.RightStart != 1 || topSkip.RightEnd != 47 {
		t.Errorf("unexpected top skip bounds: %+v", topSkip)
	}
	bottomSkip := collapsed[len(collapsed)-1]
	if bottomSkip.Type != RowSkip || bottomSkip.LeftStart != 55 || bottomSkip.LeftEnd != 100 || bottomSkip.RightStart != 55 || bottomSkip.RightEnd != 100 {
		t.Errorf("unexpected bottom skip bounds: %+v", bottomSkip)
	}
}

func verifyReconstruction(t *testing.T, a, b []string, edits []Edit) {
	t.Helper()
	var recon []string
	for _, e := range edits {
		switch e.Op {
		case OpEqual:
			if e.AIndex < 0 || e.AIndex >= len(a) || e.BIndex < 0 || e.BIndex >= len(b) {
				t.Fatalf("out of bounds indices in OpEqual: A=%d, B=%d", e.AIndex, e.BIndex)
			}
			recon = append(recon, a[e.AIndex])
		case OpInsert:
			if e.BIndex < 0 || e.BIndex >= len(b) {
				t.Fatalf("out of bounds index in OpInsert: B=%d", e.BIndex)
			}
			recon = append(recon, b[e.BIndex])
		case OpDelete:
			if e.AIndex < 0 || e.AIndex >= len(a) {
				t.Fatalf("out of bounds index in OpDelete: A=%d", e.AIndex)
			}
		}
	}
	if len(recon) != len(b) {
		t.Fatalf("reconstruction length mismatch: got %d, want %d", len(recon), len(b))
	}
	for i := range recon {
		if recon[i] != b[i] {
			t.Fatalf("reconstruction content mismatch at %d: got %q, want %q", i, recon[i], b[i])
		}
	}
}

func TestMyersAsymmetricAndFuzz(t *testing.T) {
	// Case 1: n=20, m=2 with 0 common lines
	a1 := make([]string, 20)
	for i := range a1 {
		a1[i] = fmt.Sprintf("a_%d", i)
	}
	b1 := []string{"b_0", "b_1"}
	verifyReconstruction(t, a1, b1, Edits(a1, b1))

	// Case 2: n=2, m=20 with 0 common lines
	verifyReconstruction(t, b1, a1, Edits(b1, a1))

	// Case 3: n=100, m=5 with 1 common line
	a3 := make([]string, 100)
	for i := range a3 {
		a3[i] = fmt.Sprintf("line_%d", i)
	}
	b3 := []string{"diff_0", "diff_1", "line_50", "diff_2", "diff_3"}
	verifyReconstruction(t, a3, b3, Edits(a3, b3))

	// Case 4: n=5, m=100 with 1 common line
	verifyReconstruction(t, b3, a3, Edits(b3, a3))

	// Case 5: 500 pseudo-random edits
	for seed := 1; seed <= 500; seed++ {
		lenA := (seed*17)%40 + 1
		lenB := (seed*31)%40 + 1
		curA := make([]string, lenA)
		curB := make([]string, lenB)
		for i := range curA {
			curA[i] = fmt.Sprintf("tok_%d", (seed+i*3)%15)
		}
		for j := range curB {
			curB[j] = fmt.Sprintf("tok_%d", (seed*7+j*5)%15)
		}
		edits := Edits(curA, curB)
		verifyReconstruction(t, curA, curB, edits)
	}

	// Case 6: 4,500-line file with 20 deletions and 12 additions
	n := 4500
	a6 := make([]string, n)
	for i := range a6 {
		a6[i] = fmt.Sprintf("code line %d", i+1)
	}
	b6 := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if i == 2000 {
			for k := 0; k < 12; k++ {
				b6 = append(b6, fmt.Sprintf("added %d", k))
			}
			continue
		}
		if i >= 3500 && i < 3520 {
			continue // delete 20 lines
		}
		b6 = append(b6, a6[i])
	}
	verifyReconstruction(t, a6, b6, Edits(a6, b6))
}

func TestHighlightGoIdentifiers(t *testing.T) {
	lines := []string{
		"func Calculate(total int, 世界 string, x_α int) {",
	}
	spans := HighlightLines(lines, LangGo)
	if len(spans) != 1 {
		t.Fatalf("expected 1 line of spans, got %d", len(spans))
	}

	line := lines[0]
	var idents []string
	var keywords []string
	var types []string

	for _, s := range spans[0] {
		text := line[s.Start:s.End]
		switch s.Class {
		case TokenKeyword:
			keywords = append(keywords, text)
		case TokenType:
			types = append(types, text)
		case TokenIdent:
			idents = append(idents, text)
		}
	}

	if len(keywords) != 1 || keywords[0] != "func" {
		t.Errorf("expected keyword [func], got %v", keywords)
	}
	if len(types) != 3 || types[0] != "int" || types[1] != "string" || types[2] != "int" {
		t.Errorf("expected types [int, string, int], got %v", types)
	}
	expectedIdents := []string{"Calculate", "total", "世界", "x_α"}
	if len(idents) != len(expectedIdents) {
		t.Fatalf("expected idents %v, got %v", expectedIdents, idents)
	}
	for i := range expectedIdents {
		if idents[i] != expectedIdents[i] {
			t.Errorf("ident %d: got %q, want %q", i, idents[i], expectedIdents[i])
		}
	}

	// Verify Python does NOT emit TokenIdent
	pyLines := []string{"def calculate(total, name):"}
	pySpans := HighlightLines(pyLines, LangPython)
	for _, s := range pySpans[0] {
		if s.Class == TokenIdent {
			t.Errorf("Python line unexpectedly emitted TokenIdent for %q", pyLines[0][s.Start:s.End])
		}
	}
}

func TestHighlightGoImportStrings(t *testing.T) {
	testCases := []struct {
		name     string
		line     string
		wantText string
		wantCls  TokenClass
	}{
		{"plain import", `import "fmt"`, `"fmt"`, TokenImportString},
		{"aliased import", `import h "net/http"`, `"net/http"`, TokenImportString},
		{"blank import", `import _ "embed"`, `"embed"`, TokenImportString},
		{"dot import", `import . "math"`, `"math"`, TokenImportString},
		{"raw string import", "import `os`", "`os`", TokenImportString},
		{"const string outside import", `const s = "fmt"`, `"fmt"`, TokenString},
		{"call string outside import", `fmt.Println("hi")`, `"hi"`, TokenString},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			spans := HighlightLines([]string{tc.line}, LangGo)
			var foundCls TokenClass
			for _, s := range spans[0] {
				txt := tc.line[s.Start:s.End]
				if txt == tc.wantText {
					foundCls = s.Class
					break
				}
			}
			if foundCls != tc.wantCls {
				t.Errorf("token %s in %q: got class %q, want %q", tc.wantText, tc.line, foundCls, tc.wantCls)
			}
		})
	}

	// Semicolon-separated import block on a single line
	singleLineBlock := `import ( "fmt"; "io" )`
	spansSingle := HighlightLines([]string{singleLineBlock}, LangGo)
	var singleImports []string
	for _, s := range spansSingle[0] {
		if s.Class == TokenImportString {
			singleImports = append(singleImports, singleLineBlock[s.Start:s.End])
		}
	}
	if len(singleImports) != 2 || singleImports[0] != `"fmt"` || singleImports[1] != `"io"` {
		t.Errorf("single line block imports: got %v, want [\"fmt\", \"io\"]", singleImports)
	}

	// Multi-line import block with keep[0] = false (collapsed import ( line)
	multiLines := []string{
		"import (",
		"\t\"context\"",
		"\t\"net/http\"",
		")",
		"func main() {",
		"\t_ = \"hello\"",
		"}",
	}
	keep := []bool{false, true, true, false, true, true, true}
	spansMulti := HighlightLinesKeep(multiLines, LangGo, keep)

	if len(spansMulti[1]) != 1 || spansMulti[1][0].Class != TokenImportString {
		t.Errorf("multi-line line 1: got spans %+v, want 1 TokenImportString span", spansMulti[1])
	}
	if len(spansMulti[2]) != 1 || spansMulti[2][0].Class != TokenImportString {
		t.Errorf("multi-line line 2: got spans %+v, want 1 TokenImportString span", spansMulti[2])
	}

	// Line 5 ("_ = \"hello\"") must be plain TokenString, not TokenImportString
	var line5StringCls TokenClass
	for _, s := range spansMulti[5] {
		if multiLines[5][s.Start:s.End] == `"hello"` {
			line5StringCls = s.Class
			break
		}
	}
	if line5StringCls != TokenString {
		t.Errorf("line 5 \"hello\": got class %q, want %q", line5StringCls, TokenString)
	}
}

func TestHighlightIntralineTokenNesting(t *testing.T) {
	// Case 1: Partial intraline diff on suffix of identifier: EscapePath -> Path in intra
	line1 := "res := EscapePath(arg)"
	toks1 := []TokenSpan{
		{Start: 7, End: 17, Class: TokenIdent},  // EscapePath
		{Start: 18, End: 21, Class: TokenIdent}, // arg
	}
	intra1 := []Span{
		{Start: 13, End: 17}, // Path
	}
	html1 := string(RenderTokensHTMLWS(line1, toks1, intra1, false))
	wantSub1 := `<span class="tok-id">Escape<mark class="intra">Path</mark></span>`
	if !strings.Contains(html1, wantSub1) {
		t.Errorf("got %q, want substring %q", html1, wantSub1)
	}
	if strings.Contains(html1, `</span><mark class="intra">`) {
		t.Errorf("got %q, should not close token before mark.intra", html1)
	}
	if strings.Count(html1, `class="tok-id"`) != 2 {
		t.Errorf("got %q, want exactly 2 tok-id spans", html1)
	}

	// Case 2: Partial intraline diff on prefix of identifier: NewReader -> New in intra
	line2 := "NewReader(r)"
	toks2 := []TokenSpan{
		{Start: 0, End: 9, Class: TokenIdent}, // NewReader
	}
	intra2 := []Span{
		{Start: 0, End: 3}, // New
	}
	html2 := string(RenderTokensHTMLWS(line2, toks2, intra2, false))
	wantSub2 := `<span class="tok-id"><mark class="intra">New</mark>Reader</span>`
	if !strings.Contains(html2, wantSub2) {
		t.Errorf("got %q, want substring %q", html2, wantSub2)
	}
	if strings.Count(html2, `class="tok-id"`) != 1 {
		t.Errorf("got %q, want exactly 1 tok-id span", html2)
	}

	// Case 3: Intraline diff spanning across tokens: a.B() where a.B is in intra
	line3 := "a.B()"
	toks3 := []TokenSpan{
		{Start: 0, End: 1, Class: TokenIdent}, // a
		{Start: 2, End: 3, Class: TokenIdent}, // B
	}
	intra3 := []Span{
		{Start: 0, End: 3}, // a.B
	}
	html3 := string(RenderTokensHTMLWS(line3, toks3, intra3, false))
	wantSub3 := `<span class="tok-id"><mark class="intra">a</mark></span><mark class="intra">.</mark><span class="tok-id"><mark class="intra">B</mark></span>()`
	if html3 != wantSub3 {
		t.Errorf("got %q, want %q", html3, wantSub3)
	}
}

func TestServe400HandlerIntralineAndAlignment(t *testing.T) {
	leftCode := `func checkValidHTTP2RequestHeaders(h Header) error {
	return nil
}


type serve400Handler struct {
	err error
}

func (handler serve400Handler) ServeHTTP(w *ResponseWriter, r *ServerRequest) {
	const statusBadRequest = 400

	// TODO: Dedup with http.Error?
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(statusBadRequest)
	fmt.Fprintln(w, handler.err.Error())
}

// h1ServerKeepAlivesDisabled reports whether hs has its keep-alives
func h1ServerKeepAlivesDisabled(hs ServerConfig) bool {
	return !hs.DoKeepAlives()
}`

	rightCode := `func checkValidHTTP2RequestHeaders(h Header) error {
	return nil
}

func serve400Handler(err error) func(*ResponseWriter, *ServerRequest) error {
	return func(w *ResponseWriter, r *ServerRequest) error {
		const statusBadRequest = 400

		// TODO: Dedup with http.Error?
		h := w.Header()
		h.Del("Content-Length")
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(statusBadRequest)
		fmt.Fprintln(w, err.Error())
		return nil // nil because non-nil error is only used for panic.
	}
}

// h1ServerKeepAlivesDisabled reports whether hs has its keep-alives
func h1ServerKeepAlivesDisabled(hs ServerConfig) bool {
	return !hs.DoKeepAlives()
}`

	leftLines := SplitLines([]byte(leftCode))
	rightLines := SplitLines([]byte(rightCode))
	edits := Edits(leftLines, rightLines)
	rows := BuildDiffRows(leftLines, rightLines, edits)

	// 1. Verify 1-to-1 pairing of the 6 statements
	statements := []string{
		"\t// TODO: Dedup with http.Error?",
		"\th := w.Header()",
		"\th.Del(\"Content-Length\")",
		"\th.Set(\"Content-Type\", \"text/plain; charset=utf-8\")",
		"\th.Set(\"X-Content-Type-Options\", \"nosniff\")",
		"\tw.WriteHeader(statusBadRequest)",
	}

	for _, stmt := range statements {
		found := false
		for _, r := range rows {
			if r.LeftText == stmt {
				found = true
				if r.Type != RowChange {
					t.Errorf("line %q: expected RowChange, got %v", stmt, r.Type)
				}
				if r.RightText != "\t"+stmt {
					t.Errorf("line %q: expected RightText %q, got %q", stmt, "\t"+stmt, r.RightText)
				}
				if len(r.LeftIntra) != 0 {
					t.Errorf("line %q: expected empty LeftIntra, got %+v", stmt, r.LeftIntra)
				}
				wantRightIntra := []Span{{Start: 0, End: 1}}
				if len(r.RightIntra) != 1 || r.RightIntra[0] != wantRightIntra[0] {
					t.Errorf("line %q: expected RightIntra %+v, got %+v", stmt, wantRightIntra, r.RightIntra)
				}
				break
			}
		}
		if !found {
			t.Errorf("line %q not found in rows", stmt)
		}
	}

	// 2. Verify fmt.Fprintln row
	fmtFound := false
	for _, r := range rows {
		if r.LeftText == "\tfmt.Fprintln(w, handler.err.Error())" {
			fmtFound = true
			if r.Type != RowChange {
				t.Errorf("fmt.Fprintln: expected RowChange, got %v", r.Type)
			}
			if r.RightText != "\t\tfmt.Fprintln(w, err.Error())" {
				t.Errorf("fmt.Fprintln: expected RightText %q, got %q", "\t\tfmt.Fprintln(w, err.Error())", r.RightText)
			}
			if len(r.LeftIntra) != 1 {
				t.Fatalf("fmt.Fprintln: expected 1 LeftIntra span, got %+v", r.LeftIntra)
			}
			gotLeftSub := r.LeftText[r.LeftIntra[0].Start:r.LeftIntra[0].End]
			if gotLeftSub != "handler." {
				t.Errorf("fmt.Fprintln: expected LeftIntra %q, got %q", "handler.", gotLeftSub)
			}
			wantRightIntra := []Span{{Start: 0, End: 1}}
			if len(r.RightIntra) != 1 || r.RightIntra[0] != wantRightIntra[0] {
				t.Errorf("fmt.Fprintln: expected RightIntra %+v, got %+v", wantRightIntra, r.RightIntra)
			}
			break
		}
	}
	if !fmtFound {
		t.Errorf("fmt.Fprintln row not found")
	}

	// 3. Verify return nil and \t} are RowAdd with empty RightIntra
	returnNilFound := false
	braceFound := false
	for _, r := range rows {
		if r.RightText == "\t\treturn nil // nil because non-nil error is only used for panic." {
			returnNilFound = true
			if r.Type != RowAdd {
				t.Errorf("return nil: expected RowAdd, got %v", r.Type)
			}
			if len(r.RightIntra) != 0 {
				t.Errorf("return nil: expected empty RightIntra, got %+v", r.RightIntra)
			}
		}
		if r.Type == RowAdd && r.RightText == "\t}" {
			braceFound = true
			if len(r.RightIntra) != 0 {
				t.Errorf("trailing }: expected empty RightIntra, got %+v", r.RightIntra)
			}
		}
	}
	if !returnNilFound {
		t.Errorf("return nil row not found")
	}
	if !braceFound {
		t.Errorf("trailing } RowAdd not found")
	}
}

func TestWhitespaceSlidingBoundary(t *testing.T) {
	leftCode := "x =  a\n"
	rightCode := "x =   b \n"
	leftLines := SplitLines([]byte(leftCode))
	rightLines := SplitLines([]byte(rightCode))
	edits := Edits(leftLines, rightLines)
	rows := BuildDiffRows(leftLines, rightLines, edits)
	if len(rows) != 1 || rows[0].Type != RowChange {
		t.Fatalf("expected 1 RowChange, got %+v", rows)
	}
	r := rows[0]
	if len(r.RightIntra) != 1 {
		t.Fatalf("expected 1 RightIntra span, got %+v", r.RightIntra)
	}
	span := r.RightIntra[0]
	gotSub := r.RightText[span.Start:span.End]
	if gotSub != " b " {
		t.Errorf("expected RightIntra span %q, got %q (span: %+v in %q)", " b ", gotSub, span, r.RightText)
	}
}

func FuzzMyersDiff(f *testing.F) {
	seeds := []struct {
		oldData []byte
		newData []byte
	}{
		{[]byte(""), []byte("")},
		{[]byte("hello"), []byte("hello")},
		{[]byte("hello\n"), []byte("world\n")},
		{[]byte("A\nB\nC\nA\nB\nB\nA\n"), []byte("C\nB\nA\nB\nA\nC\n")},
		{[]byte("line 1\r\nline 2\r\n"), []byte("line 1\nline 2 amended\n")},
		{[]byte("trailing newline\n"), []byte("no trailing newline")},
		{[]byte("no trailing newline"), []byte("trailing newline\n")},
		{[]byte("\x00binary\x00data\x00"), []byte("\x00binary\x00amended\x00")},
		{[]byte("🔥 UTF-8 🎉\n"), []byte("🔥 UTF-8 🚀\n")},
		{bytes.Repeat([]byte("a"), 500), bytes.Repeat([]byte("a"), 499)},
		{bytes.Repeat([]byte("abc\n"), 100), bytes.Repeat([]byte("cba\n"), 100)},
		{[]byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"), []byte("1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n10\n")},
		{[]byte("a 0\xef\nb\n"), []byte("00a00\nb2\n")},
	}

	for _, s := range seeds {
		f.Add(s.oldData, s.newData)
	}

	f.Fuzz(func(t *testing.T, oldBytes, newBytes []byte) {
		if len(oldBytes) > 8192 || len(newBytes) > 8192 {
			return
		}

		oldLines := SplitLines(oldBytes)
		newLines := SplitLines(newBytes)

		edits := Edits(oldLines, newLines)

		// 1. Verify exact reconstruction
		var recon []string
		for _, e := range edits {
			switch e.Op {
			case OpEqual:
				if e.AIndex < 0 || e.AIndex >= len(oldLines) {
					t.Fatalf("OpEqual AIndex %d out of bounds [0, %d)", e.AIndex, len(oldLines))
				}
				if e.BIndex < 0 || e.BIndex >= len(newLines) {
					t.Fatalf("OpEqual BIndex %d out of bounds [0, %d)", e.BIndex, len(newLines))
				}
				if oldLines[e.AIndex] != newLines[e.BIndex] {
					t.Fatalf("OpEqual mismatch: old[%d]=%q != new[%d]=%q", e.AIndex, oldLines[e.AIndex], e.BIndex, newLines[e.BIndex])
				}
				recon = append(recon, oldLines[e.AIndex])
			case OpInsert:
				if e.BIndex < 0 || e.BIndex >= len(newLines) {
					t.Fatalf("OpInsert BIndex %d out of bounds [0, %d)", e.BIndex, len(newLines))
				}
				recon = append(recon, newLines[e.BIndex])
			case OpDelete:
				if e.AIndex < 0 || e.AIndex >= len(oldLines) {
					t.Fatalf("OpDelete AIndex %d out of bounds [0, %d)", e.AIndex, len(oldLines))
				}
			default:
				t.Fatalf("unknown Op %d", e.Op)
			}
		}

		if len(recon) != len(newLines) {
			t.Fatalf("reconstruction length %d != len(newLines) %d", len(recon), len(newLines))
		}
		for i := range recon {
			if recon[i] != newLines[i] {
				t.Fatalf("reconstruction mismatch at %d: %q != %q", i, recon[i], newLines[i])
			}
		}

		// 2. Verify BuildRows and monotonicity of line numbers
		rows := BuildRows(oldLines, newLines, edits)
		lastL := 0
		lastR := 0
		for _, r := range rows {
			if r.LeftLine > 0 {
				if r.LeftLine < 1 || r.LeftLine > len(oldLines) {
					t.Fatalf("LeftLine %d out of bounds [1, %d]", r.LeftLine, len(oldLines))
				}
				if r.LeftLine <= lastL {
					t.Fatalf("non-monotonic LeftLine: prev=%d curr=%d", lastL, r.LeftLine)
				}
				lastL = r.LeftLine
			}
			if r.RightLine > 0 {
				if r.RightLine < 1 || r.RightLine > len(newLines) {
					t.Fatalf("RightLine %d out of bounds [1, %d]", r.RightLine, len(newLines))
				}
				if r.RightLine <= lastR {
					t.Fatalf("non-monotonic RightLine: prev=%d curr=%d", lastR, r.RightLine)
				}
				lastR = r.RightLine
			}
		}

		assertRowSpans := func(rList []Row) {
			for _, r := range rList {
				for _, sp := range r.LeftIntra {
					if sp.Start < 0 || sp.Start > sp.End || sp.End > len(r.LeftText) {
						t.Fatalf("invalid LeftIntra span %+v in LeftText of len %d (%q)", sp, len(r.LeftText), r.LeftText)
					}
				}
				for _, sp := range r.RightIntra {
					if sp.Start < 0 || sp.Start > sp.End || sp.End > len(r.RightText) {
						t.Fatalf("invalid RightIntra span %+v in RightText of len %d (%q)", sp, len(r.RightText), r.RightText)
					}
				}
				if r.Type == RowChange || r.Type == RowDel {
					_ = RenderLineHTML(r.LeftText, LangGo, r.LeftIntra)
				}
				if r.Type == RowChange || r.Type == RowAdd {
					_ = RenderLineHTML(r.RightText, LangGo, r.RightIntra)
				}
			}
		}

		assertRowSpans(rows)

		// 3. Test Collapse and Unified
		collapsed := Collapse(rows, 3, nil)
		_ = Unified(collapsed)

		// 4. Test BuildDiffRows
		diffRows := BuildDiffRows(oldLines, newLines, edits)
		assertRowSpans(diffRows)
	})
}

func FuzzIntralineAndHighlight(f *testing.F) {
	seeds := []struct {
		ext     string
		oldLine string
		newLine string
	}{
		{"go", "func Hello(name string)", "func Hello(user string)"},
		{"go", "func add(a, b int) int { return a + b }", "func add(a, b, c int) int { return a + b + c }"},
		{"go", "const greeting = \"Hello, World!\"", "const greeting = \"Hello, Go!\""},
		{"js", "const x = { name: 'Alice', age: 30 };", "const x = { name: 'Bob', age: 30 };"},
		{"py", "def process_data(items, verbose=False):", "def process_data(items, verbose=True):"},
		{"py", "    '''Triple quote docstring'''", "    \"\"\"Triple quote docstring\"\"\""},
		{"rs", "pub fn calculate(val: u32) -> Result<u32, Error> {", "pub fn calculate(val: u64) -> Result<u64, Error> {"},
		{"c", "int main(int argc, char** argv) {", "int main(void) {"},
		{"sql", "SELECT id, name FROM users WHERE active = 1;", "SELECT id, email FROM users WHERE active = 1;"},
		{"sh", "#!/bin/bash\necho \"Starting...\"", "#!/bin/bash\necho \"Done.\""},
		{"go", "var emoji = \"🎉 Party 🚀 Rocket\"", "var emoji = \"🎉 Fiesta 🚀 Rocket\""},
		{"go", "\t\treturn nil // comment with spaces   ", "\t\treturn err // comment with spaces\t"},
		{"go", "<script>alert(1)</script>", "<script>alert(2)</script>"},
		{"go", "foo & bar < baz > qux", "foo & bar <= baz >= qux"},
		{"go", "x =  a\n", "x =   b \n"},
		{"txt", "identical line", "identical line"},
		{"txt", "", ""},
		{"0", "a 0\xef", "00a00"},
	}

	for _, s := range seeds {
		f.Add(s.ext, s.oldLine, s.newLine)
	}

	f.Fuzz(func(t *testing.T, ext, oldLine, newLine string) {
		if len(oldLine) > 2048 || len(newLine) > 2048 || len(ext) > 16 {
			return
		}

		lang := DetectLang("file." + ext)

		lSpans, rSpans, ok := Intraline(oldLine, newLine)
		if ok {
			for _, s := range lSpans {
				if s.Start < 0 || s.Start > s.End || s.End > len(oldLine) {
					t.Fatalf("invalid lSpan %+v in oldLine of len %d", s, len(oldLine))
				}
			}
			for _, s := range rSpans {
				if s.Start < 0 || s.Start > s.End || s.End > len(newLine) {
					t.Fatalf("invalid rSpan %+v in newLine of len %d", s, len(newLine))
				}
			}
		}

		tokensL := HighlightLines([]string{oldLine}, lang)[0]
		tokensR := HighlightLines([]string{newLine}, lang)[0]

		for _, tok := range tokensL {
			if tok.Start < 0 || tok.Start > tok.End || tok.End > len(oldLine) {
				t.Fatalf("invalid TokenSpan %+v in oldLine of len %d", tok, len(oldLine))
			}
		}
		for _, tok := range tokensR {
			if tok.Start < 0 || tok.Start > tok.End || tok.End > len(newLine) {
				t.Fatalf("invalid TokenSpan %+v in newLine of len %d", tok, len(newLine))
			}
		}

		// Test rendering with and without whitespace highlighting
		htmlL := RenderTokensHTMLWS(oldLine, tokensL, lSpans, true)
		htmlR := RenderTokensHTMLWS(newLine, tokensR, rSpans, true)
		_ = RenderTokensHTMLWS(oldLine, tokensL, lSpans, false)
		_ = RenderTokensHTMLWS(newLine, tokensR, rSpans, false)
		_ = RenderLineHTML(oldLine, lang, lSpans)
		_ = RenderLineHTML(newLine, lang, rSpans)

		// Assertions for valid UTF-8 lines
		if utf8.ValidString(oldLine) {
			strL := string(htmlL)
			if !utf8.ValidString(strL) {
				t.Fatalf("RenderTokensHTMLWS produced invalid UTF-8 for valid UTF-8 oldLine: %q -> %q", oldLine, strL)
			}
			assertHTMLRoundtripAndSafety(t, oldLine, strL)
		}

		if utf8.ValidString(newLine) {
			strR := string(htmlR)
			if !utf8.ValidString(strR) {
				t.Fatalf("RenderTokensHTMLWS produced invalid UTF-8 for valid UTF-8 newLine: %q -> %q", newLine, strR)
			}
			assertHTMLRoundtripAndSafety(t, newLine, strR)
		}
	})
}

var reDiffTag = regexp.MustCompile(`(?s)</?[a-zA-Z0-9]+[^>]*>`)

func assertHTMLRoundtripAndSafety(t *testing.T, original, renderedHTML string) {
	// 1. Text roundtrip: stripping tags and html.UnescapeString must match original
	stripped := reDiffTag.ReplaceAllString(renderedHTML, "")
	unescaped := html.UnescapeString(stripped)
	if unescaped != original {
		t.Fatalf("HTML roundtrip failed:\norig:  %q\nstrip: %q\nunesc: %q", original, stripped, unescaped)
	}

	// 2. Safety: outside tags, raw '<' or '>' must not exist
	if strings.Contains(stripped, "<") || strings.Contains(stripped, ">") {
		t.Fatalf("raw '<' or '>' found in stripped HTML text: %q (from %q)", stripped, original)
	}
}

func TestGoDocLinkHighlighting(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		lang       Lang
		wantLinks  []string
		wantNoLink bool
	}{
		{
			name:      "single identifier",
			lines:     []string{"// See [Server] for details."},
			lang:      LangGo,
			wantLinks: []string{"Server"},
		},
		{
			name:      "pointer method",
			lines:     []string{"// Method [*Server.Shutdown] gracefully shuts down."},
			lang:      LangGo,
			wantLinks: []string{"*Server.Shutdown"},
		},
		{
			name:      "pkg symbol",
			lines:     []string{"// Returns an [http.Client] configured for pooling."},
			lang:      LangGo,
			wantLinks: []string{"http.Client"},
		},
		{
			name:      "full import path with method",
			lines:     []string{"// Implements [net/http.Server.Shutdown] method."},
			lang:      LangGo,
			wantLinks: []string{"net/http.Server.Shutdown"},
		},
		{
			name:      "package doc link",
			lines:     []string{"// Package [encoding/json] implements encoding."},
			lang:      LangGo,
			wantLinks: []string{"encoding/json"},
		},
		{
			name:      "pointer type doc link",
			lines:     []string{"// Uses [*http.Server] and [*net/http.Server]."},
			lang:      LangGo,
			wantLinks: []string{"*http.Server", "*net/http.Server"},
		},
		{
			name:      "dotted and versioned import paths",
			lines:     []string{"// Parses [gopkg.in/yaml.v3], [gopkg.in/yaml.v3.Node], [gopkg.in/yaml.v3.Node.Decode], [example.com/foo/v2.Client], [example.com/foo/v2+incompatible.Client]."},
			lang:      LangGo,
			wantLinks: []string{"gopkg.in/yaml.v3", "gopkg.in/yaml.v3.Node", "gopkg.in/yaml.v3.Node.Decode", "example.com/foo/v2.Client", "example.com/foo/v2+incompatible.Client"},
		},
		{
			name:       "inner star rejected",
			lines:      []string{"// Invalid [pkg.*Type], [pkg.*Type.Method], [a/b.*Type], [a/b.*Type.Method], [**Type]."},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:       "leading star on package path rejected",
			lines:      []string{"// Invalid [*net/http] and [*encoding/json]."},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:      "single line block comment",
			lines:     []string{"/* see [Reader] */"},
			lang:      LangGo,
			wantLinks: []string{"Reader"},
		},
		{
			name:      "inline string before comment and block comment tight closer",
			lines:     []string{`x := "https://[fake]" // [Widget]`, `/* a */ x := 1 /* [Widget]*/`},
			lang:      LangGo,
			wantLinks: []string{"Widget", "Widget"},
		},
		{
			name:      "prose colon at start of comment line still links",
			lines:     []string{"// [Server]: closes idle connections."},
			lang:      LangGo,
			wantLinks: []string{"Server"},
		},
		{
			name: "multiline block comment",
			lines: []string{
				"/*",
				" * [io.Reader] is an interface.",
				" */",
			},
			lang:      LangGo,
			wantLinks: []string{"io.Reader"},
		},
		{
			name:       "markdown link url",
			lines:      []string{"// See [text](https://example.com) for info."},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:       "markdown link ref",
			lines:      []string{"// See [text][ref] for info."},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:       "markdown link def line",
			lines:      []string{"// [ref]: https://example.com"},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:       "slice index access",
			lines:      []string{"// accesses a[i] and s[0][j]"},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:       "uppercase prose tags",
			lines:      []string{"// [TODO] fix this and [NOTE] check this"},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:       "file extensions",
			lines:      []string{"// See [README.md] and [config.json] and [go.mod]"},
			lang:       LangGo,
			wantNoLink: true,
		},
		{
			name:       "non-Go language JS",
			lines:      []string{"// See [Server] for details."},
			lang:       LangJS,
			wantNoLink: true,
		},
		{
			name:       "non-Go language Python",
			lines:      []string{"# See [Server] for details."},
			lang:       LangPython,
			wantNoLink: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokenLines := HighlightLines(tc.lines, tc.lang)
			var gotLinks []string
			for lineIdx, tokens := range tokenLines {
				line := tc.lines[lineIdx]
				for _, tok := range tokens {
					if tok.Class == TokenDocLink {
						gotLinks = append(gotLinks, line[tok.Start:tok.End])
					}
					// Ensure spans are within bounds and non-empty
					if tok.Start < 0 || tok.End > len(line) || tok.Start >= tok.End {
						t.Errorf("invalid token span [%d, %d] in line len %d", tok.Start, tok.End, len(line))
					}
				}
			}

			if tc.wantNoLink {
				if len(gotLinks) > 0 {
					t.Errorf("expected no TokenDocLink, got %v", gotLinks)
				}
			} else {
				if !slices.Equal(gotLinks, tc.wantLinks) {
					t.Errorf("expected links %v, got %v", tc.wantLinks, gotLinks)
				}
			}
		})
	}

	t.Run("HighlightLinesKeep skips doc links on collapsed lines", func(t *testing.T) {
		lines := []string{
			"/*",
			" * See [Server] here.",
			" * And [io.Reader] here.",
			" */",
		}
		keep := []bool{true, false, true, true}
		tokens := HighlightLinesKeep(lines, LangGo, keep)
		if len(tokens[1]) != 0 {
			t.Fatalf("expected collapsed line 1 to have 0 tokens, got %+v", tokens[1])
		}
		var keptLinks []string
		for _, tok := range tokens[2] {
			if tok.Class == TokenDocLink {
				keptLinks = append(keptLinks, lines[2][tok.Start:tok.End])
			}
		}
		if !slices.Equal(keptLinks, []string{"io.Reader"}) {
			t.Fatalf("expected kept line 2 doc link [io.Reader], got %v", keptLinks)
		}
	})
}
