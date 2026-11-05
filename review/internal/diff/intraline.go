package diff

import (
	"strings"
	"unicode/utf8"
)

// Span represents a byte offset range [Start, End) within a string.
type Span struct {
	Start int
	End   int
}

// runeSpan represents a rune index range [Start, End).
type runeSpan struct {
	Start int
	End   int
}

// Intraline computes rune-level intraline diff spans for modified lines.
// It merges tiny gaps (<= 4 runes), abandons intraline highlights if > 50%
// of runes changed, and converts rune offsets to UTF-8 byte offsets with zero string allocations.
func Intraline(left, right string) (lSpans, rSpans []Span, ok bool) {
	if left == right {
		return nil, nil, false
	}

	leftRunes := []rune(left)
	rightRunes := []rune(right)
	maxLen := len(leftRunes)
	if len(rightRunes) > maxLen {
		maxLen = len(rightRunes)
	}
	if maxLen == 0 {
		return nil, nil, false
	}

	edits := Edits(leftRunes, rightRunes)

	// Group edits into contiguous blocks
	type block struct {
		isEqual bool
		aStart  int
		aEnd    int
		bStart  int
		bEnd    int
	}

	var blocks []block
	i := 0
	for i < len(edits) {
		e := edits[i]
		if e.Op == OpEqual {
			aStart := e.AIndex
			bStart := e.BIndex
			for i < len(edits) && edits[i].Op == OpEqual {
				i++
			}
			blocks = append(blocks, block{
				isEqual: true,
				aStart:  aStart,
				aEnd:    aStart + (edits[i-1].AIndex - aStart + 1),
				bStart:  bStart,
				bEnd:    bStart + (edits[i-1].BIndex - bStart + 1),
			})
		} else {
			aMin, aMax := -1, -1
			bMin, bMax := -1, -1
			for i < len(edits) && edits[i].Op != OpEqual {
				cur := edits[i]
				if cur.Op == OpDelete {
					if aMin == -1 || cur.AIndex < aMin {
						aMin = cur.AIndex
					}
					if cur.AIndex+1 > aMax {
						aMax = cur.AIndex + 1
					}
				} else if cur.Op == OpInsert {
					if bMin == -1 || cur.BIndex < bMin {
						bMin = cur.BIndex
					}
					if cur.BIndex+1 > bMax {
						bMax = cur.BIndex + 1
					}
				}
				i++
			}
			blocks = append(blocks, block{
				isEqual: false,
				aStart:  aMin,
				aEnd:    aMax,
				bStart:  bMin,
				bEnd:    bMax,
			})
		}
	}

	// Merge tiny equal gaps (<= 5 runes) between non-equal blocks
	const maxGap = 5
	merged := true
	for merged {
		merged = false
		var newBlocks []block
		j := 0
		for j < len(blocks) {
			if j > 0 && j+1 < len(blocks) && blocks[j].isEqual {
				eqLen := blocks[j].aEnd - blocks[j].aStart
				if eqLen <= maxGap && !blocks[j-1].isEqual && !blocks[j+1].isEqual {
					// Merge prev, curr, next into one non-equal block
					prev := blocks[j-1]
					next := blocks[j+1]

					aStart := prev.aStart
					if aStart == -1 || (next.aStart != -1 && blocks[j].aStart < aStart) {
						aStart = blocks[j].aStart
					}
					aEnd := next.aEnd
					if aEnd == -1 || (prev.aEnd != -1 && blocks[j].aEnd > aEnd) {
						aEnd = blocks[j].aEnd
					}

					bStart := prev.bStart
					if bStart == -1 || (next.bStart != -1 && blocks[j].bStart < bStart) {
						bStart = blocks[j].bStart
					}
					bEnd := next.bEnd
					if bEnd == -1 || (prev.bEnd != -1 && blocks[j].bEnd > bEnd) {
						bEnd = blocks[j].bEnd
					}

					newBlock := block{
						isEqual: false,
						aStart:  aStart,
						aEnd:    aEnd,
						bStart:  bStart,
						bEnd:    bEnd,
					}
					newBlocks[len(newBlocks)-1] = newBlock
					j += 2
					merged = true
					continue
				}
			}
			newBlocks = append(newBlocks, blocks[j])
			j++
		}
		blocks = newBlocks
	}

	var rawL []runeSpan
	var rawR []runeSpan
	for _, b := range blocks {
		if !b.isEqual {
			if b.aStart != -1 && b.aEnd > b.aStart {
				rawL = append(rawL, runeSpan{Start: b.aStart, End: b.aEnd})
			}
			if b.bStart != -1 && b.bEnd > b.bStart {
				rawR = append(rawR, runeSpan{Start: b.bStart, End: b.bEnd})
			}
		}
	}

	mergedL := mergeRuneSpans(rawL, maxGap)
	mergedR := mergeRuneSpans(rawR, maxGap)

	// Count total changed runes in merged spans
	changedRunes := 0
	for _, s := range mergedL {
		changedRunes += (s.End - s.Start)
	}
	for _, s := range mergedR {
		changedRunes += (s.End - s.Start)
	}

	// Compute non-whitespace changed vs unchanged runes for noise suppression
	unchangedNonWSL := 0
	changedNonWSL := 0
	for idx, r := range leftRunes {
		if r == ' ' || r == '\t' {
			continue
		}
		inSpan := false
		for _, sp := range mergedL {
			if idx >= sp.Start && idx < sp.End {
				inSpan = true
				break
			}
		}
		if inSpan {
			changedNonWSL++
		} else {
			unchangedNonWSL++
		}
	}

	unchangedNonWSR := 0
	changedNonWSR := 0
	for idx, r := range rightRunes {
		if r == ' ' || r == '\t' {
			continue
		}
		inSpan := false
		for _, sp := range mergedR {
			if idx >= sp.Start && idx < sp.End {
				inSpan = true
				break
			}
		}
		if inSpan {
			changedNonWSR++
		} else {
			unchangedNonWSR++
		}
	}

	if unchangedNonWSL == 0 || (changedNonWSL > 0 && unchangedNonWSL <= 2 && changedNonWSL > unchangedNonWSL*2) ||
		unchangedNonWSR == 0 || (changedNonWSR > 0 && unchangedNonWSR <= 2 && changedNonWSR > unchangedNonWSR*2) {
		return nil, nil, false
	}

	// Abandon intraline highlight if > 50% of the runes changed
	if changedRunes > (len(leftRunes)+len(rightRunes)) || changedRunes > maxLen {
		return nil, nil, false
	}

	byteSpansL := runeSpansToByteSpans(left, mergedL)
	byteSpansR := runeSpansToByteSpans(right, mergedR)

	return byteSpansL, byteSpansR, true
}

