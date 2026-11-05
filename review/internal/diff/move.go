package diff

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MoveFile represents a file diff input and its Myers diff rows for move detection.
type MoveFile struct {
	Path       string
	LeftLines  []string
	RightLines []string
	Rows       []Row
}

// normLine strips leading and trailing whitespace from a line.
func normLine(s string) string {
	return strings.TrimSpace(s)
}

// isSignificantLine returns true if the normalized line is not trivial boilerplate,
// pure punctuation/braces, or excessively short.
func isSignificantLine(norm string) bool {
	if norm == "" || len(norm) <= 2 {
		return false
	}
	switch norm {
	case "{", "}", ")", "});", "},", "else {", "return", "return nil", "return err",
		"if err != nil {", "break", "continue", "default:", "package main":
		return false
	}
	// Check if the line consists entirely of punctuation, symbols, or whitespace.
	allPunct := true
	for _, r := range norm {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) && !unicode.IsSpace(r) {
			allPunct = false
			break
		}
	}
	return !allPunct
}

func countNonWSBytes(s string) int {
	cnt := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			cnt++
		}
	}
	return cnt
}

func meetsSignificanceThreshold(normLines []string, rawLines []string) bool {
	sig := 0
	nonEmpty := 0
	nonWS := 0
	for i, norm := range normLines {
		if norm != "" {
			nonEmpty++
		}
		if isSignificantLine(norm) {
			sig++
		}
		if i < len(rawLines) {
			nonWS += countNonWSBytes(rawLines[i])
		}
	}
	return sig >= 3 && (nonEmpty >= 4 || nonWS >= 80)
}

func lineSim(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}
	ra := utf8.RuneCountInString(s1)
	rb := utf8.RuneCountInString(s2)
	minLen := ra
	maxLen := rb
	if minLen > maxLen {
		minLen, maxLen = maxLen, minLen
	}
	if maxLen == 0 {
		return 1.0
	}
	if float64(minLen)/float64(maxLen) < 0.55 {
		return 0.0
	}
	r1 := []rune(s1)
	r2 := []rune(s2)
	edits := Edits(r1, r2)
	common := 0
	for _, e := range edits {
		if e.Op == OpEqual {
			common++
		}
	}
	return float64(common) / float64(maxLen)
}

type moveChunk struct {
	fileIdx   int
	hunkID    int
	lines     []int    // 1-based line numbers
	normLines []string // TrimSpace
	rawLines  []string // raw line text
}

type delLoc struct {
	chunkIdx int
	lineIdx  int
}

type alignedPair struct {
	dIdx int     // index in delChunk (-1 if add only)
	aIdx int     // index in addChunk (-1 if del only)
	sim  float64 // 1.0 for OpEqual, 0.55..1.0 for fuzzy pair, 0 for unpaired
}

type candidateMatch struct {
	dChunkIdx int
	aChunkIdx int
	d0, d1    int // range in delChunk
	a0, a1    int // range in addChunk
	pairs     []alignedPair
	score     float64
}

type fileLineKey struct {
	fileIdx int
	line    int
}

