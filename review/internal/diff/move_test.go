package diff

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestVerbatimIntraFileMove(t *testing.T) {
	// Function moved from top to bottom, with an unrelated function added at top
	// so Myers diff initially pairs the deleted oldFoo with added barNew as RowChange.
	leftCode := `package main

func oldFoo(x int, y int) int {
	res := x * y
	if res > 100 {
		return res - 10
	}
	return res + 10
}

func middleAnchor() {
	println("middle anchor line 1")
	println("middle anchor line 2")
	println("middle anchor line 3")
	println("middle anchor line 4")
	println("middle anchor line 5")
	println("middle anchor line 6")
	println("middle anchor line 7")
	println("middle anchor line 8")
	println("middle anchor line 9")
	println("middle anchor line 10")
}
`
	rightCode := `package main

func barNew(a string, b string) string {
	combined := a + ":" + b
	prefix := "result: "
	msg := prefix + combined
	println(msg)
	return msg
}

func middleAnchor() {
	println("middle anchor line 1")
	println("middle anchor line 2")
	println("middle anchor line 3")
	println("middle anchor line 4")
	println("middle anchor line 5")
	println("middle anchor line 6")
	println("middle anchor line 7")
	println("middle anchor line 8")
	println("middle anchor line 9")
	println("middle anchor line 10")
}

func oldFoo(x int, y int) int {
	res := x * y
	if res > 100 {
		return res - 10
	}
	return res + 10
}
`
	leftLines := SplitLines([]byte(leftCode))
	rightLines := SplitLines([]byte(rightCode))
	edits := Edits(leftLines, rightLines)
	rows := BuildRows(leftLines, rightLines, edits)

	files := []MoveFile{
		{
			Path:       "main.go",
			LeftLines:  leftLines,
			RightLines: rightLines,
			Rows:       rows,
		},
	}

	DetectMoves(files)

	resRows := files[0].Rows

	// Check that oldFoo deletion at top has MovedL, and oldFoo addition at bottom has MovedR
	var movedDelLines []int
	var movedAddLines []int

	for _, r := range resRows {
		if r.MovedL != nil {
			if r.Type != RowDel {
				t.Errorf("expected moved deletion to be RowDel, got %v", r.Type)
			}
			movedDelLines = append(movedDelLines, r.LeftLine)
			if r.MovedL.PeerPath != "" {
				t.Errorf("expected empty PeerPath for intra-file move, got %q", r.MovedL.PeerPath)
			}
			if r.MovedL.Modified {
				t.Errorf("expected verbatim line to have Modified==false, got true for line %d", r.LeftLine)
			}
		}
		if r.MovedR != nil {
			if r.Type != RowAdd {
				t.Errorf("expected moved addition to be RowAdd, got %v", r.Type)
			}
			movedAddLines = append(movedAddLines, r.RightLine)
			if r.MovedR.PeerPath != "" {
				t.Errorf("expected empty PeerPath for intra-file move, got %q", r.MovedR.PeerPath)
			}
			if r.MovedR.Modified {
				t.Errorf("expected verbatim line to have Modified==false, got true for line %d", r.RightLine)
			}
		}
	}

	if len(movedDelLines) < 6 {
		t.Fatalf("expected at least 6 moved delete lines, got %d: %v", len(movedDelLines), movedDelLines)
	}
	if len(movedAddLines) < 6 {
		t.Fatalf("expected at least 6 moved add lines, got %d: %v", len(movedAddLines), movedAddLines)
	}

	// Verify BlockStart
	firstDelFound := false
	for _, r := range resRows {
		if r.MovedL != nil {
			if !firstDelFound {
				if !r.MovedL.BlockStart {
					t.Errorf("expected BlockStart==true on first moved del line %d", r.LeftLine)
				}
				firstDelFound = true
			} else {
				if r.MovedL.BlockStart {
					t.Errorf("expected BlockStart==false on subsequent moved del line %d", r.LeftLine)
				}
			}
		}
	}

	firstAddFound := false
	for _, r := range resRows {
		if r.MovedR != nil {
			if !firstAddFound {
				if !r.MovedR.BlockStart {
					t.Errorf("expected BlockStart==true on first moved add line %d", r.RightLine)
				}
				firstAddFound = true
			} else {
				if r.MovedR.BlockStart {
					t.Errorf("expected BlockStart==false on subsequent moved add line %d", r.RightLine)
				}
			}
		}
	}
}

