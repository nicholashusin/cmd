package engine

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"regexp/syntax"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

// SearchTerm is a compiled positive or negated matcher from a Code Search query.
type SearchTerm struct {
	Raw           string
	Negated       bool
	Literal       bool
	Re            *regexp.Regexp
	caseSensitive bool
	matchesEmpty  bool
	hasAnchor     bool // true when matchesEmpty regex contains ^, \A, \b, or \B
	exactLit      string
	exactLitBytes []byte
	lowerLitStr   string
	lowerLitASCII []byte
	hasKS         bool
	reqLitBytes   []byte // guaranteed ASCII literal inside a regex (case-sensitive only)
	reqLowerASCII []byte // guaranteed ASCII literal inside a regex (case-insensitive or inline (?i))
	reqHasKS      bool
	multiRe       *regexp.Regexp // (?m) regex for blob-level precheck of ^/$ patterns
	hasDollar     bool
	wantQualPos   bool
	wantQualNeg   bool
}

func (t SearchTerm) MatchString(s string) bool {
	if t.Re == nil {
		return false
	}
	if t.exactLit != "" {
		if t.caseSensitive {
			return strings.Contains(s, t.exactLit)
		}
		if t.lowerLitStr != "" {
			if indexFoldASCIIString(s, t.lowerLitStr) >= 0 {
				return true
			}
			if !t.hasKS || (strings.IndexByte(s, 0xC5) < 0 && strings.IndexByte(s, 0xE2) < 0) {
				return false
			}
		}
	}
	if t.matchesEmpty {
		return hasNonEmptyMatchString(t.Re, s, t.hasAnchor)
	}
	return t.Re.MatchString(s)
}

func (t SearchTerm) matchBytes(b []byte) bool {
	if t.Re == nil {
		return false
	}
	if len(t.exactLitBytes) > 0 {
		if t.caseSensitive {
			return bytes.Contains(b, t.exactLitBytes)
		}
		if len(t.lowerLitASCII) > 0 {
			if indexFoldASCIIBytes(b, t.lowerLitASCII) >= 0 {
				return true
			}
			if !t.hasKS || (bytes.IndexByte(b, 0xC5) < 0 && bytes.IndexByte(b, 0xE2) < 0) {
				return false
			}
		}
	}
	if len(t.reqLitBytes) > 0 {
		if !bytes.Contains(b, t.reqLitBytes) {
			return false
		}
	} else if len(t.reqLowerASCII) > 0 {
		if indexFoldASCIIBytes(b, t.reqLowerASCII) < 0 {
			if !t.reqHasKS || (bytes.IndexByte(b, 0xC5) < 0 && bytes.IndexByte(b, 0xE2) < 0) {
				return false
			}
		}
	}
	if t.matchesEmpty {
		return hasNonEmptyMatchBytes(t.Re, b, t.hasAnchor)
	}
	return t.Re.Match(b)
}

// matchBlobPrecheck quickly rejects blobs that cannot contain a matching line.
func (t SearchTerm) matchBlobPrecheck(data []byte) bool {
	if t.Re == nil || t.Negated || t.matchesEmpty {
		return true
	}
	if len(t.exactLitBytes) > 0 {
		return t.matchBytes(data)
	}
	if len(t.reqLitBytes) > 0 {
		if !bytes.Contains(data, t.reqLitBytes) {
			return false
		}
	} else if len(t.reqLowerASCII) > 0 {
		if indexFoldASCIIBytes(data, t.reqLowerASCII) < 0 {
			if !t.reqHasKS || (bytes.IndexByte(data, 0xC5) < 0 && bytes.IndexByte(data, 0xE2) < 0) {
				return false
			}
		}
	}
	if t.multiRe != nil {
		// Under (?m), '$' does not match before '\r\n', whereas line scanning strips '\r'.
		if t.hasDollar && bytes.IndexByte(data, '\r') >= 0 {
			return true
		}
		return t.multiRe.Match(data)
	}
	return true
}

// SymbolFilter matches Go declarations by optional kind ("func", "method", "type", "struct",
// "interface", "field", "const", "var", or "" for any).
type SymbolFilter struct {
	Kind string
	Term SearchTerm
}

// SearchQuery is the parsed representation of a Code Search query string.
type SearchQuery struct {
	Raw              string
	CaseSensitive    bool
	Scope            string // "", "change", "repo", "comments"
	Terms            []SearchTerm
	FileFilters      []SearchTerm
	SymbolFilters    []SymbolFilter
	PkgFilters       []SearchTerm
	LangFilters      []SearchTerm
	AuthorFilters    []SearchTerm
	WantUnresolved   bool
	WantResolved     bool
	WantDraft        bool
	WantChanged      bool
	HasContentFilter bool
	TargetLine       int
}

type rawQueryToken struct {
	prefix  string // lowercase filter prefix without ':', or ""
	value   string
	negated bool
	literal bool
	isRegex bool
}

func isQuerySpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func tokenizeSearchQuery(raw string) []rawQueryToken {
	var tokens []rawQueryToken
	n := len(raw)
	i := 0

	skipSpace := func() {
		for i < n && isQuerySpace(raw[i]) {
			i++
		}
	}

	readQuoted := func() string {
		i++ // skip opening '"'
		var b strings.Builder
		for i < n {
			c := raw[i]
			if c == '\\' && i+1 < n {
				next := raw[i+1]
				if next == '"' || next == '\\' {
					b.WriteByte(next)
					i += 2
					continue
				}
			}
			if c == '"' {
				i++
				break
			}
			b.WriteByte(c)
			i++
		}
		return b.String()
	}

	// tryReadRegex parses a /regex/ literal only if a non-empty matching unescaped '/'
	// closes the token at whitespace or EOF. Inside /.../, only \/ is unescaped so
	// regex escapes like \\ or \d stay intact. Paths like /api/search or // fall back to words.
	tryReadRegex := func() (string, bool) {
		if i >= n || raw[i] != '/' {
			return "", false
		}
		var b strings.Builder
		j := i + 1
		for j < n {
			c := raw[j]
			if c == '\\' && j+1 < n {
				next := raw[j+1]
				if next == '/' {
					b.WriteByte('/')
				} else {
					b.WriteByte('\\')
					b.WriteByte(next)
				}
				j += 2
				continue
			}
			if c == '/' {
				if j > i+1 && (j+1 == n || isQuerySpace(raw[j+1])) {
					i = j + 1
					return b.String(), true
				}
				return "", false
			}
			b.WriteByte(c)
			j++
		}
		return "", false
	}

	readWord := func() string {
		start := i
		for i < n && !isQuerySpace(raw[i]) {
			i++
		}
		return raw[start:i]
	}

	for {
		skipSpace()
		if i >= n {
			break
		}

		neg := false
		if raw[i] == '-' && i+1 < n && !isQuerySpace(raw[i+1]) {
			neg = true
			i++
		}

		if raw[i] == '"' {
			val := readQuoted()
			if val != "" {
				tokens = append(tokens, rawQueryToken{value: val, negated: neg, literal: true})
			}
			continue
		}
		if raw[i] == '/' {
			if val, ok := tryReadRegex(); ok {
				tokens = append(tokens, rawQueryToken{value: val, negated: neg, isRegex: true})
				continue
			}
		}

		// Scan up to whitespace or ':' to see if this is a recognized filter prefix.
		start := i
		colon := -1
		for j := i; j < n && !isQuerySpace(raw[j]); j++ {
			if raw[j] == ':' {
				colon = j
				break
			}
		}

		if colon > start {
			prefix := strings.ToLower(raw[start:colon])
			if isKnownSearchFilter(prefix) {
				i = colon + 1
				var val string
				var literal, isRegex bool
				if i < n && raw[i] == '"' {
					val = readQuoted()
					literal = true
				} else if i < n && raw[i] == '/' {
					if rv, ok := tryReadRegex(); ok {
						val = rv
						isRegex = true
					} else {
						val = readWord()
					}
				} else {
					val = readWord()
				}
				if val != "" {
					tokens = append(tokens, rawQueryToken{
						prefix:  prefix,
						value:   val,
						negated: neg,
						literal: literal,
						isRegex: isRegex,
					})
				}
				continue
			}
		}

		word := readWord()
		if word != "" {
			tokens = append(tokens, rawQueryToken{value: word, negated: neg})
		}
	}

	return tokens
}

func isKnownSearchFilter(prefix string) bool {
	switch prefix {
	case "f", "file", "path",
		"s", "sym", "symbol", "func", "function", "method", "type", "struct", "interface", "field", "const", "var",
		"p", "pkg",
		"l", "lang",
		"in", "scope",
		"author",
		"is",
		"case",
		"c", "content":
		return true
	}
	return false
}

func canonicalORGroup(prefix string) string {
	switch prefix {
	case "", "c", "content":
		return "term"
	case "f", "file", "path":
		return "f"
	case "s", "sym", "symbol":
		return "s"
	case "func", "function":
		return "func"
	case "method", "type", "struct", "interface", "field", "const", "var":
		return prefix
	case "p", "pkg":
		return "p"
	case "l", "lang":
		return "l"
	case "author":
		return "author"
	default:
		return ""
	}
}

func tokenToRegexFragment(tok rawQueryToken) string {
	if tok.literal {
		return regexp.QuoteMeta(tok.value)
	}
	if _, err := regexp.Compile(tok.value); err != nil {
		return regexp.QuoteMeta(tok.value)
	}
	return tok.value
}

func isBareOR(tok rawQueryToken) bool {
	return tok.prefix == "" && !tok.negated && !tok.literal && !tok.isRegex && tok.value == "OR"
}

