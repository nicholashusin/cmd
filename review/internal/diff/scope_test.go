package diff

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestExtractScopesGo(t *testing.T) {
	src := `package main

// TopDoc should not affect start line
func SimpleFunc(a int) string {
	return "hello"
}

// ServerDoc
type Server struct {
	port int
}

// Serve starts listening.
func (s *Server) Serve(
	addr string,
	timeout int,
) error {
	return nil
}

type (
	FirstIface interface {
		Do()
	}
	SecondStruct struct {
		x int
	}
)

func GenericBox[T any](val T) *Box[T] {
	return &Box[T]{val: val}
}
`
	lines := strings.Split(src, "\n")
	scopes := ExtractScopes("server.go", []byte(src), lines)

	if len(scopes) == 0 {
		t.Fatalf("expected extracted scopes, got 0")
	}

	// Verify SimpleFunc starts at line 4 (the "func" keyword), NOT line 3 (the comment)
	var foundSimple bool
	for _, s := range scopes {
		if strings.HasPrefix(s.Header, "func SimpleFunc") {
			foundSimple = true
			if s.StartLine != 4 {
				t.Errorf("SimpleFunc StartLine = %d, want 4", s.StartLine)
			}
			if s.EndLine != 6 {
				t.Errorf("SimpleFunc EndLine = %d, want 6", s.EndLine)
			}
			if s.Header != "func SimpleFunc(a int) string" {
				t.Errorf("SimpleFunc Header = %q", s.Header)
			}
			if s.NameLine != 4 || s.NameCol != 5 {
				t.Errorf("SimpleFunc Name pos = (%d, %d), want (4, 5)", s.NameLine, s.NameCol)
			}
		}
	}
	if !foundSimple {
		t.Errorf("SimpleFunc not found in scopes")
	}

	// Verify Server struct
	var foundServer bool
	for _, s := range scopes {
		if strings.HasPrefix(s.Header, "type Server struct") {
			foundServer = true
			if s.StartLine != 9 {
				t.Errorf("Server struct StartLine = %d, want 9", s.StartLine)
			}
			if s.EndLine != 11 {
				t.Errorf("Server struct EndLine = %d, want 11", s.EndLine)
			}
		}
	}
	if !foundServer {
		t.Errorf("Server struct not found in scopes")
	}

	// Verify multi-line method Serve is collapsed to a single line header
	var foundServe bool
	for _, s := range scopes {
		if strings.HasPrefix(s.Header, "func (s *Server) Serve") {
			foundServe = true
			if s.StartLine != 14 {
				t.Errorf("Serve StartLine = %d, want 14", s.StartLine)
			}
			if s.EndLine != 19 {
				t.Errorf("Serve EndLine = %d, want 19", s.EndLine)
			}
			if strings.Contains(s.Header, "\n") {
				t.Errorf("Serve Header contains newline: %q", s.Header)
			}
			if !strings.Contains(s.Header, "Serve(addr string, timeout int) error") {
				t.Errorf("Serve Header format unexpected: %q", s.Header)
			}
			if s.NameLine != 14 || s.NameCol != 17 {
				t.Errorf("Serve Name pos = (%d, %d), want (14, 17)", s.NameLine, s.NameCol)
			}
		}
	}
	if !foundServe {
		t.Errorf("Serve method not found in scopes")
	}

	// Verify generic function
	var foundGeneric bool
	for _, s := range scopes {
		if strings.Contains(s.Header, "GenericBox[T any]") {
			foundGeneric = true
			if s.StartLine != 30 {
				t.Errorf("GenericBox StartLine = %d, want 30", s.StartLine)
			}
			if s.EndLine != 32 {
				t.Errorf("GenericBox EndLine = %d, want 32", s.EndLine)
			}
		}
	}
	if !foundGeneric {
		t.Errorf("GenericBox not found in scopes")
	}
}

func TestExtractScopesGoValidNoTopLevel(t *testing.T) {
	src := `package main

var x = func() {
	println("hello")
}

const C = 100
`
	lines := strings.Split(src, "\n")
	scopes := ExtractScopes("main.go", []byte(src), lines)
	if len(scopes) != 0 {
		t.Errorf("expected 0 scopes for valid Go file without top-level func/type decls, got %d: %+v", len(scopes), scopes)
	}
}

func TestExtractScopesGoRawStringBackslash(t *testing.T) {
	src := "package main\n\nfunc Foo() {\n\ts := `\\`\n\t_ = s\n}\n\nfunc Bar() {\n\tprintln(\"bar\")\n}\n"
	lines := strings.Split(src, "\n")
	scopes := ExtractScopes("main.go", []byte(src), lines)
	if len(scopes) != 2 {
		t.Fatalf("expected 2 scopes (Foo and Bar), got %d: %+v", len(scopes), scopes)
	}
	if scopes[0].Header != "func Foo()" {
		t.Errorf("scopes[0].Header = %q, want func Foo()", scopes[0].Header)
	}
	if scopes[1].Header != "func Bar()" {
		t.Errorf("scopes[1].Header = %q, want func Bar()", scopes[1].Header)
	}
}

