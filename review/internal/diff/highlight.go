package diff

import (
	"html/template"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Lang identifies the programming or markup language for syntax highlighting.
type Lang int

const (
	LangNone Lang = iota
	LangGo
	LangJS
	LangPython
	LangShell
	LangSQL
	LangCSS
	LangJSON
	LangYAML
	LangTOML
	LangC
	LangRust
)

// TokenClass represents the CSS class name for a highlighted token.
type TokenClass string

const (
	TokenNone         TokenClass = ""
	TokenKeyword      TokenClass = "tok-kw"
	TokenString       TokenClass = "tok-str"
	TokenImportString TokenClass = "tok-str tok-imp"
	TokenComment      TokenClass = "tok-com"
	TokenDocLink      TokenClass = "tok-com tok-doc"
	TokenNumber       TokenClass = "tok-num"
	TokenType         TokenClass = "tok-typ"
	TokenIdent        TokenClass = "tok-id"
)

// TokenSpan represents a token within a line [Start, End) byte offsets.
type TokenSpan struct {
	Class TokenClass
	Start int
	End   int
}

// DetectLang infers the language from the file extension or filename.
func DetectLang(filename string) Lang {
	ext := strings.ToLower(filepath.Ext(filename))
	base := strings.ToLower(filepath.Base(filename))

	switch ext {
	case ".go":
		return LangGo
	case ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx":
		return LangJS
	case ".py", ".pyw":
		return LangPython
	case ".sh", ".bash", ".zsh", ".env":
		return LangShell
	case ".sql":
		return LangSQL
	case ".css", ".scss", ".less":
		return LangCSS
	case ".json":
		return LangJSON
	case ".yaml", ".yml":
		return LangYAML
	case ".toml":
		return LangTOML
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp":
		return LangC
	case ".rs":
		return LangRust
	}

	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") {
		return LangShell
	}
	return LangNone
}

type scanState int

const (
	stateNormal scanState = iota
	stateBlockComment
	stateRawStringGo
	stateTemplateStringJS
	stateTripleQuotePyDouble
	stateTripleQuotePySingle
)

const maxHighlightLineBytes = 16384

// HighlightLines tokenizes lines while maintaining multi-line state across lines.
func HighlightLines(lines []string, lang Lang) [][]TokenSpan {
	return HighlightLinesKeep(lines, lang, nil)
}