// DetectMoves inspects diff rows across files to detect verbatim and fuzzy/modified code moves
// (both intra-file and cross-file). It populates MovedL and MovedR on Rows and splits any
// unrelated RowChange rows so moved blocks stay contiguous and clean.
func DetectMoves(files []MoveFile) {
	if len(files) == 0 {
		return
	}

	// 1. Extract candidate deleted and added runs from non-RowEqual hunks.
	var delChunks []moveChunk
	var addChunks []moveChunk

	totalDelLines := 0
	totalAddLines := 0

	for fileIdx, f := range files {
		hunkID := 0
		i := 0
		for i < len(f.Rows) {
			if f.Rows[i].Type == RowEqual || f.Rows[i].Type == RowSkip {
				i++
				continue
			}

			start := i
			for i < len(f.Rows) && f.Rows[i].Type != RowEqual && f.Rows[i].Type != RowSkip {
				i++
			}

			var dLines []int
			var dNorms []string
			var dRaws []string

			var aLines []int
			var aNorms []string
			var aRaws []string

			for k := start; k < i; k++ {
				r := f.Rows[k]
				if r.LeftLine > 0 && (r.Type == RowDel || r.Type == RowChange) {
					dLines = append(dLines, r.LeftLine)
					raw := ""
					if r.LeftLine-1 < len(f.LeftLines) {
						raw = f.LeftLines[r.LeftLine-1]
					}
					dRaws = append(dRaws, raw)
					dNorms = append(dNorms, normLine(raw))
				}
				if r.RightLine > 0 && (r.Type == RowAdd || r.Type == RowChange) {
					aLines = append(aLines, r.RightLine)
					raw := ""
					if r.RightLine-1 < len(f.RightLines) {
						raw = f.RightLines[r.RightLine-1]
					}
					aRaws = append(aRaws, raw)
					aNorms = append(aNorms, normLine(raw))
				}
			}

			if len(dLines) > 0 {
				delChunks = append(delChunks, moveChunk{
					fileIdx:   fileIdx,
					hunkID:    hunkID,
					lines:     dLines,
					normLines: dNorms,
					rawLines:  dRaws,
				})
				totalDelLines += len(dLines)
			}
			if len(aLines) > 0 {
				addChunks = append(addChunks, moveChunk{
					fileIdx:   fileIdx,
					hunkID:    hunkID,
					lines:     aLines,
					normLines: aNorms,
					rawLines:  aRaws,
				})
				totalAddLines += len(aLines)
			}
			hunkID++
		}
	}

	if len(delChunks) == 0 || len(addChunks) == 0 {
		return
	}

	// Guard total work: if total deleted + added lines exceeds 15,000 across files,
	// run DetectMoves per-file individually (intra-file only).
	if len(files) > 1 && totalDelLines+totalAddLines > 15000 {
		for i := range files {
			DetectMoves(files[i : i+1])
		}
		return
	}

	// 2. Build inverted index of normalized significant lines and bigrams for delChunks.
	sigIndex := make(map[string][]delLoc)
	bigramIndex := make(map[string][]delLoc)

	for dIdx, chunk := range delChunks {
		for lineIdx, norm := range chunk.normLines {
			if isSignificantLine(norm) {
				sigIndex[norm] = append(sigIndex[norm], delLoc{chunkIdx: dIdx, lineIdx: lineIdx})
			}
			if lineIdx+1 < len(chunk.normLines) {
				nextNorm := chunk.normLines[lineIdx+1]
				if norm != "" && nextNorm != "" {
					bigram := norm + "\n" + nextNorm
					bigramIndex[bigram] = append(bigramIndex[bigram], delLoc{chunkIdx: dIdx, lineIdx: lineIdx})
				}
			}
		}
	}

	// 3. Gather candidate (delChunk, addChunk) pairs.
	type chunkPairKey struct {
		dIdx int
		aIdx int
	}
	candidatePairs := make(map[chunkPairKey]bool)

	for aIdx, aChunk := range addChunks {
		matchedDels := make(map[int]bool)

		for lineIdx, norm := range aChunk.normLines {
			if isSignificantLine(norm) {
				locs := sigIndex[norm]
				if len(locs) <= 32 {
					for _, loc := range locs {
						matchedDels[loc.chunkIdx] = true
					}
				}
			}
			if lineIdx+1 < len(aChunk.normLines) {
				nextNorm := aChunk.normLines[lineIdx+1]
				if norm != "" && nextNorm != "" {
					bigram := norm + "\n" + nextNorm
					locs := bigramIndex[bigram]
					if len(locs) <= 32 {
						for _, loc := range locs {
							matchedDels[loc.chunkIdx] = true
						}
					}
				}
			}
		}

		if len(delChunks)+len(addChunks) <= 120 && len(aChunk.lines) >= 4 {
			for dIdx, dChunk := range delChunks {
				if len(dChunk.lines) >= 4 && !matchedDels[dIdx] {
					if tokenOverlap(dChunk.normLines, aChunk.normLines) >= 0.40 {
						matchedDels[dIdx] = true
					}
				}
			}
		}

		for dIdx := range matchedDels {
			dChunk := delChunks[dIdx]
			// Forbid matching delChunk with addChunk from the SAME (fileIdx, hunkID)
			if !(dChunk.fileIdx == aChunk.fileIdx && dChunk.hunkID == aChunk.hunkID) {
				candidatePairs[chunkPairKey{dIdx: dIdx, aIdx: aIdx}] = true
			}
		}
	}

	// 4. Align candidate pairs and find maximal contiguous sub-blocks.
	var candidates []candidateMatch

	for pair := range candidatePairs {
		dChunk := delChunks[pair.dIdx]
		aChunk := addChunks[pair.aIdx]

		edits := Edits(dChunk.normLines, aChunk.normLines)
		aligned := alignChunks(dChunk.normLines, aChunk.normLines, edits)
		if len(aligned) == 0 {
			continue
		}

		// Find candidate sub-blocks in aligned
		matches := findCandidateBlocks(aligned, dChunk, aChunk, pair.dIdx, pair.aIdx)
		candidates = append(candidates, matches...)
	}

	if len(candidates) == 0 {
		return
	}

	// 5. Sort candidate matches descending by score.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		lenI := candidates[i].d1 - candidates[i].d0
		lenJ := candidates[j].d1 - candidates[j].d0
		if lenI != lenJ {
			return lenI > lenJ
		}
		if candidates[i].dChunkIdx != candidates[j].dChunkIdx {
			return candidates[i].dChunkIdx < candidates[j].dChunkIdx
		}
		if candidates[i].aChunkIdx != candidates[j].aChunkIdx {
			return candidates[i].aChunkIdx < candidates[j].aChunkIdx
		}
		if candidates[i].d0 != candidates[j].d0 {
			return candidates[i].d0 < candidates[j].d0
		}
		return candidates[i].a0 < candidates[j].a0
	})

	// 6. Greedily accept non-overlapping line ranges.
	usedLeft := make(map[fileLineKey]bool)
	usedRight := make(map[fileLineKey]bool)

	leftMoveInfos := make(map[fileLineKey]*MoveInfo)
	rightMoveInfos := make(map[fileLineKey]*MoveInfo)

	nextGroupID := 1

	for _, cand := range candidates {
		dChunk := delChunks[cand.dChunkIdx]
		aChunk := addChunks[cand.aChunkIdx]

		// Check overlap
		overlapped := false
		for d := cand.d0; d <= cand.d1; d++ {
			if usedLeft[fileLineKey{fileIdx: dChunk.fileIdx, line: dChunk.lines[d]}] {
				overlapped = true
				break
			}
		}
		if overlapped {
			continue
		}
		for a := cand.a0; a <= cand.a1; a++ {
			if usedRight[fileLineKey{fileIdx: aChunk.fileIdx, line: aChunk.lines[a]}] {
				overlapped = true
				break
			}
		}
		if overlapped {
			continue
		}

		// Mark used
		for d := cand.d0; d <= cand.d1; d++ {
			usedLeft[fileLineKey{fileIdx: dChunk.fileIdx, line: dChunk.lines[d]}] = true
		}
		for a := cand.a0; a <= cand.a1; a++ {
			usedRight[fileLineKey{fileIdx: aChunk.fileIdx, line: aChunk.lines[a]}] = true
		}

		groupID := nextGroupID
		nextGroupID++

		leftStart := dChunk.lines[cand.d0]
		leftEnd := dChunk.lines[cand.d1]
		rightStart := aChunk.lines[cand.a0]
		rightEnd := aChunk.lines[cand.a1]

		leftPeerPath := ""
		if dChunk.fileIdx != aChunk.fileIdx {
			leftPeerPath = strings.TrimPrefix(files[aChunk.fileIdx].Path, "/")
		}
		rightPeerPath := ""
		if dChunk.fileIdx != aChunk.fileIdx {
			rightPeerPath = strings.TrimPrefix(files[dChunk.fileIdx].Path, "/")
		}

		pairedAddLine := make(map[int]int)
		pairedDelLine := make(map[int]int)
		for _, p := range cand.pairs {
			if p.dIdx >= cand.d0 && p.dIdx <= cand.d1 && p.aIdx >= cand.a0 && p.aIdx <= cand.a1 && p.sim >= 0.55 {
				lLine := dChunk.lines[p.dIdx]
				rLine := aChunk.lines[p.aIdx]
				pairedAddLine[lLine] = rLine
				pairedDelLine[rLine] = lLine
			}
		}

		blockModified := false
		var curBlockLeft []*MoveInfo
		var curBlockRight []*MoveInfo

		for d := cand.d0; d <= cand.d1; d++ {
			lLine := dChunk.lines[d]
			rLine := pairedAddLine[lLine]

			info := &MoveInfo{
				GroupID:   groupID,
				PeerPath:  leftPeerPath,
				PeerLine:  rLine,
				PeerStart: rightStart,
				PeerEnd:   rightEnd,
			}

			if rLine == 0 {
				info.Modified = true
				blockModified = true
			} else {
				lRaw := files[dChunk.fileIdx].LeftLines[lLine-1]
				rRaw := files[aChunk.fileIdx].RightLines[rLine-1]
				if strings.TrimSpace(lRaw) != strings.TrimSpace(rRaw) {
					info.Modified = true
					blockModified = true
					lSpans, _, ok := Intraline(lRaw, rRaw)
					if ok {
						info.MovedIntra = lSpans
					}
				}
			}
			curBlockLeft = append(curBlockLeft, info)
			leftMoveInfos[fileLineKey{fileIdx: dChunk.fileIdx, line: lLine}] = info
		}

		for a := cand.a0; a <= cand.a1; a++ {
			rLine := aChunk.lines[a]
			lLine := pairedDelLine[rLine]

			info := &MoveInfo{
				GroupID:   groupID,
				PeerPath:  rightPeerPath,
				PeerLine:  lLine,
				PeerStart: leftStart,
				PeerEnd:   leftEnd,
			}

			if lLine == 0 {
				info.Modified = true
				blockModified = true
			} else {
				lRaw := files[dChunk.fileIdx].LeftLines[lLine-1]
				rRaw := files[aChunk.fileIdx].RightLines[rLine-1]
				if strings.TrimSpace(lRaw) != strings.TrimSpace(rRaw) {
					info.Modified = true
					blockModified = true
					_, rSpans, ok := Intraline(lRaw, rRaw)
					if ok {
						info.MovedIntra = rSpans
					}
				}
			}
			curBlockRight = append(curBlockRight, info)
			rightMoveInfos[fileLineKey{fileIdx: aChunk.fileIdx, line: rLine}] = info
		}

		if blockModified {
			for _, m := range curBlockLeft {
				m.BlockModified = true
			}
			for _, m := range curBlockRight {
				m.BlockModified = true
			}
		}
	}

	// 7. Apply moves to Rows and cleanly split hunks.
	applyMovesToFiles(files, leftMoveInfos, rightMoveInfos)
}

