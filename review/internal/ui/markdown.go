package ui

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"nicholas.sh/cmd/review/internal/diff"
)

var (
	reBlockquote   = regexp.MustCompile(`^ {0,3}>[ \t]?(.*)$`)
	reFencedCode   = regexp.MustCompile(`^ {0,3}(\x60{3,}|~{3,})\s*([^\x60~]*)$`)
	reAngleAutoURL = regexp.MustCompile(`<((?:https?://[^\s<>\x00-\x1f\x7f]+)|(?:mailto:[^\s<>@\x00-\x1f\x7f]+@[^\s<>@\x00-\x1f\x7f]+))>`)
	reAutoURL      = regexp.MustCompile(`https?://[^\s<>"'\x00]+`)
)

func leadingASCIISpaces(line string) (spaces int, rest string) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	return i, line[i:]
}

func lineIndentWidth(line string) int {
	w := 0
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' {
			w++
		} else if line[i] == '\t' {
			w += 4
		} else {
			break
		}
	}
	return w
}

func parseATXHeading(line string) (level int, content string, ok bool) {
	spaces, rest := leadingASCIISpaces(line)
	if spaces > 3 || rest == "" {
		return 0, "", false
	}
	pos := 0
	for pos < len(rest) && rest[pos] == '#' && level < 7 {
		level++
		pos++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if pos < len(rest) && rest[pos] != ' ' && rest[pos] != '\t' {
		return 0, "", false
	}
	body := strings.Trim(rest[pos:], " \t")
	if body == "" {
		return level, "", true
	}
	end := len(body)
	for end > 0 && body[end-1] == '#' {
		end--
	}
	if end == 0 {
		return level, "", true
	}
	if end < len(body) && (body[end-1] == ' ' || body[end-1] == '\t') {
		body = strings.TrimRight(body[:end], " \t")
	}
	return level, body, true
}

func isThematicBreak(line string) bool {
	spaces, trimmedLeft := leadingASCIISpaces(line)
	if spaces > 3 || trimmedLeft == "" {
		return false
	}
	ch := trimmedLeft[0]
	if ch != '-' && ch != '*' && ch != '_' {
		return false
	}
	count := 0
	for i := 0; i < len(trimmedLeft); i++ {
		c := trimmedLeft[i]
		if c == ch {
			count++
		} else if c != ' ' && c != '\t' {
			return false
		}
	}
	return count >= 3
}

type listMarkerInfo struct {
	indent  int
	ordered bool
	start   int
	marker  byte
	content string
}

func parseListMarker(line string) (listMarkerInfo, bool) {
	indent := lineIndentWidth(line)
	pos := 0
	for pos < len(line) && (line[pos] == ' ' || line[pos] == '\t') {
		pos++
	}
	if pos >= len(line) {
		return listMarkerInfo{}, false
	}

	ch := line[pos]
	if ch == '*' || ch == '-' || ch == '+' {
		if pos+1 >= len(line) || (line[pos+1] != ' ' && line[pos+1] != '\t') {
			return listMarkerInfo{}, false
		}
		content := strings.TrimLeft(line[pos+2:], " \t")
		if strings.TrimSpace(content) == "" {
			return listMarkerInfo{}, false
		}
		return listMarkerInfo{
			indent:  indent,
			ordered: false,
			marker:  ch,
			content: content,
		}, true
	}

	if ch >= '0' && ch <= '9' {
		dEnd := pos
		for dEnd < len(line) && line[dEnd] >= '0' && line[dEnd] <= '9' && dEnd-pos < 9 {
			dEnd++
		}
		if dEnd >= len(line) || (line[dEnd] != '.' && line[dEnd] != ')') {
			return listMarkerInfo{}, false
		}
		if dEnd+1 >= len(line) || (line[dEnd+1] != ' ' && line[dEnd+1] != '\t') {
			return listMarkerInfo{}, false
		}
		content := strings.TrimLeft(line[dEnd+2:], " \t")
		if strings.TrimSpace(content) == "" {
			return listMarkerInfo{}, false
		}
		startNum, err := strconv.Atoi(line[pos:dEnd])
		if err != nil {
			return listMarkerInfo{}, false
		}
		return listMarkerInfo{
			indent:  indent,
			ordered: true,
			start:   startNum,
			marker:  line[dEnd],
			content: content,
		}, true
	}

	return listMarkerInfo{}, false
}

func parseTopLevelListMarker(line string) (listMarkerInfo, bool) {
	if isThematicBreak(line) {
		return listMarkerInfo{}, false
	}
	m, ok := parseListMarker(line)
	if !ok || m.indent > 3 {
		return listMarkerInfo{}, false
	}
	return m, true
}

func splitTableCells(line string) ([]string, bool) {
	spaces, rest := leadingASCIISpaces(line)
	if spaces > 3 || rest == "" || rest[0] == '\t' {
		return nil, false
	}
	s := strings.Trim(rest, " \t")
	if s == "" {
		return nil, false
	}

	var splitPos []int
	n := len(s)
	for k := 0; k < n; {
		if s[k] == '\\' && k+1 < n {
			k += 2
			continue
		}
		if s[k] == '`' {
			start := k
			for k < n && s[k] == '`' {
				k++
			}
			tickLen := k - start
			j := k
			closed := -1
			for j < n {
				if s[j] == '`' {
					cStart := j
					for j < n && s[j] == '`' {
						j++
					}
					if j-cStart == tickLen {
						closed = j
						break
					}
				} else {
					j++
				}
			}
			if closed != -1 {
				k = closed
			}
			continue
		}
		if s[k] == '|' {
			splitPos = append(splitPos, k)
		}
		k++
	}
	if len(splitPos) == 0 {
		return nil, false
	}

	hasLeadingPipe := splitPos[0] == 0
	hasTrailingPipe := splitPos[len(splitPos)-1] == n-1

	var cells []string
	if !hasLeadingPipe {
		cells = append(cells, unescapeTableCellPipes(strings.Trim(s[:splitPos[0]], " \t")))
	}
	for p := 0; p+1 < len(splitPos); p++ {
		cells = append(cells, unescapeTableCellPipes(strings.Trim(s[splitPos[p]+1:splitPos[p+1]], " \t")))
	}
	if !hasTrailingPipe {
		cells = append(cells, unescapeTableCellPipes(strings.Trim(s[splitPos[len(splitPos)-1]+1:], " \t")))
	}
	if len(cells) == 0 {
		return nil, false
	}
	return cells, true
}

func unescapeTableCellPipes(cell string) string {
	if !strings.ContainsRune(cell, '\\') {
		return cell
	}
	var sb strings.Builder
	sb.Grow(len(cell))
	for k := 0; k < len(cell); {
		if cell[k] == '\\' && k+1 < len(cell) {
			if cell[k+1] == '|' {
				sb.WriteByte('|')
			} else {
				sb.WriteString(cell[k : k+2])
			}
			k += 2
			continue
		}
		sb.WriteByte(cell[k])
		k++
	}
	return sb.String()
}

func parseTableAlignments(line string) ([]string, bool) {
	cells, ok := splitTableCells(line)
	if !ok {
		return nil, false
	}
	aligns := make([]string, len(cells))
	for i, c := range cells {
		if c == "" {
			return nil, false
		}
		leftColon := strings.HasPrefix(c, ":")
		rightColon := strings.HasSuffix(c, ":")
		mid := c
		if leftColon {
			mid = mid[1:]
		}
		if rightColon && len(mid) > 0 {
			mid = mid[:len(mid)-1]
		} else if rightColon {
			return nil, false
		}
		if len(mid) == 0 {
			return nil, false
		}
		for k := 0; k < len(mid); k++ {
			if mid[k] != '-' {
				return nil, false
			}
		}
		switch {
		case leftColon && rightColon:
			aligns[i] = "center"
		case leftColon:
			aligns[i] = "left"
		case rightColon:
			aligns[i] = "right"
		}
	}
	return aligns, true
}

func isTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	headerCells, ok := splitTableCells(lines[i])
	if !ok {
		return false
	}
	aligns, ok := parseTableAlignments(lines[i+1])
	return ok && len(headerCells) == len(aligns)
}

