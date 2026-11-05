package diff

import (
	"bytes"
)

// Op represents the edit operation type.
type Op int

const (
	OpEqual Op = iota
	OpInsert
	OpDelete
)

// Edit represents a single diff edit element.
type Edit struct {
	Op     Op
	AIndex int // 0-based index in a (-1 if OpInsert)
	BIndex int // 0-based index in b (-1 if OpDelete)
}

// RowType indicates the diff display type for a side-by-side or unified row.
type RowType int

const (
	RowEqual RowType = iota
	RowChange
	RowAdd
	RowDel
	RowSkip
)

// MoveInfo represents metadata for a moved code block or line in side-by-side or unified diffs.
type MoveInfo struct {
	GroupID       int    // 1-based move group ID
	PeerPath      string // "" when intra-file, otherwise slash path of counterpart file (no leading "/")
	PeerLine      int    // 1-based counterpart line (0 if this line is an unpaired insert/delete inside a fuzzy moved block)
	PeerStart     int    // 1-based start line of counterpart block
	PeerEnd       int    // 1-based end line of counterpart block
	BlockStart    bool   // true on the first line of this moved block
	BlockModified bool   // true if any line in this moved block was modified (for badge label)
	Modified      bool   // true if THIS line differs from PeerLine (or is an unpaired insert/delete inside the moved block)
	MovedIntra    []Span // byte-offset intraline spans on THIS line relative to PeerLine (when Modified && PeerLine > 0)
}

// Row represents a line in side-by-side or unified diff rendering.
type Row struct {
	Type       RowType
	LeftLine   int // 1-based, 0 if empty
	RightLine  int // 1-based, 0 if empty
	LeftText   string
	RightText  string
	LeftIntra  []Span
	RightIntra []Span
	RebasedL   bool
	RebasedR   bool
	MovedL     *MoveInfo
	MovedR     *MoveInfo
	SkipCount  int
	SkipGroup  int
	AtTop      bool
	AtBottom   bool
	LeftStart  int
	LeftEnd    int
	RightStart int
	RightEnd   int
}

// SplitLines splits data into lines, stripping trailing \r and \n.
// An empty slice produces an empty list of lines.
func SplitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	raw := bytes.Split(data, []byte("\n"))
	if len(raw) > 0 && len(raw[len(raw)-1]) == 0 {
		raw = raw[:len(raw)-1]
	}
	lines := make([]string, len(raw))
	for i, b := range raw {
		b = bytes.TrimSuffix(b, []byte("\r"))
		lines[i] = string(b)
	}
	return lines
}

// IsBinary checks whether data appears to be binary by searching for a NUL byte in the first 8KB.
func IsBinary(data []byte) bool {
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	return bytes.IndexByte(data[:limit], 0) != -1
}

// Edits computes the shortest edit script (SES) transforming a into b using linear-space
// divide-and-conquer (Hirschberg / Myers LCS) with O(min(N, M)) memory.
func Edits[T comparable](a, b []T) []Edit {
	var res []Edit
	var scratch []int
	editsRecursive(a, b, 0, 0, &res, 0, &scratch)
	return res
}

const maxRecursionDepth = 64