func TestExtractScopesLexicalBlockCommentBraces(t *testing.T) {
	src := `/*
 * {
 * func IgnoredInComment() {
 * }
 */
void real_function() {
    int x = 1;
}
`
	lines := strings.Split(src, "\n")
	scopes := ExtractScopes("file.c", []byte(src), lines)
	if len(scopes) != 1 {
		t.Fatalf("expected 1 scope (real_function), got %d: %+v", len(scopes), scopes)
	}
	if !strings.Contains(scopes[0].Header, "real_function") {
		t.Errorf("expected scope to contain real_function, got %q", scopes[0].Header)
	}
}

func TestScopeAtLineBoundary(t *testing.T) {
	scopes := []ScopeSpan{
		{
			StartLine: 10,
			EndLine:   25,
			Header:    "func Outer()",
		},
		{
			StartLine: 15,
			EndLine:   20,
			Header:    "type Inner struct",
		},
	}

	// line < 10 -> nil
	if s := ScopeAtLine(scopes, 9); s != nil {
		t.Errorf("ScopeAtLine(9) = %v, want nil", s)
	}

	// line == 10 (declaration line itself) -> nil (zero redundancy)
	if s := ScopeAtLine(scopes, 10); s != nil {
		t.Errorf("ScopeAtLine(10) = %v, want nil", s)
	}

	// line == 11 -> Outer
	if s := ScopeAtLine(scopes, 11); s == nil || s.Header != "func Outer()" {
		t.Errorf("ScopeAtLine(11) = %v, want Outer", s)
	}

	// line == 15 (Inner declaration signature line) -> Outer (since 10 < 15 <= 25, but 15 < 15 is false for Inner)
	if s := ScopeAtLine(scopes, 15); s == nil || s.Header != "func Outer()" {
		t.Errorf("ScopeAtLine(15) = %v, want Outer", s)
	}

	// line == 16 (inside Inner) -> Inner (innermost)
	if s := ScopeAtLine(scopes, 16); s == nil || s.Header != "type Inner struct" {
		t.Errorf("ScopeAtLine(16) = %v, want Inner", s)
	}

	// line == 20 (end of Inner) -> Inner
	if s := ScopeAtLine(scopes, 20); s == nil || s.Header != "type Inner struct" {
		t.Errorf("ScopeAtLine(20) = %v, want Inner", s)
	}

	// line == 21 (after Inner, inside Outer) -> Outer
	if s := ScopeAtLine(scopes, 21); s == nil || s.Header != "func Outer()" {
		t.Errorf("ScopeAtLine(21) = %v, want Outer", s)
	}

	// line == 25 (end of Outer) -> Outer
	if s := ScopeAtLine(scopes, 25); s == nil || s.Header != "func Outer()" {
		t.Errorf("ScopeAtLine(25) = %v, want Outer", s)
	}

	// line == 26 (after Outer) -> nil
	if s := ScopeAtLine(scopes, 26); s != nil {
		t.Errorf("ScopeAtLine(26) = %v, want nil", s)
	}
}

func TestExtractScopesLexical(t *testing.T) {
	// 1. Python
	pySrc := `class Runner:
    def __init__(self):
        self.count = 0

    def run(self):
        print("running")
`
	pyLines := strings.Split(pySrc, "\n")
	pyScopes := ExtractScopes("script.py", []byte(pySrc), pyLines)
	if len(pyScopes) < 3 {
		t.Errorf("expected at least 3 python scopes (class + 2 defs), got %d: %+v", len(pyScopes), pyScopes)
	}

	// 2. JavaScript / TypeScript
	jsSrc := `export class Widget {
  constructor() {
    this.init();
  }

  render() {
    return "<div></div>";
  }
}

function standalone(x) {
  return x * 2;
}
`
	jsLines := strings.Split(jsSrc, "\n")
	jsScopes := ExtractScopes("app.ts", []byte(jsSrc), jsLines)
	if len(jsScopes) < 2 {
		t.Errorf("expected at least 2 JS scopes, got %d: %+v", len(jsScopes), jsScopes)
	}

	// 3. Rust
	rsSrc := `pub fn calculate(x: i32) -> i32 {
    let y = x + 1;
    y * 2
}

struct Point {
    x: i32,
    y: i32,
}
`
	rsLines := strings.Split(rsSrc, "\n")
	rsScopes := ExtractScopes("lib.rs", []byte(rsSrc), rsLines)
	if len(rsScopes) < 2 {
		t.Errorf("expected at least 2 Rust scopes, got %d: %+v", len(rsScopes), rsScopes)
	}
}