// foldORTokens merges adjacent compatible positive tokens separated by bare uppercase OR
// into a single regex disjunction "(?:a)|(?:b)".
func foldORTokens(tokens []rawQueryToken) []rawQueryToken {
	if len(tokens) <= 1 {
		return tokens
	}
	out := make([]rawQueryToken, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if isBareOR(tok) {
			if len(out) == 0 {
				// Leading OR: treat as ordinary word.
				out = append(out, tok)
				continue
			}
			if i+1 >= len(tokens) {
				// Trailing OR while user is mid-typing: ignore so left term stays live.
				continue
			}
			prev := out[len(out)-1]
			next := tokens[i+1]
			if isBareOR(next) {
				// Collapse consecutive bare ORs (e.g. "alpha OR OR beta" or "alpha OR OR").
				continue
			}
			pg := canonicalORGroup(prev.prefix)
			if !prev.negated && !next.negated && pg != "" && pg == canonicalORGroup(next.prefix) {
				combinedPrefix := prev.prefix
				if combinedPrefix == "" && (next.prefix == "c" || next.prefix == "content") {
					combinedPrefix = next.prefix
				}
				out[len(out)-1] = rawQueryToken{
					prefix:  combinedPrefix,
					value:   "(?:" + tokenToRegexFragment(prev) + ")|(?:" + tokenToRegexFragment(next) + ")",
					isRegex: true,
				}
				i++ // consumed next
				continue
			}
			// Incompatible operands: keep OR as an ordinary word.
			out = append(out, tok)
			continue
		}
		out = append(out, tok)
	}
	return out
}

// hasSignificantUppercase reports whether s has an ASCII uppercase letter outside
// regex escape sequences like \S, \W, \D, \B, \A, \Z.
func hasSignificantUppercase(s string, literal bool) bool {
	for i := 0; i < len(s); i++ {
		if !literal && s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if 'A' <= s[i] && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// splitFileLineSuffix splits "path/file.go:120" or "path/file.go:120:5" into ("path/file.go", 120, true).
func splitFileLineSuffix(raw string, requirePathSep bool) (string, int, bool) {
	c1 := strings.LastIndexByte(raw, ':')
	if c1 <= 0 || c1 == len(raw)-1 {
		return "", 0, false
	}
	n1, err := strconv.Atoi(raw[c1+1:])
	if err != nil || n1 <= 0 {
		return "", 0, false
	}
	filePart := raw[:c1]
	line := n1
	if c2 := strings.LastIndexByte(filePart, ':'); c2 > 0 && c2 < len(filePart)-1 {
		if n2, err2 := strconv.Atoi(filePart[c2+1:]); err2 == nil && n2 > 0 {
			filePart = filePart[:c2]
			line = n2
		}
	}
	hasLetter := false
	hasSep := false
	for i := 0; i < len(filePart); i++ {
		c := filePart[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			hasLetter = true
		} else if c == '.' || c == '/' {
			hasSep = true
		}
	}
	if !hasLetter || (requirePathSep && !hasSep) {
		return "", 0, false
	}
	return filePart, line, true
}

func hasQualifiedSep(raw string) bool {
	if strings.Contains(raw, "/") || strings.Contains(raw, `\.`) {
		return true
	}
	isIdent := func(c byte) bool {
		return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') || c == '_'
	}
	for i := 1; i+1 < len(raw); i++ {
		if raw[i] == '.' && isIdent(raw[i-1]) && isIdent(raw[i+1]) {
			return true
		}
	}
	return false
}

func compileSearchTerm(raw string, negated, literal, caseSensitive bool) SearchTerm {
	prefix := ""
	if !caseSensitive {
		prefix = "(?i)"
	}
	pattern := raw
	if literal {
		pattern = regexp.QuoteMeta(raw)
	}
	fullPattern := prefix + pattern
	stripped := false
	if !literal {
		if s, ok := stripEmptyAltBranches(fullPattern); ok {
			fullPattern = s
			stripped = true
		}
	}
	re, err := regexp.Compile(fullPattern)
	if err != nil {
		// Graceful fallback to literal matching while the user is mid-typing a regex.
		pattern = regexp.QuoteMeta(raw)
		fullPattern = prefix + pattern
		re, _ = regexp.Compile(fullPattern)
		literal = true
		stripped = false
	}

	t := SearchTerm{
		Raw:           raw,
		Negated:       negated,
		Literal:       literal,
		Re:            re,
		caseSensitive: caseSensitive,
		matchesEmpty:  re != nil && re.MatchString(""),
	}
	if t.matchesEmpty && re != nil {
		if rp, err := syntax.Parse(re.String(), syntax.Perl); err == nil {
			t.hasAnchor = hasContextAnchor(rp)
		}
	}

	if literal || (!stripped && regexp.QuoteMeta(raw) == raw) {
		t.exactLit = raw
		t.exactLitBytes = []byte(raw)
		if !caseSensitive && isPureASCII(raw) {
			t.lowerLitStr = strings.ToLower(raw)
			t.lowerLitASCII = []byte(t.lowerLitStr)
			t.hasKS = hasASCIIKS(t.lowerLitASCII)
		}
	} else if !negated && !t.matchesEmpty && re != nil {
		if req, ok := extractRequiredLitForTerm(re.String()); ok {
			if req.foldCase {
				t.reqLowerASCII = []byte(strings.ToLower(req.lit))
				t.reqHasKS = hasASCIIKS(t.reqLowerASCII)
			} else {
				t.reqLitBytes = []byte(req.lit)
			}
		}
		if strings.ContainsAny(fullPattern, "^$") {
			mPat := "(?m)" + fullPattern
			if mp, err := syntax.Parse(mPat, syntax.Perl); err == nil && !hasTextAnchor(mp) {
				if mre, err := regexp.Compile(mPat); err == nil {
					t.multiRe = mre
					t.hasDollar = hasEndLine(mp)
				}
			}
		} else if rp, err := syntax.Parse(re.String(), syntax.Perl); err == nil && !hasTextAnchor(rp) {
			t.multiRe = re
		}
	}

	// Only check package/import/dir/module qualified symbol forms when the term can
	// plausibly reference a '.' or '/'. For negated terms, require an explicit separator
	// (including unescaped '.' between identifier chars like "-s:http.Server", but not
	// wildcard ".*") so "-s:myrepo" does not exclude every symbol via the module path.
	if literal {
		hasSep := strings.ContainsAny(raw, "./")
		t.wantQualPos = hasSep
		t.wantQualNeg = hasSep
	} else {
		t.wantQualPos = strings.ContainsAny(raw, "./[\\")
		t.wantQualNeg = hasQualifiedSep(raw)
	}
	return t
}

func compileLangTerm(raw string, negated, literal bool) SearchTerm {
	pattern := raw
	if literal {
		pattern = regexp.QuoteMeta(raw)
	}
	re, err := regexp.Compile("(?i)^(?:" + pattern + ")$")
	if err != nil {
		re, _ = regexp.Compile("(?i)^(?:" + regexp.QuoteMeta(raw) + ")$")
		literal = true
	}
	return SearchTerm{
		Raw:     raw,
		Negated: negated,
		Literal: literal,
		Re:      re,
	}
}

// ParseSearchQuery parses Code Search style queries into structured filters and terms.
func ParseSearchQuery(raw string) SearchQuery {
	q := SearchQuery{Raw: raw}
	tokens := foldORTokens(tokenizeSearchQuery(raw))

	// Pass 1: resolve global modifiers (case:yes, case:no, case:auto) regardless of token position.
	caseMode := ""
	for _, tok := range tokens {
		if tok.prefix == "case" {
			switch strings.ToLower(tok.value) {
			case "yes", "true", "1", "sensitive":
				if tok.negated {
					caseMode = "no"
				} else {
					caseMode = "yes"
				}
			case "no", "false", "0", "insensitive":
				if tok.negated {
					caseMode = "yes"
				} else {
					caseMode = "no"
				}
			case "auto", "smart":
				if !tok.negated {
					caseMode = "auto"
				}
			}
		}
	}
	switch caseMode {
	case "yes":
		q.CaseSensitive = true
	case "no":
		q.CaseSensitive = false
	case "auto":
		for _, tok := range tokens {
			if tok.negated {
				continue
			}
			switch tok.prefix {
			case "case", "in", "scope", "is", "l", "lang":
				continue
			}
			if hasSignificantUppercase(tok.value, tok.literal) {
				q.CaseSensitive = true
				break
			}
		}
	}

	// Pass 2: compile terms and filters.
	for _, tok := range tokens {
		switch tok.prefix {
		case "case":
			continue
		case "f", "file", "path":
			val := tok.value
			if !tok.negated && !tok.isRegex {
				if fp, line, ok := splitFileLineSuffix(val, false); ok {
					val = fp
					if q.TargetLine == 0 {
						q.TargetLine = line
					}
				}
			}
			q.FileFilters = append(q.FileFilters, compileSearchTerm(val, tok.negated, tok.literal, q.CaseSensitive))
		case "s", "sym", "symbol":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "func", "function":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "func",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "method":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "method",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "type":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "type",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "struct":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "struct",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "interface":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "interface",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "field":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "field",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "const":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "const",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "var":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "var",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "p", "pkg":
			q.PkgFilters = append(q.PkgFilters, compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive))
		case "l", "lang":
			q.LangFilters = append(q.LangFilters, compileLangTerm(tok.value, tok.negated, tok.literal))
		case "author":
			q.AuthorFilters = append(q.AuthorFilters, compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive))
		case "in", "scope":
			if tok.negated {
				continue
			}
			switch strings.ToLower(tok.value) {
			case "change", "diff", "cl":
				q.Scope = "change"
			case "repo", "code":
				q.Scope = "repo"
			case "comments", "comment", "threads", "thread":
				q.Scope = "comments"
			case "all":
				q.Scope = ""
			}
		case "is":
			switch strings.ToLower(tok.value) {
			case "unresolved", "open":
				if tok.negated {
					q.WantResolved = true
				} else {
					q.WantUnresolved = true
				}
			case "resolved", "closed":
				if tok.negated {
					q.WantUnresolved = true
				} else {
					q.WantResolved = true
				}
			case "draft", "drafts":
				if !tok.negated {
					q.WantDraft = true
				}
			case "comment", "comments":
				if !tok.negated && q.Scope == "" {
					q.Scope = "comments"
				}
			case "changed", "modified":
				if !tok.negated {
					q.WantChanged = true
				}
			}
		case "c", "content":
			q.HasContentFilter = true
			q.Terms = append(q.Terms, compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive))
		case "":
			q.Terms = append(q.Terms, compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive))
		}
	}

	return q
}