func alignChunks(dNorms, aNorms []string, edits []Edit) []alignedPair {
	var res []alignedPair
	i := 0
	for i < len(edits) {
		if edits[i].Op == OpEqual {
			res = append(res, alignedPair{
				dIdx: edits[i].AIndex,
				aIdx: edits[i].BIndex,
				sim:  1.0,
			})
			i++
			continue
		}

		var dList []int
		var aList []int
		for i < len(edits) && edits[i].Op != OpEqual {
			if edits[i].Op == OpDelete {
				dList = append(dList, edits[i].AIndex)
			} else if edits[i].Op == OpInsert {
				aList = append(aList, edits[i].BIndex)
			}
			i++
		}

		if len(dList) == 0 {
			for _, a := range aList {
				res = append(res, alignedPair{dIdx: -1, aIdx: a, sim: 0})
			}
			continue
		}
		if len(aList) == 0 {
			for _, d := range dList {
				res = append(res, alignedPair{dIdx: d, aIdx: -1, sim: 0})
			}
			continue
		}

		// Align dList and aList using dynamic programming
		nD := len(dList)
		nA := len(aList)
		if nD > 64 || nA > 64 {
			for _, d := range dList {
				res = append(res, alignedPair{dIdx: d, aIdx: -1, sim: 0})
			}
			for _, a := range aList {
				res = append(res, alignedPair{dIdx: -1, aIdx: a, sim: 0})
			}
			continue
		}
		dp := make([][]float64, nD+1)
		for r := range dp {
			dp[r] = make([]float64, nA+1)
		}

		sims := make([][]float64, nD)
		for r := range sims {
			sims[r] = make([]float64, nA)
			for c := range sims[r] {
				sims[r][c] = lineSim(dNorms[dList[r]], aNorms[aList[c]])
			}
		}

		for r := 1; r <= nD; r++ {
			for c := 1; c <= nA; c++ {
				score := dp[r-1][c]
				if dp[r][c-1] > score {
					score = dp[r][c-1]
				}
				s := sims[r-1][c-1]
				if s >= 0.55 {
					diag := dp[r-1][c-1] + s
					if diag > score {
						score = diag
					}
				}
				dp[r][c] = score
			}
		}

		// Backtrack to find paired elements
		type pairChoice struct {
			d int
			a int
			s float64
		}
		var matched []pairChoice
		r, c := nD, nA
		for r > 0 && c > 0 {
			s := sims[r-1][c-1]
			if s >= 0.55 && dp[r][c] == dp[r-1][c-1]+s {
				matched = append(matched, pairChoice{d: r - 1, a: c - 1, s: s})
				r--
				c--
			} else if dp[r-1][c] >= dp[r][c-1] {
				r--
			} else {
				c--
			}
		}

		// Reverse matched
		for mIdx, nIdx := 0, len(matched)-1; mIdx < nIdx; mIdx, nIdx = mIdx+1, nIdx-1 {
			matched[mIdx], matched[nIdx] = matched[nIdx], matched[mIdx]
		}

		// Emit elements in monotonic order
		curD := 0
		curA := 0
		for _, m := range matched {
			for curD < m.d {
				res = append(res, alignedPair{dIdx: dList[curD], aIdx: -1, sim: 0})
				curD++
			}
			for curA < m.a {
				res = append(res, alignedPair{dIdx: -1, aIdx: aList[curA], sim: 0})
				curA++
			}
			res = append(res, alignedPair{dIdx: dList[m.d], aIdx: aList[m.a], sim: m.s})
			curD++
			curA++
		}
		for curD < nD {
			res = append(res, alignedPair{dIdx: dList[curD], aIdx: -1, sim: 0})
			curD++
		}
		for curA < nA {
			res = append(res, alignedPair{dIdx: -1, aIdx: aList[curA], sim: 0})
			curA++
		}
	}
	return res
}

