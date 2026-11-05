package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

// ViewFile represents a file in a change view with stats and review state.
type ViewFile struct {
	*vcs.File
	Reviewed      bool
	Threads       []*db.Thread
	CommentCounts CommentCounts
}

// CommentCounts represents counts of unresolved, resolved, and draft comments.
type CommentCounts struct {
	Unresolved int `json:"unresolved"`
	Resolved   int `json:"resolved"`
	Draft      int `json:"draft"`
}

func addThreadCommentCounts(counts *CommentCounts, th *db.Thread) {
	if th == nil {
		return
	}
	if th.Resolved {
		counts.Resolved++
	} else {
		counts.Unresolved++
	}
	for _, m := range th.Messages {
		if m.Draft {
			counts.Draft++
		}
	}
}

// ComputeCommentCounts calculates comment counts from a slice of threads.
func ComputeCommentCounts(threads []*db.Thread) CommentCounts {
	var counts CommentCounts
	for _, th := range threads {
		addThreadCommentCounts(&counts, th)
	}
	return counts
}

// ChangeView holds all resolved data required to render a change or diff page.
type ChangeView struct {
	Change           *vcs.Change
	RepoRecord       *db.RepoRecord
	BaseRev          string
	TargetRev        string
	BaseDesc         string
	TargetDesc       string
	IsLiveTarget     bool
	BaseSnapNum      int
	TargetSnapNum    int
	SnapshotPending  bool
	HasCommitMsgBase bool
	TargetMessage    string
	AllFiles         []*vcs.File
	Files            []*ViewFile
	Snapshots        []*db.Snapshot
	Summary          *db.ChangeSummary
	Threads          []*db.Thread
	ChangeCounts     CommentCounts
	RelationChain    []RelationRow
}

// RelationRow represents a change in the relation chain with visual graph metadata.
type RelationRow struct {
	vcs.GraphRow
	IsCurrent bool
	LGTM      bool
	OpenCount int
}

// BuildRelationChain returns the connected chain (ancestors and descendants) of non-empty
// pending changes connected to currentKey, ordered newest-to-oldest with SVG graph lines.
func BuildRelationChain(changes []*vcs.Change, currentKey string, summaries map[string]*db.ChangeSummary) []RelationRow {
	if len(changes) == 0 {
		return nil
	}

	allByRev := make(map[string]*vcs.Change)
	for _, ch := range changes {
		allByRev[ch.Rev] = ch
	}
	isVisible := func(ch *vcs.Change) bool {
		if ch.Key == currentKey || ch.Rev == currentKey {
			return true
		}
		return !ch.Empty && !ch.Working
	}
	findVisibleParent := func(rev string) string {
		curr := rev
		for curr != "" {
			ch, ok := allByRev[curr]
			if !ok {
				break
			}
			if isVisible(ch) {
				return ch.Rev
			}
			curr = ch.Parent
		}
		return ""
	}

	byRev := make(map[string]*vcs.Change)
	visParent := make(map[string]string)
	children := make(map[string][]string)
	for _, ch := range changes {
		if !isVisible(ch) {
			continue
		}
		byRev[ch.Rev] = ch
		if ch.Parent != "" {
			if vp := findVisibleParent(ch.Parent); vp != "" {
				visParent[ch.Rev] = vp
				children[vp] = append(children[vp], ch.Rev)
			}
		}
	}

	var target *vcs.Change
	for _, ch := range changes {
		if ch.Key == currentKey || ch.Rev == currentKey {
			target = ch
			break
		}
	}
	if target == nil {
		return nil
	}

	connected := make(map[string]bool)
	connected[target.Rev] = true

	// Ancestors (bridging any empty intermediate commits)
	curr := visParent[target.Rev]
	if curr == "" && target.Parent != "" {
		curr = findVisibleParent(target.Parent)
	}
	for curr != "" {
		if _, ok := byRev[curr]; !ok {
			break
		}
		connected[curr] = true
		curr = visParent[curr]
	}

	// Descendants
	queue := []string{target.Rev}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, v := range children[u] {
			if !connected[v] {
				connected[v] = true
				queue = append(queue, v)
			}
		}
	}

	var chainChanges []*vcs.Change
	for _, ch := range changes {
		if connected[ch.Rev] {
			chainChanges = append(chainChanges, ch)
		}
	}

	graphRows := vcs.BuildGraph(chainChanges, func(c *vcs.Change) bool {
		return true
	})
	var rows []RelationRow
	for _, gr := range graphRows {
		isCur := (gr.Change.Key == target.Key || gr.Change.Rev == target.Rev)
		gr.IsCurrent = isCur
		var lgtm bool
		var openCount int
		if summaries != nil {
			if sum := summaries[gr.Change.Key]; sum != nil {
				lgtm = sum.LGTM
				openCount = sum.OpenThreads
			}
		}
		rows = append(rows, RelationRow{
			GraphRow:  gr,
			IsCurrent: isCur,
			LGTM:      lgtm,
			OpenCount: openCount,
		})
	}

	return rows
}