func editsRecursive[T comparable](a, b []T, aOff, bOff int, out *[]Edit, depth int, scratch *[]int) {
	n := len(a)
	m := len(b)

	// Strip common prefix
	prefix := 0
	for prefix < n && prefix < m && a[prefix] == b[prefix] {
		*out = append(*out, Edit{Op: OpEqual, AIndex: aOff + prefix, BIndex: bOff + prefix})
		prefix++
	}
	a = a[prefix:]
	b = b[prefix:]
	aOff += prefix
	bOff += prefix
	n = len(a)
	m = len(b)

	// Strip common suffix
	suffix := 0
	for suffix < n && suffix < m && a[n-1-suffix] == b[m-1-suffix] {
		suffix++
	}
	a = a[:n-suffix]
	b = b[:m-suffix]
	n = len(a)
	m = len(b)

	if n == 0 {
		for i := 0; i < m; i++ {
			*out = append(*out, Edit{Op: OpInsert, AIndex: -1, BIndex: bOff + i})
		}
	} else if m == 0 {
		for i := 0; i < n; i++ {
			*out = append(*out, Edit{Op: OpDelete, AIndex: aOff + i, BIndex: -1})
		}
	} else if n == 1 {
		matchIdx := -1
		for j := 0; j < m; j++ {
			if a[0] == b[j] {
				matchIdx = j
				break
			}
		}
		if matchIdx != -1 {
			for j := 0; j < matchIdx; j++ {
				*out = append(*out, Edit{Op: OpInsert, AIndex: -1, BIndex: bOff + j})
			}
			*out = append(*out, Edit{Op: OpEqual, AIndex: aOff, BIndex: bOff + matchIdx})
			for j := matchIdx + 1; j < m; j++ {
				*out = append(*out, Edit{Op: OpInsert, AIndex: -1, BIndex: bOff + j})
			}
		} else {
			*out = append(*out, Edit{Op: OpDelete, AIndex: aOff, BIndex: -1})
			for j := 0; j < m; j++ {
				*out = append(*out, Edit{Op: OpInsert, AIndex: -1, BIndex: bOff + j})
			}
		}
	} else if m == 1 {
		matchIdx := -1
		for i := 0; i < n; i++ {
			if a[i] == b[0] {
				matchIdx = i
				break
			}
		}
		if matchIdx != -1 {
			for i := 0; i < matchIdx; i++ {
				*out = append(*out, Edit{Op: OpDelete, AIndex: aOff + i, BIndex: -1})
			}
			*out = append(*out, Edit{Op: OpEqual, AIndex: aOff + matchIdx, BIndex: bOff})
			for i := matchIdx + 1; i < n; i++ {
				*out = append(*out, Edit{Op: OpDelete, AIndex: aOff + i, BIndex: -1})
			}
		} else {
			for i := 0; i < n; i++ {
				*out = append(*out, Edit{Op: OpDelete, AIndex: aOff + i, BIndex: -1})
			}
			*out = append(*out, Edit{Op: OpInsert, AIndex: -1, BIndex: bOff})
		}
	} else if depth > maxRecursionDepth {
		// Pathological depth cutoff: treat remaining as replace
		for i := 0; i < n; i++ {
			*out = append(*out, Edit{Op: OpDelete, AIndex: aOff + i, BIndex: -1})
		}
		for j := 0; j < m; j++ {
			*out = append(*out, Edit{Op: OpInsert, AIndex: -1, BIndex: bOff + j})
		}
	} else {
		uA, uB, vA, vB, ok := findMiddleSnake(a, b, scratch)
		validSnake := ok && 0 <= uA && uA <= vA && vA <= n && 0 <= uB && uB <= vB && vB <= m && (vA-uA == vB-uB)
		if !validSnake || (uA == 0 && uB == 0 && vA == 0 && vB == 0) || (uA == n && uB == m && vA == n && vB == m) {
			// Fallback: split in half
			midA := n / 2
			midB := m / 2
			editsRecursive(a[:midA], b[:midB], aOff, bOff, out, depth+1, scratch)
			editsRecursive(a[midA:], b[midB:], aOff+midA, bOff+midB, out, depth+1, scratch)
		} else {
			// Recurse left
			editsRecursive(a[:uA], b[:uB], aOff, bOff, out, depth+1, scratch)

			// Emit middle snake equals
			for k := 0; k < vA-uA; k++ {
				*out = append(*out, Edit{Op: OpEqual, AIndex: aOff + uA + k, BIndex: bOff + uB + k})
			}

			// Recurse right
			editsRecursive(a[vA:], b[vB:], aOff+vA, bOff+vB, out, depth+1, scratch)
		}
	}

	for i := 0; i < suffix; i++ {
		*out = append(*out, Edit{Op: OpEqual, AIndex: aOff + n + i, BIndex: bOff + m + i})
	}
}