func TestModifiedIntraFileMove(t *testing.T) {
	// A 20-line function moved, re-indented by 1 tab (extract to helper),
	// with 2 lines modified inside.
	leftLines := []string{
		"package main",
		"",
		"func caller() {",
		"\tif true {",
		"\t\tfirstStep := initializeState()",
		"\t\tconfig := loadDefaultConfig()",
		"\t\toptions := buildOptions(config)",
		"\t\tworkerPool := createPool(options)",
		"\t\tworkerPool.Start()",
		"\t\tdefer workerPool.Stop()",
		"\t\tfor i := 0; i < 100; i++ {",
		"\t\t\ttask := generateTask(i, firstStep)",
		"\t\t\tworkerPool.Submit(task)",
		"\t\t}",
		"\t\tresults := workerPool.Collect()",
		"\t\tlogResults(results)",
		"\t\tmetrics.Record(results)",
		"\t\tcleanupState(firstStep)",
		"\t}",
		"}",
		"",
		"func other() {}",
	}

	rightLines := []string{
		"package main",
		"",
		"func caller() {",
		"\thelperRun()",
		"}",
		"",
		"func other() {}",
		"",
		"func helperRun() {",
		"\tfirstStep := initializeState()",
		"\tconfig := loadCustomConfig()", // modified: Default -> Custom
		"\toptions := buildOptions(config)",
		"\tworkerPool := createPool(options)",
		"\tworkerPool.Start()",
		"\tdefer workerPool.Stop()",
		"\tfor i := 0; i < 200; i++ {", // modified: 100 -> 200
		"\t\ttask := generateTask(i, firstStep)",
		"\t\tworkerPool.Submit(task)",
		"\t}",
		"\tresults := workerPool.Collect()",
		"\tlogResults(results)",
		"\tmetrics.Record(results)",
		"\tcleanupState(firstStep)",
		"}",
	}

	edits := Edits(leftLines, rightLines)
	rows := BuildRows(leftLines, rightLines, edits)

	files := []MoveFile{
		{
			Path:       "worker.go",
			LeftLines:  leftLines,
			RightLines: rightLines,
			Rows:       rows,
		},
	}

	DetectMoves(files)
	resRows := files[0].Rows

	var movedDels []*MoveInfo
	var movedAdds []*MoveInfo

	for _, r := range resRows {
		if r.MovedL != nil {
			movedDels = append(movedDels, r.MovedL)
		}
		if r.MovedR != nil {
			movedAdds = append(movedAdds, r.MovedR)
		}
	}

	if len(movedDels) == 0 || len(movedAdds) == 0 {
		t.Fatalf("expected moved block to be detected, got dels=%d adds=%d", len(movedDels), len(movedAdds))
	}

	// Verify BlockModified is true for all
	for _, m := range movedDels {
		if !m.BlockModified {
			t.Errorf("expected BlockModified==true on left move info, got false")
		}
	}
	for _, m := range movedAdds {
		if !m.BlockModified {
			t.Errorf("expected BlockModified==true on right move info, got false")
		}
	}

	// Verify re-indented lines that were NOT modified have Modified == false
	// and the 2 modified lines have Modified == true with non-empty MovedIntra
	modifiedDelCount := 0
	reindentedDelCount := 0
	for _, r := range resRows {
		if r.MovedL != nil {
			if r.MovedL.Modified {
				modifiedDelCount++
				if len(r.MovedL.MovedIntra) == 0 {
					t.Errorf("expected non-empty MovedIntra for modified line %d (%s)", r.LeftLine, r.LeftText)
				}
			} else {
				reindentedDelCount++
			}
		}
	}

	if modifiedDelCount != 2 {
		t.Errorf("expected exactly 2 modified lines on left, got %d", modifiedDelCount)
	}
	if reindentedDelCount < 8 {
		t.Errorf("expected at least 8 re-indented lines with Modified==false, got %d", reindentedDelCount)
	}
}