// HighlightLinesKeep tokenizes lines while maintaining multi-line state across lines,
// skipping token span generation for lines where keep[idx] is false to avoid unnecessary allocations.
func HighlightLinesKeep(lines []string, lang Lang, keep []bool) [][]TokenSpan {
	result := make([][]TokenSpan, len(lines))
	if lang == LangNone {
		return result
	}

	state := stateNormal
	goImportAwait := false
	goImportBlock := false

	for idx, line := range lines {
		shouldKeep := (keep == nil || (idx < len(keep) && keep[idx])) && len(line) <= maxHighlightLineBytes
		var spans []TokenSpan
		addSpan := func(cls TokenClass, start, end int) {
			if shouldKeep {
				spans = append(spans, TokenSpan{Class: cls, Start: start, End: end})
			}
		}
		pos := 0
		n := len(line)

		// Resume multi-line state
		switch state {
		case stateBlockComment:
			end := strings.Index(line, "*/")
			if end != -1 {
				if lang == LangGo && shouldKeep {
					addCommentWithDocLinks(addSpan, line, 0, end+2, 0, end)
				} else {
					addSpan(TokenComment, 0, end+2)
				}
				pos = end + 2
				state = stateNormal
			} else {
				if lang == LangGo && shouldKeep {
					addCommentWithDocLinks(addSpan, line, 0, n, 0, n)
				} else {
					addSpan(TokenComment, 0, n)
				}
				result[idx] = spans
				continue
			}
		case stateRawStringGo:
			end := strings.IndexByte(line, '`')
			if end != -1 {
				addSpan(TokenString, 0, end+1)
				pos = end + 1
				state = stateNormal
			} else {
				addSpan(TokenString, 0, n)
				result[idx] = spans
				continue
			}
		case stateTemplateStringJS:
			closed := false
			for pos < n {
				if line[pos] == '\\' {
					pos += 2
					if pos > n {
						pos = n
					}
					continue
				}
				if line[pos] == '`' {
					pos++
					closed = true
					break
				}
				pos++
			}
			addSpan(TokenString, 0, pos)
			if closed {
				state = stateNormal
			} else {
				result[idx] = spans
				continue
			}
		case stateTripleQuotePyDouble:
			end := strings.Index(line, `"""`)
			if end != -1 {
				addSpan(TokenString, 0, end+3)
				pos = end + 3
				state = stateNormal
			} else {
				addSpan(TokenString, 0, n)
				result[idx] = spans
				continue
			}
		case stateTripleQuotePySingle:
			end := strings.Index(line, `'''`)
			if end != -1 {
				addSpan(TokenString, 0, end+3)
				pos = end + 3
				state = stateNormal
			} else {
				addSpan(TokenString, 0, n)
				result[idx] = spans
				continue
			}
		}

		for pos < n {
			// Skip whitespace
			if line[pos] == ' ' || line[pos] == '\t' || line[pos] == '\r' {
				pos++
				continue
			}

			// Comments
			if (lang == LangGo || lang == LangJS || lang == LangC || lang == LangRust || lang == LangCSS) &&
				pos+1 < n && line[pos] == '/' && line[pos+1] == '/' {
				if lang == LangGo && shouldKeep {
					addCommentWithDocLinks(addSpan, line, pos, n, pos+2, n)
				} else {
					addSpan(TokenComment, pos, n)
				}
				pos = n
				break
			}
			if (lang == LangGo || lang == LangJS || lang == LangC || lang == LangRust || lang == LangCSS || lang == LangSQL) &&
				pos+1 < n && line[pos] == '/' && line[pos+1] == '*' {
				end := strings.Index(line[pos+2:], "*/")
				if end != -1 {
					commentEnd := pos + 2 + end + 2
					if lang == LangGo && shouldKeep {
						addCommentWithDocLinks(addSpan, line, pos, commentEnd, pos+2, pos+2+end)
					} else {
						addSpan(TokenComment, pos, commentEnd)
					}
					pos = commentEnd
					continue
				} else {
					if lang == LangGo && shouldKeep {
						addCommentWithDocLinks(addSpan, line, pos, n, pos+2, n)
					} else {
						addSpan(TokenComment, pos, n)
					}
					pos = n
					state = stateBlockComment
					break
				}
			}
			if (lang == LangPython || lang == LangShell || lang == LangYAML || lang == LangTOML) && line[pos] == '#' {
				addSpan(TokenComment, pos, n)
				pos = n
				break
			}
			if lang == LangSQL && pos+1 < n && line[pos] == '-' && line[pos+1] == '-' {
				addSpan(TokenComment, pos, n)
				pos = n
				break
			}

			// Python multi-line triple quotes
			if lang == LangPython {
				if pos+2 < n && line[pos:pos+3] == `"""` {
					end := strings.Index(line[pos+3:], `"""`)
					if end != -1 {
						strEnd := pos + 3 + end + 3
						addSpan(TokenString, pos, strEnd)
						pos = strEnd
						continue
					} else {
						addSpan(TokenString, pos, n)
						pos = n
						state = stateTripleQuotePyDouble
						break
					}
				}
				if pos+2 < n && line[pos:pos+3] == `'''` {
					end := strings.Index(line[pos+3:], `'''`)
					if end != -1 {
						strEnd := pos + 3 + end + 3
						addSpan(TokenString, pos, strEnd)
						pos = strEnd
						continue
					} else {
						addSpan(TokenString, pos, n)
						pos = n
						state = stateTripleQuotePySingle
						break
					}
				}
			}

			// JS template strings `...` (may span multiple lines)
			if lang == LangJS && line[pos] == '`' {
				start := pos
				pos++
				closed := false
				for pos < n {
					if line[pos] == '\\' {
						pos += 2
						if pos > n {
							pos = n
						}
						continue
					}
					if line[pos] == '`' {
						pos++
						closed = true
						break
					}
					pos++
				}
				addSpan(TokenString, start, pos)
				if !closed {
					state = stateTemplateStringJS
					break
				}
				continue
			}

			// Strings (avoid treating Rust lifetimes like 'a as char literals)
			if line[pos] == '"' || (line[pos] == '\'' && (lang != LangRust || isRustCharLiteral(line, pos))) {
				quote := line[pos]
				start := pos
				pos++
				for pos < n {
					if line[pos] == '\\' {
						pos += 2
						if pos > n {
							pos = n
						}
						continue
					}
					if line[pos] == quote {
						pos++
						break
					}
					pos++
				}
				tokCls := TokenString
				if lang == LangGo && quote == '"' && (goImportAwait || goImportBlock) {
					tokCls = TokenImportString
					if goImportAwait && !goImportBlock {
						goImportAwait = false
					}
				}
				addSpan(tokCls, start, pos)
				continue
			}

			// Go raw strings `...`
			if lang == LangGo && line[pos] == '`' {
				start := pos
				end := strings.Index(line[pos+1:], "`")
				if end != -1 {
					strEnd := pos + 1 + end + 1
					tokCls := TokenString
					if goImportAwait || goImportBlock {
						tokCls = TokenImportString
						if goImportAwait && !goImportBlock {
							goImportAwait = false
						}
					}
					addSpan(tokCls, start, strEnd)
					pos = strEnd
					continue
				} else {
					addSpan(TokenString, start, n)
					pos = n
					state = stateRawStringGo
					break
				}
			}

			// Numbers
			if isDigit(line[pos]) || (line[pos] == '.' && pos+1 < n && isDigit(line[pos+1])) {
				start := pos
				for pos < n && (isAlphaNumeric(line[pos]) || line[pos] == '.' || line[pos] == '_') {
					pos++
				}
				addSpan(TokenNumber, start, pos)
				continue
			}

			// Identifiers & keywords
			if isIdentStart(line[pos]) {
				start := pos
				if lang == LangGo {
					pos = scanGoIdentEnd(line, pos+1)
					word := line[start:pos]
					if word == "import" && !goImportBlock {
						goImportAwait = true
					} else if word == "func" || word == "type" || word == "var" || word == "const" {
						goImportAwait = false
						goImportBlock = false
					}
					if shouldKeep {
						tokClass := classifyWord(word, lang)
						if tokClass != TokenNone {
							spans = append(spans, TokenSpan{Class: tokClass, Start: start, End: pos})
						}
					}
				} else {
					for pos < n && isIdentPart(line[pos]) {
						pos++
					}
					if shouldKeep {
						word := line[start:pos]
						tokClass := classifyWord(word, lang)
						if tokClass != TokenNone {
							spans = append(spans, TokenSpan{Class: tokClass, Start: start, End: pos})
						}
					}
				}
				continue
			}

			// Non-ASCII Unicode Go identifiers
			if lang == LangGo && line[pos] >= 0x80 {
				r, width := utf8.DecodeRuneInString(line[pos:])
				if unicode.IsLetter(r) || r == '_' {
					start := pos
					pos = scanGoIdentEnd(line, pos+width)
					addSpan(TokenIdent, start, pos)
					continue
				}
				pos += width
				continue
			}

			if lang == LangGo {
				if line[pos] == '(' && goImportAwait {
					goImportBlock = true
					goImportAwait = false
				} else if line[pos] == ')' && goImportBlock {
					goImportBlock = false
				} else if line[pos] == ';' && goImportAwait && !goImportBlock {
					goImportAwait = false
				}
			}

			pos++
		}
		result[idx] = spans
	}

	return result
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

func isAlphaNumeric(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}

func isIdentStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' || ch == '$'
}