func isBlockInterrupt(lines []string, i int) bool {
	cur := lines[i]
	if strings.TrimSpace(cur) == "" {
		return true
	}
	if reFencedCode.MatchString(cur) {
		return true
	}
	if _, _, ok := parseATXHeading(cur); ok {
		return true
	}
	if isThematicBreak(cur) {
		return true
	}
	if reBlockquote.MatchString(cur) {
		return true
	}
	if _, ok := parseTopLevelListMarker(cur); ok {
		return true
	}
	if isTableStart(lines, i) {
		return true
	}
	return false
}

// RenderMarkdown parses basic Markdown into XSS-safe HTML.
func RenderMarkdown(src string) template.HTML {
	return renderMarkdownDepth(src, 0)
}

func renderMarkdownDepth(src string, depth int) template.HTML {
	if depth > 16 {
		return template.HTML(html.EscapeString(src))
	}
	src = strings.ReplaceAll(src, "\x00", "")
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lines := strings.Split(src, "\n")

	var sb strings.Builder
	i := 0
	for i < len(lines) {
		line := lines[i]

		// 1. Fenced code block
		if m := reFencedCode.FindStringSubmatch(line); m != nil {
			fence := m[1]
			fenceChar := fence[0]
			fenceLen := len(fence)
			var lang string
			if fields := strings.Fields(m[2]); len(fields) > 0 {
				lang = fields[0]
			}

			i++
			var codeLines []string
			for i < len(lines) {
				cur := lines[i]
				spaces, trimLeading := leadingASCIISpaces(cur)
				if spaces <= 3 {
					trim := strings.TrimSpace(trimLeading)
					if len(trim) >= fenceLen && trim[0] == fenceChar && strings.Trim(trim, string(fenceChar)) == "" {
						i++
						break
					}
				}
				codeLines = append(codeLines, cur)
				i++
			}

			sb.WriteString(renderCodeBlock(codeLines, lang))
			continue
		}

		// 2. ATX Heading
		if level, content, ok := parseATXHeading(line); ok {
			fmt.Fprintf(&sb, "<h%d class=\"md-h%d\">%s</h%d>\n", level, level, renderInline(content), level)
			i++
			continue
		}

		// 3. Thematic break / horizontal rule
		if isThematicBreak(line) {
			sb.WriteString("<hr class=\"md-hr\">\n")
			i++
			continue
		}

		// 4. Blockquote
		if reBlockquote.MatchString(line) {
			var quoteLines []string
			for i < len(lines) && reBlockquote.MatchString(lines[i]) {
				m := reBlockquote.FindStringSubmatch(lines[i])
				quoteLines = append(quoteLines, m[1])
				i++
			}
			quoteContent := renderMarkdownDepth(strings.Join(quoteLines, "\n"), depth+1)
			fmt.Fprintf(&sb, "<blockquote class=\"md-quote\">%s</blockquote>\n", quoteContent)
			continue
		}

		// 5. GFM pipe table
		if isTableStart(lines, i) {
			tableHTML, nextI := parseTable(lines, i)
			sb.WriteString(tableHTML)
			i = nextI
			continue
		}

		// 6. Ordered or unordered list
		if _, ok := parseTopLevelListMarker(line); ok {
			listHTML, nextI := parseList(lines, i, 0)
			sb.WriteString(listHTML)
			i = nextI
			continue
		}

		// 7. Blank line
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}

		// 8. Regular paragraph
		var paraLines []string
		for i < len(lines) && !isBlockInterrupt(lines, i) {
			paraLines = append(paraLines, lines[i])
			i++
		}

		var formatted []string
		for _, pl := range paraLines {
			formatted = append(formatted, renderInline(pl))
		}
		fmt.Fprintf(&sb, "<p class=\"md-p\">%s</p>\n", strings.Join(formatted, "<br>\n"))
	}

	return template.HTML(sb.String())
}

