package diff

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ScopeSpan represents the line range and header of an enclosing declaration.
type ScopeSpan struct {
	StartLine int    `json:"s"`            // 1-based start line of declaration
	EndLine   int    `json:"e"`            // 1-based end line of declaration
	Header    string `json:"h"`            // concise single-line declaration header
	NameLine  int    `json:"nl,omitempty"` // 1-based line of Go declaration identifier
	NameCol   int    `json:"nc,omitempty"` // 0-based UTF-16 column of Go declaration identifier
}

func identLineAndUTF16Col(lines []string, pos token.Position) (int, int) {
	if pos.Line <= 0 || pos.Column <= 0 || pos.Line > len(lines) {
		return 0, 0
	}
	byteIdx := pos.Column - 1
	line := lines[pos.Line-1]
	if byteIdx > len(line) {
		byteIdx = len(line)
	}
	col := 0
	for _, r := range line[:byteIdx] {
		if r > 0xFFFF {
			col += 2
		} else {
			col++
		}
	}
	return pos.Line, col
}

// ExtractScopes finds top-level or method/class declarations in a file.
func ExtractScopes(filename string, src []byte, lines []string) []ScopeSpan {
	if len(lines) == 0 || filename == "/COMMIT_MSG" || filename == "COMMIT_MSG" {
		return nil
	}
	if src == nil && len(lines) > 0 {
		src = []byte(strings.Join(lines, "\n"))
	}

	lang := DetectLang(filename)
	if lang == LangGo {
		fset := token.NewFileSet()
		astFile, parseErr := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
		if astFile != nil {
			var spans []ScopeSpan
			for _, decl := range astFile.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					startLine := fset.Position(d.Pos()).Line
					endLine := fset.Position(d.End()).Line
					if startLine < 1 {
						startLine = 1
					}
					if endLine > len(lines) {
						endLine = len(lines)
					}
					if endLine <= startLine {
						continue
					}
					cleanDecl := &ast.FuncDecl{
						Recv: d.Recv,
						Name: d.Name,
						Type: d.Type,
					}
					var buf bytes.Buffer
					if err := printer.Fprint(&buf, fset, cleanDecl); err == nil {
						hdr := cleanSignature(buf.String())
						hdr = truncateRunes(hdr, 120)
						var nameLine, nameCol int
						if d.Name != nil {
							nameLine, nameCol = identLineAndUTF16Col(lines, fset.Position(d.Name.Pos()))
						}
						spans = append(spans, ScopeSpan{
							StartLine: startLine,
							EndLine:   endLine,
							Header:    hdr,
							NameLine:  nameLine,
							NameCol:   nameCol,
						})
					}
				case *ast.GenDecl:
					if d.Tok != token.TYPE {
						continue
					}
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						startLine := fset.Position(ts.Pos()).Line
						if len(d.Specs) == 1 {
							startLine = fset.Position(d.Pos()).Line
						}
						endLine := fset.Position(ts.End()).Line
						if startLine < 1 {
							startLine = 1
						}
						if endLine > len(lines) {
							endLine = len(lines)
						}
						if endLine <= startLine {
							continue
						}
						var typeParams string
						if ts.TypeParams != nil && len(ts.TypeParams.List) > 0 {
							var tpBuf bytes.Buffer
							if err := printer.Fprint(&tpBuf, fset, ts.TypeParams); err == nil {
								typeParams = tpBuf.String()
							}
						}
						var kindWord string
						switch ts.Type.(type) {
						case *ast.StructType:
							kindWord = "struct"
						case *ast.InterfaceType:
							kindWord = "interface"
						default:
							var tBuf bytes.Buffer
							if err := printer.Fprint(&tBuf, fset, ts.Type); err == nil {
								kindWord = tBuf.String()
							}
						}
						hdr := collapseSpaces("type " + ts.Name.Name + typeParams + " " + kindWord)
						hdr = truncateRunes(hdr, 120)
						var nameLine, nameCol int
						if ts.Name != nil {
							nameLine, nameCol = identLineAndUTF16Col(lines, fset.Position(ts.Name.Pos()))
						}
						spans = append(spans, ScopeSpan{
							StartLine: startLine,
							EndLine:   endLine,
							Header:    hdr,
							NameLine:  nameLine,
							NameCol:   nameCol,
						})
					}
				}
			}
			if parseErr == nil || len(spans) > 0 {
				sortScopes(spans)
				return spans
			}
		}
	}

	return extractScopesLexical(filename, lang, lines)
}