func (q SearchQuery) IsEmpty() bool {
	return len(q.Terms) == 0 &&
		len(q.FileFilters) == 0 &&
		len(q.SymbolFilters) == 0 &&
		len(q.PkgFilters) == 0 &&
		len(q.LangFilters) == 0 &&
		len(q.AuthorFilters) == 0 &&
		!q.WantUnresolved &&
		!q.WantResolved &&
		!q.WantDraft &&
		!q.WantChanged &&
		q.Scope == ""
}

func (q SearchQuery) hasPositiveTerm() bool {
	for _, t := range q.Terms {
		if !t.Negated {
			return true
		}
	}
	return false
}

func (q SearchQuery) hasCommentIntent() bool {
	return q.Scope == "comments" || q.WantUnresolved || q.WantResolved || q.WantDraft || len(q.AuthorFilters) > 0
}

func (q SearchQuery) hasCodeOnlyFilter() bool {
	return q.Scope == "repo" || q.Scope == "change" || q.HasContentFilter || len(q.SymbolFilters) > 0 || len(q.PkgFilters) > 0 || len(q.LangFilters) > 0 || q.WantChanged
}

// SearchMatchSpan marks byte offsets [Start, End) of a match within a line.
type SearchMatchSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// SearchFileMatch is a Quick Open file hit.
type SearchFileMatch struct {
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	InChange bool   `json:"in_change,omitempty"`
}

// SearchSymbolMatch is a Go declaration hit.
type SearchSymbolMatch struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // func, method, struct, interface, type, const, var
	Package   string `json:"package,omitempty"`
	Signature string `json:"signature,omitempty"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Col       int    `json:"col"`
	InChange  bool   `json:"in_change,omitempty"`
}

// SearchCommentHit is a matching review thread/message.
type SearchCommentHit struct {
	ThreadID      int64             `json:"thread_id"`
	MessageID     int64             `json:"message_id"`
	ChangeKey     string            `json:"change_key"`
	Path          string            `json:"path"`
	Side          string            `json:"side"`
	Line          int               `json:"line"`
	EndLine       int               `json:"end_line"`
	SnapshotNum   int               `json:"snapshot_num"`
	Author        string            `json:"author"`
	Resolved      bool              `json:"resolved"`
	Draft         bool              `json:"draft,omitempty"`
	Stale         bool              `json:"stale,omitempty"`
	InCurrentView bool              `json:"in_current_view"`
	Snippet       string            `json:"snippet"`
	Spans         []SearchMatchSpan `json:"-"`
	ActivityTime  time.Time         `json:"-"`
}

// SearchCodeLine is a single matching source line.
type SearchCodeLine struct {
	Line  int               `json:"line"`
	Col   int               `json:"col"`
	Text  string            `json:"text"`
	Spans []SearchMatchSpan `json:"-"`
}

// SearchCodeGroup groups line matches under a file path.
type SearchCodeGroup struct {
	Path     string           `json:"path"`
	InChange bool             `json:"in_change,omitempty"`
	Lines    []SearchCodeLine `json:"lines"`
}

// CodeSearchResult holds grouped search results across files, symbols, comments, and code.
type CodeSearchResult struct {
	Query     string              `json:"query"`
	Files     []SearchFileMatch   `json:"files,omitempty"`
	Symbols   []SearchSymbolMatch `json:"symbols,omitempty"`
	Comments  []SearchCommentHit  `json:"comments,omitempty"`
	Code      []SearchCodeGroup   `json:"code,omitempty"`
	TotalCode int                 `json:"total_code"`
	Truncated bool                `json:"truncated,omitempty"`
}

// CodeSearchOptions configures CodeSearch execution.
type CodeSearchOptions struct {
	DB           *db.DB
	Repo         vcs.Repo
	RepoID       int64
	Change       *vcs.Change
	BaseRev      string
	TargetRev    string
	SearchRev    string // optional revision to search code/files/symbols at (defaults to TargetRev)
	Snapshots    []*db.Snapshot
	IsLiveTarget bool
	Query        string
	MaxFiles     int
	MaxSymbols   int
	MaxComments  int
	MaxCodeLines int
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes] + "…"
}

type indexedGoSym struct {
	Name      string
	ShortName string
	Kind      string
	Signature string
	Line      int
	Col       int
}

type cachedGoFileSyms struct {
	pkgName string
	syms    []indexedGoSym
}

var goSymCache = struct {
	mu   sync.RWMutex
	data map[string]cachedGoFileSyms
}{
	data: make(map[string]cachedGoFileSyms),
}

const (
	maxCachedGoSymFiles = 65536
	maxGoSymFileBytes   = 4 << 20
)

func parseAndCacheGoSyms(oid string, data []byte) cachedGoFileSyms {
	if oid != "" {
		goSymCache.mu.RLock()
		if cached, ok := goSymCache.data[oid]; ok {
			goSymCache.mu.RUnlock()
			return cached
		}
		goSymCache.mu.RUnlock()
	}

	res := extractGoSymsFromBytes(data)
	if oid != "" {
		goSymCache.mu.Lock()
		if len(goSymCache.data) >= maxCachedGoSymFiles {
			goSymCache.data = make(map[string]cachedGoFileSyms)
		}
		goSymCache.data[oid] = res
		goSymCache.mu.Unlock()
	}
	return res
}

func extractGoSymsFromBytes(data []byte) cachedGoFileSyms {
	if len(data) == 0 || len(data) > maxGoSymFileBytes || diff.IsBinary(data) {
		return cachedGoFileSyms{}
	}
	fset := token.NewFileSet()
	parseSrc := data
	stripped := false
	if s, ok := stripGoFuncBodies(data); ok {
		parseSrc = s
		stripped = true
	}
	file, err := parser.ParseFile(fset, "", parseSrc, parser.SkipObjectResolution)
	if stripped && (err != nil || file == nil) {
		fset = token.NewFileSet()
		file, err = parser.ParseFile(fset, "", data, parser.SkipObjectResolution)
	}
	if file == nil {
		return cachedGoFileSyms{}
	}
	_ = err // partial ASTs on syntax errors still yield valid declarations

	var out cachedGoFileSyms
	if file.Name != nil {
		out.pkgName = file.Name.Name
	}

	col0 := func(pos token.Position) int {
		return max(pos.Column-1, 0)
	}

	lineSnippet := func(pos token.Pos) string {
		off := fset.PositionFor(pos, false).Offset
		if off < 0 || off >= len(data) {
			return ""
		}
		lineStart := bytes.LastIndexByte(data[:off], '\n') + 1
		lineEnd := bytes.IndexByte(data[off:], '\n')
		if lineEnd < 0 {
			lineEnd = len(data)
		} else {
			lineEnd += off
		}
		s := strings.TrimSpace(string(data[lineStart:lineEnd]))
		s = strings.TrimSuffix(s, "{")
		s = strings.TrimSpace(s)
		return truncateUTF8(s, 120)
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil || d.Name.Name == "_" {
				continue
			}
			pos := fset.PositionFor(d.Name.Pos(), false)
			sym := indexedGoSym{
				Name:      d.Name.Name,
				ShortName: d.Name.Name,
				Kind:      "func",
				Signature: lineSnippet(d.Pos()),
				Line:      pos.Line,
				Col:       col0(pos),
			}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv := receiverTypeName(d.Recv.List[0].Type)
				if recv != "" {
					sym.Name = recv + "." + d.Name.Name
				}
				sym.Kind = "method"
			}
			out.syms = append(out.syms, sym)

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name == nil || sp.Name.Name == "_" {
						continue
					}
					kind := "type"
					var stType *ast.StructType
					var ifType *ast.InterfaceType
					switch tt := sp.Type.(type) {
					case *ast.StructType:
						kind = "struct"
						stType = tt
					case *ast.InterfaceType:
						kind = "interface"
						ifType = tt
					}
					pos := fset.PositionFor(sp.Name.Pos(), false)
					typeName := sp.Name.Name
					out.syms = append(out.syms, indexedGoSym{
						Name:      typeName,
						ShortName: typeName,
						Kind:      kind,
						Signature: lineSnippet(sp.Pos()),
						Line:      pos.Line,
						Col:       col0(pos),
					})
					if stType != nil && stType.Fields != nil {
						for _, field := range stType.Fields.List {
							if len(field.Names) == 0 {
								if embID := embeddedFieldIdent(field.Type); embID != nil && embID.Name != "_" {
									fp := fset.PositionFor(embID.Pos(), false)
									out.syms = append(out.syms, indexedGoSym{
										Name:      typeName + "." + embID.Name,
										ShortName: embID.Name,
										Kind:      "field",
										Signature: lineSnippet(field.Pos()),
										Line:      fp.Line,
										Col:       col0(fp),
									})
								}
								continue
							}
							for _, id := range field.Names {
								if id == nil || id.Name == "_" {
									continue
								}
								fp := fset.PositionFor(id.Pos(), false)
								out.syms = append(out.syms, indexedGoSym{
									Name:      typeName + "." + id.Name,
									ShortName: id.Name,
									Kind:      "field",
									Signature: lineSnippet(id.Pos()),
									Line:      fp.Line,
									Col:       col0(fp),
								})
							}
						}
					}
					if ifType != nil && ifType.Methods != nil {
						for _, meth := range ifType.Methods.List {
							if _, isFunc := meth.Type.(*ast.FuncType); !isFunc {
								continue
							}
							for _, id := range meth.Names {
								if id == nil || id.Name == "_" {
									continue
								}
								mp := fset.PositionFor(id.Pos(), false)
								out.syms = append(out.syms, indexedGoSym{
									Name:      typeName + "." + id.Name,
									ShortName: id.Name,
									Kind:      "method",
									Signature: lineSnippet(id.Pos()),
									Line:      mp.Line,
									Col:       col0(mp),
								})
							}
						}
					}
				case *ast.ValueSpec:
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for _, id := range sp.Names {
						if id == nil || id.Name == "_" {
							continue
						}
						pos := fset.PositionFor(id.Pos(), false)
						out.syms = append(out.syms, indexedGoSym{
							Name:      id.Name,
							ShortName: id.Name,
							Kind:      kind,
							Signature: lineSnippet(sp.Pos()),
							Line:      pos.Line,
							Col:       col0(pos),
						})
					}
				}
			}
		}
	}

	return out
}

func embeddedFieldIdent(expr ast.Expr) *ast.Ident {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return embeddedFieldIdent(t.X)
	case *ast.Ident:
		return t
	case *ast.SelectorExpr:
		return t.Sel
	case *ast.IndexExpr:
		return embeddedFieldIdent(t.X)
	case *ast.IndexListExpr:
		return embeddedFieldIdent(t.X)
	default:
		return nil
	}
}

func pkgFiltersMatch(filters []SearchTerm, dir, pkgName, modImport string) bool {
	if len(filters) == 0 {
		return true
	}
	importDir := strings.TrimPrefix(dir, "src/")
	var cands [4]string
	n := 0
	if dir != "" && dir != "." {
		cands[n] = dir
		n++
	}
	if importDir != "" && importDir != "." && importDir != dir {
		cands[n] = importDir
		n++
	}
	if pkgName != "" {
		cands[n] = pkgName
		n++
	}
	if modImport != "" {
		cands[n] = modImport
		n++
	}
	if n == 0 {
		cands[0] = dir
		n = 1
	}
	for _, f := range filters {
		matched := false
		for _, c := range cands[:n] {
			if f.MatchString(c) {
				matched = true
				break
			}
		}
		if f.Negated && matched {
			return false
		}
		if !f.Negated && !matched {
			return false
		}
	}
	return true
}

func pathOnlyPkgPreFilters(pkgFilters []SearchTerm) []SearchTerm {
	var out []SearchTerm
	for _, pf := range pkgFilters {
		if !pf.Negated && strings.Contains(pf.Raw, "/") && (pf.Literal || !strings.ContainsAny(pf.Raw, "|(?*{[")) {
			out = append(out, pf)
		}
	}
	return out
}

func moduleImportPath(rootMod, dir string) string {
	if rootMod == "" {
		return ""
	}
	if dir == "" || dir == "." {
		return rootMod
	}
	return rootMod + "/" + dir
}

// symPathPreFilter extracts a guaranteed directory/import-path pre-filter from a positive
// path-qualified symbol query (e.g. "net/http.Server" or "^net/http\.Server$") so cold-cache
// symbol search only streams Go files under matching directories.
func symPathPreFilter(t SearchTerm, caseSensitive bool) (SearchTerm, bool) {
	if t.Negated || !strings.Contains(t.Raw, "/") {
		return SearchTerm{}, false
	}
	if t.Literal {
		slash := strings.LastIndexByte(t.Raw, '/')
		prefix := t.Raw
		if dot := strings.IndexByte(t.Raw[slash+1:], '.'); dot >= 0 {
			prefix = t.Raw[:slash+1+dot]
		}
		if prefix == "" {
			return SearchTerm{}, false
		}
		return compileSearchTerm(prefix, false, true, caseSensitive), true
	}

	// Only pre-filter simple regexes with no alternation, grouping, quantifiers, or escapes other than "\.".
	raw := t.Raw
	s := strings.TrimSuffix(raw, "$")
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '|', '(', '?', '*', '{', '[', '+', '$':
			return SearchTerm{}, false
		case '^':
			if i != 0 {
				return SearchTerm{}, false
			}
		case '\\':
			if i+1 >= len(s) || s[i+1] != '.' {
				return SearchTerm{}, false
			}
			i++
		}
	}
	slash := strings.LastIndexByte(s, '/')
	if slash < 0 {
		return SearchTerm{}, false
	}
	prefix := s
	for i := slash + 1; i < len(s); i++ {
		if s[i] == '.' || s[i] == '\\' {
			prefix = s[:i]
			break
		}
	}
	if prefix == "" || prefix == "^" {
		return SearchTerm{}, false
	}
	return compileSearchTerm(prefix, false, false, caseSensitive), true
}

func readRootModulePath(repo vcs.Repo, rev string) string {
	if repo == nil || rev == "" {
		return ""
	}
	data, err := repo.Content(rev, "go.mod")
	if err != nil || len(data) == 0 {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			mod := strings.TrimSpace(strings.TrimPrefix(line, "module "))
			if idx := strings.Index(mod, "//"); idx >= 0 {
				mod = strings.TrimSpace(mod[:idx])
			}
			return strings.Trim(mod, `"`)
		}
	}
	return ""
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	default:
		return ""
	}
}