func TestExtractScopesMultiLineAndCForwardPrototype(t *testing.T) {
	// 1. Multi-line JS function signature
	jsSrc := `export function render(
    a: number,
    b: string
) {
    return a;
}
`
	jsLines := strings.Split(jsSrc, "\n")
	jsScopes := ExtractScopes("render.ts", []byte(jsSrc), jsLines)
	if len(jsScopes) != 1 {
		t.Fatalf("expected 1 scope for multi-line JS function, got %d: %+v", len(jsScopes), jsScopes)
	}
	if jsScopes[0].StartLine != 1 || jsScopes[0].EndLine != 6 {
		t.Errorf("JS scope span = [%d..%d], want [1..6]", jsScopes[0].StartLine, jsScopes[0].EndLine)
	}
	if jsScopes[0].Header != "export function render(a: number, b: string)" {
		t.Errorf("JS scope header = %q, want 'export function render(a: number, b: string)'", jsScopes[0].Header)
	}

	// 2. Multi-line Rust function signature
	rsSrc := `pub fn process_data(
    input: &[u8],
    multiplier: i32,
) -> i32 {
    multiplier * 2
}
`
	rsLines := strings.Split(rsSrc, "\n")
	rsScopes := ExtractScopes("lib.rs", []byte(rsSrc), rsLines)
	if len(rsScopes) != 1 {
		t.Fatalf("expected 1 scope for multi-line Rust function, got %d: %+v", len(rsScopes), rsScopes)
	}
	if rsScopes[0].StartLine != 1 || rsScopes[0].EndLine != 6 {
		t.Errorf("Rust scope span = [%d..%d], want [1..6]", rsScopes[0].StartLine, rsScopes[0].EndLine)
	}
	if !strings.HasPrefix(rsScopes[0].Header, "pub fn process_data(input: &[u8], multiplier: i32) -> i32") {
		t.Errorf("Rust scope header = %q", rsScopes[0].Header)
	}

	// 3. C multi-line forward prototype followed by real definition
	cSrc := `int helper(
    int a,
    int b
);

int helper(int a, int b) {
    return a + b;
}
`
	cLines := strings.Split(cSrc, "\n")
	cScopes := ExtractScopes("math.c", []byte(cSrc), cLines)
	if len(cScopes) != 1 {
		t.Fatalf("expected 1 scope (real definition only, forward proto discarded), got %d: %+v", len(cScopes), cScopes)
	}
	if cScopes[0].StartLine != 6 || cScopes[0].EndLine != 8 {
		t.Errorf("C scope span = [%d..%d], want [6..8]", cScopes[0].StartLine, cScopes[0].EndLine)
	}
	if cScopes[0].Header != "int helper(int a, int b)" {
		t.Errorf("C scope header = %q, want 'int helper(int a, int b)'", cScopes[0].Header)
	}

	// 4. Single-line & multi-line JS destructuring and default arguments with strings containing '//'
	jsDestructSrc := `export function render({ popup, data }, url = "http://localhost:8080", opts = {}) {
  return popup;
}

export function renderMulti(
  { popup, data },
  url = "http://localhost:8080",
  opts = {}
) {
  return data;
}
`
	jsDestructLines := strings.Split(jsDestructSrc, "\n")
	jsDestructScopes := ExtractScopes("render.js", []byte(jsDestructSrc), jsDestructLines)
	if len(jsDestructScopes) != 2 {
		t.Fatalf("expected 2 scopes for JS destructuring/default params, got %d: %+v", len(jsDestructScopes), jsDestructScopes)
	}
	if jsDestructScopes[0].Header != `export function render({ popup, data }, url = "http://localhost:8080", opts = {})` {
		t.Errorf("JS scope[0] header = %q", jsDestructScopes[0].Header)
	}
	if jsDestructScopes[1].StartLine != 5 || jsDestructScopes[1].EndLine != 11 {
		t.Errorf("JS scope[1] span = [%d..%d], want [5..11]", jsDestructScopes[1].StartLine, jsDestructScopes[1].EndLine)
	}
	if jsDestructScopes[1].Header != `export function renderMulti({ popup, data }, url = "http://localhost:8080", opts = {})` {
		t.Errorf("JS scope[1] header = %q", jsDestructScopes[1].Header)
	}

	// 5. Rust lifetime parameters and reference types
	rsLifetimeSrc := `pub fn parse<'a>(s: &'a str) -> &'a str {
    s.trim()
}
`
	rsLifetimeLines := strings.Split(rsLifetimeSrc, "\n")
	rsLifetimeScopes := ExtractScopes("parser.rs", []byte(rsLifetimeSrc), rsLifetimeLines)
	if len(rsLifetimeScopes) != 1 {
		t.Fatalf("expected 1 scope for Rust lifetime function, got %d: %+v", len(rsLifetimeScopes), rsLifetimeScopes)
	}
	if rsLifetimeScopes[0].StartLine != 1 || rsLifetimeScopes[0].EndLine != 3 {
		t.Errorf("Rust lifetime scope span = [%d..%d], want [1..3]", rsLifetimeScopes[0].StartLine, rsLifetimeScopes[0].EndLine)
	}
	if !strings.HasPrefix(rsLifetimeScopes[0].Header, "pub fn parse<'a>(s: &'a str) -> &'a str") {
		t.Errorf("Rust lifetime scope header = %q", rsLifetimeScopes[0].Header)
	}

	// 6. Multi-line Python def signature
	pyMultiSrc := `def fetch(
    url: str,
    timeout: int = 5,
) -> bytes:
    res = do_fetch(url)
    return res
`
	pyMultiLines := strings.Split(pyMultiSrc, "\n")
	pyMultiScopes := ExtractScopes("fetcher.py", []byte(pyMultiSrc), pyMultiLines)
	if len(pyMultiScopes) != 1 {
		t.Fatalf("expected 1 scope for multi-line Python def, got %d: %+v", len(pyMultiScopes), pyMultiScopes)
	}
	if pyMultiScopes[0].StartLine != 1 || pyMultiScopes[0].EndLine != 6 {
		t.Errorf("Python multi-line def span = [%d..%d], want [1..6]", pyMultiScopes[0].StartLine, pyMultiScopes[0].EndLine)
	}
	if pyMultiScopes[0].Header != "def fetch(url: str, timeout: int = 5) -> bytes" {
		t.Errorf("Python multi-line def header = %q, want 'def fetch(url: str, timeout: int = 5) -> bytes'", pyMultiScopes[0].Header)
	}
}