func findMiddleSnake[T comparable](a, b []T, scratch *[]int) (snakeStartA, snakeStartB, snakeEndA, snakeEndB int, found bool) {
	n := len(a)
	m := len(b)
	delta := n - m
	odd := (delta&1 != 0)

	maxD := (n + m + 1) / 2
	if maxD > 8192 {
		maxD = 8192
	}

	absDelta := delta
	if absDelta < 0 {
		absDelta = -absDelta
	}

	offset := maxD + absDelta + 4
	size := 2*offset + 8
	need := 2 * size
	if scratch != nil {
		if cap(*scratch) < need {
			*scratch = make([]int, need)
		} else {
			*scratch = (*scratch)[:need]
		}
	} else {
		buf := make([]int, need)
		scratch = &buf
	}
	vf := (*scratch)[:size]
	vb := (*scratch)[size:need]

	for i := range vf {
		vf[i] = -1
		vb[i] = n + 1
	}
	vf[offset+1] = 0
	vb[offset+delta-1] = n

	for d := 0; d <= maxD; d++ {
		// Forward pass
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && vf[k-1+offset] < vf[k+1+offset]) {
				x = vf[k+1+offset]
			} else {
				x = vf[k-1+offset] + 1
			}
			y := x - k
			startX := x
			startY := y

			for x >= 0 && y >= 0 && x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			vf[k+offset] = x

			if odd && k >= delta-(d-1) && k <= delta+(d-1) {
				if vb[k+offset] <= n && x >= vb[k+offset] {
					if startX >= 0 && startX <= n && startY >= 0 && startY <= m && x >= 0 && x <= n && y >= 0 && y <= m {
						return startX, startY, x, y, true
					}
				}
			}
		}

		// Backward pass
		for k := delta - d; k <= delta+d; k += 2 {
			var x int
			if k == delta+d || (k != delta-d && vb[k-1+offset] <= vb[k+1+offset]) {
				x = vb[k-1+offset]
			} else {
				x = vb[k+1+offset] - 1
			}
			y := x - k
			endX := x
			endY := y

			for x > 0 && y > 0 && x <= n && y <= m && a[x-1] == b[y-1] {
				x--
				y--
			}
			vb[k+offset] = x

			if !odd && k >= -d && k <= d {
				if vf[k+offset] >= 0 && vf[k+offset] >= x {
					if x >= 0 && x <= n && y >= 0 && y <= m && endX >= 0 && endX <= n && endY >= 0 && endY <= m {
						return x, y, endX, endY, true
					}
				}
			}
		}
	}

	return 0, 0, 0, 0, false
}

// BuildRows creates side-by-side rows from left lines, right lines, and edits.
func BuildRows(left, right []string, edits []Edit) []Row {
	var rows []Row
	i := 0
	for i < len(edits) {
		e := edits[i]
		if e.Op == OpEqual {
			rows = append(rows, Row{
				Type:      RowEqual,
				LeftLine:  e.AIndex + 1,
				RightLine: e.BIndex + 1,
				LeftText:  left[e.AIndex],
				RightText: right[e.BIndex],
			})
			i++
			continue
		}

		// Collect contiguous deletions and insertions
		var dels []int
		var inss []int
		for i < len(edits) && edits[i].Op != OpEqual {
			if edits[i].Op == OpDelete {
				dels = append(dels, edits[i].AIndex)
			} else {
				inss = append(inss, edits[i].BIndex)
			}
			i++
		}

		// Pair deletions and insertions as RowChange
		minLen := len(dels)
		if len(inss) < minLen {
			minLen = len(inss)
		}

		for k := 0; k < minLen; k++ {
			rows = append(rows, Row{
				Type:      RowChange,
				LeftLine:  dels[k] + 1,
				RightLine: inss[k] + 1,
				LeftText:  left[dels[k]],
				RightText: right[inss[k]],
			})
		}
		for k := minLen; k < len(dels); k++ {
			rows = append(rows, Row{
				Type:     RowDel,
				LeftLine: dels[k] + 1,
				LeftText: left[dels[k]],
			})
		}
		for k := minLen; k < len(inss); k++ {
			rows = append(rows, Row{
				Type:      RowAdd,
				RightLine: inss[k] + 1,
				RightText: right[inss[k]],
			})
		}
	}
	return rows
}