func mergeRuneSpans(spans []runeSpan, maxGap int) []runeSpan {
	if len(spans) == 0 {
		return nil
	}
	var res []runeSpan
	curr := spans[0]
	for i := 1; i < len(spans); i++ {
		next := spans[i]
		if next.Start <= curr.End+maxGap {
			if next.End > curr.End {
				curr.End = next.End
			}
		} else {
			res = append(res, curr)
			curr = next
		}
	}
	res = append(res, curr)
	return res
}

func runeSpansToByteSpans(s string, rSpans []runeSpan) []Span {
	if len(rSpans) == 0 {
		return nil
	}
	spans := make([]Span, len(rSpans))
	rIdx := 0
	byteOffset := 0
	sIdx := 0
	n := len(s)

	for sIdx < len(rSpans) {
		target := rSpans[sIdx]
		// Advance until target.Start
		for rIdx < target.Start && byteOffset < n {
			_, width := utf8.DecodeRuneInString(s[byteOffset:])
			byteOffset += width
			rIdx++
		}
		startByte := byteOffset
		// Advance until target.End
		for rIdx < target.End && byteOffset < n {
			_, width := utf8.DecodeRuneInString(s[byteOffset:])
			byteOffset += width
			rIdx++
		}
		endByte := byteOffset
		spans[sIdx] = Span{Start: startByte, End: endByte}
		sIdx++
	}
	return spans
}