func parseTable(lines []string, i int) (string, int) {
	headerCells, _ := splitTableCells(lines[i])
	aligns, _ := parseTableAlignments(lines[i+1])
	numCols := len(headerCells)

	var sb strings.Builder
	sb.WriteString("<table class=\"md-table\">\n<thead>\n<tr>\n")
	for c := 0; c < numCols; c++ {
		if aligns[c] != "" {
			fmt.Fprintf(&sb, "<th align=\"%s\">%s</th>\n", aligns[c], renderInline(headerCells[c]))
		} else {
			fmt.Fprintf(&sb, "<th>%s</th>\n", renderInline(headerCells[c]))
		}
	}
	sb.WriteString("</tr>\n</thead>\n")

	i += 2
	var bodyRows [][]string
	for i < len(lines) {
		if strings.TrimSpace(lines[i]) == "" ||
			reFencedCode.MatchString(lines[i]) ||
			isThematicBreak(lines[i]) ||
			reBlockquote.MatchString(lines[i]) {
			break
		}
		if _, _, isH := parseATXHeading(lines[i]); isH {
			break
		}
		if _, isL := parseTopLevelListMarker(lines[i]); isL {
			break
		}
		rowCells, ok := splitTableCells(lines[i])
		if !ok {
			break
		}
		bodyRows = append(bodyRows, rowCells)
		i++
	}

	if len(bodyRows) > 0 {
		sb.WriteString("<tbody>\n")
		for _, row := range bodyRows {
			sb.WriteString("<tr>\n")
			for c := 0; c < numCols; c++ {
				cellText := ""
				if c < len(row) {
					cellText = row[c]
				}
				if aligns[c] != "" {
					fmt.Fprintf(&sb, "<td align=\"%s\">%s</td>\n", aligns[c], renderInline(cellText))
				} else {
					fmt.Fprintf(&sb, "<td>%s</td>\n", renderInline(cellText))
				}
			}
			sb.WriteString("</tr>\n")
		}
		sb.WriteString("</tbody>\n")
	}

	sb.WriteString("</table>\n")
	return sb.String(), i
}