func isIdentPart(ch byte) bool {
	return isIdentStart(ch) || isDigit(ch)
}

func scanGoIdentEnd(line string, i int) int {
	n := len(line)
	for i < n {
		if isIdentPart(line[i]) {
			i++
		} else if line[i] >= 0x80 {
			r, width := utf8.DecodeRuneInString(line[i:])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				i += width
			} else {
				break
			}
		} else {
			break
		}
	}
	return i
}

func classifyWord(word string, lang Lang) TokenClass {
	switch lang {
	case LangGo:
		switch word {
		case "break", "case", "chan", "const", "continue", "default", "defer", "else",
			"fallthrough", "for", "func", "go", "goto", "if", "import", "interface",
			"map", "package", "range", "return", "select", "struct", "switch", "type", "var":
			return TokenKeyword
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64",
			"uintptr", "float32", "float64", "complex64", "complex128", "string", "bool", "byte",
			"rune", "error", "any", "nil", "true", "false", "iota":
			return TokenType
		default:
			return TokenIdent
		}
	case LangJS:
		switch word {
		case "const", "let", "var", "function", "class", "return", "if", "else", "for",
			"while", "do", "switch", "case", "default", "break", "continue", "try", "catch",
			"finally", "throw", "new", "this", "typeof", "instanceof", "import", "export",
			"from", "as", "async", "await", "yield", "super", "extends", "interface", "type",
			"enum", "implements", "public", "private", "protected", "readonly", "static":
			return TokenKeyword
		case "number", "string", "boolean", "any", "void", "null", "undefined", "true", "false",
			"never", "unknown":
			return TokenType
		}
	case LangPython:
		switch word {
		case "def", "class", "if", "elif", "else", "while", "for", "in", "try", "except",
			"finally", "with", "as", "import", "from", "return", "yield", "raise", "pass",
			"break", "continue", "lambda", "global", "nonlocal", "assert", "async", "await",
			"and", "or", "not", "is":
			return TokenKeyword
		case "int", "float", "str", "bool", "list", "dict", "set", "tuple", "bytes", "None",
			"True", "False":
			return TokenType
		}
	case LangShell:
		switch word {
		case "if", "then", "else", "elif", "fi", "case", "esac", "for", "while", "until",
			"do", "done", "in", "function", "select", "time", "return", "exit", "export", "local":
			return TokenKeyword
		case "true", "false":
			return TokenType
		}
	case LangSQL:
		u := strings.ToUpper(word)
		switch u {
		case "SELECT", "FROM", "WHERE", "INSERT", "INTO", "UPDATE", "DELETE", "JOIN",
			"LEFT", "RIGHT", "INNER", "OUTER", "ON", "GROUP", "BY", "ORDER", "HAVING",
			"LIMIT", "OFFSET", "CREATE", "TABLE", "INDEX", "DROP", "ALTER", "ADD", "PRIMARY",
			"KEY", "FOREIGN", "REFERENCES", "DEFAULT", "NOT", "NULL", "AND", "OR", "IN",
			"EXISTS", "BETWEEN", "LIKE", "UNION", "ALL", "VIEW", "CASCADE", "PRAGMA", "AS",
			"SET", "VALUES", "CASE", "WHEN", "THEN", "END", "ELSE":
			return TokenKeyword
		case "INTEGER", "TEXT", "BLOB", "REAL", "NUMERIC", "BOOLEAN", "DATETIME", "VARCHAR":
			return TokenType
		}
	case LangC, LangRust:
		switch word {
		case "fn", "let", "mut", "struct", "enum", "impl", "trait", "pub", "use", "mod",
			"match", "ref", "move", "int", "char", "float", "double", "void", "auto",
			"if", "else", "for", "while", "return", "switch", "case", "default", "break",
			"continue", "class", "public", "private", "protected", "virtual", "override",
			"template", "typename", "namespace", "const", "static", "constexpr", "nullptr",
			"sizeof", "typedef":
			return TokenKeyword
		case "bool", "true", "false", "uint8_t", "uint16_t", "uint32_t", "uint64_t",
			"int8_t", "int16_t", "int32_t", "int64_t", "size_t", "usize", "isize",
			"u8", "u16", "u32", "u64", "i8", "i16", "i32", "i64", "f32", "f64":
			return TokenType
		}
	}
	return TokenNone
}