func findCandidateBlocks(aligned []alignedPair, dChunk, aChunk moveChunk, dChunkIdx, aChunkIdx int) []candidateMatch {
	var results []candidateMatch

	n := len(aligned)
	p0 := 0
	for p0 < n {
		// Must start on an anchor (exact-norm match or high similarity >= 0.70)
		startPair := aligned[p0]
		if startPair.dIdx < 0 || startPair.aIdx < 0 || (startPair.sim < 1.0 && startPair.sim < 0.70) {
			p0++
			continue
		}

		consecutiveUnmatched := 0
		bestP1 := -1

		p := p0
		for ; p < n; p++ {
			pair := aligned[p]
			if pair.dIdx < 0 || pair.aIdx < 0 || pair.sim < 0.55 {
				consecutiveUnmatched++
				if consecutiveUnmatched > 3 {
					break
				}
			} else {
				consecutiveUnmatched = 0
				if pair.sim == 1.0 || pair.sim >= 0.70 {
					bestP1 = p
				}
			}
		}

		if bestP1 < p0 {
			p0++
			continue
		}

		p1 := bestP1
		nextP0 := p
		if nextP0 <= p0 {
			nextP0 = p0 + 1
		}
		startIdx := p0
		p0 = nextP0

		// Determine line bounds in dChunk and aChunk
		minD, maxD := -1, -1
		minA, maxA := -1, -1
		exactAligned := 0
		fuzzyAligned := 0
		exactSigLines := 0
		sumSim := 0.0

		var subPairs []alignedPair
		for k := startIdx; k <= p1; k++ {
			pair := aligned[k]
			subPairs = append(subPairs, pair)
			if pair.dIdx >= 0 {
				if minD == -1 || pair.dIdx < minD {
					minD = pair.dIdx
				}
				if pair.dIdx > maxD {
					maxD = pair.dIdx
				}
			}
			if pair.aIdx >= 0 {
				if minA == -1 || pair.aIdx < minA {
					minA = pair.aIdx
				}
				if pair.aIdx > maxA {
					maxA = pair.aIdx
				}
			}
			if pair.dIdx >= 0 && pair.aIdx >= 0 && pair.sim >= 0.55 {
				if pair.sim == 1.0 {
					exactAligned++
					if isSignificantLine(dChunk.normLines[pair.dIdx]) {
						exactSigLines++
					}
				} else {
					fuzzyAligned++
				}
				sumSim += pair.sim
			}
		}

		if minD == -1 || maxD == -1 || minA == -1 || maxA == -1 {
			continue
		}

		dLen := maxD - minD + 1
		aLen := maxA - minA + 1
		maxLen := dLen
		if aLen > maxLen {
			maxLen = aLen
		}

		// 1. Overall aligned ratio >= 0.65
		if float64(exactAligned+fuzzyAligned)/float64(maxLen) < 0.65 {
			continue
		}

		// 2. At least 2 exact-norm significant lines or average similarity >= 0.75
		avgSim := sumSim / float64(maxLen)
		if exactSigLines < 2 && avgSim < 0.75 {
			continue
		}

		// 3. Minimum significance threshold on both deleted and added sides
		if !meetsSignificanceThreshold(dChunk.normLines[minD:maxD+1], dChunk.rawLines[minD:maxD+1]) {
			continue
		}
		if !meetsSignificanceThreshold(aChunk.normLines[minA:maxA+1], aChunk.rawLines[minA:maxA+1]) {
			continue
		}

		// Score computation
		nonWS := 0
		for _, raw := range dChunk.rawLines[minD : maxD+1] {
			nonWS += countNonWSBytes(raw)
		}
		for _, raw := range aChunk.rawLines[minA : maxA+1] {
			nonWS += countNonWSBytes(raw)
		}

		score := float64(exactSigLines*10+fuzzyAligned*6) + float64(nonWS)/20.0
		if dChunk.fileIdx == aChunk.fileIdx {
			score += 1.0 // Intra-file tie breaker
		}

		results = append(results, candidateMatch{
			dChunkIdx: dChunkIdx,
			aChunkIdx: aChunkIdx,
			d0:        minD,
			d1:        maxD,
			a0:        minA,
			a1:        maxA,
			pairs:     subPairs,
			score:     score,
		})
	}
	return results
}