func TestCrossFileMove(t *testing.T) {
	// Move a function from a.go to b.go
	aLeft := []string{
		"package a",
		"",
		"func ProcessRecord(rec *Record) error {",
		"\tif rec == nil {",
		"\t\treturn ErrNilRecord",
		"\t}",
		"\tvalidate(rec)",
		"\ttransform(rec)",
		"\treturn save(rec)",
		"}",
		"",
		"func ExistingA() {}",
	}
	aRight := []string{
		"package a",
		"",
		"func ExistingA() {}",
	}

	bLeft := []string{
		"package b",
		"",
		"func ExistingB() {}",
	}
	bRight := []string{
		"package b",
		"",
		"func ExistingB() {}",
		"",
		"func ProcessRecord(rec *Record) error {",
		"\tif rec == nil {",
		"\t\treturn ErrNilRecord",
		"\t}",
		"\tvalidate(rec)",
		"\ttransform(rec)",
		"\treturn save(rec)",
		"}",
	}

	rowsA := BuildRows(aLeft, aRight, Edits(aLeft, aRight))
	rowsB := BuildRows(bLeft, bRight, Edits(bLeft, bRight))

	files := []MoveFile{
		{
			Path:       "pkg/a.go",
			LeftLines:  aLeft,
			RightLines: aRight,
			Rows:       rowsA,
		},
		{
			Path:       "pkg/b.go",
			LeftLines:  bLeft,
			RightLines: bRight,
			Rows:       rowsB,
		},
	}

	DetectMoves(files)

	// Check a.go
	foundDel := false
	for _, r := range files[0].Rows {
		if r.MovedL != nil {
			foundDel = true
			if r.MovedL.PeerPath != "pkg/b.go" {
				t.Errorf("expected PeerPath pkg/b.go on a.go del, got %q", r.MovedL.PeerPath)
			}
			if r.MovedL.PeerStart <= 0 || r.MovedL.PeerEnd < r.MovedL.PeerStart {
				t.Errorf("invalid PeerStart/PeerEnd: %d..%d", r.MovedL.PeerStart, r.MovedL.PeerEnd)
			}
			if r.MovedL.Modified {
				t.Errorf("expected verbatim move to have Modified==false, got true")
			}
		}
	}
	if !foundDel {
		t.Fatalf("expected moved lines in a.go")
	}

	// Check b.go
	foundAdd := false
	for _, r := range files[1].Rows {
		if r.MovedR != nil {
			foundAdd = true
			if r.MovedR.PeerPath != "pkg/a.go" {
				t.Errorf("expected PeerPath pkg/a.go on b.go add, got %q", r.MovedR.PeerPath)
			}
			if r.MovedR.PeerStart <= 0 || r.MovedR.PeerEnd < r.MovedR.PeerStart {
				t.Errorf("invalid PeerStart/PeerEnd: %d..%d", r.MovedR.PeerStart, r.MovedR.PeerEnd)
			}
			if r.MovedR.Modified {
				t.Errorf("expected verbatim move to have Modified==false, got true")
			}
		}
	}
	if !foundAdd {
		t.Fatalf("expected moved lines in b.go")
	}
}