func parseList(lines []string, i int, depth int) (string, int) {
	first, ok := parseListMarker(lines[i])
	if !ok {
		return "", i
	}
	listIndent := first.indent
	ordered := first.ordered
	marker := first.marker
	startNum := first.start

	var sb strings.Builder
	if ordered {
		if startNum != 1 {
			fmt.Fprintf(&sb, "<ol class=\"md-ol\" start=\"%d\">\n", startNum)
		} else {
			sb.WriteString("<ol class=\"md-ol\">\n")
		}
	} else {
		sb.WriteString("<ul class=\"md-ul\">\n")
	}

	for i < len(lines) {
		if isThematicBreak(lines[i]) {
			break
		}
		m, ok := parseListMarker(lines[i])
		if !ok || m.indent != listIndent || m.ordered != ordered || m.marker != marker {
			break
		}

		var itemLines []string
		itemLines = append(itemLines, renderInline(m.content))
		i++

		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" ||
				reFencedCode.MatchString(lines[i]) ||
				isThematicBreak(lines[i]) ||
				reBlockquote.MatchString(lines[i]) ||
				isTableStart(lines, i) {
				break
			}
			if _, _, isH := parseATXHeading(lines[i]); isH {
				break
			}
			if _, ok := parseListMarker(lines[i]); ok {
				break
			}
			if lineIndentWidth(lines[i]) <= listIndent {
				break
			}
			itemLines = append(itemLines, renderInline(strings.TrimLeft(lines[i], " \t")))
			i++
		}

		subHTML := ""
		if i < len(lines) && !isThematicBreak(lines[i]) && depth < 16 {
			if subM, ok := parseListMarker(lines[i]); ok && subM.indent > listIndent {
				subHTML, i = parseList(lines, i, depth+1)
			}
		}

		if subHTML != "" {
			fmt.Fprintf(&sb, "<li>%s\n%s</li>\n", strings.Join(itemLines, "<br>\n"), subHTML)
		} else {
			fmt.Fprintf(&sb, "<li>%s</li>\n", strings.Join(itemLines, "<br>\n"))
		}
	}

	if ordered {
		sb.WriteString("</ol>\n")
	} else {
		sb.WriteString("</ul>\n")
	}
	return sb.String(), i
}

func renderCodeBlock(codeLines []string, lang string) string {
	detectedLang := diff.LangNone
	if lang != "" {
		detectedLang = diff.DetectLang("file." + lang)
	}

	var renderedLines []string
	if detectedLang != diff.LangNone && len(codeLines) > 0 {
		tokens := diff.HighlightLines(codeLines, detectedLang)
		for j, cl := range codeLines {
			renderedLines = append(renderedLines, string(diff.RenderTokensHTMLWS(cl, tokens[j], nil, false)))
		}
	} else {
		for _, cl := range codeLines {
			renderedLines = append(renderedLines, html.EscapeString(cl))
		}
	}

	langClass := ""
	if lang != "" {
		langClass = fmt.Sprintf(" class=\"lang-%s\"", html.EscapeString(lang))
	}
	return fmt.Sprintf("<pre class=\"md-code-block\"><code%s>%s</code></pre>\n", langClass, strings.Join(renderedLines, "\n"))
}

func isASCIIPunct(ch byte) bool {
	return (ch >= '!' && ch <= '/') ||
		(ch >= ':' && ch <= '@') ||
		(ch >= '[' && ch <= '`') ||
		(ch >= '{' && ch <= '~')
}