// RenderTokensHTML renders line text combining tokens, intraline spans, and trailing whitespace
// into safe HTML-escaped markup without splitting UTF-8 characters or mis-nesting tags.
func RenderTokensHTML(line string, tokens []TokenSpan, intra []Span) template.HTML {
	return RenderTokensHTMLWS(line, tokens, intra, true)
}

// writeHTMLEscaped appends HTML-escaped s to sb matching html.EscapeString without allocating.
func writeHTMLEscaped(sb *strings.Builder, s string) {
	last := 0
	for i := 0; i < len(s); i++ {
		var esc string
		switch s[i] {
		case '&':
			esc = "&amp;"
		case '<':
			esc = "&lt;"
		case '>':
			esc = "&gt;"
		case '"':
			esc = "&#34;"
		case '\'':
			esc = "&#39;"
		default:
			continue
		}
		if i > last {
			sb.WriteString(s[last:i])
		}
		sb.WriteString(esc)
		last = i + 1
	}
	if last < len(s) {
		sb.WriteString(s[last:])
	}
}

// RenderTokensHTMLWS renders line text, optionally highlighting trailing whitespace.
func RenderTokensHTMLWS(line string, tokens []TokenSpan, intra []Span, showWS bool) template.HTML {
	n := len(line)
	if n == 0 {
		return ""
	}

	// Detect trailing whitespace
	wsStart := n
	if showWS {
		for wsStart > 0 && (line[wsStart-1] == ' ' || line[wsStart-1] == '\t') {
			wsStart--
		}
	}

	var sb strings.Builder
	sb.Grow(n + len(tokens)*28 + len(intra)*28)

	ti := 0
	ii := 0
	pos := 0
	openTokIdx := -1

	for pos < n {
		for ti < len(tokens) && (tokens[ti].End <= pos || tokens[ti].Start >= tokens[ti].End || tokens[ti].Class == TokenNone || tokens[ti].Start >= n) {
			ti++
		}
		for ii < len(intra) && (intra[ii].End <= pos || intra[ii].Start >= intra[ii].End || intra[ii].Start >= n) {
			ii++
		}

		next := n
		if showWS && pos < wsStart && wsStart < next {
			next = wsStart
		}

		tokIdx := -1
		if ti < len(tokens) {
			tStart := max(tokens[ti].Start, 0)
			tEnd := min(tokens[ti].End, n)
			if tStart <= pos {
				tokIdx = ti
				if tEnd < next {
					next = tEnd
				}
			} else if tStart < next {
				next = tStart
			}
		}

		isIntra := false
		if ii < len(intra) {
			iStart := max(intra[ii].Start, 0)
			iEnd := min(intra[ii].End, n)
			if iStart <= pos {
				isIntra = true
				if iEnd < next {
					next = iEnd
				}
			} else if iStart < next {
				next = iStart
			}
		}

		if next <= pos {
			next = pos + 1
		}

		if tokIdx != openTokIdx {
			if openTokIdx >= 0 {
				sb.WriteString("</span>")
			}
			if tokIdx >= 0 {
				sb.WriteString(`<span class="`)
				sb.WriteString(string(tokens[tokIdx].Class))
				sb.WriteString(`">`)
			}
			openTokIdx = tokIdx
		}

		isWS := pos >= wsStart
		if isIntra {
			sb.WriteString(`<mark class="intra">`)
		}
		if isWS {
			sb.WriteString(`<span class="ws">`)
		}
		writeHTMLEscaped(&sb, line[pos:next])
		if isWS {
			sb.WriteString("</span>")
		}
		if isIntra {
			sb.WriteString("</mark>")
		}
		pos = next
	}

	if openTokIdx >= 0 {
		sb.WriteString("</span>")
	}

	return template.HTML(sb.String())
}