func sortScopes(spans []ScopeSpan) {
	slices.SortFunc(spans, func(a, b ScopeSpan) int {
		if a.StartLine != b.StartLine {
			return a.StartLine - b.StartLine
		}
		return b.EndLine - a.EndLine
	})
}

// extractScopesLexical is the fallback extractor for Python and brace-based languages.
func extractScopesLexical(filename string, lang Lang, lines []string) []ScopeSpan {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".md", ".txt", ".json", ".yaml", ".yml", ".toml", ".csv", ".tsv", ".xml", ".html", ".css", ".scss", ".less":
		return nil
	}

	if lang == LangPython {
		return extractScopesPython(lines)
	}

	// Brace-based languages: C/C++, Rust, JS/TS, Java, Go fallback
	isBraceLang := (lang == LangGo || lang == LangJS || lang == LangC || lang == LangRust ||
		ext == ".java" || ext == ".kt" || ext == ".cs" || ext == ".php" || ext == ".sh")
	if isBraceLang {
		return extractScopesBraces(lines, lang)
	}

	return nil
}

func extractScopesPython(lines []string) []ScopeSpan {
	var spans []ScopeSpan
	type pyScope struct {
		startLine  int
		indent     int
		header     string
		parenDepth int
		opened     bool
	}
	var stack []pyScope

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		codeWithoutComment, openParens, closeParens := pythonCodeAndParens(trimmed)
		if codeWithoutComment == "" {
			continue
		}

		indent := countLeadingSpaces(line)

		// If top of stack is not opened yet, it's accumulating a multi-line def/class signature
		if len(stack) > 0 && !stack[len(stack)-1].opened {
			top := &stack[len(stack)-1]
			if lineNum > top.startLine {
				top.header += " " + trimmed
			}
			top.parenDepth += openParens - closeParens
			if top.parenDepth < 0 {
				top.parenDepth = 0
			}
			if top.parenDepth == 0 && strings.HasSuffix(codeWithoutComment, ":") {
				hdr := strings.TrimSuffix(top.header, ":")
				hdr = cleanSignature(hdr)
				hdr = truncateRunes(hdr, 120)
				top.header = hdr
				top.opened = true
			}
			continue
		}

		// Close any stack entries whose indent >= current indent
		for len(stack) > 0 && stack[len(stack)-1].opened && stack[len(stack)-1].indent >= indent {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			endLine := i
			for endLine > top.startLine && strings.TrimSpace(lines[endLine-1]) == "" {
				endLine--
			}
			if endLine > top.startLine {
				spans = append(spans, ScopeSpan{
					StartLine: top.startLine,
					EndLine:   endLine,
					Header:    top.header,
				})
			}
		}

		isDef := strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ")
		isClass := strings.HasPrefix(trimmed, "class ")
		if (isDef || isClass) && indent <= 8 {
			initParen := openParens - closeParens
			if initParen < 0 {
				initParen = 0
			}
			opened := (initParen == 0 && strings.HasSuffix(codeWithoutComment, ":"))
			hdr := trimmed
			if opened {
				hdr = strings.TrimSuffix(hdr, ":")
				hdr = cleanSignature(hdr)
				hdr = truncateRunes(hdr, 120)
			}
			stack = append(stack, pyScope{
				startLine:  lineNum,
				indent:     indent,
				header:     hdr,
				parenDepth: initParen,
				opened:     opened,
			})
		}
	}

	// Close remaining entries at EOF
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !top.opened {
			continue
		}
		endLine := len(lines)
		for endLine > top.startLine && strings.TrimSpace(lines[endLine-1]) == "" {
			endLine--
		}
		if endLine > top.startLine {
			spans = append(spans, ScopeSpan{
				StartLine: top.startLine,
				EndLine:   endLine,
				Header:    top.header,
			})
		}
	}

	sortScopes(spans)
	return spans
}

