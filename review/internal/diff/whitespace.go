package diff

import "strings"

// CountDiffLines computes added and deleted line counts without running
// intraline character diffs. When ignoreWS is true, whitespace outside
// string literals is ignored.
func CountDiffLines(left, right []string, lang Lang, ignoreWS bool) (added, deleted int) {
	a, b := left, right
	if ignoreWS {
		a, _ = wsKeysImpl(left, lang, false)
		b, _ = wsKeysImpl(right, lang, false)
	}
	for _, e := range Edits(a, b) {
		switch e.Op {
		case OpInsert:
			added++
		case OpDelete:
			deleted++
		}
	}
	return added, deleted
}

// wsKeys returns lines without their whitespace outside of string literals,
// so that lines only differing in such whitespace compare equal. It also
// returns the string literals of each line.
func wsKeys(lines []string, lang Lang) (keys []string, strs [][]Span) {
	return wsKeysImpl(lines, lang, true)
}

func wsKeysImpl(lines []string, lang Lang, wantStrs bool) (keys []string, strs [][]Span) {
	keys = make([]string, len(lines))
	if wantStrs {
		strs = make([][]Span, len(lines))
	}

	state := stateNormal
	var spanBuf []Span
	var b strings.Builder

	for i, line := range lines {
		var lineStrs []Span
		if lang != LangNone {
			spanBuf = spanBuf[:0]
			spanBuf, state = scanLineStringSpans(line, lang, state, spanBuf)
			if len(spanBuf) > 0 {
				if wantStrs {
					lineStrs = append([]Span(nil), spanBuf...)
					strs[i] = lineStrs
				} else {
					lineStrs = spanBuf
				}
			}
		}

		if strings.IndexAny(line, " \t") < 0 {
			keys[i] = line
			continue
		}

		si := 0
		runStart := 0
		hasWS := false
		for j := 0; j < len(line); j++ {
			c := line[j]
			if c != ' ' && c != '\t' {
				continue
			}
			for si < len(lineStrs) && lineStrs[si].End <= j {
				si++
			}
			if si < len(lineStrs) && lineStrs[si].Start <= j {
				continue
			}
			if !hasWS {
				b.Reset()
				b.Grow(len(line))
				hasWS = true
			}
			if j > runStart {
				b.WriteString(line[runStart:j])
			}
			runStart = j + 1
		}
		if !hasWS {
			keys[i] = line
		} else {
			if runStart < len(line) {
				b.WriteString(line[runStart:])
			}
			keys[i] = b.String()
		}
	}
	return keys, strs
}

// scanLineStringSpans scans only comments and string literals on line, updating
// multi-line state, without allocating keywords/identifiers/doc-links.
func scanLineStringSpans(line string, lang Lang, state scanState, dst []Span) ([]Span, scanState) {
	pos := 0
	n := len(line)

	switch state {
	case stateBlockComment:
		end := strings.Index(line, "*/")
		if end == -1 {
			return dst, state
		}
		pos = end + 2
		state = stateNormal
	case stateRawStringGo:
		end := strings.IndexByte(line, '`')
		if end == -1 {
			if n > 0 {
				dst = append(dst, Span{Start: 0, End: n})
			}
			return dst, state
		}
		dst = append(dst, Span{Start: 0, End: end + 1})
		pos = end + 1
		state = stateNormal
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
		if pos > 0 {
			dst = append(dst, Span{Start: 0, End: pos})
		}
		if !closed {
			return dst, state
		}
		state = stateNormal
	case stateTripleQuotePyDouble:
		end := strings.Index(line, `"""`)
		if end == -1 {
			if n > 0 {
				dst = append(dst, Span{Start: 0, End: n})
			}
			return dst, state
		}
		dst = append(dst, Span{Start: 0, End: end + 3})
		pos = end + 3
		state = stateNormal
	case stateTripleQuotePySingle:
		end := strings.Index(line, `'''`)
		if end == -1 {
			if n > 0 {
				dst = append(dst, Span{Start: 0, End: n})
			}
			return dst, state
		}
		dst = append(dst, Span{Start: 0, End: end + 3})
		pos = end + 3
		state = stateNormal
	}

	hasSlashComment := lang == LangGo || lang == LangJS || lang == LangC || lang == LangRust || lang == LangCSS
	hasBlockComment := hasSlashComment || lang == LangSQL
	hasHashComment := lang == LangPython || lang == LangShell || lang == LangYAML || lang == LangTOML

	for pos < n {
		c := line[pos]
		if c == '/' && pos+1 < n {
			if hasSlashComment && line[pos+1] == '/' {
				return dst, stateNormal
			}
			if hasBlockComment && line[pos+1] == '*' {
				end := strings.Index(line[pos+2:], "*/")
				if end == -1 {
					return dst, stateBlockComment
				}
				pos += 2 + end + 2
				continue
			}
		}
		if hasHashComment && c == '#' {
			return dst, stateNormal
		}
		if lang == LangSQL && c == '-' && pos+1 < n && line[pos+1] == '-' {
			return dst, stateNormal
		}
		if lang == LangPython && pos+2 < n && (c == '"' || c == '\'') && line[pos+1] == c && line[pos+2] == c {
			delim := `"""`
			nextState := stateTripleQuotePyDouble
			if c == '\'' {
				delim = `'''`
				nextState = stateTripleQuotePySingle
			}
			end := strings.Index(line[pos+3:], delim)
			if end == -1 {
				dst = append(dst, Span{Start: pos, End: n})
				return dst, nextState
			}
			strEnd := pos + 3 + end + 3
			dst = append(dst, Span{Start: pos, End: strEnd})
			pos = strEnd
			continue
		}
		if lang == LangGo && c == '`' {
			end := strings.IndexByte(line[pos+1:], '`')
			if end == -1 {
				dst = append(dst, Span{Start: pos, End: n})
				return dst, stateRawStringGo
			}
			strEnd := pos + 1 + end + 1
			dst = append(dst, Span{Start: pos, End: strEnd})
			pos = strEnd
			continue
		}
		if lang == LangJS && c == '`' {
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
			dst = append(dst, Span{Start: start, End: pos})
			if !closed {
				return dst, stateTemplateStringJS
			}
			continue
		}
		if c == '"' || (c == '\'' && (lang != LangRust || isRustCharLiteral(line, pos))) {
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
				if line[pos] == c {
					pos++
					break
				}
				pos++
			}
			dst = append(dst, Span{Start: start, End: pos})
			continue
		}
		pos++
	}
	return dst, stateNormal
}

// ignorable reports whether line[i] is whitespace outside of the string
// literals strs.
func ignorable(line string, strs []Span, i int) bool {
	if line[i] != ' ' && line[i] != '\t' {
		return false
	}
	for _, s := range strs {
		if s.Start <= i && i < s.End {
			return false
		}
	}
	return true
}

// trimWS shrinks spans on line to exclude ignorable whitespace at their ends,
// dropping the spans that only cover such whitespace.
func trimWS(line string, strs, spans []Span) []Span {
	var trimmed []Span
	for _, s := range spans {
		for s.Start < s.End && ignorable(line, strs, s.Start) {
			s.Start++
		}
		for s.Start < s.End && ignorable(line, strs, s.End-1) {
			s.End--
		}
		if s.Start < s.End {
			trimmed = append(trimmed, s)
		}
	}
	return trimmed
}