func matchAllFilters(filters []SearchTerm, candidate string) bool {
	for _, f := range filters {
		matched := f.MatchString(candidate)
		if f.Negated && matched {
			return false
		}
		if !f.Negated && !matched {
			return false
		}
	}
	return true
}

var (
	langAliasesDockerfile = []string{"dockerfile", "docker"}
	langAliasesMakefile   = []string{"makefile", "make", "mk"}
	extLangAliases        = map[string][]string{
		"go":       {"go", "golang"},
		"ts":       {"ts", "typescript"},
		"tsx":      {"ts", "typescript", "tsx"},
		"mts":      {"ts", "typescript", "mts"},
		"cts":      {"ts", "typescript", "cts"},
		"js":       {"js", "javascript"},
		"jsx":      {"js", "javascript", "jsx"},
		"mjs":      {"js", "javascript", "mjs"},
		"cjs":      {"js", "javascript", "cjs"},
		"c":        {"c"},
		"h":        {"c", "h"},
		"cpp":      {"cpp", "c++", "cc", "cxx"},
		"cc":       {"cpp", "c++", "cc", "cxx"},
		"cxx":      {"cpp", "c++", "cc", "cxx"},
		"c++":      {"cpp", "c++", "cc", "cxx"},
		"hpp":      {"cpp", "c++", "cc", "cxx", "hpp"},
		"hh":       {"cpp", "c++", "cc", "cxx", "hh"},
		"hxx":      {"cpp", "c++", "cc", "cxx", "hxx"},
		"html":     {"html", "tmpl"},
		"htm":      {"html", "tmpl", "htm"},
		"tmpl":     {"html", "tmpl"},
		"md":       {"md", "markdown"},
		"markdown": {"md", "markdown"},
		"proto":    {"proto", "protobuf"},
		"xml":      {"xml"},
		"svg":      {"xml", "svg"},
		"s":        {"asm", "assembly", "s"},
		"asm":      {"asm", "assembly"},
		"txt":      {"txt", "text"},
		"css":      {"css"},
		"json":     {"json"},
		"py":       {"py", "python"},
		"rs":       {"rs", "rust"},
		"sh":       {"sh", "bash", "shell"},
		"bash":     {"sh", "bash", "shell"},
		"zsh":      {"sh", "bash", "shell", "zsh"},
		"yaml":     {"yaml", "yml"},
		"yml":      {"yaml", "yml"},
		"toml":     {"toml"},
		"sql":      {"sql"},
	}
)

func fileLangAliases(filePath string) []string {
	base := strings.ToLower(path.Base(filePath))
	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") || strings.HasSuffix(base, ".dockerfile") {
		return langAliasesDockerfile
	}
	if base == "makefile" || base == "gnumakefile" || strings.HasSuffix(base, ".mk") {
		return langAliasesMakefile
	}
	ext := strings.TrimPrefix(path.Ext(base), ".")
	if aliases, ok := extLangAliases[ext]; ok {
		return aliases
	}
	switch diff.DetectLang(filePath) {
	case diff.LangGo:
		return []string{"go", "golang"}
	case diff.LangJS:
		return []string{"js", "javascript", ext}
	case diff.LangCSS:
		return []string{"css", ext}
	case diff.LangJSON:
		return []string{"json", ext}
	case diff.LangPython:
		return []string{"py", "python", ext}
	case diff.LangRust:
		return []string{"rs", "rust", ext}
	case diff.LangShell:
		return []string{"sh", "bash", "shell", ext}
	case diff.LangYAML:
		return []string{"yaml", "yml", ext}
	case diff.LangTOML:
		return []string{"toml", ext}
	case diff.LangSQL:
		return []string{"sql", ext}
	case diff.LangC:
		return []string{"c", ext}
	}
	if ext != "" {
		return []string{ext}
	}
	return nil
}

func matchLangFilters(filters []SearchTerm, filePath string) bool {
	aliases := fileLangAliases(filePath)
	for _, f := range filters {
		matched := false
		for _, a := range aliases {
			if a != "" && f.MatchString(a) {
				matched = true
				break
			}
		}
		if f.Negated && matched {
			return false
		}
		if !f.Negated && !matched {
			return false
		}
	}
	return true
}

func filePassesFilters(q SearchQuery, filePath string, inChange bool) bool {
	if (q.Scope == "change" || q.WantChanged) && !inChange {
		return false
	}
	if len(q.FileFilters) > 0 && !matchAllFilters(q.FileFilters, filePath) {
		return false
	}
	if len(q.LangFilters) > 0 && !matchLangFilters(q.LangFilters, filePath) {
		return false
	}
	return true
}

func mergeMatchSpans(spans []SearchMatchSpan) []SearchMatchSpan {
	if len(spans) <= 1 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		return spans[i].End < spans[j].End
	})
	merged := spans[:1]
	for _, sp := range spans[1:] {
		last := &merged[len(merged)-1]
		if sp.Start <= last.End {
			if sp.End > last.End {
				last.End = sp.End
			}
		} else {
			merged = append(merged, sp)
		}
	}
	return merged
}

func findMatchSpans(terms []SearchTerm, line string) ([]SearchMatchSpan, bool) {
	return findMatchSpansBytes(terms, []byte(line))
}

func matchTermsBytes(terms []SearchTerm, line []byte) bool {
	for _, t := range terms {
		if t.Re == nil {
			continue
		}
		if t.Negated {
			if t.matchBytes(line) {
				return false
			}
			continue
		}
		if !t.matchBytes(line) {
			return false
		}
	}
	return true
}

// findMatchSpansBytes checks whether line satisfies all positive/negated terms
// without allocating on non-matching lines, then collects merged highlight spans.
func findMatchSpansBytes(terms []SearchTerm, line []byte) ([]SearchMatchSpan, bool) {
	if !matchTermsBytes(terms, line) {
		return nil, false
	}
	return findHighlightSpansBytes(terms, line), true
}