func pythonCodeAndParens(s string) (codeWithoutComment string, openCount, closeCount int) {
	inQuote := byte(0)
	isTriple := false
	var code strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == '\\' {
				code.WriteByte(c)
				if i+1 < len(s) {
					i++
					code.WriteByte(s[i])
				}
				continue
			}
			code.WriteByte(c)
			if isTriple {
				if c == inQuote && i+2 < len(s) && s[i+1] == inQuote && s[i+2] == inQuote {
					code.WriteByte(s[i+1])
					code.WriteByte(s[i+2])
					i += 2
					inQuote = 0
					isTriple = false
				}
			} else if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if (c == '"' || c == '\'') && i+2 < len(s) && s[i+1] == c && s[i+2] == c {
			inQuote = c
			isTriple = true
			code.WriteByte(c)
			code.WriteByte(c)
			code.WriteByte(c)
			i += 2
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			code.WriteByte(c)
			continue
		}
		if c == '#' {
			break
		}
		code.WriteByte(c)
		switch c {
		case '(', '[', '{':
			openCount++
		case ')', ']', '}':
			closeCount++
		}
	}
	return strings.TrimSpace(code.String()), openCount, closeCount
}

func isRustCharLiteral(s string, i int) bool {
	if i+1 >= len(s) {
		return false
	}
	if s[i+1] == '\\' {
		// e.g. '\n', '\t', '\'', '\\', '\x7f', '\u{1234}'
		rest := s[i+2:]
		idx := strings.IndexByte(rest, '\'')
		return idx >= 1 && idx <= 8
	}
	return i+2 < len(s) && s[i+2] == '\''
}