// Collapse collapses unchanged rows outside ctxLines around changed rows.
// keepLine, if non-nil, forces keeping lines for which keepLine(left, right) is true.
func Collapse(rows []Row, ctxLines int, keepLine func(leftLine, rightLine int) bool) []Row {
	if len(rows) == 0 {
		return nil
	}
	if ctxLines < 0 {
		ctxLines = 0
	}

	keep := make([]bool, len(rows))
	for i, r := range rows {
		if r.Type != RowEqual || (keepLine != nil && keepLine(r.LeftLine, r.RightLine)) {
			start := i - ctxLines
			if start < 0 {
				start = 0
			}
			end := i + ctxLines
			if end >= len(rows) {
				end = len(rows) - 1
			}
			for j := start; j <= end; j++ {
				keep[j] = true
			}
		}
	}

	var res []Row
	skipGroup := 0
	i := 0
	for i < len(rows) {
		if keep[i] {
			res = append(res, rows[i])
			i++
			continue
		}

		start := i
		for i < len(rows) && !keep[i] {
			i++
		}
		count := i - start
		skipGroup++

		atTop := (start == 0)
		atBottom := (i == len(rows))

		res = append(res, Row{
			Type:       RowSkip,
			SkipCount:  count,
			SkipGroup:  skipGroup,
			AtTop:      atTop,
			AtBottom:   atBottom,
			LeftStart:  rows[start].LeftLine,
			LeftEnd:    rows[i-1].LeftLine,
			RightStart: rows[start].RightLine,
			RightEnd:   rows[i-1].RightLine,
		})
	}
	return res
}

// Unified converts side-by-side rows into a unified diff row sequence.
func Unified(rows []Row) []Row {
	var res []Row
	i := 0
	for i < len(rows) {
		r := rows[i]
		if r.Type == RowEqual || r.Type == RowSkip {
			res = append(res, r)
			i++
			continue
		}

		var dels []Row
		var inss []Row
		for i < len(rows) && rows[i].Type != RowEqual && rows[i].Type != RowSkip {
			cur := rows[i]
			switch cur.Type {
			case RowDel:
				dels = append(dels, Row{
					Type:      RowDel,
					LeftLine:  cur.LeftLine,
					LeftText:  cur.LeftText,
					LeftIntra: cur.LeftIntra,
					RebasedL:  cur.RebasedL,
					MovedL:    cur.MovedL,
					SkipGroup: cur.SkipGroup,
				})
			case RowAdd:
				inss = append(inss, Row{
					Type:       RowAdd,
					RightLine:  cur.RightLine,
					RightText:  cur.RightText,
					RightIntra: cur.RightIntra,
					RebasedR:   cur.RebasedR,
					MovedR:     cur.MovedR,
					SkipGroup:  cur.SkipGroup,
				})
			case RowChange:
				dels = append(dels, Row{
					Type:      RowDel,
					LeftLine:  cur.LeftLine,
					LeftText:  cur.LeftText,
					LeftIntra: cur.LeftIntra,
					RebasedL:  cur.RebasedL,
					MovedL:    cur.MovedL,
					SkipGroup: cur.SkipGroup,
				})
				inss = append(inss, Row{
					Type:       RowAdd,
					RightLine:  cur.RightLine,
					RightText:  cur.RightText,
					RightIntra: cur.RightIntra,
					RebasedR:   cur.RebasedR,
					MovedR:     cur.MovedR,
					SkipGroup:  cur.SkipGroup,
				})
			}
			i++
		}
		res = append(res, dels...)
		res = append(res, inss...)
	}
	return res
}