type inlineParser struct {
	codeSpans     []string
	escChars      []byte
	linkSpans     []string
	angleLinkURLs map[int]string
}

func (p *inlineParser) extractInlineCode(text string) string {
	var sb strings.Builder
	i := 0
	n := len(text)
	for i < n {
		if text[i] == '\\' && i+1 < n && isASCIIPunct(text[i+1]) {
			sb.WriteString(text[i : i+2])
			i += 2
			continue
		}
		if text[i] != '`' {
			sb.WriteByte(text[i])
			i++
			continue
		}

		start := i
		for i < n && text[i] == '`' {
			i++
		}
		k := i - start

		closeIdx := -1
		j := i
		for j < n {
			if text[j] == '`' {
				cStart := j
				for j < n && text[j] == '`' {
					j++
				}
				if j-cStart == k {
					closeIdx = cStart
					break
				}
			} else {
				j++
			}
		}

		if closeIdx == -1 {
			sb.WriteString(text[start:i])
			continue
		}

		inner := text[i:closeIdx]
		i = closeIdx + k

		if len(inner) >= 2 && inner[0] == ' ' && inner[len(inner)-1] == ' ' && strings.Trim(inner, " ") != "" {
			inner = inner[1 : len(inner)-1]
		}

		idx := len(p.codeSpans)
		p.codeSpans = append(p.codeSpans, fmt.Sprintf("<code class=\"md-inline-code\">%s</code>", html.EscapeString(inner)))
		fmt.Fprintf(&sb, "\x00MDCODE%d\x00", idx)
	}
	return sb.String()
}

func (p *inlineParser) extractBackslashEscapes(text string) string {
	var sb strings.Builder
	i := 0
	n := len(text)
	for i < n {
		if text[i] == '\\' && i+1 < n && isASCIIPunct(text[i+1]) {
			idx := len(p.escChars)
			p.escChars = append(p.escChars, text[i+1])
			fmt.Fprintf(&sb, "\x00MDESC%d\x00", idx)
			i += 2
			continue
		}
		sb.WriteByte(text[i])
		i++
	}
	return sb.String()
}

func (p *inlineParser) restoreEscapesRaw(s string) string {
	if !strings.Contains(s, "\x00MDESC") {
		return s
	}
	for idx, ch := range p.escChars {
		ph := fmt.Sprintf("\x00MDESC%d\x00", idx)
		if strings.Contains(s, ph) {
			s = strings.ReplaceAll(s, ph, string(ch))
		}
	}
	return s
}

func (p *inlineParser) extractAngleAutolinks(text string) string {
	if !strings.ContainsRune(text, '<') {
		return text
	}
	return reAngleAutoURL.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		if !isSafeURL(inner) {
			return m
		}
		if strings.HasPrefix(inner, "http") {
			if schemeEnd := strings.Index(inner, "://"); schemeEnd == -1 || len(inner) <= schemeEnd+3 {
				return m
			}
		}
		safeURL := html.EscapeString(strings.TrimSpace(inner))
		label := safeURL
		if strings.HasPrefix(inner, "mailto:") {
			label = html.EscapeString(strings.TrimPrefix(inner, "mailto:"))
		}
		idx := len(p.linkSpans)
		p.linkSpans = append(p.linkSpans, fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener noreferrer">%s</a>`, safeURL, label))
		if p.angleLinkURLs == nil {
			p.angleLinkURLs = make(map[int]string)
		}
		p.angleLinkURLs[idx] = inner
		return fmt.Sprintf("\x00MDLINK%d\x00", idx)
	})
}