// RenderLineHTML produces safe HTML-escaped markup for a single line.
func RenderLineHTML(line string, lang Lang, intra []Span) template.HTML {
	if lang == LangNone {
		return RenderTokensHTML(line, nil, intra)
	}
	tokens := HighlightLines([]string{line}, lang)[0]
	return RenderTokensHTML(line, tokens, intra)
}

const maxGoDocLinkScan = 257

func addCommentWithDocLinks(addSpan func(TokenClass, int, int), line string, commentStart, commentEnd, bodyStart, bodyEnd int) {
	cur := commentStart
	scanPos := bodyStart
	for scanPos < bodyEnd {
		open := strings.IndexByte(line[scanPos:bodyEnd], '[')
		if open == -1 {
			break
		}
		openBracket := scanPos + open
		searchEnd := min(openBracket+1+maxGoDocLinkScan, bodyEnd)
		close := strings.IndexByte(line[openBracket+1:searchEnd], ']')
		if close == -1 {
			scanPos = openBracket + 1
			continue
		}
		closeBracket := openBracket + 1 + close
		inner := line[openBracket+1 : closeBracket]

		if IsValidGoDocLink(line, openBracket, closeBracket, bodyStart, bodyEnd, inner) {
			if openBracket+1 > cur {
				addSpan(TokenComment, cur, openBracket+1)
			}
			addSpan(TokenDocLink, openBracket+1, closeBracket)
			cur = closeBracket
			scanPos = closeBracket + 1
		} else {
			scanPos = openBracket + 1
		}
	}
	if cur < commentEnd {
		addSpan(TokenComment, cur, commentEnd)
	}
}