// findHighlightSpans collects highlight spans for every positive term that appears in text,
// even if other terms matched elsewhere (e.g. comment author or path).
func findHighlightSpans(terms []SearchTerm, text string) []SearchMatchSpan {
	return findHighlightSpansBytes(terms, []byte(text))
}

func findHighlightSpansBytes(terms []SearchTerm, b []byte) []SearchMatchSpan {
	var spans []SearchMatchSpan
	for _, t := range terms {
		if t.Re == nil || t.Negated {
			continue
		}
		if t.matchesEmpty {
			spans = append(spans, findNonEmptySpansBytes(t.Re, b, 16, t.hasAnchor)...)
			continue
		}
		for _, loc := range t.Re.FindAllIndex(b, 16) {
			if loc[1] > loc[0] {
				spans = append(spans, SearchMatchSpan{Start: loc[0], End: loc[1]})
			}
		}
	}
	return mergeMatchSpans(spans)
}

// CodeSearch executes a Code Search query across files, Go symbols, review comments, and code lines.
func CodeSearch(ctx context.Context, opts CodeSearchOptions) (*CodeSearchResult, error) {
	q := ParseSearchQuery(opts.Query)
	res := &CodeSearchResult{Query: opts.Query}
	if q.IsEmpty() {
		return res, nil
	}

	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 30
	}
	if opts.MaxSymbols <= 0 {
		opts.MaxSymbols = 40
	}
	if opts.MaxComments <= 0 {
		opts.MaxComments = 40
	}
	if opts.MaxCodeLines <= 0 {
		opts.MaxCodeLines = 120
	}

	searchRev := opts.SearchRev
	if searchRev == "" {
		searchRev = opts.TargetRev
	}

	commentsOnly := q.Scope == "comments" || (q.hasCommentIntent() && !q.hasCodeOnlyFilter())
	searchComments := opts.DB != nil && opts.RepoID > 0 && (commentsOnly || (q.Scope == "" && !q.hasCodeOnlyFilter()))
	searchCodeArea := opts.Repo != nil && searchRev != "" && !commentsOnly

	// 1. Gather changed files & repo file blobs when searching code/files/symbols.
	changedSet := make(map[string]bool)
	var changedPaths []string
	var allBlobs []vcs.GoFileBlob
	var blobRepo vcs.AllBlobRepo

	if searchCodeArea {
		if opts.Change != nil && opts.BaseRev != "" && opts.TargetRev != "" && opts.BaseRev != opts.TargetRev {
			if files, err := opts.Repo.Files(opts.BaseRev, opts.TargetRev); err == nil {
				for _, f := range files {
					if f == nil || f.Status == vcs.FileDeleted || vcs.IsCommitMsgPath(f.Path) {
						continue
					}
					p := strings.TrimPrefix(f.Path, "/")
					if p != "" && !changedSet[p] {
						changedSet[p] = true
						changedPaths = append(changedPaths, p)
					}
				}
			}
		}

		if br, ok := opts.Repo.(vcs.AllBlobRepo); ok {
			if blobs, err := br.ListAllFileBlobs(searchRev); err == nil {
				allBlobs = blobs
				blobRepo = br
			}
		}
		if len(allBlobs) == 0 && len(changedPaths) > 0 {
			for _, p := range changedPaths {
				allBlobs = append(allBlobs, vcs.GoFileBlob{Path: p})
			}
		}
	}

	wantSymbols := searchCodeArea && !q.HasContentFilter && (len(q.SymbolFilters) > 0 || q.hasPositiveTerm() || len(q.PkgFilters) > 0)
	if searchCodeArea && q.HasContentFilter && len(q.SymbolFilters) > 0 {
		wantSymbols = true
	}
	wantQuickOpen := searchCodeArea && len(q.SymbolFilters) == 0 && !q.HasContentFilter
	wantCodeLines := searchCodeArea && q.hasPositiveTerm()

	rootMod := ""
	if searchCodeArea && (len(q.PkgFilters) > 0 || wantSymbols) {
		rootMod = readRootModulePath(opts.Repo, searchRev)
	}

	// Run independent search phases concurrently. When p: filters are active, Quick Open
	// runs after searchGoSymbols so it can reuse parsed Go package names from goSymCache.
	var wg sync.WaitGroup
	var symPkgNames map[string]string

	if wantQuickOpen && len(q.PkgFilters) == 0 {
		res.Files = searchQuickOpenFiles(allBlobs, changedSet, rootMod, q, nil, opts.MaxFiles)
	}

	if wantSymbols {
		maxSyms := opts.MaxSymbols
		if len(q.SymbolFilters) == 0 && len(q.PkgFilters) == 0 {
			maxSyms = min(maxSyms, 15)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res.Symbols, symPkgNames = searchGoSymbols(ctx, opts.Repo, blobRepo, searchRev, rootMod, allBlobs, changedSet, q, maxSyms, wantQuickOpen && len(q.PkgFilters) > 0)
		}()
	}

	if searchComments {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res.Comments = searchReviewComments(opts, q, opts.MaxComments)
		}()
	}

	if wantCodeLines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res.Code, res.TotalCode, res.Truncated = searchCodeLines(ctx, opts, searchRev, rootMod, blobRepo, allBlobs, changedSet, q)
		}()
	}

	wg.Wait()

	if wantQuickOpen && len(q.PkgFilters) > 0 {
		res.Files = searchQuickOpenFiles(allBlobs, changedSet, rootMod, q, symPkgNames, opts.MaxFiles)
	}

	return res, nil
}

func searchQuickOpenFiles(allBlobs []vcs.GoFileBlob, changedSet map[string]bool, rootMod string, q SearchQuery, pkgByPath map[string]string, maxFiles int) []SearchFileMatch {
	pureFileSearch := !q.hasPositiveTerm() && (len(q.FileFilters) > 0 || len(q.LangFilters) > 0 || len(q.PkgFilters) > 0 || q.WantChanged || q.Scope == "change")
	if !pureFileSearch {
		maxFiles = min(maxFiles, 12)
	}

	// Support bare Quick Open "path/file.go:120" jump syntax when a single positive term has :line.
	fileTerms := q.Terms
	targetLine := q.TargetLine
	if len(q.FileFilters) == 0 && targetLine == 0 {
		posCount := 0
		posIdx := -1
		for i, t := range q.Terms {
			if !t.Negated {
				posCount++
				posIdx = i
			}
		}
		if posCount == 1 && !strings.ContainsAny(q.Terms[posIdx].Raw, "|(?*{[\\^$") {
			if fp, line, ok := splitFileLineSuffix(q.Terms[posIdx].Raw, true); ok {
				fileTerms = slices.Clone(q.Terms)
				fileTerms[posIdx] = compileSearchTerm(fp, false, true, q.CaseSensitive)
				targetLine = line
			}
		}
	}

	// Extract a primary needle for exact/prefix/substring basename ranking.
	rawNeedle := ""
	for _, f := range q.FileFilters {
		if !f.Negated {
			rawNeedle = f.Raw
			break
		}
	}
	if rawNeedle == "" {
		for _, t := range fileTerms {
			if !t.Negated {
				rawNeedle = t.Raw
				break
			}
		}
	}
	cleanNeedle := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(rawNeedle, "^"), "$"))

	type rankedFile struct {
		path     string
		inChange bool
		score    int
	}
	var matches []rankedFile
	for _, b := range allBlobs {
		if len(q.PkgFilters) > 0 && !strings.HasSuffix(b.Path, ".go") {
			continue
		}
		inCh := changedSet[b.Path]
		if !filePassesFilters(q, b.Path, inCh) {
			continue
		}
		if len(q.PkgFilters) > 0 {
			dir := path.Dir(b.Path)
			pkgName := pkgByPath[b.Path]
			if !pkgFiltersMatch(q.PkgFilters, dir, pkgName, moduleImportPath(rootMod, dir)) {
				continue
			}
		}
		if !pureFileSearch {
			if !matchAllFilters(fileTerms, b.Path) {
				continue
			}
		}

		score := 0
		if inCh {
			score += 100
		}
		base := path.Base(b.Path)
		lowBase := strings.ToLower(base)
		baseNoExt := strings.TrimSuffix(lowBase, path.Ext(lowBase))
		switch {
		case cleanNeedle != "" && (lowBase == cleanNeedle || baseNoExt == cleanNeedle):
			score += 200
			if base == rawNeedle {
				score += 25
			}
		case cleanNeedle != "" && strings.HasPrefix(lowBase, cleanNeedle):
			score += 120
		case (len(fileTerms) > 0 && matchAllFilters(fileTerms, base)) || (len(q.FileFilters) > 0 && matchAllFilters(q.FileFilters, base)):
			score += 60
		}
		matches = append(matches, rankedFile{path: b.Path, inChange: inCh, score: score})
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if len(matches[i].path) != len(matches[j].path) {
			return len(matches[i].path) < len(matches[j].path)
		}
		return matches[i].path < matches[j].path
	})
	if len(matches) > maxFiles {
		matches = matches[:maxFiles]
	}
	out := make([]SearchFileMatch, len(matches))
	for i, m := range matches {
		out[i] = SearchFileMatch{Path: m.path, Line: targetLine, InChange: m.inChange}
	}
	return out
}

func isTestdataGoPath(p string) bool {
	return strings.HasPrefix(p, "testdata/") || strings.Contains(p, "/testdata/")
}