func (p *inlineParser) replaceMarkdownLinks(text string) string {
	if !strings.Contains(text, "](") {
		return text
	}
	var sb strings.Builder
	i := 0
	n := len(text)
	for i < n {
		if text[i] == '\x00' {
			endRel := strings.IndexByte(text[i+1:], '\x00')
			if endRel != -1 {
				end := i + 1 + endRel + 1
				sb.WriteString(text[i:end])
				i = end
				continue
			}
		}
		if text[i] != '[' {
			sb.WriteByte(text[i])
			i++
			continue
		}
		if strings.Index(text[i:], "](") == -1 {
			sb.WriteString(text[i:])
			break
		}

		depth := 1
		closeBracket := -1
		for j := i + 1; j < n; j++ {
			if text[j] == '\x00' {
				if endRel := strings.IndexByte(text[j+1:], '\x00'); endRel != -1 {
					j += 1 + endRel
					continue
				}
			}
			if text[j] == '[' {
				depth++
			} else if text[j] == ']' {
				depth--
				if depth == 0 {
					closeBracket = j
					break
				}
			}
		}
		if closeBracket == -1 || closeBracket <= i+1 || closeBracket+1 >= n || text[closeBracket+1] != '(' {
			sb.WriteByte(text[i])
			i++
			continue
		}

		parenDepth := 1
		closeParen := -1
		for j := closeBracket + 2; j < n; j++ {
			if text[j] == '\x00' {
				if endRel := strings.IndexByte(text[j+1:], '\x00'); endRel != -1 {
					j += 1 + endRel
					continue
				}
			}
			if text[j] == '\n' || text[j] == '\r' {
				break
			}
			if text[j] == '(' {
				parenDepth++
			} else if text[j] == ')' {
				parenDepth--
				if parenDepth == 0 {
					closeParen = j
					break
				}
			}
		}
		if closeParen == -1 {
			sb.WriteByte(text[i])
			i++
			continue
		}

		labelRaw := text[i+1 : closeBracket]
		if strings.Contains(labelRaw, "\x00MDLINK") {
			sb.WriteByte(text[i])
			i++
			continue
		}

		rawDest := p.restoreEscapesRaw(text[closeBracket+2 : closeParen])
		var rawURL string
		usedAngleDest := false
		if p.angleLinkURLs != nil {
			trimmedDest := strings.TrimSpace(rawDest)
			if strings.HasPrefix(trimmedDest, "\x00MDLINK") && strings.HasSuffix(trimmedDest, "\x00") {
				if id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(trimmedDest, "\x00MDLINK"), "\x00")); err == nil {
					if angleURL, ok := p.angleLinkURLs[id]; ok {
						rawURL = angleURL
						usedAngleDest = true
					}
				}
			}
		}
		if !usedAngleDest {
			if strings.ContainsRune(rawDest, '\x00') {
				sb.WriteByte(text[i])
				i++
				continue
			}
			rawURL = html.UnescapeString(rawDest)
		}
		if !isSafeURL(rawURL) {
			sb.WriteByte(text[i])
			i++
			continue
		}

		labelHTML := p.renderEmphasis(labelRaw)
		safeURL := html.EscapeString(strings.TrimSpace(rawURL))
		idx := len(p.linkSpans)
		p.linkSpans = append(p.linkSpans, fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener noreferrer">%s</a>`, safeURL, labelHTML))
		fmt.Fprintf(&sb, "\x00MDLINK%d\x00", idx)
		i = closeParen + 1
	}
	return sb.String()
}

type unitKind uint8

const (
	unitWhitespace unitKind = iota
	unitPunct
	unitWord
)

type emphNode struct {
	isDelim    bool
	text       string
	kind       unitKind
	char       byte
	origCount  int
	count      int
	canOpen    bool
	canClose   bool
	beforeTags []string
	afterTags  []string
}