// ExtractGoDocLinkInRange returns the Go doc-comment symbol link inner text at byteIdx
// within the comment body [bodyStart, bodyEnd) of line.
func ExtractGoDocLinkInRange(line string, bodyStart, bodyEnd, byteIdx int) (string, bool) {
	if bodyStart < 0 {
		bodyStart = 0
	}
	if bodyEnd > len(line) {
		bodyEnd = len(line)
	}
	if byteIdx < bodyStart || byteIdx >= bodyEnd {
		return "", false
	}
	scanPos := bodyStart
	for scanPos < bodyEnd {
		open := strings.IndexByte(line[scanPos:bodyEnd], '[')
		if open == -1 {
			break
		}
		openBracket := scanPos + open
		searchEnd := min(openBracket+1+maxGoDocLinkScan, bodyEnd)
		close := strings.IndexByte(line[openBracket+1:searchEnd], ']')
		if close == -1 {
			scanPos = openBracket + 1
			continue
		}
		closeBracket := openBracket + 1 + close
		inner := line[openBracket+1 : closeBracket]

		if IsValidGoDocLink(line, openBracket, closeBracket, bodyStart, bodyEnd, inner) {
			if byteIdx >= openBracket && byteIdx <= closeBracket {
				return inner, true
			}
			scanPos = closeBracket + 1
		} else {
			scanPos = openBracket + 1
		}
	}
	return "", false
}

// IsValidGoDocLink reports whether line[openBracket+1:closeBracket] (inner) within
// comment body [bodyStart, bodyEnd) forms a valid Go doc-comment symbol link.
func IsValidGoDocLink(line string, openBracket, closeBracket, bodyStart, bodyEnd int, inner string) bool {
	if !IsValidGoDocLinkTarget(inner) {
		return false
	}
	if bodyStart < 0 {
		bodyStart = 0
	}
	if bodyEnd > len(line) {
		bodyEnd = len(line)
	}
	if openBracket < bodyStart || closeBracket >= bodyEnd || openBracket >= closeBracket {
		return false
	}
	before := line[bodyStart:openBracket]
	after := line[closeBracket+1 : bodyEnd]

	// Boundary before [
	if before != "" {
		r, _ := utf8.DecodeLastRuneInString(before)
		if r != ' ' && r != '\t' && !unicode.IsPunct(r) {
			return false
		}
		if r == ']' || r == ')' || r == '_' || r == '\\' || r == '`' {
			return false
		}
	}

	// Boundary after ]
	if after != "" {
		r, _ := utf8.DecodeRuneInString(after)
		if r != ' ' && r != '\t' && !unicode.IsPunct(r) {
			return false
		}
		if r == '(' || r == '[' || r == '_' || r == '`' {
			return false
		}
		if isDocLinkDefAfter(before, after) {
			return false
		}
	}

	return true
}

func isDocLinkDefAfter(before, after string) bool {
	if strings.TrimSpace(before) != "" {
		return false
	}
	if len(after) < 3 || after[0] != ':' || (after[1] != ' ' && after[1] != '\t') {
		return false
	}
	url := strings.TrimSpace(after[2:])
	j := strings.Index(url, "://")
	if j < 0 {
		return false
	}
	return isDocLinkScheme(url[:j])
}

func isDocLinkScheme(s string) bool {
	switch s {
	case "file", "ftp", "gopher", "http", "https", "mailto", "nntp":
		return true
	}
	return false
}

var commonDocExts = map[string]bool{
	"go": true, "md": true, "txt": true, "json": true, "yaml": true,
	"yml": true, "toml": true, "html": true, "css": true, "js": true,
	"ts": true, "py": true, "sh": true, "sql": true, "c": true,
	"h": true, "cc": true, "cpp": true, "rs": true, "proto": true,
	"sum": true, "mod": true,
}