func searchGoSymbols(ctx context.Context, repo vcs.Repo, blobRepo vcs.AllBlobRepo, rev, rootMod string, allBlobs []vcs.GoFileBlob, changedSet map[string]bool, q SearchQuery, limit int, returnPkgNames bool) ([]SearchSymbolMatch, map[string]string) {
	hasFileFilter := len(q.FileFilters) > 0

	// Pre-filter Go files before streaming blobs whenever a positive p: filter or path-qualified
	// symbol term requires a '/' in the directory/import path (which a bare Go package name cannot contain).
	pathOnlyPkgFilters := pathOnlyPkgPreFilters(q.PkgFilters)
	for _, sf := range q.SymbolFilters {
		if pre, ok := symPathPreFilter(sf.Term, q.CaseSensitive); ok {
			pathOnlyPkgFilters = append(pathOnlyPkgFilters, pre)
		}
	}
	if !returnPkgNames {
		for _, t := range q.Terms {
			if pre, ok := symPathPreFilter(t, q.CaseSensitive); ok {
				pathOnlyPkgFilters = append(pathOnlyPkgFilters, pre)
			}
		}
	}

	var goBlobs []vcs.GoFileBlob
	for _, b := range allBlobs {
		if !strings.HasSuffix(b.Path, ".go") {
			continue
		}
		if !hasFileFilter && isTestdataGoPath(b.Path) {
			continue
		}
		inCh := changedSet[b.Path]
		if !filePassesFilters(q, b.Path, inCh) {
			continue
		}
		if len(pathOnlyPkgFilters) > 0 {
			dir := path.Dir(b.Path)
			if !pkgFiltersMatch(pathOnlyPkgFilters, dir, "", moduleImportPath(rootMod, dir)) {
				continue
			}
		}
		goBlobs = append(goBlobs, b)
	}
	if len(goBlobs) == 0 {
		return nil, nil
	}

	// Check cache first; partition misses into OID-backed (streamed & deduplicated)
	// vs OID-less (working-tree fallback) so one missing OID never disables StreamBlobs.
	parsedByIdx := make([]cachedGoFileSyms, len(goBlobs))
	var missOIDs []string
	missOIDGroups := make(map[string][]int)
	var missNoOIDIdx []int

	goSymCache.mu.RLock()
	for i, b := range goBlobs {
		if b.OID != "" {
			if cached, ok := goSymCache.data[b.OID]; ok {
				parsedByIdx[i] = cached
				continue
			}
			if _, seen := missOIDGroups[b.OID]; !seen {
				missOIDs = append(missOIDs, b.OID)
			}
			missOIDGroups[b.OID] = append(missOIDGroups[b.OID], i)
		} else {
			missNoOIDIdx = append(missNoOIDIdx, i)
		}
	}
	goSymCache.mu.RUnlock()

	if len(missOIDs) > 0 {
		if blobRepo != nil {
			if len(missOIDs) <= 16 {
				_ = blobRepo.StreamBlobs(missOIDs, func(_ int, oid string, data []byte) {
					if ctx.Err() != nil {
						return
					}
					parsed := parseAndCacheGoSyms(oid, data)
					for _, idx := range missOIDGroups[oid] {
						parsedByIdx[idx] = parsed
					}
				})
			} else {
				type parseJob struct {
					oidIdx int
					data   []byte
				}
				workers := min(runtime.NumCPU(), 8)
				if workers < 2 {
					workers = 2
				}
				jobs := make(chan parseJob, workers*4)
				parsedOID := make([]cachedGoFileSyms, len(missOIDs))
				parsedOK := make([]bool, len(missOIDs))
				var wg sync.WaitGroup
				for w := 0; w < workers; w++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for job := range jobs {
							if ctx.Err() != nil {
								continue
							}
							parsedOID[job.oidIdx] = extractGoSymsFromBytes(job.data)
							parsedOK[job.oidIdx] = true
						}
					}()
				}
				// Stream in chunks so keystroke cancellation stops git cat-file promptly.
				const streamChunk = 256
				for start := 0; start < len(missOIDs) && ctx.Err() == nil; start += streamChunk {
					end := min(start+streamChunk, len(missOIDs))
					_ = blobRepo.StreamBlobs(missOIDs[start:end], func(k int, _ string, data []byte) {
						if ctx.Err() != nil {
							return
						}
						oidIdx := start + k
						if len(data) == 0 || len(data) > maxGoSymFileBytes {
							parsedOK[oidIdx] = true
							return
						}
						jobs <- parseJob{oidIdx: oidIdx, data: data}
					})
				}
				close(jobs)
				wg.Wait()

				parsedCount := 0
				for oidIdx, ok := range parsedOK {
					if !ok {
						continue
					}
					parsedCount++
					oid := missOIDs[oidIdx]
					for _, idx := range missOIDGroups[oid] {
						parsedByIdx[idx] = parsedOID[oidIdx]
					}
				}
				if parsedCount > 0 {
					goSymCache.mu.Lock()
					if len(goSymCache.data)+parsedCount >= maxCachedGoSymFiles {
						goSymCache.data = make(map[string]cachedGoFileSyms)
					}
					for oidIdx, ok := range parsedOK {
						if ok {
							goSymCache.data[missOIDs[oidIdx]] = parsedOID[oidIdx]
						}
					}
					goSymCache.mu.Unlock()
				}
			}
		} else {
			for _, oid := range missOIDs {
				if ctx.Err() != nil {
					break
				}
				idxs := missOIDGroups[oid]
				data, err := repo.Content(rev, goBlobs[idxs[0]].Path)
				if err != nil {
					continue
				}
				parsed := parseAndCacheGoSyms(oid, data)
				for _, idx := range idxs {
					parsedByIdx[idx] = parsed
				}
			}
		}
	}

	for _, idx := range missNoOIDIdx {
		if ctx.Err() != nil {
			break
		}
		data, err := repo.Content(rev, goBlobs[idx].Path)
		if err != nil {
			continue
		}
		parsedByIdx[idx] = extractGoSymsFromBytes(data)
	}

	var pkgNames map[string]string
	if returnPkgNames {
		pkgNames = make(map[string]string, len(goBlobs))
		for i, b := range goBlobs {
			if parsedByIdx[i].pkgName != "" {
				pkgNames[b.Path] = parsedByIdx[i].pkgName
			}
		}
	}

	type rankedSym struct {
		match SearchSymbolMatch
		score int
	}
	var hits []rankedSym

	symMatchesKind := func(filterKind, symKind string) bool {
		switch filterKind {
		case "":
			return true
		case "func":
			return symKind == "func" || symKind == "method"
		case "method":
			return symKind == "method"
		case "type":
			return symKind == "type" || symKind == "struct" || symKind == "interface"
		default:
			return symKind == filterKind
		}
	}

	needle := ""
	for _, sf := range q.SymbolFilters {
		if !sf.Term.Negated {
			needle = sf.Term.Raw
			break
		}
	}
	if needle == "" {
		for _, t := range q.Terms {
			if !t.Negated {
				needle = t.Raw
				break
			}
		}
	}
	rawCleanNeedle := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(needle, "^"), "$"), `\.`, ".")
	cleanNeedle := strings.ToLower(rawCleanNeedle)
	needleHasQual := strings.ContainsAny(cleanNeedle, "./")

	for i, b := range goBlobs {
		parsed := parsedByIdx[i]
		if len(parsed.syms) == 0 {
			continue
		}
		dir := path.Dir(b.Path)
		importDir := dir
		if strings.HasPrefix(dir, "src/") {
			importDir = strings.TrimPrefix(dir, "src/")
		}
		modImport := moduleImportPath(rootMod, dir)
		if !pkgFiltersMatch(q.PkgFilters, dir, parsed.pkgName, modImport) {
			continue
		}

		inCh := changedSet[b.Path]
		isTestFile := strings.HasSuffix(b.Path, "_test.go")

		// Precompute qualified prefixes once per file (allocated lazily only when a term wants qualification).
		pkgPrefix := ""
		if parsed.pkgName != "" {
			pkgPrefix = parsed.pkgName + "."
		}
		importPrefix := ""
		if importDir != "." && importDir != "" && importDir != parsed.pkgName {
			importPrefix = importDir + "."
		}
		dirPrefix := ""
		if dir != "." && dir != "" && dir != importDir && dir != parsed.pkgName {
			dirPrefix = dir + "."
		}
		modPrefix := ""
		if modImport != "" && modImport != importDir && modImport != dir && modImport != parsed.pkgName {
			modPrefix = modImport + "."
		}

		for _, sym := range parsed.syms {
			var qualBuf [4]string
			nQual := -1 // lazily populated if any term needs qualified matching
			getQuals := func() []string {
				if nQual >= 0 {
					return qualBuf[:nQual]
				}
				nQual = 0
				if pkgPrefix != "" {
					qualBuf[nQual] = pkgPrefix + sym.Name
					nQual++
				}
				if importPrefix != "" {
					qualBuf[nQual] = importPrefix + sym.Name
					nQual++
				}
				if dirPrefix != "" {
					qualBuf[nQual] = dirPrefix + sym.Name
					nQual++
				}
				if modPrefix != "" {
					qualBuf[nQual] = modPrefix + sym.Name
					nQual++
				}
				return qualBuf[:nQual]
			}

			termMatchesSym := func(t SearchTerm) bool {
				if t.MatchString(sym.Name) {
					return true
				}
				if sym.ShortName != sym.Name && t.MatchString(sym.ShortName) {
					return true
				}
				if (t.Negated && t.wantQualNeg) || (!t.Negated && t.wantQualPos) {
					for _, qc := range getQuals() {
						if t.MatchString(qc) {
							return true
						}
					}
				}
				return false
			}

			ok := true
			for _, sf := range q.SymbolFilters {
				matched := symMatchesKind(sf.Kind, sym.Kind) && termMatchesSym(sf.Term)
				if sf.Term.Negated && matched {
					ok = false
					break
				}
				if !sf.Term.Negated && !matched {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}

			if len(q.Terms) > 0 {
				// No negated term may match, and all positive terms must match a single candidate form.
				anyNeg := false
				anyPosWantQual := false
				for _, t := range q.Terms {
					if t.Negated {
						if termMatchesSym(t) {
							anyNeg = true
							break
						}
					} else if t.wantQualPos {
						anyPosWantQual = true
					}
				}
				if anyNeg {
					continue
				}

				matchAllPos := func(cand string) bool {
					for _, t := range q.Terms {
						if t.Negated {
							continue
						}
						if !t.MatchString(cand) {
							return false
						}
					}
					return true
				}

				allPosMatched := matchAllPos(sym.Name) || (sym.ShortName != sym.Name && matchAllPos(sym.ShortName))
				if !allPosMatched && anyPosWantQual {
					for _, qc := range getQuals() {
						if matchAllPos(qc) {
							allPosMatched = true
							break
						}
					}
				}
				if !allPosMatched {
					continue
				}
			}

			score := 0
			if inCh {
				score += 100
			}
			if !isTestFile && !strings.HasPrefix(cleanNeedle, "test") {
				score += 15
			}
			if cleanNeedle != "" {
				lowShort := strings.ToLower(sym.ShortName)
				lowName := strings.ToLower(sym.Name)
				exact := lowShort == cleanNeedle || lowName == cleanNeedle
				suffixSlash := false
				prefixMatch := strings.HasPrefix(lowShort, cleanNeedle) || strings.HasPrefix(lowName, cleanNeedle)
				if !exact && needleHasQual {
					slashNeedle := "/" + cleanNeedle
					for _, qc := range getQuals() {
						lowQ := strings.ToLower(qc)
						if lowQ == cleanNeedle {
							exact = true
							break
						}
						if strings.HasSuffix(lowQ, slashNeedle) {
							suffixSlash = true
						} else if strings.HasPrefix(lowQ, cleanNeedle) {
							prefixMatch = true
						}
					}
				}
				switch {
				case exact:
					score += 200
					if sym.ShortName == rawCleanNeedle || sym.Name == rawCleanNeedle {
						score += 25
					}
				case suffixSlash:
					score += 160
				case prefixMatch:
					score += 60
				}
			}
			switch sym.Kind {
			case "type", "struct", "interface", "func":
				score += 20
			case "method":
				score += 10
			case "const", "var":
				score += 5
			}

			displayPkg := parsed.pkgName
			if importDir != "." && importDir != "" {
				displayPkg = importDir
			}
			hits = append(hits, rankedSym{
				match: SearchSymbolMatch{
					Name:      sym.Name,
					Kind:      sym.Kind,
					Package:   displayPkg,
					Signature: sym.Signature,
					Path:      b.Path,
					Line:      sym.Line,
					Col:       sym.Col,
					InChange:  inCh,
				},
				score: score,
			})
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if len(hits[i].match.Name) != len(hits[j].match.Name) {
			return len(hits[i].match.Name) < len(hits[j].match.Name)
		}
		if hits[i].match.Path != hits[j].match.Path {
			return hits[i].match.Path < hits[j].match.Path
		}
		return hits[i].match.Line < hits[j].match.Line
	})

	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]SearchSymbolMatch, len(hits))
	for i, h := range hits {
		out[i] = h.match
	}
	return out, pkgNames
}