func (p *inlineParser) renderEmphasis(text string) string {
	if !strings.ContainsAny(text, "*_~") {
		return text
	}

	var nodes []emphNode
	i := 0
	n := len(text)
	for i < n {
		if text[i] == '\x00' {
			endRel := strings.IndexByte(text[i+1:], '\x00')
			if endRel != -1 {
				end := i + 1 + endRel + 1
				ph := text[i:end]
				kind := unitWord
				if strings.HasPrefix(ph, "\x00MDESC") {
					kind = unitPunct
				}
				nodes = append(nodes, emphNode{
					text: ph,
					kind: kind,
				})
				i = end
				continue
			}
		}

		ch := text[i]
		if ch == '*' || ch == '_' || ch == '~' {
			start := i
			for i < n && text[i] == ch {
				i++
			}
			runLen := i - start
			nodes = append(nodes, emphNode{
				isDelim:   true,
				char:      ch,
				origCount: runLen,
				count:     runLen,
			})
			continue
		}

		r, w := utf8.DecodeRuneInString(text[i:])
		kind := unitWord
		if unicode.IsSpace(r) {
			kind = unitWhitespace
		} else if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			kind = unitPunct
		}
		nodes = append(nodes, emphNode{
			text: text[i : i+w],
			kind: kind,
		})
		i += w
	}

	for idx := range nodes {
		if !nodes[idx].isDelim {
			continue
		}
		prevKind := unitWhitespace
		if idx > 0 {
			if nodes[idx-1].isDelim {
				prevKind = unitPunct
			} else {
				prevKind = nodes[idx-1].kind
			}
		}
		nextKind := unitWhitespace
		if idx+1 < len(nodes) {
			if nodes[idx+1].isDelim {
				nextKind = unitPunct
			} else {
				nextKind = nodes[idx+1].kind
			}
		}

		leftFlanking := nextKind != unitWhitespace &&
			(nextKind != unitPunct || prevKind == unitWhitespace || prevKind == unitPunct)
		rightFlanking := prevKind != unitWhitespace &&
			(prevKind != unitPunct || nextKind == unitWhitespace || nextKind == unitPunct)

		switch nodes[idx].char {
		case '*':
			nodes[idx].canOpen = leftFlanking
			nodes[idx].canClose = rightFlanking
		case '_':
			nodes[idx].canOpen = leftFlanking && (!rightFlanking || prevKind == unitPunct)
			nodes[idx].canClose = rightFlanking && (!leftFlanking || nextKind == unitPunct)
		case '~':
			if nodes[idx].origCount == 2 {
				nodes[idx].canOpen = leftFlanking
				nodes[idx].canClose = rightFlanking
			}
		}
	}

	type openerKey struct {
		char    byte
		canOpen bool
		mod3    int
	}
	openersBottom := make(map[openerKey]int, 18)

	for cIdx := 0; cIdx < len(nodes); cIdx++ {
		if !nodes[cIdx].isDelim {
			continue
		}
		for nodes[cIdx].canClose && nodes[cIdx].count > 0 {
			c := &nodes[cIdx]
			k := openerKey{char: c.char, canOpen: c.canOpen, mod3: c.origCount % 3}
			bottom := openersBottom[k]

			found := -1
			for oIdx := cIdx - 1; oIdx >= bottom; oIdx-- {
				o := &nodes[oIdx]
				if !o.isDelim || !o.canOpen || o.count <= 0 || o.char != c.char {
					continue
				}
				if c.char == '*' || c.char == '_' {
					if (o.canClose || c.canOpen) && (o.origCount+c.origCount)%3 == 0 && (o.origCount%3 != 0 || c.origCount%3 != 0) {
						continue
					}
				}
				found = oIdx
				break
			}

			if found == -1 {
				openersBottom[k] = cIdx
				break
			}

			o := &nodes[found]
			useCount := 1
			var openTag, closeTag string
			if c.char == '~' {
				useCount = 2
				openTag = "<del>"
				closeTag = "</del>"
			} else if o.count >= 2 && c.count >= 2 {
				useCount = 2
				openTag = "<strong>"
				closeTag = "</strong>"
			} else {
				openTag = "<em>"
				closeTag = "</em>"
			}

			o.count -= useCount
			c.count -= useCount
			o.afterTags = append([]string{openTag}, o.afterTags...)
			c.beforeTags = append(c.beforeTags, closeTag)

			for mid := found + 1; mid < cIdx; mid++ {
				nodes[mid].canOpen = false
				nodes[mid].canClose = false
			}
		}
	}

	var sb strings.Builder
	for _, nd := range nodes {
		if !nd.isDelim {
			sb.WriteString(nd.text)
			continue
		}
		for _, tag := range nd.beforeTags {
			sb.WriteString(tag)
		}
		for k := 0; k < nd.count; k++ {
			sb.WriteByte(nd.char)
		}
		for _, tag := range nd.afterTags {
			sb.WriteString(tag)
		}
	}
	return sb.String()
}