func TestBoilerplateNegative(t *testing.T) {
	// Repeated error checks and closing braces should NOT trigger move detection
	left := []string{
		"package main",
		"",
		"func f1() error {",
		"\tif err != nil {",
		"\t\treturn err",
		"\t}",
		"\treturn nil",
		"}",
	}
	right := []string{
		"package main",
		"",
		"func f2() error {",
		"\tif err != nil {",
		"\t\treturn err",
		"\t}",
		"\treturn nil",
		"}",
	}

	rows := BuildRows(left, right, Edits(left, right))
	files := []MoveFile{
		{
			Path:       "main.go",
			LeftLines:  left,
			RightLines: right,
			Rows:       rows,
		},
	}

	DetectMoves(files)

	for _, r := range files[0].Rows {
		if r.MovedL != nil || r.MovedR != nil {
			t.Fatalf("boilerplate should not be detected as moved, found MovedL=%v MovedR=%v", r.MovedL, r.MovedR)
		}
	}
}

func TestPreservationAcrossCollapseAndUnified(t *testing.T) {
	left := []string{
		"package main",
		"",
		"// Header padding 1",
		"// Header padding 2",
		"// Header padding 3",
		"// Header padding 4",
		"// Header padding 5",
		"// Header padding 6",
		"// Header padding 7",
		"// Header padding 8",
		"// Header padding 9",
		"// Header padding 10",
		"func movedFunc() {",
		"\ta := 1",
		"\tb := 2",
		"\tc := a + b",
		"\tprintln(c)",
		"}",
		"// Footer padding 1",
		"// Footer padding 2",
		"// Footer padding 3",
		"// Footer padding 4",
		"// Footer padding 5",
		"// Footer padding 6",
		"// Footer padding 7",
		"// Footer padding 8",
	}

	right := []string{
		"package main",
		"",
		"// Header padding 1",
		"// Header padding 2",
		"// Header padding 3",
		"// Header padding 4",
		"// Header padding 5",
		"// Header padding 6",
		"// Header padding 7",
		"// Header padding 8",
		"// Header padding 9",
		"// Header padding 10",
		"// Footer padding 1",
		"// Footer padding 2",
		"// Footer padding 3",
		"// Footer padding 4",
		"// Footer padding 5",
		"// Footer padding 6",
		"// Footer padding 7",
		"// Footer padding 8",
		"func movedFunc() {",
		"\ta := 1",
		"\tb := 2",
		"\tc := a + b",
		"\tprintln(c)",
		"}",
	}

	rows := BuildRows(left, right, Edits(left, right))
	files := []MoveFile{
		{
			Path:       "main.go",
			LeftLines:  left,
			RightLines: right,
			Rows:       rows,
		},
	}

	DetectMoves(files)

	// Test Collapse
	collapsed := Collapse(files[0].Rows, 3, nil)
	hasMovedL := false
	hasMovedR := false
	for _, r := range collapsed {
		if r.MovedL != nil {
			hasMovedL = true
		}
		if r.MovedR != nil {
			hasMovedR = true
		}
	}
	if !hasMovedL || !hasMovedR {
		t.Fatalf("Collapse did not preserve MovedL or MovedR: hasL=%v hasR=%v", hasMovedL, hasMovedR)
	}

	// Test Unified
	unified := Unified(collapsed)
	uMovedL := false
	uMovedR := false
	for _, r := range unified {
		if r.MovedL != nil {
			if r.Type != RowDel {
				t.Errorf("Unified row with MovedL must have RowDel, got %v", r.Type)
			}
			uMovedL = true
		}
		if r.MovedR != nil {
			if r.Type != RowAdd {
				t.Errorf("Unified row with MovedR must have RowAdd, got %v", r.Type)
			}
			uMovedR = true
		}
	}
	if !uMovedL || !uMovedR {
		t.Fatalf("Unified did not preserve MovedL or MovedR: uL=%v uR=%v", uMovedL, uMovedR)
	}
}