func searchReviewComments(opts CodeSearchOptions, q SearchQuery, limit int) []SearchCommentHit {
	var rawThreads []*db.Thread
	var err error
	if opts.Change != nil && opts.Change.Key != "" {
		rawThreads, err = opts.DB.ThreadsForChange(opts.RepoID, opts.Change.Key)
	} else {
		rawThreads, err = opts.DB.ThreadsForRepo(opts.RepoID)
	}
	if err != nil || len(rawThreads) == 0 {
		return nil
	}

	// Filter, sort, and cap in memory BEFORE invoking VCS-backed ReanchorThreadsForView
	// so non-matching or overflow threads never trigger git/jj I/O.
	type candidateHit struct {
		hit    SearchCommentHit
		thread *db.Thread
	}
	var cands []candidateHit
	perMessage := len(q.Terms) > 0 || len(q.AuthorFilters) > 0 || q.WantDraft

	for _, th := range rawThreads {
		if th == nil || len(th.Messages) == 0 {
			continue
		}
		if q.WantUnresolved && th.Resolved {
			continue
		}
		if q.WantResolved && !th.Resolved {
			continue
		}
		cleanPath := strings.TrimPrefix(th.Path, "/")
		if vcs.IsCommitMsgPath(th.Path) {
			cleanPath = "COMMIT_MSG"
		}
		if len(q.FileFilters) > 0 && !matchAllFilters(q.FileFilters, cleanPath) {
			continue
		}

		if !perMessage {
			var firstMsg *db.Message
			latestTime := th.CreatedAt
			for _, m := range th.Messages {
				if m == nil {
					continue
				}
				if firstMsg == nil && strings.TrimSpace(m.Body) != "" {
					firstMsg = m
				}
				mt := m.UpdatedAt
				if m.CreatedAt.After(mt) {
					mt = m.CreatedAt
				}
				if mt.After(latestTime) {
					latestTime = mt
				}
			}
			if firstMsg == nil {
				firstMsg = th.Messages[0]
			}
			if firstMsg == nil {
				continue
			}
			cands = append(cands, candidateHit{
				thread: th,
				hit: SearchCommentHit{
					ThreadID:      th.ID,
					MessageID:     firstMsg.ID,
					ChangeKey:     th.ChangeKey,
					Path:          cleanPath,
					Side:          th.Side,
					Line:          th.StartLine,
					EndLine:       th.EndLine,
					SnapshotNum:   th.SnapshotNum,
					Author:        firstMsg.Author,
					Resolved:      th.Resolved,
					Draft:         firstMsg.Draft,
					Stale:         th.Stale,
					InCurrentView: true,
					Snippet:       buildCommentSearchSnippet(firstMsg.Body, nil),
					ActivityTime:  latestTime,
				},
			})
			continue
		}

		for _, m := range th.Messages {
			if m == nil {
				continue
			}
			if q.WantDraft && !m.Draft {
				continue
			}
			if len(q.AuthorFilters) > 0 && !matchAllFilters(q.AuthorFilters, m.Author) {
				continue
			}

			var spans []SearchMatchSpan
			if len(q.Terms) > 0 {
				combined := m.Body + "\n" + m.Author + "\n" + cleanPath
				if !matchTermsBytes(q.Terms, []byte(combined)) {
					continue
				}
			}
			snippet := buildCommentSearchSnippet(m.Body, q.Terms)
			if len(q.Terms) > 0 {
				spans = findHighlightSpans(q.Terms, snippet)
			}

			actTime := m.UpdatedAt
			if m.CreatedAt.After(actTime) {
				actTime = m.CreatedAt
			}
			if actTime.IsZero() {
				actTime = th.CreatedAt
			}

			cands = append(cands, candidateHit{
				thread: th,
				hit: SearchCommentHit{
					ThreadID:      th.ID,
					MessageID:     m.ID,
					ChangeKey:     th.ChangeKey,
					Path:          cleanPath,
					Side:          th.Side,
					Line:          th.StartLine,
					EndLine:       th.EndLine,
					SnapshotNum:   th.SnapshotNum,
					Author:        m.Author,
					Resolved:      th.Resolved,
					Draft:         m.Draft,
					Stale:         th.Stale,
					InCurrentView: true,
					Snippet:       snippet,
					Spans:         spans,
					ActivityTime:  actTime,
				},
			})
		}
	}

	if len(cands) == 0 {
		return nil
	}

	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].hit.ActivityTime.Equal(cands[j].hit.ActivityTime) {
			return cands[i].hit.ActivityTime.After(cands[j].hit.ActivityTime)
		}
		return cands[i].hit.MessageID > cands[j].hit.MessageID
	})

	if len(cands) > limit {
		cands = cands[:limit]
	}

	// Reanchor only the distinct threads present in the final top hits.
	if opts.Change != nil && opts.Repo != nil && opts.TargetRev != "" {
		scope := NewViewScope(opts.Snapshots, opts.BaseRev, opts.TargetRev, opts.IsLiveTarget)
		var uniqueThreads []*db.Thread
		seen := make(map[int64]bool, len(cands))
		for _, c := range cands {
			if !seen[c.thread.ID] {
				seen[c.thread.ID] = true
				uniqueThreads = append(uniqueThreads, c.thread)
			}
		}
		tvs := ReanchorThreadsForView(opts.Repo, opts.Change, opts.Snapshots, opts.TargetRev, opts.BaseRev, opts.IsLiveTarget, uniqueThreads, nil)
		tvByID := make(map[int64]*ThreadView, len(tvs))
		for _, tv := range tvs {
			if tv != nil && tv.Thread != nil {
				tvByID[tv.Thread.ID] = tv
			}
		}
		for i := range cands {
			th := cands[i].thread
			if tv, ok := tvByID[th.ID]; ok {
				cands[i].hit.Line = tv.CurrentStart
				cands[i].hit.EndLine = tv.CurrentEnd
				cands[i].hit.Stale = tv.Stale
				_, inView := scope.ThreadSide(th)
				cands[i].hit.InCurrentView = inView
				if inView {
					cands[i].hit.Side = tv.DisplaySide()
				}
			}
		}
	}

	hits := make([]SearchCommentHit, len(cands))
	for i, c := range cands {
		hits[i] = c.hit
	}
	return hits
}

func buildCommentSearchSnippet(body string, terms []SearchTerm) string {
	posCount := 0
	for _, t := range terms {
		if !t.Negated && t.Re != nil {
			posCount++
		}
	}
	if posCount > 0 {
		lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
		bestLine := ""
		bestScore := 0
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			score := 0
			for _, t := range terms {
				if !t.Negated && t.MatchString(trimmed) {
					score++
				}
			}
			if score > bestScore {
				bestScore = score
				bestLine = trimmed
				if bestScore == posCount {
					break
				}
			}
		}
		if bestLine != "" {
			return truncateUTF8(bestLine, 200)
		}
	}
	// Otherwise collapse the start of the message.
	flat := strings.Join(strings.Fields(body), " ")
	return truncateUTF8(flat, 200)
}

// windowMatchedLine caps huge (e.g. minified) lines around the first match so
// search responses stay small while keeping highlight spans aligned.
func windowMatchedLine(line []byte, spans []SearchMatchSpan) (string, []SearchMatchSpan) {
	const maxStoredLine = 4096
	if len(line) <= maxStoredLine {
		return string(line), spans
	}
	start := 0
	if len(spans) > 0 && spans[0].Start > 256 {
		start = spans[0].Start - 256
		for start < len(line) && !utf8.RuneStart(line[start]) {
			start++
		}
	}
	end := min(start+1024, len(line))
	for end > start && end < len(line) && !utf8.RuneStart(line[end]) {
		end--
	}
	prefix := ""
	if start > 0 {
		prefix = "…"
	}
	suffix := ""
	if end < len(line) {
		suffix = "…"
	}
	adj := make([]SearchMatchSpan, 0, len(spans))
	for _, sp := range spans {
		s := sp.Start - start
		e := sp.End - start
		if e <= 0 || s >= end-start {
			continue
		}
		if s < 0 {
			s = 0
		}
		if e > end-start {
			e = end - start
		}
		adj = append(adj, SearchMatchSpan{Start: s + len(prefix), End: e + len(prefix)})
	}
	return prefix + string(line[start:end]) + suffix, adj
}