func (p *inlineParser) render(text string) string {
	// 1. Extract inline code spans to protect them from escapes, links, and formatting
	text = p.extractInlineCode(text)

	// 2. Protect backslash-escaped ASCII punctuation outside code spans
	text = p.extractBackslashEscapes(text)

	// 3. Extract angle-bracket autolinks <https://...> and <mailto:...> before HTML escaping
	text = p.extractAngleAutolinks(text)

	// 4. HTML-escape remaining text for XSS safety
	text = html.EscapeString(text)

	// 5. Parse Markdown links [label](url) with balanced parens and formatted labels
	text = p.replaceMarkdownLinks(text)

	// 6. Auto-link bare URLs, stopping cleanly before quotes, angle brackets, emphasis '*', or trailing punct
	text = reAutoURL.ReplaceAllStringFunc(text, func(m string) string {
		raw := html.UnescapeString(m)
		quoteSuffix := ""
		if idx := strings.IndexAny(raw, "<>\"'"); idx != -1 {
			quoteSuffix = raw[idx:]
			raw = raw[:idx]
		}
		cleanURL, trailing := trimMarkdownURLTrailingPunct(raw)
		if schemeEnd := strings.Index(cleanURL, "://"); schemeEnd != -1 && len(cleanURL) > schemeEnd+3 && isSafeURL(cleanURL) {
			safeURL := html.EscapeString(cleanURL)
			idx := len(p.linkSpans)
			p.linkSpans = append(p.linkSpans, fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener noreferrer">%s</a>`, safeURL, safeURL))
			return fmt.Sprintf("\x00MDLINK%d\x00%s", idx, html.EscapeString(trailing+quoteSuffix))
		}
		return m
	})

	// 7. Bold, italic, strikethrough with Go-safe CommonMark flanking rules
	text = p.renderEmphasis(text)

	// 8. Restore link spans first (which may contain MDESC or MDCODE placeholders)
	for idx, span := range p.linkSpans {
		placeholder := fmt.Sprintf("\x00MDLINK%d\x00", idx)
		text = strings.ReplaceAll(text, placeholder, span)
	}

	// 9. Restore backslash-escaped ASCII punctuation (HTML-escaped)
	for idx, ch := range p.escChars {
		placeholder := fmt.Sprintf("\x00MDESC%d\x00", idx)
		text = strings.ReplaceAll(text, placeholder, html.EscapeString(string(ch)))
	}

	// 10. Restore inline code spans
	for idx, span := range p.codeSpans {
		placeholder := fmt.Sprintf("\x00MDCODE%d\x00", idx)
		text = strings.ReplaceAll(text, placeholder, span)
	}

	return text
}

func renderInline(text string) string {
	if text == "" {
		return ""
	}
	var p inlineParser
	return p.render(text)
}

func trimMarkdownURLTrailingPunct(u string) (string, string) {
	cut := len(u)
	for cut > 0 {
		last := u[cut-1]
		if strings.IndexByte(".,;:!?*", last) != -1 {
			cut--
			continue
		}
		if last == ')' && strings.Count(u[:cut], ")") > strings.Count(u[:cut], "(") {
			cut--
			continue
		}
		break
	}
	return u[:cut], u[cut:]
}

// trimURLTrailingPunct trims trailing punctuation (.,;:!?) and unmatched trailing closing parentheses.
func trimURLTrailingPunct(u string) (string, string) {
	cut := len(u)
	for cut > 0 {
		last := u[cut-1]
		if strings.IndexByte(".,;:!?", last) != -1 {
			cut--
			continue
		}
		if last == ')' && strings.Count(u[:cut], ")") > strings.Count(u[:cut], "(") {
			cut--
			continue
		}
		break
	}
	return u[:cut], u[cut:]
}

func isSafeURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}

	// Repeatedly decode HTML entities (e.g. &#x3A;, &colon;) to reveal true URL scheme.
	// Bound to at most 8 iterations; if still changing, reject as suspicious obfuscation.
	decoded := raw
	for iter := 0; ; iter++ {
		next := html.UnescapeString(decoded)
		if next == decoded {
			break
		}
		if iter >= 8 {
			return false
		}
		decoded = next
	}

	// Strip all ASCII whitespace and control characters that browsers ignore in URLs.
	var clean strings.Builder
	for i := 0; i < len(decoded); i++ {
		b := decoded[i]
		if b > ' ' && b != 0x7f {
			clean.WriteByte(b)
		}
	}
	cleanStr := clean.String()
	if cleanStr == "" {
		return false
	}
	lower := strings.ToLower(cleanStr)

	// Block dangerous schemes explicitly
	if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "vbscript:") || strings.HasPrefix(lower, "data:") {
		return false
	}

	// Disallow protocol-relative URLs (e.g. "//evil.com")
	if strings.HasPrefix(lower, "//") {
		return false
	}

	// Allow absolute path references within site and in-page anchor links
	if strings.HasPrefix(lower, "/") || strings.HasPrefix(lower, "#") {
		return true
	}

	// Allow relative paths without scheme/colon
	if !strings.Contains(lower, ":") {
		return true
	}

	// For URLs with a scheme, allow only http, https, mailto
	u, err := url.Parse(cleanStr)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https" || scheme == "mailto"
}