// BuildDiffRows creates side-by-side rows from left and right lines using
// multi-line hunk intraline diffing and alignment.
func BuildDiffRows(left, right []string, edits []Edit) []Row {
	type lineHunk struct {
		a0, a1 int
		b0, b1 int
	}

	var hunks []lineHunk
	curA, curB := 0, 0
	i := 0
	for i < len(edits) {
		if edits[i].Op == OpEqual {
			curA = edits[i].AIndex + 1
			curB = edits[i].BIndex + 1
			i++
			continue
		}
		hunkA0, hunkB0 := curA, curB
		hunkA1, hunkB1 := curA, curB
		for i < len(edits) && edits[i].Op != OpEqual {
			e := edits[i]
			if e.Op == OpDelete {
				if e.AIndex+1 > hunkA1 {
					hunkA1 = e.AIndex + 1
				}
				curA = e.AIndex + 1
			} else if e.Op == OpInsert {
				if e.BIndex+1 > hunkB1 {
					hunkB1 = e.BIndex + 1
				}
				curB = e.BIndex + 1
			}
			i++
		}
		hunks = append(hunks, lineHunk{a0: hunkA0, a1: hunkA1, b0: hunkB0, b1: hunkB1})
	}

	isBlankLine := func(s string) bool {
		t := strings.Trim(s, " \t")
		return t == "" || t == "{" || t == "}" || t == "/*" || t == "/**" || t == "*"
	}

	isControlBlockStart := func(s string) bool {
		t := strings.TrimRight(s, " \t")
		return strings.HasSuffix(t, "{") || strings.HasSuffix(t, ":")
	}

	countHunkRunes := func(lines []string) int {
		n := 0
		for _, s := range lines {
			n += utf8.RuneCountInString(s) + 1
		}
		return n
	}

	// Step A: combineLineEdits
	var mergedHunks []lineHunk
	for _, h := range hunks {
		if len(mergedHunks) == 0 {
			mergedHunks = append(mergedHunks, h)
			continue
		}
		prev := mergedHunks[len(mergedHunks)-1]
		gap := h.a0 - prev.a1
		canMerge := false
		if gap >= 1 {
			allBlank := true
			for idx := prev.a1; idx < h.a0; idx++ {
				if !isBlankLine(left[idx]) {
					allBlank = false
					break
				}
			}
			if allBlank {
				canMerge = true
			} else if gap == 1 && isControlBlockStart(left[prev.a1]) {
				canMerge = true
			}
		}
		if canMerge {
			combinedRunes := countHunkRunes(left[prev.a0:h.a1]) + countHunkRunes(right[prev.b0:h.b1])
			if combinedRunes <= 20000 {
				mergedHunks[len(mergedHunks)-1] = lineHunk{
					a0: prev.a0,
					a1: h.a1,
					b0: prev.b0,
					b1: h.b1,
				}
				continue
			}
		}
		mergedHunks = append(mergedHunks, h)
	}

	diffHunk := func(h lineHunk) []Row {
		hLinesA := left[h.a0:h.a1]
		hLinesB := right[h.b0:h.b1]
		numA := len(hLinesA)
		numB := len(hLinesB)

		if numA == 0 {
			var rows []Row
			for idx, s := range hLinesB {
				rows = append(rows, Row{
					Type:      RowAdd,
					RightLine: h.b0 + idx + 1,
					RightText: s,
				})
			}
			return rows
		}
		if numB == 0 {
			var rows []Row
			for idx, s := range hLinesA {
				rows = append(rows, Row{
					Type:     RowDel,
					LeftLine: h.a0 + idx + 1,
					LeftText: s,
				})
			}
			return rows
		}

		// Step B: Multi-line hunk character diff
		aStart := make([]int, numA)
		aNL := make([]int, numA)
		var aRunes []rune
		for la := 0; la < numA; la++ {
			aStart[la] = len(aRunes)
			r := []rune(hLinesA[la])
			aRunes = append(aRunes, r...)
			aNL[la] = len(aRunes)
			aRunes = append(aRunes, '\n')
		}

		bStart := make([]int, numB)
		bNL := make([]int, numB)
		var bRunes []rune
		for lb := 0; lb < numB; lb++ {
			bStart[lb] = len(bRunes)
			r := []rune(hLinesB[lb])
			bRunes = append(bRunes, r...)
			bNL[lb] = len(bRunes)
			bRunes = append(bRunes, '\n')
		}

		if len(aRunes)+len(bRunes) > 20000 {
			minLen := numA
			if numB < minLen {
				minLen = numB
			}
			var rows []Row
			for k := 0; k < minLen; k++ {
				lSpans, rSpans, ok := Intraline(hLinesA[k], hLinesB[k])
				r := Row{
					Type:      RowChange,
					LeftLine:  h.a0 + k + 1,
					RightLine: h.b0 + k + 1,
					LeftText:  hLinesA[k],
					RightText: hLinesB[k],
				}
				if ok {
					r.LeftIntra = lSpans
					r.RightIntra = rSpans
				}
				rows = append(rows, r)
			}
			for k := minLen; k < numA; k++ {
				rows = append(rows, Row{
					Type:     RowDel,
					LeftLine: h.a0 + k + 1,
					LeftText: hLinesA[k],
				})
			}
			for k := minLen; k < numB; k++ {
				rows = append(rows, Row{
					Type:      RowAdd,
					RightLine: h.b0 + k + 1,
					RightText: hLinesB[k],
				})
			}
			return rows
		}

		charEditsRaw := Edits(aRunes, bRunes)

		type charEdit struct {
			ab, ae int
			bb, be int
		}

		var cEdits []charEdit
		curPosA, curPosB := 0, 0
		for i := 0; i < len(charEditsRaw); {
			e := charEditsRaw[i]
			if e.Op == OpEqual {
				curPosA = e.AIndex + 1
				curPosB = e.BIndex + 1
				i++
				continue
			}
			ab := curPosA
			bb := curPosB
			ae := curPosA
			be := curPosB
			for i < len(charEditsRaw) && charEditsRaw[i].Op != OpEqual {
				cur := charEditsRaw[i]
				if cur.Op == OpDelete {
					ae = cur.AIndex + 1
					curPosA = cur.AIndex + 1
				} else if cur.Op == OpInsert {
					be = cur.BIndex + 1
					curPosB = cur.BIndex + 1
				}
				i++
			}
			cEdits = append(cEdits, charEdit{ab: ab, ae: ae, bb: bb, be: be})
		}

		containsNL := func(runes []rune, start, end int) bool {
			for idx := start; idx < end; idx++ {
				if runes[idx] == '\n' {
					return true
				}
			}
			return false
		}

		// Coalesce edits separated by <= 5 runes without newlines
		var coalesced []charEdit
		for _, next := range cEdits {
			if len(coalesced) == 0 {
				coalesced = append(coalesced, next)
				continue
			}
			last := &coalesced[len(coalesced)-1]
			if (next.ab-last.ae <= 5 || next.bb-last.be <= 5) &&
				!containsNL(aRunes, last.ae, next.ab) &&
				!containsNL(bRunes, last.be, next.bb) {
				last.ae = next.ae
				last.be = next.be
			} else {
				coalesced = append(coalesced, next)
			}
		}
		cEdits = coalesced

		// Cleanup loop
		for iter := 0; iter < 10; iter++ {
			changed := false
			var merged []charEdit
			for _, cur := range cEdits {
				if len(merged) == 0 {
					merged = append(merged, cur)
					continue
				}
				prev := &merged[len(merged)-1]
				if prev.ae >= cur.ab || prev.be >= cur.bb {
					if cur.ae > prev.ae {
						prev.ae = cur.ae
					}
					if cur.be > prev.be {
						prev.be = cur.be
					}
					changed = true
				} else {
					merged = append(merged, cur)
				}
			}
			cEdits = merged

			var clean []charEdit
			for _, cur := range cEdits {
				// Trim common prefix
				for cur.ab < cur.ae && cur.bb < cur.be && aRunes[cur.ab] == bRunes[cur.bb] {
					cur.ab++
					cur.bb++
					changed = true
				}
				// Trim common suffix
				for cur.ab < cur.ae && cur.bb < cur.be && aRunes[cur.ae-1] == bRunes[cur.be-1] {
					cur.ae--
					cur.be--
					changed = true
				}
				// Drop empty
				if cur.ab == cur.ae && cur.bb == cur.be {
					changed = true
					continue
				}

				clean = append(clean, cur)
			}
			cEdits = clean
			if !changed {
				break
			}
		}

		// Step C: Per-line intraline span extraction and noise suppression
		extractHunkLineSpans := func(hLines []string, starts, nls []int, runes []rune, getBounds func(e charEdit) (int, int)) ([][]Span, []bool) {
			numLines := len(hLines)
			lineSpans := make([][]Span, numLines)
			hasEdits := make([]bool, numLines)
			for l := 0; l < numLines; l++ {
				st := starts[l]
				nl := nls[l]
				lineRunes := runes[st:nl]

				wholeLine := false
				hasAny := false
				for _, e := range cEdits {
					es, ee := getBounds(e)
					if es < nl+1 && ee > st {
						hasAny = true
					}
					if es <= st && ee >= nl+1 {
						wholeLine = true
						break
					}
				}
				hasEdits[l] = hasAny
				if wholeLine {
					lineSpans[l] = nil
					continue
				}

				var spans []runeSpan
				for _, e := range cEdits {
					es, ee := getBounds(e)
					s := es
					if s < st {
						s = st
					}
					end := ee
					if end > nl {
						end = nl
					}
					if s < end {
						spans = append(spans, runeSpan{Start: s - st, End: end - st})
					}
				}
				if len(spans) == 0 {
					lineSpans[l] = nil
					continue
				}

				// Slide leading whitespace edits to the start of indentation
				prevSpanEnd := 0
				for sIdx := range spans {
					if spans[sIdx].Start < len(lineRunes) {
						ws := lineRunes[spans[sIdx].Start]
						if (ws == ' ' || ws == '\t') && spans[sIdx].Start > prevSpanEnd {
							allSame := true
							for _, r := range lineRunes[spans[sIdx].Start:spans[sIdx].End] {
								if r != ws {
									allSame = false
									break
								}
							}
							if allSame {
								for spans[sIdx].Start > prevSpanEnd && lineRunes[spans[sIdx].Start-1] == ws {
									spans[sIdx].Start--
									spans[sIdx].End--
								}
							}
						}
					}
					prevSpanEnd = spans[sIdx].End
				}

				merged := mergeRuneSpans(spans, 0)
				unchangedNonWS := 0
				changedNonWS := 0
				for idx, r := range lineRunes {
					if r == ' ' || r == '\t' {
						continue
					}
					inSpan := false
					for _, sp := range merged {
						if idx >= sp.Start && idx < sp.End {
							inSpan = true
							break
						}
					}
					if inSpan {
						changedNonWS++
					} else {
						unchangedNonWS++
					}
				}
				if unchangedNonWS == 0 || (changedNonWS > 0 && unchangedNonWS <= 2 && changedNonWS > unchangedNonWS*2) {
					lineSpans[l] = nil
				} else {
					lineSpans[l] = runeSpansToByteSpans(hLines[l], merged)
				}
			}
			return lineSpans, hasEdits
		}

		leftSpans, lineHasEditsA := extractHunkLineSpans(hLinesA, aStart, aNL, aRunes, func(e charEdit) (int, int) { return e.ab, e.ae })
		rightSpans, lineHasEditsB := extractHunkLineSpans(hLinesB, bStart, bNL, bRunes, func(e charEdit) (int, int) { return e.bb, e.be })

		// Step D: Hunk row alignment using matched '\n' anchors
		nlToLineA := make(map[int]int, numA)
		for la, pos := range aNL {
			nlToLineA[pos] = la
		}
		nlToLineB := make(map[int]int, numB)
		for lb, pos := range bNL {
			nlToLineB[pos] = lb
		}

		type anchorPair struct {
			la, lb int
		}

		sharedNonWS := make(map[anchorPair]int)
		var rawAnchors []anchorPair

		lineCursorA, lineCursorB := 0, 0
		scanUnchanged := func(startA, endA, startB int) {
			segLen := endA - startA
			for k := 0; k < segLen; k++ {
				pA := startA + k
				pB := startB + k

				for lineCursorA < numA && pA > aNL[lineCursorA] {
					lineCursorA++
				}
				for lineCursorB < numB && pB > bNL[lineCursorB] {
					lineCursorB++
				}

				rA := aRunes[pA]
				if rA == '\n' {
					la, okA := nlToLineA[pA]
					lb, okB := nlToLineB[pB]
					if okA && okB {
						rawAnchors = append(rawAnchors, anchorPair{la: la, lb: lb})
					}
				} else if rA != ' ' && rA != '\t' {
					la := -1
					if lineCursorA < numA && pA >= aStart[lineCursorA] && pA <= aNL[lineCursorA] {
						la = lineCursorA
					}
					lb := -1
					if lineCursorB < numB && pB >= bStart[lineCursorB] && pB <= bNL[lineCursorB] {
						lb = lineCursorB
					}
					if la != -1 && lb != -1 {
						sharedNonWS[anchorPair{la: la, lb: lb}]++
					}
				}
			}
		}

		curA, curB := 0, 0
		for _, e := range cEdits {
			if e.ab > curA {
				scanUnchanged(curA, e.ab, curB)
			}
			curA = e.ae
			curB = e.be
		}
		if curA < len(aRunes) {
			scanUnchanged(curA, len(aRunes), curB)
		}

		isOnlyWS := func(s string) bool {
			return strings.Trim(s, " \t") == ""
		}

		onlyWSDiff := func(s1, s2 string) bool {
			return strings.ReplaceAll(strings.ReplaceAll(s1, " ", ""), "\t", "") ==
				strings.ReplaceAll(strings.ReplaceAll(s2, " ", ""), "\t", "")
		}

		var validAnchors []anchorPair
		for _, p := range rawAnchors {
			if hLinesA[p.la] == hLinesB[p.lb] && !lineHasEditsA[p.la] && !lineHasEditsB[p.lb] {
				validAnchors = append(validAnchors, p)
				continue
			}
			if sharedNonWS[p] > 0 || (isOnlyWS(hLinesA[p.la]) && isOnlyWS(hLinesB[p.lb])) || (!isOnlyWS(hLinesA[p.la]) && onlyWSDiff(hLinesA[p.la], hLinesB[p.lb])) {
				validAnchors = append(validAnchors, p)
				continue
			}
		}

		var hunkRows []Row
		emitSub := func(startA, endA, startB, endB int) {
			cntA := endA - startA
			cntB := endB - startB
			if cntA == 0 && cntB == 0 {
				return
			}
			if cntA == 0 {
				for lb := startB; lb < endB; lb++ {
					hunkRows = append(hunkRows, Row{
						Type:      RowAdd,
						RightLine: h.b0 + lb + 1,
						RightText: hLinesB[lb],
					})
				}
				return
			}
			if cntB == 0 {
				for la := startA; la < endA; la++ {
					hunkRows = append(hunkRows, Row{
						Type:     RowDel,
						LeftLine: h.a0 + la + 1,
						LeftText: hLinesA[la],
					})
				}
				return
			}
			minLen := cntA
			if cntB < minLen {
				minLen = cntB
			}
			for k := 0; k < minLen; k++ {
				la := startA + k
				lb := startB + k
				hunkRows = append(hunkRows, Row{
					Type:       RowChange,
					LeftLine:   h.a0 + la + 1,
					RightLine:  h.b0 + lb + 1,
					LeftText:   hLinesA[la],
					RightText:  hLinesB[lb],
					LeftIntra:  leftSpans[la],
					RightIntra: rightSpans[lb],
				})
			}
			for la := startA + minLen; la < endA; la++ {
				hunkRows = append(hunkRows, Row{
					Type:     RowDel,
					LeftLine: h.a0 + la + 1,
					LeftText: hLinesA[la],
				})
			}
			for lb := startB + minLen; lb < endB; lb++ {
				hunkRows = append(hunkRows, Row{
					Type:      RowAdd,
					RightLine: h.b0 + lb + 1,
					RightText: hLinesB[lb],
				})
			}
		}

		curLineA, curLineB := 0, 0
		for _, p := range validAnchors {
			emitSub(curLineA, p.la, curLineB, p.lb)

			if hLinesA[p.la] == hLinesB[p.lb] && !lineHasEditsA[p.la] && !lineHasEditsB[p.lb] {
				hunkRows = append(hunkRows, Row{
					Type:      RowEqual,
					LeftLine:  h.a0 + p.la + 1,
					RightLine: h.b0 + p.lb + 1,
					LeftText:  hLinesA[p.la],
					RightText: hLinesB[p.lb],
				})
			} else {
				hunkRows = append(hunkRows, Row{
					Type:       RowChange,
					LeftLine:   h.a0 + p.la + 1,
					RightLine:  h.b0 + p.lb + 1,
					LeftText:   hLinesA[p.la],
					RightText:  hLinesB[p.lb],
					LeftIntra:  leftSpans[p.la],
					RightIntra: rightSpans[p.lb],
				})
			}
			curLineA = p.la + 1
			curLineB = p.lb + 1
		}
		emitSub(curLineA, numA, curLineB, numB)

		return hunkRows
	}

	var rows []Row
	curEqualA, curEqualB := 0, 0
	for _, h := range mergedHunks {
		for curEqualA < h.a0 && curEqualB < h.b0 {
			rows = append(rows, Row{
				Type:      RowEqual,
				LeftLine:  curEqualA + 1,
				RightLine: curEqualB + 1,
				LeftText:  left[curEqualA],
				RightText: right[curEqualB],
			})
			curEqualA++
			curEqualB++
		}
		rows = append(rows, diffHunk(h)...)
		curEqualA = h.a1
		curEqualB = h.b1
	}
	for curEqualA < len(left) && curEqualB < len(right) {
		rows = append(rows, Row{
			Type:      RowEqual,
			LeftLine:  curEqualA + 1,
			RightLine: curEqualB + 1,
			LeftText:  left[curEqualA],
			RightText: right[curEqualB],
		})
		curEqualA++
		curEqualB++
	}

	return rows
}

