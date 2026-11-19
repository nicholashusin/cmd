package diff

import "strings"

// wsKeys returns lines without their whitespace outside of string literals,
// so that lines only differing in such whitespace compare equal. It also
// returns the string literals of each line.
func wsKeys(lines []string, lang Lang) (keys []string, strs [][]Span) {
	toks := HighlightLines(lines, lang)
	keys = make([]string, len(lines))
	strs = make([][]Span, len(lines))
	for i, line := range lines {
		for _, tok := range toks[i] {
			if tok.Class == TokenString || tok.Class == TokenImportString {
				strs[i] = append(strs[i], Span{Start: tok.Start, End: tok.End})
			}
		}
		var b strings.Builder
		b.Grow(len(line))
		for j := range len(line) {
			if !ignorable(line, strs[i], j) {
				b.WriteByte(line[j])
			}
		}
		keys[i] = b.String()
	}
	return keys, strs
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