func TestDetectMovesMonotonicMixedHunk(t *testing.T) {
	leftCode := `package main

// Anchor start 1
// Anchor start 2
// Anchor start 3
// Anchor start 4
// Anchor start 5

func oldHeaderAlpha() string {
	return "alpha old header"
}

func functionToMoveAround() int {
	a := 10
	b := 20
	c := a + b
	if c > 15 {
		return c * 2
	}
	return c
}

func oldFooterBeta() string {
	return "beta old footer"
}

// Anchor end 1
// Anchor end 2
// Anchor end 3
// Anchor end 4
// Anchor end 5
`

	rightCode := `package main

// Anchor start 1
// Anchor start 2
// Anchor start 3
// Anchor start 4
// Anchor start 5

func newHeaderAlpha() string {
	return "alpha new header"
}

func newFooterBeta() string {
	return "beta new footer"
}

// Anchor end 1
// Anchor end 2
// Anchor end 3
// Anchor end 4
// Anchor end 5

func functionToMoveAround() int {
	a := 10
	b := 20
	c := a + b
	if c > 15 {
		return c * 2
	}
	return c
}
`

	leftLines := SplitLines([]byte(leftCode))
	rightLines := SplitLines([]byte(rightCode))
	edits := Edits(leftLines, rightLines)
	rows := BuildRows(leftLines, rightLines, edits)

	files := []MoveFile{
		{
			Path:       "main.go",
			LeftLines:  leftLines,
			RightLines: rightLines,
			Rows:       rows,
		},
	}

	DetectMoves(files)

	resRows := files[0].Rows
	prevLeft := 0
	prevRight := 0
	foundMove := false

	for idx, r := range resRows {
		if r.LeftLine > 0 {
			if r.LeftLine <= prevLeft {
				t.Fatalf("row %d non-monotonic LeftLine: prev=%d, cur=%d (type=%v)", idx, prevLeft, r.LeftLine, r.Type)
			}
			prevLeft = r.LeftLine
		}
		if r.RightLine > 0 {
			if r.RightLine <= prevRight {
				t.Fatalf("row %d non-monotonic RightLine: prev=%d, cur=%d (type=%v)", idx, prevRight, r.RightLine, r.Type)
			}
			prevRight = r.RightLine
		}
		if r.MovedL != nil || r.MovedR != nil {
			foundMove = true
		}
	}

	if !foundMove {
		t.Fatal("expected functionToMoveAround to be detected as a move, but found no MovedL/MovedR")
	}
}