// ownLines returns a boolean slice for lines in file (1-based index mapped to 0-based),
// where true means the line was added or modified relative to parent.
func ownLines(parent, file []byte) []bool {
	fLines := SplitLines(file)
	if len(fLines) == 0 {
		return nil
	}
	if len(parent) == 0 {
		owned := make([]bool, len(fLines))
		for i := range owned {
			owned[i] = true
		}
		return owned
	}

	pLines := SplitLines(parent)
	edits := Edits(pLines, fLines)
	owned := make([]bool, len(fLines))
	for _, e := range edits {
		if e.Op == OpInsert {
			owned[e.BIndex] = true
		}
	}
	return owned
}

// MarkRebased marks lines in rows that were not introduced by the commit relative
// to its own parent (e.g. changes introduced purely by rebasing over upstream changes).
func MarkRebased(rows []Row, old, new, oldParent, newParent []byte) {
	var leftOwned []bool
	if len(oldParent) > 0 && len(old) > 0 {
		leftOwned = ownLines(oldParent, old)
	}
	var rightOwned []bool
	if len(newParent) > 0 && len(new) > 0 {
		rightOwned = ownLines(newParent, new)
	}

	for i := range rows {
		r := &rows[i]
		if r.Type == RowEqual || r.Type == RowSkip {
			continue
		}
		if leftOwned != nil && r.LeftLine > 0 && r.LeftLine <= len(leftOwned) {
			if !leftOwned[r.LeftLine-1] {
				r.RebasedL = true
			}
		}
		if rightOwned != nil && r.RightLine > 0 && r.RightLine <= len(rightOwned) {
			if !rightOwned[r.RightLine-1] {
				r.RebasedR = true
			}
		}
	}
}

// AllRebased returns true if all modified rows in rows are rebase-inherited.
// Returns false if there are no modified rows.
func AllRebased(rows []Row) bool {
	hasDiff := false
	for _, r := range rows {
		if r.Type == RowEqual || r.Type == RowSkip {
			continue
		}
		hasDiff = true
		switch r.Type {
		case RowDel:
			if !r.RebasedL {
				return false
			}
		case RowAdd:
			if !r.RebasedR {
				return false
			}
		case RowChange:
			if !r.RebasedL || !r.RebasedR {
				return false
			}
		}
	}
	return hasDiff
}