func stripLineComment(s string, lang Lang) string {
	inQuote := byte(0)
	inBlock := false
	for i := 0; i < len(s); i++ {
		if inBlock {
			if s[i] == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		c := s[i]
		if inQuote != 0 {
			if c == '\\' && (inQuote != '`' || lang != LangGo) {
				i++
				continue
			}
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' {
			inQuote = c
			continue
		}
		if c == '`' && (lang == LangGo || lang == LangJS) {
			inQuote = c
			continue
		}
		if c == '\'' {
			if lang == LangRust {
				if isRustCharLiteral(s, i) {
					inQuote = c
				}
			} else {
				inQuote = c
			}
			continue
		}
		if c == '/' && i+1 < len(s) {
			if s[i+1] == '/' {
				return strings.TrimSpace(s[:i])
			}
			if s[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
		}
	}
	return strings.TrimSpace(s)
}

func extractScopesBraces(lines []string, lang Lang) []ScopeSpan {
	var spans []ScopeSpan
	type braceScope struct {
		startLine  int
		header     string
		braceDepth int
		parenDepth int
		opened     bool
	}
	var stack []braceScope
	depth := 0
	inBlockComment := false

	declPrefixes := []string{
		"func ", "fn ", "pub fn ", "impl ", "struct ", "enum ", "trait ",
		"class ", "interface ", "function ", "async function ",
		"export function ", "export async function ", "export class ",
		"export interface ", "export default function ",
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// Strip line comments for header assembly and forward declaration detection
		lineCode := stripLineComment(trimmed, lang)

		// Count braces and parens outside string literals and comments
		open, close, openParen, closeParen := countBraces(trimmed, &inBlockComment, lang)

		// If the top of stack is !opened and lineNum > top.startLine, append continuation up to '{'
		if len(stack) > 0 && !stack[len(stack)-1].opened {
			top := &stack[len(stack)-1]
			nextParen := top.parenDepth + openParen - closeParen
			if nextParen < 0 {
				nextParen = 0
			}
			top.parenDepth = nextParen
			if lineNum > top.startLine && lineCode != "" {
				part := lineCode
				if top.parenDepth == 0 {
					part = strings.TrimSpace(strings.TrimSuffix(part, "{"))
				}
				if part != "" {
					top.header += " " + part
				}
			}
			// If top of stack is !opened, outside parens, and ends with ';', it's a forward declaration (no body)
			if top.parenDepth == 0 && strings.HasSuffix(lineCode, ";") {
				stack = stack[:len(stack)-1]
			}
		}

		// Check for a new declaration only if stack is empty OR top of stack is already opened
		canStartDecl := (len(stack) == 0 || stack[len(stack)-1].opened)
		indent := countLeadingSpaces(line)
		if canStartDecl && !inBlockComment && indent <= 4 && lineCode != "" && !strings.HasPrefix(trimmed, "/*") {
			isDecl := false
			for _, p := range declPrefixes {
				if strings.HasPrefix(lineCode, p) {
					isDecl = true
					break
				}
			}
			// Check C/Java-style function: type name(args) { or type name(args) [with { on later line]
			if !isDecl && strings.Contains(lineCode, "(") {
				parenIdx := strings.Index(lineCode, "(")
				prefix := strings.TrimSpace(lineCode[:parenIdx])
				// Ensure it is not control flow
				if !strings.HasPrefix(lineCode, "if ") && !strings.HasPrefix(lineCode, "for ") &&
					!strings.HasPrefix(lineCode, "while ") && !strings.HasPrefix(lineCode, "switch ") &&
					!strings.HasPrefix(lineCode, "catch ") && !strings.HasPrefix(lineCode, "return ") &&
					strings.Contains(prefix, " ") { // must have return type and name, e.g. "int helper"
					isDecl = true
				}
			}

			if isDecl {
				initParen := openParen - closeParen
				if initParen < 0 {
					initParen = 0
				}
				hdr := lineCode
				if initParen == 0 {
					hdr = strings.TrimSpace(strings.TrimSuffix(hdr, "{"))
				}
				// Check if it ends with ';' on the same line (single-line forward declaration: int foo(int a);)
				if !(initParen == 0 && strings.HasSuffix(hdr, ";")) {
					stack = append(stack, braceScope{
						startLine:  lineNum,
						header:     hdr,
						braceDepth: depth,
						parenDepth: initParen,
						opened:     false,
					})
				}
			}
		}

		depth += open - close

		// For any entry on stack with !opened, if parens are closed and open > 0, mark opened and finalize header
		if open > 0 && len(stack) > 0 {
			for j := len(stack) - 1; j >= 0; j-- {
				if !stack[j].opened && stack[j].parenDepth == 0 {
					stack[j].opened = true
					stack[j].header = truncateRunes(cleanSignature(stack[j].header), 120)
				}
			}
		}

		// Pop closed scopes
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.opened && depth <= top.braceDepth {
				stack = stack[:len(stack)-1]
				if lineNum > top.startLine {
					spans = append(spans, ScopeSpan{
						StartLine: top.startLine,
						EndLine:   lineNum,
						Header:    top.header,
					})
				}
			} else {
				break
			}
		}
	}

	sortScopes(spans)
	return spans
}

func countBraces(s string, inBlockComment *bool, lang Lang) (open, close, openParen, closeParen int) {
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		if *inBlockComment {
			if s[i] == '*' && i+1 < len(s) && s[i+1] == '/' {
				*inBlockComment = false
				i++
			}
			continue
		}
		c := s[i]
		if inQuote != 0 {
			if c == '\\' && (inQuote != '`' || lang != LangGo) {
				i++
				continue
			}
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' {
			inQuote = c
			continue
		}
		if c == '`' && (lang == LangGo || lang == LangJS) {
			inQuote = c
			continue
		}
		if c == '\'' {
			if lang == LangRust {
				if isRustCharLiteral(s, i) {
					inQuote = c
				}
			} else {
				inQuote = c
			}
			continue
		}
		if c == '/' && i+1 < len(s) {
			if s[i+1] == '/' {
				break
			}
			if s[i+1] == '*' {
				*inBlockComment = true
				i++
				continue
			}
		}
		switch c {
		case '{':
			open++
		case '}':
			close++
		case '(':
			openParen++
		case ')':
			closeParen++
		}
	}
	return open, close, openParen, closeParen
}

func countLeadingSpaces(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
		} else if r == '\t' {
			n += 4
		} else {
			break
		}
	}
	return n
}

func cleanSignature(s string) string {
	s = collapseSpaces(s)
	s = strings.ReplaceAll(s, "( ", "(")
	s = strings.ReplaceAll(s, " )", ")")
	s = strings.ReplaceAll(s, ",)", ")")
	s = strings.ReplaceAll(s, ", )", ")")
	return s
}

func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
}

// ScopeAtLine returns the innermost ScopeSpan containing line in its body (StartLine < line <= EndLine).
// The declaration line itself (line == StartLine) returns nil.
func ScopeAtLine(scopes []ScopeSpan, line int) *ScopeSpan {
	var best *ScopeSpan
	for i := range scopes {
		s := &scopes[i]
		if s.StartLine < line && line <= s.EndLine {
			if best == nil || s.StartLine > best.StartLine || (s.StartLine == best.StartLine && s.EndLine < best.EndLine) {
				best = s
			}
		}
	}
	return best
}