func FuzzDetectMoves(f *testing.F) {
	blockA := "func movedFunc(x, y int) int {\n\tres := x * y\n\tif res > 100 {\n\t\treturn res - 10\n\t}\n\treturn res + 10\n}\n"
	blockB := "func otherFunc(a, b string) string {\n\tmsg := a + \": \" + b\n\tprintln(msg)\n\treturn msg\n}\n"
	anchor := "func anchor() {\n\tprintln(\"1\")\n\tprintln(\"2\")\n\tprintln(\"3\")\n\tprintln(\"4\")\n\tprintln(\"5\")\n\tprintln(\"6\")\n\tprintln(\"7\")\n\tprintln(\"8\")\n\tprintln(\"9\")\n\tprintln(\"10\")\n}\n"

	seeds := []struct {
		oldA, newA, oldB, newB string
	}{
		{blockA + anchor, anchor + blockA, "", ""},
		{blockA, "", "", blockA},
		{blockA + anchor, anchor + blockB, "", ""},
		{"", "", "", ""},
		{"func foo() {\n}\n", "func bar() {\n}\n", "", ""},
		{strings.Repeat("line\n", 50), strings.Repeat("line\n", 50), "", ""},
		{"🔥 UTF-8 block\nline 1\nline 2\nline 3\nline 4\nline 5\n", "", "", "🔥 UTF-8 block\nline 1\nline 2\nline 3\nline 4\nline 5\n"},
	}

	for _, s := range seeds {
		f.Add(s.oldA, s.newA, s.oldB, s.newB)
	}

	f.Fuzz(func(t *testing.T, oldA, newA, oldB, newB string) {
		if len(oldA) > 4096 || len(newA) > 4096 || len(oldB) > 4096 || len(newB) > 4096 {
			return
		}

		oldALines := SplitLines([]byte(oldA))
		newALines := SplitLines([]byte(newA))
		oldBLines := SplitLines([]byte(oldB))
		newBLines := SplitLines([]byte(newB))

		buildFiles := func(includeB bool) []MoveFile {
			fA := MoveFile{
				Path:       "a.go",
				LeftLines:  oldALines,
				RightLines: newALines,
				Rows:       BuildRows(oldALines, newALines, Edits(oldALines, newALines)),
			}
			if !includeB {
				return []MoveFile{fA}
			}
			fB := MoveFile{
				Path:       "b.go",
				LeftLines:  oldBLines,
				RightLines: newBLines,
				Rows:       BuildRows(oldBLines, newBLines, Edits(oldBLines, newBLines)),
			}
			return []MoveFile{fA, fB}
		}

		validateFiles := func(files []MoveFile) {
			fileMap := make(map[string]*MoveFile)
			for i := range files {
				fileMap[files[i].Path] = &files[i]
			}

			for _, file := range files {
				prevL := 0
				prevR := 0
				for idx, r := range file.Rows {
					if r.LeftLine > 0 {
						if r.LeftLine <= prevL {
							t.Fatalf("file %s row %d non-monotonic LeftLine: prev=%d curr=%d", file.Path, idx, prevL, r.LeftLine)
						}
						prevL = r.LeftLine
					}
					if r.RightLine > 0 {
						if r.RightLine <= prevR {
							t.Fatalf("file %s row %d non-monotonic RightLine: prev=%d curr=%d", file.Path, idx, prevR, r.RightLine)
						}
						prevR = r.RightLine
					}

					// Verify MovedL
					if r.MovedL != nil {
						if r.LeftLine < 1 || r.LeftLine > len(file.LeftLines) {
							t.Fatalf("file %s MovedL row LeftLine %d out of bounds [1, %d]", file.Path, r.LeftLine, len(file.LeftLines))
						}
						if r.MovedL.PeerStart < 1 || r.MovedL.PeerStart > r.MovedL.PeerEnd {
							t.Fatalf("file %s MovedL invalid peer range: %d..%d", file.Path, r.MovedL.PeerStart, r.MovedL.PeerEnd)
						}
						peerPath := r.MovedL.PeerPath
						if peerPath == "" {
							peerPath = file.Path
						}
						peerFile := fileMap[peerPath]
						if peerFile != nil {
							if r.MovedL.PeerEnd > len(peerFile.RightLines) {
								t.Fatalf("file %s MovedL PeerEnd %d exceeds peer line count %d", file.Path, r.MovedL.PeerEnd, len(peerFile.RightLines))
							}
							if r.MovedL.PeerLine > 0 {
								if r.MovedL.PeerLine > len(peerFile.RightLines) {
									t.Fatalf("file %s MovedL PeerLine %d exceeds peer line count %d", file.Path, r.MovedL.PeerLine, len(peerFile.RightLines))
								}
								// Check symmetry
								var counterpartRow *Row
								for ri := range peerFile.Rows {
									if peerFile.Rows[ri].RightLine == r.MovedL.PeerLine {
										counterpartRow = &peerFile.Rows[ri]
										break
									}
								}
								if counterpartRow == nil || counterpartRow.MovedR == nil {
									t.Fatalf("file %s LeftLine %d has MovedL PeerLine %d, but counterpart has no MovedR", file.Path, r.LeftLine, r.MovedL.PeerLine)
								}
								if counterpartRow.MovedR.PeerLine != r.LeftLine {
									t.Fatalf("asymmetric peer mapping: file %s L%d -> R%d, but R%d -> L%d", file.Path, r.LeftLine, r.MovedL.PeerLine, r.MovedL.PeerLine, counterpartRow.MovedR.PeerLine)
								}
								if counterpartRow.MovedR.GroupID != r.MovedL.GroupID {
									t.Fatalf("mismatched GroupID: %d vs %d", r.MovedL.GroupID, counterpartRow.MovedR.GroupID)
								}
							}
						}

						// Check intraline spans
						lRaw := file.LeftLines[r.LeftLine-1]
						for _, sp := range r.MovedL.MovedIntra {
							if sp.Start < 0 || sp.Start > sp.End || sp.End > len(lRaw) {
								t.Fatalf("file %s MovedL span %+v out of bounds [0, %d]", file.Path, sp, len(lRaw))
							}
							if utf8.ValidString(lRaw) && !utf8.ValidString(lRaw[sp.Start:sp.End]) {
								t.Fatalf("file %s MovedL span %+v split UTF-8 in %q", file.Path, sp, lRaw)
							}
						}
					}

					// Verify MovedR
					if r.MovedR != nil {
						if r.RightLine < 1 || r.RightLine > len(file.RightLines) {
							t.Fatalf("file %s MovedR row RightLine %d out of bounds [1, %d]", file.Path, r.RightLine, len(file.RightLines))
						}
						if r.MovedR.PeerStart < 1 || r.MovedR.PeerStart > r.MovedR.PeerEnd {
							t.Fatalf("file %s MovedR invalid peer range: %d..%d", file.Path, r.MovedR.PeerStart, r.MovedR.PeerEnd)
						}
						peerPath := r.MovedR.PeerPath
						if peerPath == "" {
							peerPath = file.Path
						}
						peerFile := fileMap[peerPath]
						if peerFile != nil {
							if r.MovedR.PeerEnd > len(peerFile.LeftLines) {
								t.Fatalf("file %s MovedR PeerEnd %d exceeds peer line count %d", file.Path, r.MovedR.PeerEnd, len(peerFile.LeftLines))
							}
							if r.MovedR.PeerLine > 0 {
								if r.MovedR.PeerLine > len(peerFile.LeftLines) {
									t.Fatalf("file %s MovedR PeerLine %d exceeds peer line count %d", file.Path, r.MovedR.PeerLine, len(peerFile.LeftLines))
								}
								// Check symmetry
								var counterpartRow *Row
								for ri := range peerFile.Rows {
									if peerFile.Rows[ri].LeftLine == r.MovedR.PeerLine {
										counterpartRow = &peerFile.Rows[ri]
										break
									}
								}
								if counterpartRow == nil || counterpartRow.MovedL == nil {
									t.Fatalf("file %s RightLine %d has MovedR PeerLine %d, but counterpart has no MovedL", file.Path, r.RightLine, r.MovedR.PeerLine)
								}
								if counterpartRow.MovedL.PeerLine != r.RightLine {
									t.Fatalf("asymmetric peer mapping: file %s R%d -> L%d, but L%d -> R%d", file.Path, r.RightLine, r.MovedR.PeerLine, r.MovedR.PeerLine, counterpartRow.MovedL.PeerLine)
								}
								if counterpartRow.MovedL.GroupID != r.MovedR.GroupID {
									t.Fatalf("mismatched GroupID: %d vs %d", r.MovedR.GroupID, counterpartRow.MovedL.GroupID)
								}
							}
						}

						// Check intraline spans
						rRaw := file.RightLines[r.RightLine-1]
						for _, sp := range r.MovedR.MovedIntra {
							if sp.Start < 0 || sp.Start > sp.End || sp.End > len(rRaw) {
								t.Fatalf("file %s MovedR span %+v out of bounds [0, %d]", file.Path, sp, len(rRaw))
							}
							if utf8.ValidString(rRaw) && !utf8.ValidString(rRaw[sp.Start:sp.End]) {
								t.Fatalf("file %s MovedR span %+v split UTF-8 in %q", file.Path, sp, rRaw)
							}
						}
					}
				}
			}
		}

		// 1-file move test
		f1 := buildFiles(false)
		DetectMoves(f1)
		validateFiles(f1)

		// 2-file move test
		f2 := buildFiles(true)
		DetectMoves(f2)
		validateFiles(f2)
	})
}