type fileScanResult struct {
	lines     []SearchCodeLine
	truncated bool
}

func scanBlobForCodeLines(filePath, rootMod string, data []byte, q SearchQuery, maxPerFile int) fileScanResult {
	if len(data) == 0 || len(data) > 1<<20 || diff.IsBinary(data) {
		return fileScanResult{}
	}
	for _, t := range q.Terms {
		if !t.matchBlobPrecheck(data) {
			return fileScanResult{}
		}
	}
	if len(q.PkgFilters) > 0 {
		dir := path.Dir(filePath)
		pkgName := extractGoPackageName(data)
		if !pkgFiltersMatch(q.PkgFilters, dir, pkgName, moduleImportPath(rootMod, dir)) {
			return fileScanResult{}
		}
	}

	var res fileScanResult
	lineNum := 0
	rem := data
	for len(rem) > 0 {
		lineNum++
		nl := bytes.IndexByte(rem, '\n')
		var lineBytes []byte
		if nl < 0 {
			lineBytes = rem
			rem = nil
		} else {
			lineBytes = rem[:nl]
			rem = rem[nl+1:]
		}
		if len(lineBytes) > 0 && lineBytes[len(lineBytes)-1] == '\r' {
			lineBytes = lineBytes[:len(lineBytes)-1]
		}

		// Once maxPerFile hits are collected, scan zero-alloc just to see if another match exists.
		if len(res.lines) >= maxPerFile {
			if matchTermsBytes(q.Terms, lineBytes) {
				res.truncated = true
				break
			}
			continue
		}

		spans, ok := findMatchSpansBytes(q.Terms, lineBytes)
		if !ok {
			continue
		}
		col := 0
		if len(spans) > 0 {
			col = spans[0].Start
		}
		text, adjSpans := windowMatchedLine(lineBytes, spans)
		res.lines = append(res.lines, SearchCodeLine{
			Line:  lineNum,
			Col:   col,
			Text:  text,
			Spans: adjSpans,
		})
	}
	return res
}

func searchCodeLines(ctx context.Context, opts CodeSearchOptions, searchRev, rootMod string, blobRepo vcs.AllBlobRepo, allBlobs []vcs.GoFileBlob, changedSet map[string]bool, q SearchQuery) ([]SearchCodeGroup, int, bool) {
	maxTotal := opts.MaxCodeLines
	maxFiles := 40
	maxPerFile := 5
	if len(q.FileFilters) > 0 {
		maxPerFile = 15
	}

	var groups []SearchCodeGroup
	totalLines := 0
	truncated := false
	stopScanning := false

	appendFileResult := func(filePath string, inChange bool, res fileScanResult) {
		if len(res.lines) == 0 {
			return
		}
		if res.truncated {
			truncated = true
		}
		if len(groups) >= maxFiles || totalLines >= maxTotal {
			truncated = true
			stopScanning = true
			return
		}
		lines := res.lines
		if totalLines+len(lines) > maxTotal {
			lines = lines[:maxTotal-totalLines]
			truncated = true
		}
		groups = append(groups, SearchCodeGroup{
			Path:     filePath,
			InChange: inChange,
			Lines:    lines,
		})
		totalLines += len(lines)
		if truncated && (len(groups) >= maxFiles || totalLines >= maxTotal) {
			stopScanning = true
		}
	}

	// 1. Search /COMMIT_MSG first when viewing a change's target revision and filters allow it.
	if opts.Change != nil && searchRev == opts.TargetRev && filePassesFilters(q, "COMMIT_MSG", true) && len(q.PkgFilters) == 0 {
		if data, err := opts.Repo.Content(searchRev, vcs.CommitMsgPath); err == nil && len(data) > 0 {
			appendFileResult("COMMIT_MSG", true, scanBlobForCodeLines("COMMIT_MSG", rootMod, data, q, maxPerFile))
		}
	}

	// 2. Filter candidate blobs and put changed files first.
	// Pre-filter with cached trigram bloom filters to skip 95-99% of blobs before git cat-file.
	bq := buildBloomQuery(q.Terms, q.CaseSensitive)
	pathPkgPre := pathOnlyPkgPreFilters(q.PkgFilters)

	type candidateBlob struct {
		blob     vcs.GoFileBlob
		inChange bool
		hasBloom bool
	}
	var changedCandidates, otherCandidates []candidateBlob

	if blobRepo != nil {
		blobBloomCache.mu.RLock()
	}
	for _, b := range allBlobs {
		if len(q.PkgFilters) > 0 && !strings.HasSuffix(b.Path, ".go") {
			continue
		}
		inCh := changedSet[b.Path]
		if !filePassesFilters(q, b.Path, inCh) {
			continue
		}
		if len(pathPkgPre) > 0 {
			dir := path.Dir(b.Path)
			if !pkgFiltersMatch(pathPkgPre, dir, "", moduleImportPath(rootMod, dir)) {
				continue
			}
		}
		hasBloom := false
		if blobRepo != nil && b.OID != "" {
			if bl, ok := blobBloomCache.data[b.OID]; ok {
				hasBloom = true
				if bl.skip || (len(bq.groups) > 0 && !bq.mightMatch(bl)) {
					continue
				}
			}
		}
		cand := candidateBlob{blob: b, inChange: inCh, hasBloom: hasBloom}
		if inCh {
			changedCandidates = append(changedCandidates, cand)
		} else {
			otherCandidates = append(otherCandidates, cand)
		}
	}
	if blobRepo != nil {
		blobBloomCache.mu.RUnlock()
	}
	candidates := slices.Concat(changedCandidates, otherCandidates)

	// 3. Stream in chunks and scan each chunk in parallel while preserving exact deterministic order.
	workers := min(runtime.NumCPU(), 8)
	if workers < 2 {
		workers = 2
	}

	if blobRepo != nil {
		const chunkSize = 256
		for start := 0; start < len(candidates) && !stopScanning && ctx.Err() == nil; start += chunkSize {
			end := min(start+chunkSize, len(candidates))
			chunk := candidates[start:end]

			chunkData := make([][]byte, len(chunk))
			computeBloom := make([]bool, len(chunk))
			var oids []string
			oidIndex := make(map[string]int, len(chunk))
			var oidPositions [][]int
			var noOIDPos []int
			for i, c := range chunk {
				if c.blob.OID != "" {
					if idx, exists := oidIndex[c.blob.OID]; exists {
						oidPositions[idx] = append(oidPositions[idx], i)
					} else {
						oidIndex[c.blob.OID] = len(oids)
						oids = append(oids, c.blob.OID)
						oidPositions = append(oidPositions, []int{i})
					}
				} else {
					noOIDPos = append(noOIDPos, i)
				}
			}
			if len(oids) > 0 {
				_ = blobRepo.StreamBlobs(oids, func(k int, _ string, data []byte) {
					positions := oidPositions[k]
					for idx, pos := range positions {
						chunkData[pos] = data
						// Only cache blooms for blobs actually delivered by StreamBlobs,
						// and only once per unique OID in the chunk.
						if idx == 0 && !chunk[pos].hasBloom {
							computeBloom[pos] = true
						}
					}
				})
			}
			for _, pos := range noOIDPos {
				if ctx.Err() != nil {
					break
				}
				if data, err := opts.Repo.Content(searchRev, chunk[pos].blob.Path); err == nil {
					chunkData[pos] = data
				}
			}
			if ctx.Err() != nil {
				break
			}

			results := make([]fileScanResult, len(chunk))
			newBlooms := make([]oidBloom, len(chunk))
			nWorkers := min(workers, len(chunk))
			if nWorkers <= 1 || len(chunk) <= 4 {
				for i, c := range chunk {
					data := chunkData[i]
					if computeBloom[i] {
						newBlooms[i] = oidBloom{oid: c.blob.OID, bloom: buildBlobBloom(data)}
					}
					results[i] = scanBlobForCodeLines(c.blob.Path, rootMod, data, q, maxPerFile)
				}
			} else {
				var wg sync.WaitGroup
				perWorker := (len(chunk) + nWorkers - 1) / nWorkers
				for w := 0; w < nWorkers; w++ {
					lo := w * perWorker
					hi := min(lo+perWorker, len(chunk))
					if lo >= hi {
						break
					}
					wg.Add(1)
					go func(lo, hi int) {
						defer wg.Done()
						for i := lo; i < hi; i++ {
							c := chunk[i]
							data := chunkData[i]
							if computeBloom[i] {
								newBlooms[i] = oidBloom{oid: c.blob.OID, bloom: buildBlobBloom(data)}
							}
							results[i] = scanBlobForCodeLines(c.blob.Path, rootMod, data, q, maxPerFile)
						}
					}(lo, hi)
				}
				wg.Wait()
			}

			// Batch-insert any newly computed bloom filters into the content-addressed cache.
			var toStore []oidBloom
			for _, nb := range newBlooms {
				if nb.oid != "" {
					toStore = append(toStore, nb)
				}
			}
			if len(toStore) > 0 {
				putBlobBlooms(toStore)
			}

			for i, c := range chunk {
				if stopScanning {
					break
				}
				appendFileResult(c.blob.Path, c.inChange, results[i])
			}
		}
	} else {
		for _, c := range candidates {
			if stopScanning || ctx.Err() != nil {
				break
			}
			if data, err := opts.Repo.Content(searchRev, c.blob.Path); err == nil {
				appendFileResult(c.blob.Path, c.inChange, scanBlobForCodeLines(c.blob.Path, rootMod, data, q, maxPerFile))
			}
		}
	}

	return groups, totalLines, truncated
}