func isAllLower(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsLower(r) {
				return false
			}
		}
	}
	return hasLetter
}

func isAllUpper(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return false
			}
		}
	}
	return hasLetter
}

func isGoIdent(s string) bool {
	if s == "" || isGoKeyword(s) {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
		} else {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				return false
			}
		}
	}
	return true
}

func isExportedGoIdent(s string) bool {
	if !isGoIdent(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

func splitExportedDocName(text string) (before, name string, foundDot bool) {
	i := strings.LastIndexByte(text, '.')
	if i < 0 {
		return text, "", false
	}
	name = text[i+1:]
	if !isExportedGoIdent(name) {
		return text, "", false
	}
	return text[:i], name, true
}

func validImportPath(path string) bool {
	if !utf8.ValidString(path) || path == "" || path[0] == '-' {
		return false
	}
	if strings.Contains(path, "//") || path[len(path)-1] == '/' {
		return false
	}
	elemStart := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			if !validImportPathElem(path[elemStart:i]) {
				return false
			}
			elemStart = i + 1
		}
	}
	return validImportPathElem(path[elemStart:])
}

func validImportPathElem(elem string) bool {
	if elem == "" || elem[0] == '.' || elem[len(elem)-1] == '.' {
		return false
	}
	for i := 0; i < len(elem); i++ {
		if !importPathOK(elem[i]) {
			return false
		}
	}
	return true
}

func importPathOK(c byte) bool {
	return (c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '~' || c == '_' || c == '+'
}

// ParseGoDocSlashTarget splits a slash-containing Go doc-link target (with any
// leading '*' already stripped) into package import path, optional receiver type,
// and optional exported symbol/member name, matching go/doc/comment rules.
func ParseGoDocSlashTarget(s string) (impPath, recv, name string, ok bool) {
	if !strings.Contains(s, "/") {
		return "", "", "", false
	}
	pkg := s
	if before, sym, found := splitExportedDocName(pkg); found {
		pkg = before
		name = sym
		if before2, r, found2 := splitExportedDocName(pkg); found2 {
			pkg = before2
			recv = r
		}
	}
	if !strings.Contains(pkg, "/") || !validImportPath(pkg) {
		return "", "", "", false
	}
	return pkg, recv, name, true
}

func isGoKeyword(word string) bool {
	switch word {
	case "break", "case", "chan", "const", "continue", "default", "defer", "else",
		"fallthrough", "for", "func", "go", "goto", "if", "import", "interface",
		"map", "package", "range", "return", "select", "struct", "switch", "type", "var":
		return true
	}
	return false
}

// IsValidGoDocLinkTarget reports whether inner matches Go doc-comment symbol link syntax.
func IsValidGoDocLinkTarget(inner string) bool {
	if len(inner) < 1 || len(inner) > 256 {
		return false
	}
	if strings.ContainsAny(inner, " \t\r\n[]") {
		return false
	}
	hadLeadingStar := strings.HasPrefix(inner, "*")
	s := strings.TrimPrefix(inner, "*")
	if s == "" {
		return false
	}

	if strings.Contains(s, "/") {
		_, _, name, ok := ParseGoDocSlashTarget(s)
		if !ok {
			return false
		}
		if hadLeadingStar && name == "" {
			return false
		}
		return true
	}

	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return false
	}
	if len(parts) == 1 {
		name := parts[0]
		if !isGoIdent(name) {
			return false
		}
		switch name {
		case "TODO", "FIXME", "NOTE", "WIP", "HACK", "XXX", "BUG":
			return false
		}
		return true
	}
	if len(parts) == 2 {
		if !isGoIdent(parts[0]) || !isGoIdent(parts[1]) {
			return false
		}
		if !hadLeadingStar {
			if commonDocExts[strings.ToLower(parts[1])] && (isAllLower(parts[0]) || isAllUpper(parts[0])) {
				return false
			}
		}
		return true
	}
	// len(parts) == 3
	return isGoIdent(parts[0]) && isGoIdent(parts[1]) && isGoIdent(parts[2])
}
