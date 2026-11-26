package engine

import (
	"bytes"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/diff"
)

var lowerASCII [256]byte

func init() {
	for i := range 256 {
		b := byte(i)
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		lowerASCII[i] = b
	}
}

func isPureASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func hasASCIIKS(b []byte) bool {
	for _, c := range b {
		if c == 'k' || c == 's' {
			return true
		}
	}
	return false
}

// indexFoldASCIIBytes finds the first ASCII case-insensitive occurrence of
// lowerNeedle (which must be non-empty, pure ASCII, and already lowercase) in haystack.
func indexFoldASCIIBytes(haystack, lowerNeedle []byte) int {
	n := len(lowerNeedle)
	if n == 0 || len(haystack) < n {
		return -1
	}
	limit := len(haystack) - n

	// If lowerNeedle contains a non-letter byte (e.g. '.', '_', '/', ' ', digits),
	// anchor on it with SIMD bytes.IndexByte for fast skipping on large blobs.
	anchorIdx := -1
	for i, c := range lowerNeedle {
		if c < 'a' || c > 'z' {
			anchorIdx = i
			break
		}
	}
	if anchorIdx >= 0 {
		anchor := lowerNeedle[anchorIdx]
		pos := 0
		for pos <= limit {
			rel := bytes.IndexByte(haystack[pos+anchorIdx:limit+anchorIdx+1], anchor)
			if rel < 0 {
				return -1
			}
			cand := pos + rel
			if equalFoldASCIIBytes(haystack[cand:cand+n], lowerNeedle) {
				return cand
			}
			pos = cand + 1
		}
		return -1
	}

	// All bytes in lowerNeedle are 'a'..'z'; (c | 0x20) == b matches iff c == b || c == b-32.
	b0 := lowerNeedle[0]
	if n == 1 {
		for i := 0; i <= limit; i++ {
			if (haystack[i] | 0x20) == b0 {
				return i
			}
		}
		return -1
	}
	bLast := lowerNeedle[n-1]
	for i := 0; i <= limit; i++ {
		if (haystack[i]|0x20) != b0 || (haystack[i+n-1]|0x20) != bLast {
			continue
		}
		match := true
		for j := 1; j < n-1; j++ {
			if (haystack[i+j] | 0x20) != lowerNeedle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func equalFoldASCIIBytes(a, lowerB []byte) bool {
	for i, b := range lowerB {
		if 'a' <= b && b <= 'z' {
			if (a[i] | 0x20) != b {
				return false
			}
		} else if a[i] != b {
			return false
		}
	}
	return true
}

// indexFoldASCIIString finds the first ASCII case-insensitive occurrence of
// lowerNeedle (non-empty, pure ASCII, lowercase) in haystack.
func indexFoldASCIIString(haystack, lowerNeedle string) int {
	n := len(lowerNeedle)
	if n == 0 || len(haystack) < n {
		return -1
	}
	limit := len(haystack) - n

	anchorIdx := -1
	for i := 0; i < n; i++ {
		c := lowerNeedle[i]
		if c < 'a' || c > 'z' {
			anchorIdx = i
			break
		}
	}
	if anchorIdx >= 0 {
		anchor := lowerNeedle[anchorIdx]
		pos := 0
		for pos <= limit {
			rel := strings.IndexByte(haystack[pos+anchorIdx:limit+anchorIdx+1], anchor)
			if rel < 0 {
				return -1
			}
			cand := pos + rel
			match := true
			for j := 0; j < n; j++ {
				b := lowerNeedle[j]
				if 'a' <= b && b <= 'z' {
					if (haystack[cand+j] | 0x20) != b {
						match = false
						break
					}
				} else if haystack[cand+j] != b {
					match = false
					break
				}
			}
			if match {
				return cand
			}
			pos = cand + 1
		}
		return -1
	}

	b0 := lowerNeedle[0]
	if n == 1 {
		for i := 0; i <= limit; i++ {
			if (haystack[i] | 0x20) == b0 {
				return i
			}
		}
		return -1
	}
	bLast := lowerNeedle[n-1]
	for i := 0; i <= limit; i++ {
		if (haystack[i]|0x20) != b0 || (haystack[i+n-1]|0x20) != bLast {
			continue
		}
		match := true
		for j := 1; j < n-1; j++ {
			if (haystack[i+j] | 0x20) != lowerNeedle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func regexpWalk(re *syntax.Regexp, fn func(*syntax.Regexp) bool) bool {
	if re == nil {
		return false
	}
	if fn(re) {
		return true
	}
	for _, sub := range re.Sub {
		if regexpWalk(sub, fn) {
			return true
		}
	}
	return false
}

func hasTextAnchor(re *syntax.Regexp) bool {
	return regexpWalk(re, func(n *syntax.Regexp) bool {
		return n.Op == syntax.OpBeginText || n.Op == syntax.OpEndText
	})
}

func hasEndLine(re *syntax.Regexp) bool {
	return regexpWalk(re, func(n *syntax.Regexp) bool {
		return n.Op == syntax.OpEndLine
	})
}

func hasContextAnchor(re *syntax.Regexp) bool {
	return regexpWalk(re, func(n *syntax.Regexp) bool {
		return n.Op == syntax.OpBeginLine || n.Op == syntax.OpBeginText ||
			n.Op == syntax.OpWordBoundary || n.Op == syntax.OpNoWordBoundary
	})
}

func isEmptyBranch(re *syntax.Regexp) bool {
	for re != nil && re.Op == syntax.OpCapture && len(re.Sub) == 1 {
		re = re.Sub[0]
	}
	return re != nil && re.Op == syntax.OpEmptyMatch
}

// stripEmptyAltBranches drops top-level empty alternation branches (e.g. mid-typing
// "foo|", "^foo|", "\bfoo|") so the compiled regex stays non-empty-matching.
func stripEmptyAltBranches(pattern string) (string, bool) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil || parsed.Op != syntax.OpAlternate {
		return pattern, false
	}
	kept := make([]*syntax.Regexp, 0, len(parsed.Sub))
	dropped := false
	for _, sub := range parsed.Sub {
		if isEmptyBranch(sub) {
			dropped = true
			continue
		}
		kept = append(kept, sub)
	}
	if !dropped || len(kept) == 0 {
		return pattern, false
	}
	if len(kept) == 1 {
		return kept[0].String(), true
	}
	parsed.Sub = kept
	return parsed.String(), true
}

const maxEmptyMatchScan = 4096

// findNonEmptySpansBytes finds up to maxMatches non-empty matches for a regex
// that can match the empty string (e.g. "a*" or "(?:^foo)?").
func findNonEmptySpansBytes(re *regexp.Regexp, line []byte, maxMatches int, hasAnchor bool) []SearchMatchSpan {
	if hasAnchor || len(line) <= maxEmptyMatchScan {
		locs := re.FindAllIndex(line, maxEmptyMatchScan)
		var spans []SearchMatchSpan
		for _, loc := range locs {
			if loc[1] > loc[0] {
				spans = append(spans, SearchMatchSpan{Start: loc[0], End: loc[1]})
				if len(spans) >= maxMatches {
					break
				}
			}
		}
		return spans
	}
	var spans []SearchMatchSpan
	off := 0
	for steps := 0; off < len(line) && len(spans) < maxMatches && steps < maxEmptyMatchScan*4; steps++ {
		loc := re.FindIndex(line[off:])
		if loc == nil {
			break
		}
		if loc[1] > loc[0] {
			spans = append(spans, SearchMatchSpan{Start: off + loc[0], End: off + loc[1]})
			off += loc[1]
		} else {
			_, w := utf8.DecodeRune(line[off+loc[0]:])
			if w <= 0 {
				w = 1
			}
			off += loc[0] + w
		}
	}
	return spans
}

func hasNonEmptyMatchString(re *regexp.Regexp, s string, hasAnchor bool) bool {
	if hasAnchor || len(s) <= maxEmptyMatchScan {
		for _, loc := range re.FindAllStringIndex(s, maxEmptyMatchScan) {
			if loc[1] > loc[0] {
				return true
			}
		}
		return false
	}
	off := 0
	for steps := 0; off < len(s) && steps < maxEmptyMatchScan*4; steps++ {
		loc := re.FindStringIndex(s[off:])
		if loc == nil {
			return false
		}
		if loc[1] > loc[0] {
			return true
		}
		_, w := utf8.DecodeRuneInString(s[off+loc[0]:])
		if w <= 0 {
			w = 1
		}
		off += loc[0] + w
	}
	return false
}

func hasNonEmptyMatchBytes(re *regexp.Regexp, b []byte, hasAnchor bool) bool {
	if hasAnchor || len(b) <= maxEmptyMatchScan {
		for _, loc := range re.FindAllIndex(b, maxEmptyMatchScan) {
			if loc[1] > loc[0] {
				return true
			}
		}
		return false
	}
	off := 0
	for steps := 0; off < len(b) && steps < maxEmptyMatchScan*4; steps++ {
		loc := re.FindIndex(b[off:])
		if loc == nil {
			return false
		}
		if loc[1] > loc[0] {
			return true
		}
		_, w := utf8.DecodeRune(b[off+loc[0]:])
		if w <= 0 {
			w = 1
		}
		off += loc[0] + w
	}
	return false
}

// blobBloom is a content-addressed 2048-bit case-folded ASCII trigram bloom filter
// keyed by immutable Git blob OID. It lets repeat Code Search queries skip 95-99%
// of non-matching blobs before reading them from git cat-file.
type blobBloom struct {
	bits        [32]uint64
	skip        bool // empty, >1MB, or binary
	hasNonASCII bool // true if any byte >= 0x80 (for Unicode k/s case-fold soundness)
}

const maxCachedBlobBlooms = 32768

var blobBloomCache = struct {
	mu   sync.RWMutex
	data map[string]blobBloom
}{
	data: make(map[string]blobBloom),
}

func trigramBit(a, b, c byte) uint16 {
	h := uint32(a) | uint32(b)<<8 | uint32(c)<<16
	h ^= h >> 13
	h *= 0x5bd1e995
	h ^= h >> 15
	return uint16(h & 2047)
}

func buildBlobBloom(data []byte) blobBloom {
	if len(data) == 0 || len(data) > 1<<20 || diff.IsBinary(data) {
		return blobBloom{skip: true}
	}
	var bl blobBloom
	var orAll byte
	orAll |= data[0]
	if len(data) > 1 {
		orAll |= data[1]
	}
	if len(data) >= 3 {
		a := lowerASCII[data[0]]
		b := lowerASCII[data[1]]
		for i := 2; i < len(data); i++ {
			raw := data[i]
			orAll |= raw
			c := lowerASCII[raw]
			bit := trigramBit(a, b, c)
			bl.bits[bit>>6] |= 1 << (bit & 63)
			a, b = b, c
		}
	}
	bl.hasNonASCII = orAll >= 0x80
	return bl
}

func putBlobBlooms(entries []oidBloom) {
	if len(entries) == 0 {
		return
	}
	blobBloomCache.mu.Lock()
	if len(blobBloomCache.data)+len(entries) >= maxCachedBlobBlooms {
		blobBloomCache.data = make(map[string]blobBloom, len(entries)*2)
	}
	for _, e := range entries {
		if e.oid != "" {
			blobBloomCache.data[e.oid] = e.bloom
		}
	}
	blobBloomCache.mu.Unlock()
}

type oidBloom struct {
	oid   string
	bloom blobBloom
}

type trigramLit struct {
	bits     []uint16
	hasKS    bool
	foldCase bool
}

// bloomQuery represents AND-of-OR required ASCII trigram checks derived from positive search terms.
type bloomQuery struct {
	groups [][]trigramLit
}

func makeTrigramLit(s string, foldCase bool) (trigramLit, bool) {
	if len(s) < 3 || !isPureASCII(s) {
		return trigramLit{}, false
	}
	bits := make([]uint16, 0, len(s)-2)
	hasKS := false
	for i := 0; i < len(s); i++ {
		c := lowerASCII[s[i]]
		if c == 'k' || c == 's' {
			hasKS = true
		}
	}
	a := lowerASCII[s[0]]
	b := lowerASCII[s[1]]
	for i := 2; i < len(s); i++ {
		c := lowerASCII[s[i]]
		bits = append(bits, trigramBit(a, b, c))
		a, b = b, c
	}
	return trigramLit{bits: bits, hasKS: hasKS, foldCase: foldCase}, true
}

func splitASCIILitRuns(s string) []string {
	var runs []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' || s[i] == '\r' || s[i] >= 0x80 {
			if i-start >= 3 {
				runs = append(runs, s[start:i])
			}
			start = i + 1
		}
	}
	return runs
}

type reqLit struct {
	lit      string
	foldCase bool
}

func collectRequiredLitGroups(re *syntax.Regexp) [][]reqLit {
	if re == nil {
		return nil
	}
	switch re.Op {
	case syntax.OpLiteral:
		runs := splitASCIILitRuns(string(re.Rune))
		if len(runs) == 0 {
			return nil
		}
		fold := re.Flags&syntax.FoldCase != 0
		out := make([][]reqLit, 0, len(runs))
		for _, r := range runs {
			out = append(out, []reqLit{{lit: r, foldCase: fold}})
		}
		return out
	case syntax.OpCapture, syntax.OpPlus:
		if len(re.Sub) > 0 {
			return collectRequiredLitGroups(re.Sub[0])
		}
		return nil
	case syntax.OpConcat:
		var out [][]reqLit
		var cur strings.Builder
		curFold := false
		flush := func() {
			if cur.Len() >= 3 {
				for _, r := range splitASCIILitRuns(cur.String()) {
					out = append(out, []reqLit{{lit: r, foldCase: curFold}})
				}
			}
			cur.Reset()
			curFold = false
		}
		for _, sub := range re.Sub {
			if sub.Op == syntax.OpLiteral {
				subFold := sub.Flags&syntax.FoldCase != 0
				for _, r := range sub.Rune {
					if r < 0x80 && r != '\n' && r != '\r' {
						cur.WriteByte(byte(r))
						if subFold {
							curFold = true
						}
					} else {
						flush()
					}
				}
				continue
			}
			flush()
			out = append(out, collectRequiredLitGroups(sub)...)
		}
		flush()
		return out
	case syntax.OpAlternate:
		if len(re.Sub) == 0 || len(re.Sub) > 16 {
			return nil
		}
		alts := make([]reqLit, 0, len(re.Sub))
		for _, sub := range re.Sub {
			subGroups := collectRequiredLitGroups(sub)
			var best reqLit
			for _, g := range subGroups {
				if len(g) == 1 && len(g[0].lit) > len(best.lit) {
					best = g[0]
				}
			}
			if best.lit == "" {
				return nil
			}
			alts = append(alts, best)
		}
		return [][]reqLit{alts}
	default:
		return nil
	}
}

func buildBloomQuery(terms []SearchTerm, caseSensitive bool) bloomQuery {
	var bq bloomQuery
	for _, t := range terms {
		if t.Negated || t.Re == nil || t.matchesEmpty {
			continue
		}
		if t.exactLit != "" {
			for _, r := range splitASCIILitRuns(t.exactLit) {
				if tl, ok := makeTrigramLit(r, !caseSensitive); ok {
					bq.groups = append(bq.groups, []trigramLit{tl})
				}
			}
			continue
		}
		parsed, err := syntax.Parse(t.Re.String(), syntax.Perl)
		if err != nil {
			continue
		}
		for _, g := range collectRequiredLitGroups(parsed.Simplify()) {
			var tls []trigramLit
			okAll := true
			for _, rl := range g {
				tl, ok := makeTrigramLit(rl.lit, !caseSensitive || rl.foldCase)
				if !ok {
					okAll = false
					break
				}
				tls = append(tls, tl)
			}
			if okAll && len(tls) > 0 {
				bq.groups = append(bq.groups, tls)
			}
		}
	}
	return bq
}

func (bq bloomQuery) mightMatch(bl blobBloom) bool {
	if bl.skip {
		return false
	}
	for _, group := range bq.groups {
		groupOK := false
		for _, tl := range group {
			// Unicode U+017F ('ſ') and U+212A ('K') case-fold to ASCII 's'/'k'.
			if tl.foldCase && bl.hasNonASCII && tl.hasKS {
				groupOK = true
				break
			}
			allBits := true
			for _, bit := range tl.bits {
				if bl.bits[bit>>6]&(1<<(bit&63)) == 0 {
					allBits = false
					break
				}
			}
			if allBits {
				groupOK = true
				break
			}
		}
		if !groupOK {
			return false
		}
	}
	return true
}

// extractRequiredLitForTerm returns the longest guaranteed ASCII literal run
// for a single compiled regex pattern (used as a fast byte precheck before running RE2).
func extractRequiredLitForTerm(pattern string) (reqLit, bool) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return reqLit{}, false
	}
	groups := collectRequiredLitGroups(parsed.Simplify())
	var best reqLit
	for _, g := range groups {
		if len(g) == 1 && len(g[0].lit) > len(best.lit) {
			best = g[0]
		}
	}
	if len(best.lit) < 3 {
		return reqLit{}, false
	}
	return best, true
}

// extractGoPackageName scans past leading comments/whitespace to read "package <name>"
// without invoking go/parser.
func extractGoPackageName(data []byte) string {
	// Skip UTF-8 BOM if present.
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	i := 0
	n := len(data)
	skipSpaceAndComments := func() {
		for i < n {
			c := data[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				i++
				continue
			}
			if c == '/' && i+1 < n {
				if data[i+1] == '/' {
					i += 2
					for i < n && data[i] != '\n' {
						i++
					}
					continue
				}
				if data[i+1] == '*' {
					i += 2
					for i+1 < n && !(data[i] == '*' && data[i+1] == '/') {
						i++
					}
					if i+1 < n {
						i += 2
					} else {
						i = n
					}
					continue
				}
			}
			break
		}
	}
	skipSpaceAndComments()
	if i+7 >= n || string(data[i:i+7]) != "package" {
		return ""
	}
	next := data[i+7]
	if next != ' ' && next != '\t' && next != '\r' && next != '\n' && next != '/' {
		return ""
	}
	i += 7
	skipSpaceAndComments()
	start := i
	for i < n {
		c := data[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '/' {
			break
		}
		i++
	}
	return string(data[start:i])
}

// stripGoFuncBodies replaces the interiors of top-level Go function/method bodies
// with spaces while preserving every newline and exact byte offset. This lets
// extractGoSymsFromBytes parse declarations 4-5x faster on cold caches.
// Returns (data, false) if nothing was stripped or if unbalanced braces are seen.
func stripGoFuncBodies(data []byte) ([]byte, bool) {
	n := len(data)
	if n < 32 {
		return data, false
	}

	var out []byte
	stripped := false
	braceDepth := 0
	parenDepth := 0
	bracketDepth := 0
	inFuncHeader := false
	lineStart := true
	prevStructOrIface := false

	isIdentStart := func(c byte) bool {
		return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || c == '_' || c >= 0x80
	}
	isIdentCont := func(c byte) bool {
		return isIdentStart(c) || ('0' <= c && c <= '9')
	}

	i := 0
	for i < n {
		c := data[i]
		switch c {
		case ' ', '\t', '\r':
			i++
		case '\n':
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				lineStart = true
			}
			i++
		case '/':
			if i+1 < n && data[i+1] == '/' {
				i += 2
				for i < n && data[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < n && data[i+1] == '*' {
				i += 2
				for i+1 < n && !(data[i] == '*' && data[i+1] == '/') {
					i++
				}
				if i+1 < n {
					i += 2
				} else {
					return data, false
				}
				continue
			}
			prevStructOrIface = false
			lineStart = false
			i++
		case '"':
			prevStructOrIface = false
			lineStart = false
			i++
			for i < n {
				if data[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if data[i] == '"' {
					i++
					break
				}
				i++
			}
		case '`':
			prevStructOrIface = false
			lineStart = false
			i++
			for i < n && data[i] != '`' {
				i++
			}
			if i < n {
				i++
			}
		case '\'':
			prevStructOrIface = false
			lineStart = false
			i++
			for i < n {
				if data[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if data[i] == '\'' {
					i++
					break
				}
				i++
			}
		case '(':
			parenDepth++
			prevStructOrIface = false
			lineStart = false
			i++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
			prevStructOrIface = false
			lineStart = false
			i++
		case '[':
			bracketDepth++
			prevStructOrIface = false
			lineStart = false
			i++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
			prevStructOrIface = false
			lineStart = false
			i++
		case ';':
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				inFuncHeader = false
				lineStart = true
			}
			prevStructOrIface = false
			i++
		case '{':
			if inFuncHeader && braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 && !prevStructOrIface {
				bodyStart := i + 1
				bodyDepth := 1
				j := bodyStart
				for j < n && bodyDepth > 0 {
					bc := data[j]
					switch bc {
					case '{':
						bodyDepth++
						j++
					case '}':
						bodyDepth--
						if bodyDepth == 0 {
							goto bodyEndFound
						}
						j++
					case '/':
						if j+1 < n && data[j+1] == '/' {
							j += 2
							for j < n && data[j] != '\n' {
								j++
							}
							continue
						}
						if j+1 < n && data[j+1] == '*' {
							j += 2
							for j+1 < n && !(data[j] == '*' && data[j+1] == '/') {
								j++
							}
							if j+1 < n {
								j += 2
							} else {
								return data, false
							}
							continue
						}
						j++
					case '"':
						j++
						for j < n {
							if data[j] == '\\' && j+1 < n {
								j += 2
								continue
							}
							if data[j] == '"' {
								j++
								break
							}
							j++
						}
					case '`':
						j++
						for j < n && data[j] != '`' {
							j++
						}
						if j < n {
							j++
						}
					case '\'':
						j++
						for j < n {
							if data[j] == '\\' && j+1 < n {
								j += 2
								continue
							}
							if data[j] == '\'' {
								j++
								break
							}
							j++
						}
					default:
						j++
					}
				}
			bodyEndFound:
				if bodyDepth != 0 {
					return data, false
				}
				if j > bodyStart {
					if out == nil {
						out = make([]byte, n)
						copy(out, data)
					}
					for k := bodyStart; k < j; k++ {
						if out[k] != '\n' {
							out[k] = ' '
						}
					}
					stripped = true
				}
				i = j + 1
				inFuncHeader = false
				lineStart = true
				prevStructOrIface = false
				continue
			}
			braceDepth++
			prevStructOrIface = false
			lineStart = false
			i++
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				lineStart = true
			}
			prevStructOrIface = false
			i++
		default:
			if isIdentStart(c) {
				start := i
				i++
				for i < n && isIdentCont(data[i]) {
					i++
				}
				word := data[start:i]
				if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
					prevStructOrIface = false
					switch string(word) {
					case "func":
						if lineStart {
							inFuncHeader = true
						}
					case "type", "var", "const", "import", "package":
						inFuncHeader = false
					case "struct", "interface":
						prevStructOrIface = true
					}
				} else {
					prevStructOrIface = false
				}
				lineStart = false
				continue
			}
			prevStructOrIface = false
			lineStart = false
			i++
		}
	}

	if !stripped {
		return data, false
	}
	return out, true
}