func FuzzExtractScopes(f *testing.F) {
	supportedExts := []string{".go", ".js", ".ts", ".py", ".rs", ".c", ".java", ".cpp", ".sh"}

	seeds := []struct {
		extIdx uint8
		src    string
	}{
		{0, "package main\n\nfunc Hello() string {\n\treturn \"hi\"\n}\n"},
		{0, "package p\n\ntype User struct {\n\tName string\n}\n"},
		{1, "function calculate(a, b) {\n  return a + b;\n}\n"},
		{2, "export class Service {\n  start() {\n  }\n}\n"},
		{3, "def process(items):\n    for x in items:\n        pass\n"},
		{3, "class Runner:\n    def run(self):\n        pass\n"},
		{4, "pub fn execute() -> Result<(), Error> {\n    Ok(())\n}\n"},
		{5, "int main(int argc, char** argv) {\n    return 0;\n}\n"},
		{6, "public class App {\n    public static void main(String[] args) {\n    }\n}\n"},
		{0, strings.Repeat("{\n", 50) + strings.Repeat("}\n", 50)},
		{3, strings.Repeat("(\n", 50) + strings.Repeat(")\n", 50)},
		{0, "func invalid( {\n"},
		{1, "/* unclosed comment\nfunc f() {}"},
		{3, "''' unclosed python docstring\ndef f():\n  pass"},
		{0, "package main\n\nfunc 🎉EmojiFunc🚀() {\n}\n"},
		{0, ""},
	}

	for _, s := range seeds {
		f.Add(s.extIdx, s.src)
	}

	f.Fuzz(func(t *testing.T, extIdx uint8, src string) {
		if len(src) > 8192 {
			return
		}

		ext := supportedExts[int(extIdx)%len(supportedExts)]
		filename := "testfile" + ext
		lines := SplitLines([]byte(src))

		scopes := ExtractScopes(filename, []byte(src), lines)

		for _, s := range scopes {
			if s.StartLine < 1 || s.StartLine > s.EndLine || s.EndLine > len(lines) {
				t.Fatalf("invalid scope bounds: [%d..%d] (totalLines=%d) in %s with header %q",
					s.StartLine, s.EndLine, len(lines), filename, s.Header)
			}
			if utf8.ValidString(src) && !utf8.ValidString(s.Header) {
				t.Fatalf("scope header is invalid UTF-8 for valid UTF-8 source in %s: %q", filename, s.Header)
			}
			if s.NameLine > 0 {
				if s.NameLine < 1 || s.NameLine > len(lines) {
					t.Fatalf("scope NameLine %d out of bounds [1, %d] in %s", s.NameLine, len(lines), filename)
				}
				if s.NameCol < 0 {
					t.Fatalf("scope NameCol %d is negative in %s", s.NameCol, filename)
				}
			}
		}

		// Test ScopeAtLine does not panic
		for line := 1; line <= len(lines)+1; line++ {
			_ = ScopeAtLine(scopes, line)
		}
	})
}