// SearchLineMatch represents a single matching line in a search query.
type SearchLineMatch struct {
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// SearchFileResult represents all search matches in a file.
type SearchFileResult struct {
	Path    string            `json:"path"`
	Matches []SearchLineMatch `json:"matches"`
}

// GCReport contains the actions taken or proposed by RunGC.
type GCReport struct {
	ForgottenRepos []string
	PrunedChanges  []string
	PrunedPins     []string
}

func shortHash(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// SnapshotNumForRev returns the newest snapshot number matching rev, or 0 if none.
func SnapshotNumForRev(snaps []*db.Snapshot, rev string) int {
	if rev == "" {
		return 0
	}
	for i := len(snaps) - 1; i >= 0; i-- {
		if snaps[i].Rev == rev {
			return snaps[i].Num
		}
	}
	return 0
}

// ViewScope precomputes snapshot lookups for evaluating thread visibility and
// effective display side within a single diff view.
type ViewScope struct {
	baseRev      string
	targetRev    string
	baseSnap     int
	targetSnap   int
	isLiveTarget bool
	snapByRev    map[string]int
}

// NewViewScope constructs a ViewScope for a diff comparing baseRev against targetRev.
func NewViewScope(snaps []*db.Snapshot, baseRev, targetRev string, isLiveTarget bool) ViewScope {
	var snapByRev map[string]int
	if len(snaps) > 0 {
		snapByRev = make(map[string]int, len(snaps))
		for _, s := range snaps {
			if s != nil && s.Rev != "" {
				snapByRev[s.Rev] = s.Num
			}
		}
	}
	var baseSnap, targetSnap int
	if snapByRev != nil {
		if baseRev != "" {
			baseSnap = snapByRev[baseRev]
		}
		if targetRev != "" {
			targetSnap = snapByRev[targetRev]
		}
	}
	return ViewScope{
		baseRev:      baseRev,
		targetRev:    targetRev,
		baseSnap:     baseSnap,
		targetSnap:   targetSnap,
		isLiveTarget: isLiveTarget,
		snapByRev:    snapByRev,
	}
}

// SnapForRev returns the newest snapshot number matching rev, or 0 if none.
func (vs ViewScope) SnapForRev(rev string) int {
	if rev == "" || vs.snapByRev == nil {
		return 0
	}
	return vs.snapByRev[rev]
}

// ThreadSide reports whether thread th should appear inline in this view,
// along with the effective side ("L" or "R") it should be anchored on.
func (vs ViewScope) ThreadSide(th *db.Thread) (effectiveSide string, ok bool) {
	if th == nil {
		return "", false
	}

	if th.SnapshotNum > 0 {
		if vs.targetSnap == th.SnapshotNum {
			if th.Rev != "" && th.Rev == vs.targetRev {
				if vs.baseRev == vs.targetRev && th.Side == "L" {
					return "L", true
				}
				return "R", true
			}
			if th.Side == "L" {
				if (vs.baseSnap > 0 || vs.SnapForRev(th.Rev) > 0) && th.Rev != "" && th.Rev != vs.baseRev {
					return "", false
				}
				return "L", true
			}
			return "R", true
		}
		if vs.baseSnap > 0 && vs.baseSnap == th.SnapshotNum {
			if th.Rev == vs.baseRev || th.Side != "L" {
				return "L", true
			}
			return "", false
		}
		return "", false
	}

	// Unsnapshotted thread (snapshot_num == 0).
	if th.Rev != "" && th.Rev == vs.targetRev {
		if vs.baseRev == vs.targetRev && th.Side == "L" {
			return "L", true
		}
		if th.Side != "L" {
			return "R", true
		}
	}
	if th.Rev != "" && th.Rev == vs.baseRev && (vs.baseSnap > 0 || th.Side != "L") {
		return "L", true
	}
	if vs.isLiveTarget {
		if th.Side == "L" {
			if (vs.baseSnap > 0 || vs.SnapForRev(th.Rev) > 0) && th.Rev != "" && th.Rev != vs.baseRev {
				return "", false
			}
			return "L", true
		}
		return "R", true
	}
	return "", false
}

// ThreadVisibleInView reports whether thread th should appear inline in a diff view
// comparing baseRev against targetRev (where isLiveTarget is true when target is the live
// commit), along with the effective side ("L" or "R") it should be anchored on.
func ThreadVisibleInView(th *db.Thread, snaps []*db.Snapshot, baseRev, targetRev string, isLiveTarget bool) (effectiveSide string, ok bool) {
	return NewViewScope(snaps, baseRev, targetRev, isLiveTarget).ThreadSide(th)
}

// BuildChangeView resolves base and target revisions and prepares files, threads, and marks.
func BuildChangeView(d *db.DB, repo vcs.Repo, rec *db.RepoRecord, c *vcs.Change, baseSel, targetSel string) (*ChangeView, error) {
	return buildChangeView(d, repo, rec, c, baseSel, targetSel, "")
}

// BuildChangeViewForFile builds a change view optimized for a single file, avoiding full repo summaries,
// relation chain computation, and unrelated thread reanchoring.
func BuildChangeViewForFile(d *db.DB, repo vcs.Repo, rec *db.RepoRecord, c *vcs.Change, baseSel, targetSel, filePath string) (*ChangeView, error) {
	return buildChangeView(d, repo, rec, c, baseSel, targetSel, filePath)
}

func buildChangeView(d *db.DB, repo vcs.Repo, rec *db.RepoRecord, c *vcs.Change, baseSel, targetSel, onlyFile string) (*ChangeView, error) {
	if vcs.IsCommitMsgPath(onlyFile) {
		onlyFile = vcs.CommitMsgPath
	}

	snaps, err := d.Snapshots(rec.ID, c.Key)
	if err != nil {
		return nil, fmt.Errorf("loading snapshots: %w", err)
	}

	var summaries map[string]*db.ChangeSummary
	summary := &db.ChangeSummary{ChangeKey: c.Key}
	if onlyFile == "" {
		summaries, _ = d.RepoSummaries(rec.ID)
		if s := summaries[c.Key]; s != nil {
			summary = s
		}
	}

	// 1. Resolve Target
	targetRev := c.Rev
	targetDesc := "Live Snapshot"
	isLiveTarget := true
	targetSnapNum := 0
	snapshotPending := true

	if targetSel == "" || targetSel == "live" {
		targetSnapNum = 0
		isLiveTarget = true
		targetRev = c.Rev
		if snapNum := SnapshotNumForRev(snaps, c.Rev); snapNum > 0 {
			snapshotPending = false
			targetDesc = fmt.Sprintf("Snapshot %d", snapNum)
		} else {
			snapshotPending = true
			targetDesc = "Live Snapshot"
		}
	} else if strings.HasPrefix(targetSel, "snap:") {
		var num int
		if _, err := fmt.Sscanf(targetSel, "snap:%d", &num); err == nil {
			for _, s := range snaps {
				if s.Num == num {
					targetSnapNum = s.Num
					isLiveTarget = false
					snapshotPending = false
					targetRev = s.Rev
					targetDesc = fmt.Sprintf("Snapshot %d", s.Num)
					break
				}
			}
		}
	} else {
		targetRev = targetSel
		isLiveTarget = (targetRev == c.Rev)
		snapshotPending = isLiveTarget
		targetDesc = shortHash(targetRev)
		for i := len(snaps) - 1; i >= 0; i-- {
			if snaps[i].Rev == targetRev {
				targetSnapNum = snaps[i].Num
				snapshotPending = false
				targetDesc = fmt.Sprintf("Snapshot %d", snaps[i].Num)
				break
			}
		}
	}

	// 2. Resolve Base
	baseRev := c.Parent
	baseDesc := "Parent"
	baseSnapNum := 0

	if strings.HasPrefix(baseSel, "snap:") {
		var num int
		if _, err := fmt.Sscanf(baseSel, "snap:%d", &num); err == nil {
			for _, s := range snaps {
				if s.Num == num {
					baseSnapNum = s.Num
					baseRev = s.Rev
					baseDesc = fmt.Sprintf("Snapshot %d", s.Num)
					break
				}
			}
		}
	} else if baseSel != "" && baseSel != "parent" {
		baseRev = baseSel
		if baseRev != c.Parent {
			baseDesc = shortHash(baseRev)
			for i := len(snaps) - 1; i >= 0; i-- {
				if snaps[i].Rev == baseRev {
					baseSnapNum = snaps[i].Num
					baseDesc = fmt.Sprintf("Snapshot %d", snaps[i].Num)
					break
				}
			}
		}
	}

	// 3. Load files diff between baseRev and targetRev
	vcsFiles, err := repo.Files(baseRev, targetRev)
	if err != nil {
		return nil, fmt.Errorf("loading files: %w", err)
	}

	allFiles := vcsFiles

	if onlyFile != "" {
		normOnly := strings.TrimPrefix(onlyFile, "/")
		var filtered []*vcs.File
		for _, f := range vcsFiles {
			if f.Path == onlyFile || strings.TrimPrefix(f.Path, "/") == normOnly {
				filtered = append(filtered, f)
				break
			}
		}
		if len(filtered) == 0 {
			filtered = []*vcs.File{{Path: onlyFile, Status: vcs.FileModified}}
		}
		vcsFiles = filtered
	}

	// 4. Load threads for change
	allThreads, err := d.ThreadsForChange(rec.ID, c.Key)
	if err != nil {
		return nil, fmt.Errorf("loading threads: %w", err)
	}
	changeCounts := ComputeCommentCounts(allThreads)

	if onlyFile != "" {
		normOnly := strings.TrimPrefix(onlyFile, "/")
		var filteredThreads []*db.Thread
		for _, th := range allThreads {
			if th.Path == onlyFile || strings.TrimPrefix(th.Path, "/") == normOnly {
				filteredThreads = append(filteredThreads, th)
			}
		}
		allThreads = filteredThreads
	}

	// Re-anchor view-visible threads onto targetRev (or baseRev for effective side "L")
	threadsByFile := make(map[string][]*db.Thread)
	linesCache := make(map[string][]string)
	getLines := func(rev, path string) []string {
		norm := strings.TrimPrefix(path, "/")
		key := rev + "\x00" + norm
		if lines, ok := linesCache[key]; ok {
			return lines
		}
		data, err := repo.Content(rev, path)
		if err != nil || len(data) == 0 {
			linesCache[key] = nil
			return nil
		}
		lines := diff.SplitLines(data)
		linesCache[key] = lines
		return lines
	}

	hasCommitMsgBase := CommitMsgHasBase(repo, c, snaps, baseRev, targetRev)
	scope := NewViewScope(snaps, baseRev, targetRev, isLiveTarget)
	var visibleThreads []*db.Thread
	for _, th := range allThreads {
		effectiveSide, ok := scope.ThreadSide(th)
		if !ok {
			continue
		}
		if vcs.IsCommitMsgPath(th.Path) && effectiveSide == "L" && !hasCommitMsgBase {
			th.Stale = true
		} else {
			rev := targetRev
			if effectiveSide == "L" && baseRev != "" {
				rev = baseRev
			}
			if th.StartLine == 0 {
				th.Stale = false
			} else if rev != "" && repo != nil {
				lines := getLines(rev, th.Path)
				if lines == nil {
					th.Stale = true
				} else if newStart, newEnd, matched := matchAnchorLines(lines, th.StartLine, th.AnchorLines); matched {
					th.StartLine = newStart
					th.EndLine = newEnd
					th.Stale = false
				} else if th.AnchorLines != "" {
					th.Stale = true
				}
			}
		}
		visibleThreads = append(visibleThreads, th)
		norm := strings.TrimPrefix(th.Path, "/")
		threadsByFile[norm] = append(threadsByFile[norm], th)
		if norm != th.Path {
			threadsByFile[th.Path] = append(threadsByFile[th.Path], th)
		}
	}

	// 5. Load file marks (and apply carry-forward for unchanged files)
	curMarks, _ := d.FileMarks(rec.ID, c.Key, targetRev)
	snapMarksCache := make(map[string]map[string]bool)
	getSnapMarks := func(rev string) map[string]bool {
		if m, ok := snapMarksCache[rev]; ok {
			return m
		}
		m, _ := d.FileMarks(rec.ID, c.Key, rev)
		snapMarksCache[rev] = m
		return m
	}

	targetMessage := c.Message
	targetCommit := c
	if targetRev != "" && targetRev != c.Rev && repo != nil {
		if tc, err := repo.Commit(targetRev); err == nil && tc != nil {
			targetCommit = tc
			targetMessage = tc.Message
		}
	}

	commitMsgPath := strings.TrimPrefix(vcs.CommitMsgPath, "/")
	var includeCommitMsg bool
	if !hasCommitMsgBase {
		includeCommitMsg = true
	} else if baseRev != targetRev && targetCommit != nil && repo != nil {
		if baseCommit, err := repo.Commit(baseRev); err == nil && baseCommit != nil {
			includeCommitMsg = strings.TrimSpace(baseCommit.Message) != strings.TrimSpace(targetCommit.Message) ||
				baseCommit.Author != targetCommit.Author ||
				len(threadsByFile[commitMsgPath]) > 0
		}
	}

	targetAuthor := ""
	if targetCommit != nil {
		targetAuthor = targetCommit.Author
	}

	type oidKey struct {
		rev  string
		path string
	}
	oidCache := make(map[oidKey]string)
	oidErrCache := make(map[oidKey]error)

	getOID := func(rev, path string) (string, error) {
		k := oidKey{rev, path}
		if err, ok := oidErrCache[k]; ok {
			return "", err
		}
		if oid, ok := oidCache[k]; ok {
			return oid, nil
		}
		if oider, ok := repo.(vcs.BlobOIDer); ok {
			oid, err := oider.BlobOID(rev, path)
			if err != nil {
				oidErrCache[k] = err
				return "", err
			}
			oidCache[k] = oid
			return oid, nil
		}
		err := errors.New("BlobOIDer not supported")
		oidErrCache[k] = err
		return "", err
	}

	filesEqualAtRevs := func(revA, revB, path string, isDeleted bool) bool {
		if repo == nil {
			return false
		}
		oidA, errA := getOID(revA, path)
		oidB, errB := getOID(revB, path)
		if errA == nil && errB == nil {
			return oidA == oidB
		}
		if isDeleted && errors.Is(errA, os.ErrNotExist) && errors.Is(errB, os.ErrNotExist) {
			return true
		}
		if errors.Is(errA, os.ErrNotExist) || errors.Is(errB, os.ErrNotExist) {
			return false
		}
		contentA, errA := repo.Content(revA, path)
		contentB, errB := repo.Content(revB, path)
		if errA == nil && errB == nil && bytes.Equal(contentA, contentB) {
			return true
		}
		if isDeleted && errA != nil && errB != nil {
			return true
		}
		return false
	}

	unchangedAtRev := func(otherRev string, f *vcs.File) bool {
		if f.Path == vcs.CommitMsgPath {
			if repo == nil {
				return false
			}
			oc, err := repo.Commit(otherRev)
			return err == nil && oc != nil &&
				strings.TrimSpace(oc.Message) == strings.TrimSpace(targetMessage) &&
				oc.Author == targetAuthor
		}
		return filesEqualAtRevs(targetRev, otherRev, f.Path, f.Status == vcs.FileDeleted)
	}

	var viewFiles []*ViewFile
	for _, f := range vcsFiles {
		if f.Path == vcs.CommitMsgPath && !includeCommitMsg {
			continue
		}
		reviewed := false
		decided := false
		norm := strings.TrimPrefix(f.Path, "/")
		if val, ok := curMarks[f.Path]; ok {
			reviewed = val
			decided = true
		} else if val, ok := curMarks[norm]; ok {
			reviewed = val
			decided = true
		} else if len(snaps) > 0 && repo != nil {
			// Walk prior snapshots newest-to-oldest
			for i := len(snaps) - 1; i >= 0; i-- {
				snap := snaps[i]
				if targetSnapNum > 0 && snap.Num >= targetSnapNum {
					continue
				}
				if snap.Rev == targetRev {
					continue
				}
				sm := getSnapMarks(snap.Rev)
				val, exists := sm[f.Path]
				if !exists {
					val, exists = sm[norm]
				}
				if exists {
					decided = true
					if val && unchangedAtRev(snap.Rev, f) {
						reviewed = true
					}
					break
				}
			}
		}

		if !decided && baseRev != "" && baseRev != c.Parent && repo != nil {
			prevMarks := getSnapMarks(baseRev)
			if prevMarks[f.Path] || prevMarks[norm] {
				if unchangedAtRev(baseRev, f) {
					reviewed = true
				}
			}
		}

		viewFiles = append(viewFiles, &ViewFile{
			File:          f,
			Reviewed:      reviewed,
			Threads:       threadsByFile[norm],
			CommentCounts: ComputeCommentCounts(threadsByFile[norm]),
		})
	}

	var relChain []RelationRow
	if onlyFile == "" {
		allChanges, _ := repo.Changes()
		relChain = BuildRelationChain(allChanges, c.Key, summaries)
	}

	return &ChangeView{
		Change:           c,
		RepoRecord:       rec,
		BaseRev:          baseRev,
		TargetRev:        targetRev,
		BaseDesc:         baseDesc,
		TargetDesc:       targetDesc,
		IsLiveTarget:     isLiveTarget,
		BaseSnapNum:      baseSnapNum,
		TargetSnapNum:    targetSnapNum,
		SnapshotPending:  snapshotPending,
		HasCommitMsgBase: hasCommitMsgBase,
		TargetMessage:    targetMessage,
		AllFiles:         allFiles,
		Files:            viewFiles,
		Snapshots:        snaps,
		Summary:          summary,
		Threads:          visibleThreads,
		ChangeCounts:     changeCounts,
		RelationChain:    relChain,
	}, nil
}

// ThreadView wraps a comment thread with re-anchored position for diff rendering.
type ThreadView struct {
	*db.Thread
	CurrentStart  int  `json:"current_line"`
	CurrentEnd    int  `json:"current_end_line"`
	Stale         bool `json:"stale"`
	AnchorText    string
	EffectiveSide string `json:"-"`
}

// DisplaySide returns the diff side ("L" or "R") where this thread should render in the current view.
func (tv *ThreadView) DisplaySide() string {
	if tv != nil {
		if tv.EffectiveSide == "L" || tv.EffectiveSide == "R" {
			return tv.EffectiveSide
		}
		if tv.Thread != nil && (tv.Thread.Side == "L" || tv.Thread.Side == "R") {
			return tv.Thread.Side
		}
	}
	return "R"
}

// HasRange reports whether the thread spans multiple lines or has character offsets.
func (tv *ThreadView) HasRange() bool {
	return !tv.Stale && tv.CurrentStart > 0 && (tv.CurrentEnd > tv.CurrentStart || tv.StartCol > 0 || tv.EndCol > 0)
}

// matchAnchorLines attempts to locate anchor within lines, first trying startLine, then a radial ±30 line window.
func matchAnchorLines(lines []string, startLine int, anchor string) (newStart, newEnd int, matched bool) {
	if len(lines) == 0 || anchor == "" {
		return 0, 0, false
	}
	anchorParts := strings.Split(anchor, "\n")
	numAnchor := len(anchorParts)
	maxStart := len(lines) - numAnchor + 1

	checkAt := func(line int) bool {
		if line < 1 || line > maxStart {
			return false
		}
		for k := 0; k < numAnchor; k++ {
			if lines[line-1+k] != anchorParts[k] {
				return false
			}
		}
		return true
	}

	// Check if current position matches
	if startLine >= 1 && checkAt(startLine) {
		return startLine, startLine + numAnchor - 1, true
	}

	// Radial outward search ±30 lines around startLine so nearby line shifts match before distant duplicates
	for delta := 1; delta <= 30; delta++ {
		if line := startLine + delta; checkAt(line) {
			return line, line + numAnchor - 1, true
		}
		if line := startLine - delta; checkAt(line) {
			return line, line + numAnchor - 1, true
		}
	}

	return 0, 0, false
}

// CommitMsgHasBase reports whether leftRev represents a valid prior revision of
// the same change (such as an earlier snapshot) rather than a parent commit or
// missing base revision. When it returns false, /COMMIT_MSG has no left-hand
// counterpart to anchor against.
func CommitMsgHasBase(repo vcs.Repo, c *vcs.Change, snaps []*db.Snapshot, leftRev, rightRev string) bool {
	if leftRev == "" || strings.Trim(leftRev, "0") == "" {
		return false
	}
	if c != nil && leftRev == c.Parent {
		return false
	}
	for _, sn := range snaps {
		if sn.ParentRev != "" && leftRev == sn.ParentRev && (rightRev == "" || sn.Rev == rightRev) {
			return false
		}
	}
	for _, sn := range snaps {
		if sn.Rev == leftRev {
			return true
		}
	}
	if rightRev != "" && repo != nil {
		if rc, err := repo.Commit(rightRev); err == nil && rc != nil && leftRev == rc.Parent {
			return false
		}
	}
	return true
}

// ReanchorThreads re-anchors threads against live targetRev (or parentRev for Side == "L").
func ReanchorThreads(repo vcs.Repo, targetRev, parentRev string, threads []*db.Thread) []*ThreadView {
	return ReanchorThreadsCached(repo, targetRev, parentRev, threads, nil)
}

// ReanchorThreadsCached re-anchors threads against live targetRev (or parentRev for Side == "L"), reusing linesCache.
func ReanchorThreadsCached(repo vcs.Repo, targetRev, parentRev string, threads []*db.Thread, linesCache map[string][]string) []*ThreadView {
	return ReanchorThreadsForChange(repo, nil, nil, targetRev, parentRev, threads, linesCache)
}

// ReanchorThreadsForChange re-anchors threads against targetRev (or baseRev for
// Side == "L"), marking Side == "L" /COMMIT_MSG threads stale when baseRev is a
// parent or empty base revision rather than an earlier revision of the change.
func ReanchorThreadsForChange(repo vcs.Repo, c *vcs.Change, snaps []*db.Snapshot, targetRev, baseRev string, threads []*db.Thread, linesCache map[string][]string) []*ThreadView {
	return reanchorThreads(repo, c, snaps, nil, targetRev, baseRev, threads, linesCache)
}

// ReanchorThreadsForView re-anchors threads for a specific diff view comparing
// baseRev against targetRev. Visible threads use ViewScope.ThreadSide (handling
// both R -> L and L -> R interdiff flips), while non-visible threads fall back
// to ReanchorThreadsForChange side semantics.
func ReanchorThreadsForView(repo vcs.Repo, c *vcs.Change, snaps []*db.Snapshot, targetRev, baseRev string, isLiveTarget bool, threads []*db.Thread, linesCache map[string][]string) []*ThreadView {
	scope := NewViewScope(snaps, baseRev, targetRev, isLiveTarget)
	return reanchorThreads(repo, c, snaps, &scope, targetRev, baseRev, threads, linesCache)
}

func reanchorThreads(repo vcs.Repo, c *vcs.Change, snaps []*db.Snapshot, scope *ViewScope, targetRev, baseRev string, threads []*db.Thread, linesCache map[string][]string) []*ThreadView {
	views := make([]*ThreadView, len(threads))
	if linesCache == nil {
		linesCache = make(map[string][]string)
	}
	getLines := func(rev, path string) []string {
		norm := strings.TrimPrefix(path, "/")
		key := rev + "\x00" + norm
		if lines, seen := linesCache[key]; seen {
			return lines
		}
		data, err := repo.Content(rev, path)
		if err != nil || len(data) == 0 {
			linesCache[key] = nil
			return nil
		}
		lines := diff.SplitLines(data)
		linesCache[key] = lines
		return lines
	}

	var commitMsgBaseChecked bool
	var hasCommitMsgBase bool
	checkCommitMsgBase := func() bool {
		if !commitMsgBaseChecked {
			hasCommitMsgBase = CommitMsgHasBase(repo, c, snaps, baseRev, targetRev)
			commitMsgBaseChecked = true
		}
		return hasCommitMsgBase
	}

	for i, th := range threads {
		effectiveSide := th.Side
		if scope != nil {
			if side, ok := scope.ThreadSide(th); ok {
				effectiveSide = side
			} else if th.Rev != "" && baseRev != targetRev && th.Side == "R" && th.Rev == baseRev {
				effectiveSide = "L"
			}
		} else if th.Rev != "" && baseRev != targetRev && th.Side == "R" && th.Rev == baseRev {
			effectiveSide = "L"
		}
		tv := &ThreadView{
			Thread:        th,
			CurrentStart:  th.StartLine,
			CurrentEnd:    th.EndLine,
			Stale:         th.Stale,
			AnchorText:    th.AnchorLines,
			EffectiveSide: effectiveSide,
		}

		if vcs.IsCommitMsgPath(th.Path) && effectiveSide == "L" && !checkCommitMsgBase() {
			tv.Stale = true
			views[i] = tv
			continue
		}

		rev := targetRev
		if effectiveSide == "L" && baseRev != "" {
			rev = baseRev
		}

		if th.StartLine == 0 {
			tv.Stale = false
		} else if rev != "" && repo != nil {
			lines := getLines(rev, th.Path)
			if lines == nil {
				tv.Stale = true
			} else if newStart, newEnd, matched := matchAnchorLines(lines, th.StartLine, th.AnchorLines); matched {
				tv.CurrentStart = newStart
				tv.CurrentEnd = newEnd
				tv.Stale = false
			} else if th.AnchorLines != "" {
				tv.Stale = true
			}
		}

		views[i] = tv
	}
	return views
}

func anchorThreadWithLines(th *db.Thread, effectiveSide string, leftLines, rightLines []string, forceStale bool, fileThreads []*ThreadView, leftThreads, rightThreads map[int][]*ThreadView) []*ThreadView {
	tv := &ThreadView{
		Thread:        th,
		CurrentStart:  th.StartLine,
		CurrentEnd:    th.EndLine,
		Stale:         th.Stale,
		AnchorText:    th.AnchorLines,
		EffectiveSide: effectiveSide,
	}

	if forceStale {
		tv.Stale = true
		return append(fileThreads, tv)
	}

	if th.StartLine == 0 {
		tv.Stale = false
		return append(fileThreads, tv)
	}

	side := effectiveSide
	if side != "L" && side != "R" {
		side = th.Side
	}
	targetLines := rightLines
	if side == "L" {
		targetLines = leftLines
	}

	if len(targetLines) == 0 {
		tv.Stale = true
		return append(fileThreads, tv)
	}

	if newStart, newEnd, matched := matchAnchorLines(targetLines, th.StartLine, th.AnchorLines); matched {
		tv.CurrentStart = newStart
		tv.CurrentEnd = newEnd
		tv.Stale = false
		if side == "L" {
			leftThreads[tv.CurrentEnd] = append(leftThreads[tv.CurrentEnd], tv)
		} else {
			rightThreads[tv.CurrentEnd] = append(rightThreads[tv.CurrentEnd], tv)
		}
	} else {
		if th.AnchorLines != "" {
			tv.Stale = true
		}
		fileThreads = append(fileThreads, tv)
	}
	return fileThreads
}

// ReanchorThreadsWithLines re-anchors threads against pre-split leftLines and rightLines.
func ReanchorThreadsWithLines(leftLines, rightLines []string, threads []*db.Thread) (fileThreads []*ThreadView, leftThreads map[int][]*ThreadView, rightThreads map[int][]*ThreadView) {
	leftThreads = make(map[int][]*ThreadView)
	rightThreads = make(map[int][]*ThreadView)

	for _, th := range threads {
		fileThreads = anchorThreadWithLines(th, "", leftLines, rightLines, false, fileThreads, leftThreads, rightThreads)
	}

	return fileThreads, leftThreads, rightThreads
}

// ReanchorViewThreadsWithLines filters threads visible in the diff view (baseRev, targetRev,
// isLiveTarget), computes their visible comment counts, and re-anchors them against pre-split
// leftLines and rightLines, routing interdiff threads to their effective displayed pane.
func ReanchorViewThreadsWithLines(leftLines, rightLines []string, baseRev, targetRev string, snaps []*db.Snapshot, isLiveTarget, hasCommitMsgBase bool, threads []*db.Thread) (fileThreads []*ThreadView, leftThreads map[int][]*ThreadView, rightThreads map[int][]*ThreadView, counts CommentCounts) {
	leftThreads = make(map[int][]*ThreadView)
	rightThreads = make(map[int][]*ThreadView)
	scope := NewViewScope(snaps, baseRev, targetRev, isLiveTarget)

	for _, th := range threads {
		effectiveSide, ok := scope.ThreadSide(th)
		if !ok {
			continue
		}
		addThreadCommentCounts(&counts, th)
		forceStale := vcs.IsCommitMsgPath(th.Path) && effectiveSide == "L" && !hasCommitMsgBase
		fileThreads = anchorThreadWithLines(th, effectiveSide, leftLines, rightLines, forceStale, fileThreads, leftThreads, rightThreads)
	}

	return fileThreads, leftThreads, rightThreads, counts
}

// GetThreadContextCached returns surrounding source lines around a thread from its anchored revision,
// reusing linesCache.
func GetThreadContextCached(repo vcs.Repo, tv *ThreadView, ctxLines int, linesCache map[string][]string) string {
	if repo != nil && tv.Thread != nil && tv.Thread.Rev != "" && tv.Thread.Path != "" {
		var lines []string
		norm := strings.TrimPrefix(tv.Thread.Path, "/")
		key := tv.Thread.Rev + "\x00" + norm
		var seen bool
		if linesCache != nil {
			lines, seen = linesCache[key]
		}
		if !seen {
			data, err := repo.Content(tv.Thread.Rev, tv.Thread.Path)
			if err == nil && len(data) > 0 {
				lines = diff.SplitLines(data)
			} else {
				lines = nil
			}
			if linesCache != nil {
				linesCache[key] = lines
			}
		}
		if len(lines) > 0 {
			start := tv.Thread.StartLine - ctxLines
			if start < 1 {
				start = 1
			}
			end := tv.Thread.EndLine + ctxLines
			if end > len(lines) {
				end = len(lines)
			}
			if start <= end && start <= len(lines) {
				var sb strings.Builder
				for i := start; i <= end; i++ {
					prefix := "  "
					if i >= tv.Thread.StartLine && i <= tv.Thread.EndLine {
						prefix = "> "
					}
					fmt.Fprintf(&sb, "%s%4d | %s\n", prefix, i, lines[i-1])
				}
				return sb.String()
			}
		}
	}
	if tv.AnchorText != "" {
		return tv.AnchorText + "\n"
	}
	return ""
}

// AddCommentRange anchors and creates a new thread with character column range in the database.
func AddCommentRange(d *db.DB, repo vcs.Repo, repoID int64, c *vcs.Change, rev, path, side string, startLine, endLine, startCol, endCol int, author, body string, draft, noPin bool) (*db.Thread, *db.Message, error) {
	return AddCommentRangeWithTarget(d, repo, repoID, c, rev, "", path, side, startLine, endLine, startCol, endCol, author, body, draft, noPin)
}

// AddCommentRangeWithTarget anchors and creates a new thread with character column range in the database,
// using targetRev (the displayed right-side revision, if known) to disambiguate left-side parent comments
// across snapshots.
func AddCommentRangeWithTarget(d *db.DB, repo vcs.Repo, repoID int64, c *vcs.Change, rev, targetRev, path, side string, startLine, endLine, startCol, endCol int, author, body string, draft, noPin bool) (*db.Thread, *db.Message, error) {
	if c == nil {
		return nil, nil, errors.New("nil change")
	}
	if vcs.IsCommitMsgPath(path) {
		path = vcs.CommitMsgPath
	}
	if strings.EqualFold(side, "L") || strings.EqualFold(side, "left") {
		side = "L"
	} else {
		side = "R"
	}
	snaps, _ := d.Snapshots(repoID, c.Key)
	if path == vcs.CommitMsgPath && side == "L" && !CommitMsgHasBase(repo, c, snaps, rev, targetRev) {
		side = "R"
		if targetRev != "" {
			rev = targetRev
		} else {
			rev = c.Rev
		}
	}
	if rev == "" {
		if side == "L" && c.Parent != "" {
			rev = c.Parent
		} else {
			rev = c.Rev
		}
	}

	// Extract anchor text from content
	var anchorLines string
	data, err := repo.Content(rev, path)
	if err == nil && len(data) > 0 {
		lines := diff.SplitLines(data)
		if startLine > 0 && startLine <= len(lines) {
			end := endLine
			if end < startLine || end > len(lines) {
				end = startLine
			}
			anchorLines = strings.Join(lines[startLine-1:end], "\n")
		}
	}

	// Determine snapshot number (0 if unsnapshotted live commit)
	snapNum := 0
	effectiveTarget := targetRev
	if effectiveTarget == "" && rev == c.Parent {
		effectiveTarget = c.Rev
	}
	for i := len(snaps) - 1; i >= 0; i-- {
		s := snaps[i]
		if s.Rev == rev || (side == "L" && s.ParentRev == rev && (effectiveTarget == "" || s.Rev == effectiveTarget)) {
			snapNum = s.Num
			break
		}
	}

	th, msg, err := d.AddThreadRange(repoID, c.Key, rev, snapNum, path, side, startLine, endLine, startCol, endCol, anchorLines, author, body, draft)
	if err != nil {
		return nil, nil, err
	}

	// If commenting on an unsnapshotted live commit, pin it in VCS
	if snapNum == 0 && !noPin && rev != "" {
		shortRev := rev
		if len(shortRev) > 12 {
			shortRev = shortRev[:12]
		}
		pinSuffix := fmt.Sprintf("%s/rev-%s", c.Key, shortRev)
		_ = repo.Pin(pinSuffix, rev)
	}

	return th, msg, nil
}

// AddComment anchors and creates a new thread in the database, automatically pinning
// unsnapshotted commits in VCS unless noPin is true.
func AddComment(d *db.DB, repo vcs.Repo, repoID int64, c *vcs.Change, rev, path, side string, startLine, endLine int, author, body string, draft, noPin bool) (*db.Thread, *db.Message, error) {
	return AddCommentRange(d, repo, repoID, c, rev, path, side, startLine, endLine, 0, 0, author, body, draft, noPin)
}

// SearchChange searches /COMMIT_MSG and all changed files in change c for query.
func SearchChange(repo vcs.Repo, c *vcs.Change, rev, parentRev, query string, caseInsensitive bool) ([]SearchFileResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}

	files, err := repo.Files(parentRev, rev)
	if err != nil {
		return nil, err
	}

	q := query
	if caseInsensitive {
		q = strings.ToLower(q)
	}

	var results []SearchFileResult

	// 1. Search /COMMIT_MSG first so line numbers match /f/COMMIT_MSG.
	// Prefer repo.Content so real repos match the exact rendered bytes,
	// falling back to vcs.FormatCommitMsg when CommitMsgPath is unavailable.
	var msg string
	if repo != nil && rev != "" {
		if data, err := repo.Content(rev, vcs.CommitMsgPath); err == nil && len(data) > 0 {
			msg = string(data)
		}
	}
	if msg == "" {
		var targetCommit *vcs.Change
		if c != nil && (c.Rev == rev || rev == "") {
			targetCommit = c
		} else if repo != nil && rev != "" {
			if rc, err := repo.Commit(rev); err == nil && rc != nil {
				targetCommit = rc
			} else if c != nil {
				targetCommit = c
			}
		} else if c != nil {
			targetCommit = c
		}
		msg = vcs.FormatCommitMsg(targetCommit)
	}
	if len(msg) > 0 {
		msgLines := diff.SplitLines([]byte(msg))
		var msgMatches []SearchLineMatch
		for i, line := range msgLines {
			lineText := line
			if caseInsensitive {
				lineText = strings.ToLower(line)
			}
			if strings.Contains(lineText, q) {
				msgMatches = append(msgMatches, SearchLineMatch{
					Line:    i + 1,
					Snippet: strings.TrimSpace(line),
				})
			}
		}
		if len(msgMatches) > 0 {
			results = append(results, SearchFileResult{
				Path:    vcs.CommitMsgPath,
				Matches: msgMatches,
			})
		}
	}

	// 2. Search changed files
	for _, f := range files {
		if f.Path == vcs.CommitMsgPath {
			continue
		}
		data, err := repo.Content(rev, f.Path)
		if err != nil {
			continue
		}
		if diff.IsBinary(data) {
			continue
		}

		lines := diff.SplitLines(data)
		var matches []SearchLineMatch

		for i, line := range lines {
			lineText := line
			if caseInsensitive {
				lineText = strings.ToLower(line)
			}
			if strings.Contains(lineText, q) {
				matches = append(matches, SearchLineMatch{
					Line:    i + 1,
					Snippet: strings.TrimSpace(line),
				})
			}
		}

		if len(matches) > 0 {
			results = append(results, SearchFileResult{
				Path:    f.Path,
				Matches: matches,
			})
		}
	}

	return results, nil
}

// TakeSnapshot records a snapshot and optionally pins the revision in VCS.
func TakeSnapshot(d *db.DB, repo vcs.Repo, repoID int64, c *vcs.Change, pin bool) (*db.Snapshot, bool, error) {
	snap, created, err := d.TakeSnapshot(repoID, c)
	if err != nil {
		return nil, false, err
	}
	if created && pin && repo != nil {
		_ = repo.Pin(fmt.Sprintf("%s/snap-%d", c.Key, snap.Num), c.Rev)
	}
	return snap, created, nil
}

// PublishResult describes the outcome of publishing draft comments.
type PublishResult struct {
	Published       int64
	Snapshot        *db.Snapshot
	CreatedSnapshot bool
}

// PublishChange publishes all draft comments on changeKey after ensuring every
// unsnapshotted round with draft comments has a snapshot recorded. If a snapshot
// already exists for the commented revision, no redundant snapshot is taken.
func PublishChange(d *db.DB, repo vcs.Repo, repoID int64, changeKey string, c *vcs.Change, pin bool) (*PublishResult, error) {
	if c == nil && repo != nil && changeKey != "" {
		if resolved, err := repo.Commit(changeKey); err == nil && resolved != nil {
			c = resolved
		}
	}
	res := &PublishResult{}

	threads, err := d.ThreadsForChange(repoID, changeKey)
	if err != nil {
		return nil, fmt.Errorf("loading threads for publish: %w", err)
	}

	hasDraftMsg := func(th *db.Thread) bool {
		for _, m := range th.Messages {
			if m.Draft {
				return true
			}
		}
		return false
	}

	anyDrafts := false
	var zeroDrafts []*db.Thread
	for _, th := range threads {
		if !hasDraftMsg(th) {
			continue
		}
		anyDrafts = true
		if th.SnapshotNum == 0 {
			zeroDrafts = append(zeroDrafts, th)
		}
	}
	if !anyDrafts {
		n, err := d.PublishChange(repoID, changeKey)
		if err != nil {
			return nil, err
		}
		res.Published = n
		return res, nil
	}

	recordSnap := func(snap *db.Snapshot, created bool) {
		if snap == nil {
			return
		}
		if created {
			res.CreatedSnapshot = true
			res.Snapshot = snap
			return
		}
		if res.Snapshot == nil {
			res.Snapshot = snap
		}
	}

	if len(zeroDrafts) > 0 {
		if repo != nil {
			type revOrder struct {
				rev   string
				minID int64
			}
			seen := make(map[string]int64)
			for _, th := range zeroDrafts {
				if th.Rev == "" || th.Side == "L" || (c != nil && th.Rev == c.Rev) {
					continue
				}
				if prev, ok := seen[th.Rev]; !ok || th.ID < prev {
					seen[th.Rev] = th.ID
				}
			}
			var priorRevs []revOrder
			for rev, minID := range seen {
				priorRevs = append(priorRevs, revOrder{rev: rev, minID: minID})
			}
			sort.Slice(priorRevs, func(i, j int) bool {
				return priorRevs[i].minID < priorRevs[j].minID
			})
			for _, item := range priorRevs {
				rc, err := repo.Commit(item.rev)
				if err != nil || rc == nil {
					continue
				}
				rcCopy := *rc
				rcCopy.Key = changeKey
				snap, created, err := TakeSnapshot(d, repo, repoID, &rcCopy, pin)
				if err != nil {
					return nil, err
				}
				recordSnap(snap, created)
			}
		}

		// Reload to check which snapshot_num = 0 drafts remain (e.g. on c.Rev, side L, or unreachable revs).
		threads, err = d.ThreadsForChange(repoID, changeKey)
		if err != nil {
			return nil, fmt.Errorf("reloading threads for publish: %w", err)
		}
		hasRemainingZero := false
		for _, th := range threads {
			if th.SnapshotNum == 0 && hasDraftMsg(th) {
				hasRemainingZero = true
				break
			}
		}
		if hasRemainingZero && c != nil {
			snap, created, err := TakeSnapshot(d, repo, repoID, c, pin)
			if err != nil {
				return nil, err
			}
			recordSnap(snap, created)
			if err := d.PromoteOrphanedZeroThreads(repoID, changeKey, snap.Num); err != nil {
				return nil, err
			}
		}
	}

	n, err := d.PublishChange(repoID, changeKey)
	if err != nil {
		return nil, err
	}
	res.Published = n
	return res, nil
}

// PublishRepo publishes all draft comments in the repository, automatically taking
// snapshots for any changes with unsnapshotted draft rounds.
func PublishRepo(d *db.DB, repo vcs.Repo, repoID int64, pin bool) (*PublishResult, error) {
	keys, err := d.ChangeKeysWithDrafts(repoID)
	if err != nil {
		return nil, fmt.Errorf("finding changes with drafts: %w", err)
	}

	res := &PublishResult{}
	changeByKey := make(map[string]*vcs.Change)
	if len(keys) > 0 && repo != nil {
		if changes, err := repo.Changes(); err == nil {
			for _, ch := range changes {
				if ch != nil {
					changeByKey[ch.Key] = ch
				}
			}
		}
	}

	for _, key := range keys {
		sub, err := PublishChange(d, repo, repoID, key, changeByKey[key], pin)
		if err != nil {
			return nil, err
		}
		res.Published += sub.Published
		if sub.CreatedSnapshot {
			res.CreatedSnapshot = true
			res.Snapshot = sub.Snapshot
		} else if res.Snapshot == nil && sub.Snapshot != nil {
			res.Snapshot = sub.Snapshot
		}
	}

	rem, err := d.PublishRepo(repoID)
	if err != nil {
		return nil, err
	}
	res.Published += rem
	return res, nil
}

// RunGC cleans up missing repositories and prunes stale review DB data and VCS pins.
func RunGC(d *db.DB, repos []*db.RepoRecord, dryRun, keepMissing bool) (*GCReport, error) {
	report := &GCReport{}

	for _, rec := range repos {
		// 1. Check if repo directory exists
		if _, err := os.Stat(rec.RootPath); err != nil {
			if !keepMissing {
				report.ForgottenRepos = append(report.ForgottenRepos, rec.Slug)
				if !dryRun {
					_, _ = d.ForgetRepo(rec.Slug)
				}
			}
			continue
		}

		// 2. Open repo with a per-repo cache closed before moving to the next repo.
		func() {
			cache := vcs.NewCache()
			defer cache.Close()

			repo, err := vcs.Open(rec.RootPath, cache)
			if err != nil {
				return
			}

			changes, err := repo.Changes()
			if err != nil {
				return
			}

			activeKeys := make(map[string]bool)
			for _, c := range changes {
				activeKeys[c.Key] = true
			}

			// 3. Prune DB changes not pending
			if dryRun {
				staleKeys, _ := d.StaleChangeKeys(rec.ID, activeKeys)
				for _, p := range staleKeys {
					report.PrunedChanges = append(report.PrunedChanges, filepath.Join(rec.Slug, p))
				}
			} else {
				pruned, err := d.PruneChanges(rec.ID, activeKeys)
				if err == nil && len(pruned) > 0 {
					for _, p := range pruned {
						report.PrunedChanges = append(report.PrunedChanges, filepath.Join(rec.Slug, p))
					}
				}
			}

			// 4. Prune VCS pins
			if dryRun {
				stalePins, _ := vcs.StalePins(repo, activeKeys)
				report.PrunedPins = append(report.PrunedPins, stalePins...)
			} else {
				prunedPins, err := vcs.PrunePins(repo, activeKeys)
				if err == nil {
					report.PrunedPins = append(report.PrunedPins, prunedPins...)
				}
			}
		}()
	}

	return report, nil
}
