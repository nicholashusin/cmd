package engine

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"runtime"
	"slices"
	"sort"
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
	Raw     string
	Negated bool
	Literal bool
	Re      *regexp.Regexp
}

func (t SearchTerm) MatchString(s string) bool {
	if t.Re == nil {
		return false
	}
	return t.Re.MatchString(s)
}

// SymbolFilter matches Go declarations by optional kind ("func", "method", "type", or "" for any).
type SymbolFilter struct {
	Kind string
	Term SearchTerm
}

// SearchQuery is the parsed representation of a Code Search query string.
type SearchQuery struct {
	Raw            string
	CaseSensitive  bool
	Scope          string // "", "change", "repo", "comments"
	Terms          []SearchTerm
	FileFilters    []SearchTerm
	SymbolFilters  []SymbolFilter
	PkgFilters     []SearchTerm
	LangFilters    []SearchTerm
	AuthorFilters  []SearchTerm
	WantUnresolved bool
	WantResolved   bool
	WantDraft      bool
	WantChanged    bool
}

type rawQueryToken struct {
	prefix  string // lowercase filter prefix without ':', or ""
	value   string
	negated bool
	literal bool
	isRegex bool
}

func tokenizeSearchQuery(raw string) []rawQueryToken {
	var tokens []rawQueryToken
	n := len(raw)
	i := 0

	skipSpace := func() {
		for i < n && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
			i++
		}
	}

	readDelimited := func(delim byte) string {
		i++ // skip opening delim
		var b strings.Builder
		for i < n {
			c := raw[i]
			if c == '\\' && i+1 < n {
				next := raw[i+1]
				if next == delim || next == '\\' {
					b.WriteByte(next)
					i += 2
					continue
				}
			}
			if c == delim {
				i++
				break
			}
			b.WriteByte(c)
			i++
		}
		return b.String()
	}

	readWord := func() string {
		start := i
		for i < n && raw[i] != ' ' && raw[i] != '\t' && raw[i] != '\n' && raw[i] != '\r' {
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
		if raw[i] == '-' && i+1 < n && raw[i+1] != ' ' && raw[i+1] != '\t' {
			neg = true
			i++
		}

		if raw[i] == '"' {
			val := readDelimited('"')
			if val != "" {
				tokens = append(tokens, rawQueryToken{value: val, negated: neg, literal: true})
			}
			continue
		}
		if raw[i] == '/' {
			val := readDelimited('/')
			if val != "" {
				tokens = append(tokens, rawQueryToken{value: val, negated: neg, isRegex: true})
			}
			continue
		}

		// Scan up to whitespace or ':' to see if this is a recognized filter prefix.
		start := i
		colon := -1
		for j := i; j < n && raw[j] != ' ' && raw[j] != '\t' && raw[j] != '\n' && raw[j] != '\r'; j++ {
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
					val = readDelimited('"')
					literal = true
				} else if i < n && raw[i] == '/' {
					val = readDelimited('/')
					isRegex = true
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
		"s", "sym", "symbol", "func", "method", "type", "field",
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

func compileSearchTerm(raw string, negated, literal, caseSensitive bool) SearchTerm {
	pattern := raw
	if literal {
		pattern = regexp.QuoteMeta(raw)
	}
	prefix := ""
	if !caseSensitive {
		prefix = "(?i)"
	}
	re, err := regexp.Compile(prefix + pattern)
	if err != nil {
		// Graceful fallback to literal matching while the user is mid-typing a regex.
		re, _ = regexp.Compile(prefix + regexp.QuoteMeta(raw))
		literal = true
	}
	return SearchTerm{
		Raw:     raw,
		Negated: negated,
		Literal: literal,
		Re:      re,
	}
}

// ParseSearchQuery parses Google Code Search style queries into structured filters and terms.
func ParseSearchQuery(raw string) SearchQuery {
	q := SearchQuery{Raw: raw}
	tokens := tokenizeSearchQuery(raw)

	// Pass 1: resolve global modifiers (e.g. case:yes) regardless of token position.
	for _, tok := range tokens {
		if tok.prefix == "case" {
			switch strings.ToLower(tok.value) {
			case "yes", "true", "1", "sensitive":
				q.CaseSensitive = !tok.negated
			case "no", "false", "0", "insensitive":
				q.CaseSensitive = tok.negated
			}
		}
	}

	// Pass 2: compile terms and filters.
	for _, tok := range tokens {
		switch tok.prefix {
		case "case":
			continue
		case "f", "file", "path":
			q.FileFilters = append(q.FileFilters, compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive))
		case "s", "sym", "symbol":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "func":
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
		case "field":
			q.SymbolFilters = append(q.SymbolFilters, SymbolFilter{
				Kind: "field",
				Term: compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive),
			})
		case "p", "pkg":
			q.PkgFilters = append(q.PkgFilters, compileSearchTerm(tok.value, tok.negated, tok.literal, q.CaseSensitive))
		case "l", "lang":
			q.LangFilters = append(q.LangFilters, compileSearchTerm(tok.value, tok.negated, tok.literal, false))
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
		case "c", "content", "":
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
	return q.Scope == "repo" || q.Scope == "change" || len(q.SymbolFilters) > 0 || len(q.PkgFilters) > 0 || len(q.LangFilters) > 0 || q.WantChanged
}

// SearchMatchSpan marks byte offsets [Start, End) of a match within a line.
type SearchMatchSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// SearchFileMatch is a Quick Open file hit.
type SearchFileMatch struct {
	Path     string `json:"path"`
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

const maxCachedGoSymFiles = 65536

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
	if len(data) == 0 {
		return cachedGoFileSyms{}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", data, parser.SkipObjectResolution)
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
		off := fset.Position(pos).Offset
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
			pos := fset.Position(d.Name.Pos())
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
					pos := fset.Position(sp.Name.Pos())
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
									fp := fset.Position(embID.Pos())
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
								fp := fset.Position(id.Pos())
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
								mp := fset.Position(id.Pos())
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
						pos := fset.Position(id.Pos())
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

func fileLangAliases(filePath string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(filePath), "."))
	switch diff.DetectLang(filePath) {
	case diff.LangGo:
		return "go golang " + ext
	case diff.LangJS:
		return "js javascript ts typescript " + ext
	case diff.LangCSS:
		return "css " + ext
	case diff.LangJSON:
		return "json " + ext
	case diff.LangPython:
		return "py python " + ext
	case diff.LangRust:
		return "rs rust " + ext
	case diff.LangShell:
		return "sh bash shell " + ext
	case diff.LangYAML:
		return "yaml yml " + ext
	case diff.LangTOML:
		return "toml " + ext
	case diff.LangSQL:
		return "sql " + ext
	case diff.LangC:
		return "c cpp cc cxx h hpp " + ext
	}
	switch ext {
	case "html", "htm", "tmpl":
		return "html tmpl " + ext
	case "md", "markdown":
		return "md markdown " + ext
	default:
		return ext
	}
}

func filePassesFilters(q SearchQuery, filePath string, inChange bool) bool {
	if (q.Scope == "change" || q.WantChanged) && !inChange {
		return false
	}
	if len(q.FileFilters) > 0 && !matchAllFilters(q.FileFilters, filePath) {
		return false
	}
	if len(q.LangFilters) > 0 && !matchAllFilters(q.LangFilters, fileLangAliases(filePath)) {
		return false
	}
	return true
}

func findMatchSpans(terms []SearchTerm, line string) ([]SearchMatchSpan, bool) {
	var spans []SearchMatchSpan
	for _, t := range terms {
		if t.Re == nil {
			continue
		}
		if t.Negated {
			if t.Re.MatchString(line) {
				return nil, false
			}
			continue
		}
		locs := t.Re.FindAllStringIndex(line, 16)
		if len(locs) == 0 {
			return nil, false
		}
		for _, loc := range locs {
			if loc[1] > loc[0] {
				spans = append(spans, SearchMatchSpan{Start: loc[0], End: loc[1]})
			}
		}
	}
	if len(spans) <= 1 {
		return spans, true
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
	return merged, true
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

	wantSymbols := searchCodeArea && (len(q.SymbolFilters) > 0 || q.hasPositiveTerm() || len(q.PkgFilters) > 0)
	rootMod := ""
	if searchCodeArea && (len(q.PkgFilters) > 0 || wantSymbols) {
		rootMod = readRootModulePath(opts.Repo, searchRev)
	}

	// 2. Quick Open file matches.
	if searchCodeArea && len(q.SymbolFilters) == 0 {
		pureFileSearch := !q.hasPositiveTerm() && (len(q.FileFilters) > 0 || len(q.LangFilters) > 0 || len(q.PkgFilters) > 0 || q.WantChanged || q.Scope == "change")
		maxFiles := opts.MaxFiles
		if !pureFileSearch {
			maxFiles = min(maxFiles, 12)
		}

		type rankedFile struct {
			path     string
			inChange bool
			score    int
		}
		var matches []rankedFile
		for _, b := range allBlobs {
			inCh := changedSet[b.Path]
			if !filePassesFilters(q, b.Path, inCh) {
				continue
			}
			dir := path.Dir(b.Path)
			if !pkgFiltersMatch(q.PkgFilters, dir, "", moduleImportPath(rootMod, dir)) {
				continue
			}
			if !pureFileSearch {
				if !matchAllFilters(q.Terms, b.Path) {
					continue
				}
			}
			score := 0
			if inCh {
				score += 100
			}
			base := path.Base(b.Path)
			if len(q.Terms) > 0 && matchAllFilters(q.Terms, base) {
				score += 50
			} else if len(q.FileFilters) > 0 && matchAllFilters(q.FileFilters, base) {
				score += 50
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
		for _, m := range matches {
			res.Files = append(res.Files, SearchFileMatch{Path: m.path, InChange: m.inChange})
		}
	}

	// 3. Go Symbol search.
	if wantSymbols {
		maxSyms := opts.MaxSymbols
		if len(q.SymbolFilters) == 0 && len(q.PkgFilters) == 0 {
			maxSyms = min(maxSyms, 15)
		}
		res.Symbols = searchGoSymbols(ctx, opts.Repo, blobRepo, searchRev, rootMod, allBlobs, changedSet, q, maxSyms)
	}

	// 4. Review Comment search.
	if searchComments {
		res.Comments = searchReviewComments(opts, q, opts.MaxComments)
	}

	// 5. Code line search.
	if searchCodeArea && q.hasPositiveTerm() {
		res.Code, res.TotalCode, res.Truncated = searchCodeLines(ctx, opts, searchRev, rootMod, blobRepo, allBlobs, changedSet, q)
	}

	return res, nil
}

func isTestdataGoPath(p string) bool {
	return strings.HasPrefix(p, "testdata/") || strings.Contains(p, "/testdata/")
}

func searchGoSymbols(ctx context.Context, repo vcs.Repo, blobRepo vcs.AllBlobRepo, rev, rootMod string, allBlobs []vcs.GoFileBlob, changedSet map[string]bool, q SearchQuery, limit int) []SearchSymbolMatch {
	hasFileFilter := len(q.FileFilters) > 0

	// Pre-filter Go files before streaming blobs whenever a positive p: filter or path-qualified
	// symbol term requires a '/' in the directory/import path (which a bare Go package name cannot contain).
	var pathOnlyPkgFilters []SearchTerm
	for _, pf := range q.PkgFilters {
		if !pf.Negated && strings.Contains(pf.Raw, "/") && (pf.Literal || !strings.ContainsAny(pf.Raw, "|(?*{[")) {
			pathOnlyPkgFilters = append(pathOnlyPkgFilters, pf)
		}
	}
	for _, sf := range q.SymbolFilters {
		if pre, ok := symPathPreFilter(sf.Term, q.CaseSensitive); ok {
			pathOnlyPkgFilters = append(pathOnlyPkgFilters, pre)
		}
	}
	for _, t := range q.Terms {
		if pre, ok := symPathPreFilter(t, q.CaseSensitive); ok {
			pathOnlyPkgFilters = append(pathOnlyPkgFilters, pre)
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
		return nil
	}

	// Check cache first; stream and parse only uncached Go blobs (in parallel for large repos).
	parsedByIdx := make([]cachedGoFileSyms, len(goBlobs))
	var missIdx []int
	var missOIDs []string

	goSymCache.mu.RLock()
	for i, b := range goBlobs {
		if b.OID != "" {
			if cached, ok := goSymCache.data[b.OID]; ok {
				parsedByIdx[i] = cached
				continue
			}
			missIdx = append(missIdx, i)
			missOIDs = append(missOIDs, b.OID)
		} else {
			missIdx = append(missIdx, i)
		}
	}
	goSymCache.mu.RUnlock()

	if len(missOIDs) > 0 && blobRepo != nil && len(missOIDs) == len(missIdx) {
		if len(missOIDs) <= 16 {
			_ = blobRepo.StreamBlobs(missOIDs, func(k int, oid string, data []byte) {
				if ctx.Err() != nil {
					return
				}
				idx := missIdx[k]
				parsedByIdx[idx] = parseAndCacheGoSyms(oid, data)
			})
		} else {
			type parseJob struct {
				idx  int
				oid  string
				data []byte
			}
			workers := min(runtime.NumCPU(), 8)
			if workers < 2 {
				workers = 2
			}
			jobs := make(chan parseJob, workers*4)
			parsedOK := make([]bool, len(goBlobs))
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for job := range jobs {
						if ctx.Err() != nil {
							continue
						}
						parsedByIdx[job.idx] = extractGoSymsFromBytes(job.data)
						parsedOK[job.idx] = true
					}
				}()
			}
			// Stream in chunks so keystroke cancellation stops git cat-file promptly.
			const streamChunk = 256
			for start := 0; start < len(missOIDs) && ctx.Err() == nil; start += streamChunk {
				end := min(start+streamChunk, len(missOIDs))
				_ = blobRepo.StreamBlobs(missOIDs[start:end], func(k int, oid string, data []byte) {
					if ctx.Err() != nil {
						return
					}
					idx := missIdx[start+k]
					if len(data) == 0 {
						parsedOK[idx] = true
						return
					}
					// Copy data because StreamBlobs may reuse buffers across callbacks.
					buf := append([]byte(nil), data...)
					jobs <- parseJob{idx: idx, oid: oid, data: buf}
				})
			}
			close(jobs)
			wg.Wait()

			parsedCount := 0
			for _, idx := range missIdx {
				if parsedOK[idx] {
					parsedCount++
				}
			}
			if parsedCount > 0 {
				goSymCache.mu.Lock()
				if len(goSymCache.data)+parsedCount >= maxCachedGoSymFiles {
					goSymCache.data = make(map[string]cachedGoFileSyms)
				}
				for k, idx := range missIdx {
					if oid := missOIDs[k]; oid != "" && parsedOK[idx] {
						goSymCache.data[oid] = parsedByIdx[idx]
					}
				}
				goSymCache.mu.Unlock()
			}
		}
	} else {
		for _, idx := range missIdx {
			if ctx.Err() != nil {
				break
			}
			data, err := repo.Content(rev, goBlobs[idx].Path)
			if err != nil {
				continue
			}
			parsedByIdx[idx] = parseAndCacheGoSyms(goBlobs[idx].OID, data)
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
	cleanNeedle := strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(needle, "^"), "$"), `\.`, "."))

	var candsBuf [6]string
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

		for _, sym := range parsed.syms {
			// Build Code Search style qualified names:
			//   Server, Server.ServeHTTP, http.Server, net/http.Server, src/net/http.Server, mod/pkg.Server
			nCands := 0
			candsBuf[nCands] = sym.Name
			nCands++
			if sym.ShortName != sym.Name {
				candsBuf[nCands] = sym.ShortName
				nCands++
			}
			pkgQual := ""
			if parsed.pkgName != "" {
				pkgQual = parsed.pkgName + "." + sym.Name
				candsBuf[nCands] = pkgQual
				nCands++
			}
			importQual := ""
			if importDir != "." && importDir != "" {
				importQual = importDir + "." + sym.Name
				if importQual != pkgQual {
					candsBuf[nCands] = importQual
					nCands++
				}
			}
			dirQual := ""
			if dir != "." && dir != "" && dir != importDir {
				dirQual = dir + "." + sym.Name
				candsBuf[nCands] = dirQual
				nCands++
			}
			modQual := ""
			if modImport != "" {
				modQual = modImport + "." + sym.Name
				if modQual != importQual && modQual != dirQual && modQual != pkgQual {
					candsBuf[nCands] = modQual
					nCands++
				}
			}
			cands := candsBuf[:nCands]

			ok := true
			for _, sf := range q.SymbolFilters {
				matched := false
				if symMatchesKind(sf.Kind, sym.Kind) {
					for _, cand := range cands {
						if sf.Term.MatchString(cand) {
							matched = true
							break
						}
					}
				}
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
				// All positive terms must match a candidate form, and no negated term may match any form.
				anyNeg := false
				for _, t := range q.Terms {
					if !t.Negated {
						continue
					}
					for _, cand := range cands {
						if t.MatchString(cand) {
							anyNeg = true
							break
						}
					}
					if anyNeg {
						break
					}
				}
				if anyNeg {
					continue
				}
				allPosMatched := false
				for _, cand := range cands {
					posOK := true
					for _, t := range q.Terms {
						if t.Negated {
							continue
						}
						if !t.MatchString(cand) {
							posOK = false
							break
						}
					}
					if posOK {
						allPosMatched = true
						break
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
				lowPkg := strings.ToLower(pkgQual)
				lowImp := strings.ToLower(importQual)
				lowDir := strings.ToLower(dirQual)
				lowMod := strings.ToLower(modQual)
				switch {
				case lowShort == cleanNeedle || lowName == cleanNeedle || lowPkg == cleanNeedle || lowImp == cleanNeedle || lowDir == cleanNeedle || lowMod == cleanNeedle:
					score += 200
				case strings.HasSuffix(lowImp, "/"+cleanNeedle) || strings.HasSuffix(lowDir, "/"+cleanNeedle) || strings.HasSuffix(lowMod, "/"+cleanNeedle):
					score += 160
				case strings.HasPrefix(lowShort, cleanNeedle) || strings.HasPrefix(lowName, cleanNeedle) || strings.HasPrefix(lowPkg, cleanNeedle) || strings.HasPrefix(lowImp, cleanNeedle) || strings.HasPrefix(lowDir, cleanNeedle) || strings.HasPrefix(lowMod, cleanNeedle):
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
	return out
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

	var tvs []*ThreadView
	var scope *ViewScope
	if opts.Change != nil && opts.Repo != nil && opts.TargetRev != "" {
		s := NewViewScope(opts.Snapshots, opts.BaseRev, opts.TargetRev, opts.IsLiveTarget)
		scope = &s
		tvs = ReanchorThreadsForView(opts.Repo, opts.Change, opts.Snapshots, opts.TargetRev, opts.BaseRev, opts.IsLiveTarget, rawThreads, nil)
	} else {
		tvs = make([]*ThreadView, len(rawThreads))
		for i, th := range rawThreads {
			tvs[i] = &ThreadView{
				Thread:       th,
				CurrentStart: th.StartLine,
				CurrentEnd:   th.EndLine,
				Stale:        th.Stale,
			}
		}
	}

	var hits []SearchCommentHit
	for _, tv := range tvs {
		th := tv.Thread
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

		inView := true
		side := th.Side
		if scope != nil {
			_, inView = scope.ThreadSide(th)
			if inView {
				side = tv.DisplaySide()
			}
		}

		perMessage := len(q.Terms) > 0 || len(q.AuthorFilters) > 0 || q.WantDraft
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
			hits = append(hits, SearchCommentHit{
				ThreadID:      th.ID,
				MessageID:     firstMsg.ID,
				ChangeKey:     th.ChangeKey,
				Path:          cleanPath,
				Side:          side,
				Line:          tv.CurrentStart,
				EndLine:       tv.CurrentEnd,
				SnapshotNum:   th.SnapshotNum,
				Author:        firstMsg.Author,
				Resolved:      th.Resolved,
				Draft:         firstMsg.Draft,
				Stale:         tv.Stale,
				InCurrentView: inView,
				Snippet:       buildCommentSearchSnippet(firstMsg.Body, nil),
				ActivityTime:  latestTime,
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
			snippet := buildCommentSearchSnippet(m.Body, q.Terms)
			if len(q.Terms) > 0 {
				combined := m.Body + "\n" + m.Author + "\n" + cleanPath
				if _, ok := findMatchSpans(q.Terms, combined); !ok {
					continue
				}
				spans, _ = findMatchSpans(q.Terms, snippet)
			}

			actTime := m.UpdatedAt
			if m.CreatedAt.After(actTime) {
				actTime = m.CreatedAt
			}
			if actTime.IsZero() {
				actTime = th.CreatedAt
			}

			hits = append(hits, SearchCommentHit{
				ThreadID:      th.ID,
				MessageID:     m.ID,
				ChangeKey:     th.ChangeKey,
				Path:          cleanPath,
				Side:          side,
				Line:          tv.CurrentStart,
				EndLine:       tv.CurrentEnd,
				SnapshotNum:   th.SnapshotNum,
				Author:        m.Author,
				Resolved:      th.Resolved,
				Draft:         m.Draft,
				Stale:         tv.Stale,
				InCurrentView: inView,
				Snippet:       snippet,
				Spans:         spans,
				ActivityTime:  actTime,
			})
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if !hits[i].ActivityTime.Equal(hits[j].ActivityTime) {
			return hits[i].ActivityTime.After(hits[j].ActivityTime)
		}
		return hits[i].MessageID > hits[j].MessageID
	})

	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func buildCommentSearchSnippet(body string, terms []SearchTerm) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	// Prefer the first line that matches a positive term.
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, t := range terms {
			if !t.Negated && t.MatchString(trimmed) {
				return truncateUTF8(trimmed, 200)
			}
		}
	}
	// Otherwise collapse the start of the message.
	flat := strings.Join(strings.Fields(body), " ")
	return truncateUTF8(flat, 200)
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

	// Pre-filter terms that don't use line anchors (^/$) against the raw blob before splitting lines.
	var blobPrecheck []*regexp.Regexp
	for _, t := range q.Terms {
		if t.Negated || t.Re == nil {
			continue
		}
		if t.Literal || (!strings.ContainsAny(t.Raw, "^$") && !strings.Contains(t.Raw, `\A`) && !strings.Contains(t.Raw, `\z`) && !strings.Contains(t.Raw, `\Z`)) {
			blobPrecheck = append(blobPrecheck, t.Re)
		}
	}

	scanBytes := func(filePath string, inChange bool, data []byte) {
		if stopScanning || len(data) == 0 || len(data) > 1<<20 || diff.IsBinary(data) {
			return
		}
		for _, re := range blobPrecheck {
			if !re.Match(data) {
				return
			}
		}
		lines := diff.SplitLines(data)
		var matched []SearchCodeLine
		for i, line := range lines {
			spans, ok := findMatchSpans(q.Terms, line)
			if !ok {
				continue
			}
			col := 0
			if len(spans) > 0 {
				col = spans[0].Start
			}
			matched = append(matched, SearchCodeLine{
				Line:  i + 1,
				Col:   col,
				Text:  line,
				Spans: spans,
			})
			totalLines++
			if totalLines >= maxTotal {
				truncated = true
				stopScanning = true
				break
			}
			if len(matched) >= maxPerFile {
				if i+1 < len(lines) {
					truncated = true
				}
				break
			}
		}
		if len(matched) > 0 {
			groups = append(groups, SearchCodeGroup{
				Path:     filePath,
				InChange: inChange,
				Lines:    matched,
			})
			if len(groups) >= maxFiles {
				truncated = true
				stopScanning = true
			}
		}
	}

	// 1. Search /COMMIT_MSG first when viewing a change's target revision and filters allow it.
	if opts.Change != nil && searchRev == opts.TargetRev && filePassesFilters(q, "COMMIT_MSG", true) && len(q.PkgFilters) == 0 {
		if data, err := opts.Repo.Content(searchRev, vcs.CommitMsgPath); err == nil && len(data) > 0 {
			scanBytes("COMMIT_MSG", true, data)
		}
	}

	// 2. Filter candidate blobs and put changed files first.
	var changedCandidates, otherCandidates []vcs.GoFileBlob
	for _, b := range allBlobs {
		inCh := changedSet[b.Path]
		if !filePassesFilters(q, b.Path, inCh) {
			continue
		}
		dir := path.Dir(b.Path)
		if !pkgFiltersMatch(q.PkgFilters, dir, "", moduleImportPath(rootMod, dir)) {
			continue
		}
		if inCh {
			changedCandidates = append(changedCandidates, b)
		} else {
			otherCandidates = append(otherCandidates, b)
		}
	}
	candidates := slices.Concat(changedCandidates, otherCandidates)

	// 3. Stream in chunks so we stop early as soon as result cap is reached.
	if blobRepo != nil {
		const chunkSize = 64
		for start := 0; start < len(candidates) && !stopScanning && ctx.Err() == nil; start += chunkSize {
			end := min(start+chunkSize, len(candidates))
			chunk := candidates[start:end]
			oids := make([]string, len(chunk))
			allHaveOID := true
			for i, b := range chunk {
				if b.OID == "" {
					allHaveOID = false
					break
				}
				oids[i] = b.OID
			}
			if !allHaveOID {
				for _, b := range chunk {
					if stopScanning || ctx.Err() != nil {
						break
					}
					if data, err := opts.Repo.Content(searchRev, b.Path); err == nil {
						scanBytes(b.Path, changedSet[b.Path], data)
					}
				}
				continue
			}
			_ = blobRepo.StreamBlobs(oids, func(k int, _ string, data []byte) {
				if stopScanning || ctx.Err() != nil {
					return
				}
				b := chunk[k]
				scanBytes(b.Path, changedSet[b.Path], data)
			})
		}
	} else {
		for _, b := range candidates {
			if stopScanning || ctx.Err() != nil {
				break
			}
			if data, err := opts.Repo.Content(searchRev, b.Path); err == nil {
				scanBytes(b.Path, changedSet[b.Path], data)
			}
		}
	}

	return groups, totalLines, truncated
}