func applyMovesToFiles(files []MoveFile, leftMoveInfos, rightMoveInfos map[fileLineKey]*MoveInfo) {
	for fileIdx := range files {
		f := &files[fileIdx]

		// Attach MoveInfos to original rows
		for i := range f.Rows {
			r := &f.Rows[i]
			if r.LeftLine > 0 {
				if m, ok := leftMoveInfos[fileLineKey{fileIdx: fileIdx, line: r.LeftLine}]; ok {
					cp := *m
					r.MovedL = &cp
				}
			}
			if r.RightLine > 0 {
				if m, ok := rightMoveInfos[fileLineKey{fileIdx: fileIdx, line: r.RightLine}]; ok {
					cp := *m
					r.MovedR = &cp
				}
			}
		}

		// Walk hunks and split unrelated RowChanges where one side is moved and the other is not
		var newRows []Row
		i := 0
		for i < len(f.Rows) {
			if f.Rows[i].Type == RowEqual || f.Rows[i].Type == RowSkip {
				newRows = append(newRows, f.Rows[i])
				i++
				continue
			}

			start := i
			for i < len(f.Rows) && f.Rows[i].Type != RowEqual && f.Rows[i].Type != RowSkip {
				i++
			}
			hunk := f.Rows[start:i]

			needsSplit := false
			for _, r := range hunk {
				if r.Type == RowChange {
					if (r.MovedL != nil) != (r.MovedR != nil) || (r.MovedL != nil && r.MovedR != nil && r.MovedL.GroupID != r.MovedR.GroupID) {
						needsSplit = true
						break
					}
				}
			}

			if !needsSplit {
				newRows = append(newRows, hunk...)
				continue
			}

			// Cleanly split hunk
			type entry struct {
				line    int
				text    string
				moved   *MoveInfo
				rebased bool
			}

			var leftEntries []entry
			var rightEntries []entry

			for _, r := range hunk {
				if r.LeftLine > 0 {
					leftEntries = append(leftEntries, entry{
						line:    r.LeftLine,
						text:    r.LeftText,
						moved:   r.MovedL,
						rebased: r.RebasedL,
					})
				}
				if r.RightLine > 0 {
					rightEntries = append(rightEntries, entry{
						line:    r.RightLine,
						text:    r.RightText,
						moved:   r.MovedR,
						rebased: r.RebasedR,
					})
				}
			}

			li, ri := 0, 0
			for li < len(leftEntries) || ri < len(rightEntries) {
				// 1. While li < len(leftEntries) && leftEntries[li].moved != nil: emit RowDel
				for li < len(leftEntries) && leftEntries[li].moved != nil {
					newRows = append(newRows, Row{
						Type:     RowDel,
						LeftLine: leftEntries[li].line,
						LeftText: leftEntries[li].text,
						MovedL:   leftEntries[li].moved,
						RebasedL: leftEntries[li].rebased,
					})
					li++
				}

				// 2. While ri < len(rightEntries) && rightEntries[ri].moved != nil: emit RowAdd
				for ri < len(rightEntries) && rightEntries[ri].moved != nil {
					newRows = append(newRows, Row{
						Type:      RowAdd,
						RightLine: rightEntries[ri].line,
						RightText: rightEntries[ri].text,
						MovedR:    rightEntries[ri].moved,
						RebasedR:  rightEntries[ri].rebased,
					})
					ri++
				}

				// 3. Find lEnd and rEnd
				lEnd := li
				for lEnd < len(leftEntries) && leftEntries[lEnd].moved == nil {
					lEnd++
				}
				rEnd := ri
				for rEnd < len(rightEntries) && rightEntries[rEnd].moved == nil {
					rEnd++
				}

				// 4. If li < lEnd || ri < rEnd: pair common unmoved as RowChange, emit remaining
				if li < lEnd || ri < rEnd {
					common := lEnd - li
					if rEnd-ri < common {
						common = rEnd - ri
					}
					for k := 0; k < common; k++ {
						lSpans, rSpans, ok := Intraline(leftEntries[li+k].text, rightEntries[ri+k].text)
						row := Row{
							Type:      RowChange,
							LeftLine:  leftEntries[li+k].line,
							RightLine: rightEntries[ri+k].line,
							LeftText:  leftEntries[li+k].text,
							RightText: rightEntries[ri+k].text,
							RebasedL:  leftEntries[li+k].rebased,
							RebasedR:  rightEntries[ri+k].rebased,
						}
						if ok {
							row.LeftIntra = lSpans
							row.RightIntra = rSpans
						}
						newRows = append(newRows, row)
					}
					for k := li + common; k < lEnd; k++ {
						newRows = append(newRows, Row{
							Type:     RowDel,
							LeftLine: leftEntries[k].line,
							LeftText: leftEntries[k].text,
							RebasedL: leftEntries[k].rebased,
						})
					}
					for k := ri + common; k < rEnd; k++ {
						newRows = append(newRows, Row{
							Type:      RowAdd,
							RightLine: rightEntries[k].line,
							RightText: rightEntries[k].text,
							RebasedR:  rightEntries[k].rebased,
						})
					}
					li = lEnd
					ri = rEnd
				}
			}
		}

		// Set BlockStart = true on the first row of each contiguous GroupID run
		prevLGroupID := 0
		prevRGroupID := 0
		for idx := range newRows {
			r := &newRows[idx]
			if r.MovedL != nil {
				cp := *r.MovedL
				if cp.GroupID != prevLGroupID {
					cp.BlockStart = true
					prevLGroupID = cp.GroupID
				} else {
					cp.BlockStart = false
				}
				r.MovedL = &cp
			} else {
				prevLGroupID = 0
			}

			if r.MovedR != nil {
				cp := *r.MovedR
				if cp.GroupID != prevRGroupID {
					cp.BlockStart = true
					prevRGroupID = cp.GroupID
				} else {
					cp.BlockStart = false
				}
				r.MovedR = &cp
			} else {
				prevRGroupID = 0
			}
		}

		f.Rows = newRows
	}
}

func tokenOverlap(dNorms, aNorms []string) float64 {
	dTokens := make(map[string]bool)
	for _, l := range dNorms {
		for _, w := range strings.Fields(l) {
			if len(w) > 2 {
				dTokens[w] = true
			}
		}
	}
	aTokens := make(map[string]bool)
	for _, l := range aNorms {
		for _, w := range strings.Fields(l) {
			if len(w) > 2 {
				aTokens[w] = true
			}
		}
	}
	if len(dTokens) == 0 || len(aTokens) == 0 {
		return 0
	}
	common := 0
	for t := range aTokens {
		if dTokens[t] {
			common++
		}
	}
	minLen := len(dTokens)
	if len(aTokens) < minLen {
		minLen = len(aTokens)
	}
	return float64(common) / float64(minLen)
}
