package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

// ViewFile represents a file in a change view with stats and review state.
type ViewFile struct {
	*vcs.File
	Reviewed       bool
	WhitespaceOnly bool
	Threads        []*db.Thread
	CommentCounts  CommentCounts
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
	ForgottenRepos  []string
	PrunedChanges   []string
	PrunedSnapshots []string
	PrunedPins      []string
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
	snaps, _ = SyncLiveRebasedSnapshot(d, repo, rec.ID, c, snaps)

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
		if err := vcs.ValidateRev(targetSel); err != nil {
			return nil, fmt.Errorf("invalid target revision: %w", err)
		}
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
		if err := vcs.ValidateRev(baseSel); err != nil {
			return nil, fmt.Errorf("invalid base revision: %w", err)
		}
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
	oldPathByNew := make(map[string]string)
	for _, f := range allFiles {
		if f != nil && f.Status == vcs.FileRenamed && f.OldPath != "" {
			oldPathByNew[strings.TrimPrefix(f.Path, "/")] = f.OldPath
		}
	}
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
				if lines == nil && effectiveSide == "L" {
					if oldP := oldPathByNew[strings.TrimPrefix(th.Path, "/")]; oldP != "" {
						lines = getLines(rev, oldP)
					}
				}
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
	CurrentStart   int  `json:"current_line"`
	CurrentEnd     int  `json:"current_end_line"`
	Stale          bool `json:"stale"`
	AnchorText     string
	EffectiveSide  string    `json:"-"`
	SnapshotTime   time.Time `json:"-"`
	ReplyRoundNums []int     `json:"-"`
}

// ReplyRound returns the snapshot number when message idx transitioned to a newer
// snapshot round than the previous message, or 0 if it stayed in the same round.
func (tv *ThreadView) ReplyRound(idx int) int {
	if tv == nil || idx < 0 || idx >= len(tv.ReplyRoundNums) {
		return 0
	}
	return tv.ReplyRoundNums[idx]
}

func populateRoundContext(tv *ThreadView, snaps []*db.Snapshot) {
	if tv == nil || tv.Thread == nil || len(snaps) == 0 {
		return
	}
	if tv.SnapshotNum > 0 {
		for _, sn := range snaps {
			if sn.Num == tv.SnapshotNum {
				tv.SnapshotTime = sn.CreatedAt
				break
			}
		}
	}
	if len(tv.Messages) <= 1 {
		return
	}
	currRound := tv.SnapshotNum
	var rounds []int
	for i := 1; i < len(tv.Messages); i++ {
		m := tv.Messages[i]
		if m == nil || m.CreatedAt.IsZero() {
			continue
		}
		msgRound := 0
		for _, sn := range snaps {
			if !sn.CreatedAt.IsZero() && !m.CreatedAt.Before(sn.CreatedAt) && sn.Num > msgRound {
				msgRound = sn.Num
			}
		}
		if msgRound > currRound {
			if rounds == nil {
				rounds = make([]int, len(tv.Messages))
			}
			rounds[i] = msgRound
			currRound = msgRound
		}
	}
	tv.ReplyRoundNums = rounds
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
	return matchAnchorLinesWindow(lines, startLine, anchor, 30)
}

// matchAnchorLinesRebase attempts to locate anchor within lines, first trying the ±30 line
// window and then continuing outward across the entire file for large upstream shifts.
func matchAnchorLinesRebase(lines []string, startLine int, anchor string) (newStart, newEnd int, matched bool) {
	return matchAnchorLinesWindow(lines, startLine, anchor, -1)
}

func matchAnchorLinesWindow(lines []string, startLine int, anchor string, maxDelta int) (newStart, newEnd int, matched bool) {
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

	// Radial outward search around startLine so nearby line shifts match before distant duplicates
	for delta := 1; ; delta++ {
		if maxDelta >= 0 && delta > maxDelta {
			break
		}
		hi := startLine + delta
		lo := startLine - delta
		if hi > maxStart && lo < 1 {
			break
		}
		if checkAt(hi) {
			return hi, hi + numAnchor - 1, true
		}
		if checkAt(lo) {
			return lo, lo + numAnchor - 1, true
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

	var renamedOldPathChecked bool
	var oldPathByNew map[string]string
	lookupOldPath := func(path string) string {
		if !renamedOldPathChecked {
			renamedOldPathChecked = true
			if repo != nil && baseRev != "" && targetRev != "" && baseRev != targetRev {
				if files, err := repo.Files(baseRev, targetRev); err == nil {
					for _, f := range files {
						if f != nil && f.Status == vcs.FileRenamed && f.OldPath != "" {
							if oldPathByNew == nil {
								oldPathByNew = make(map[string]string)
							}
							oldPathByNew[strings.TrimPrefix(f.Path, "/")] = f.OldPath
						}
					}
				}
			}
		}
		return oldPathByNew[strings.TrimPrefix(path, "/")]
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
		populateRoundContext(tv, snaps)

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
			if lines == nil && effectiveSide == "L" {
				if oldP := lookupOldPath(th.Path); oldP != "" {
					lines = getLines(rev, oldP)
				}
			}
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

func anchorThreadWithLines(th *db.Thread, effectiveSide string, snaps []*db.Snapshot, leftLines, rightLines []string, forceStale bool, fileThreads []*ThreadView, leftThreads, rightThreads map[int][]*ThreadView) []*ThreadView {
	tv := &ThreadView{
		Thread:        th,
		CurrentStart:  th.StartLine,
		CurrentEnd:    th.EndLine,
		Stale:         th.Stale,
		AnchorText:    th.AnchorLines,
		EffectiveSide: effectiveSide,
	}
	populateRoundContext(tv, snaps)

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
		fileThreads = anchorThreadWithLines(th, "", nil, leftLines, rightLines, false, fileThreads, leftThreads, rightThreads)
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
		fileThreads = anchorThreadWithLines(th, effectiveSide, snaps, leftLines, rightLines, forceStale, fileThreads, leftThreads, rightThreads)
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
	snaps, _ = SyncLiveRebasedSnapshot(d, repo, repoID, c, snaps)
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
	if rev != "" {
		if err := vcs.ValidateRev(rev); err != nil {
			return nil, nil, err
		}
	}
	if targetRev != "" {
		if err := vcs.ValidateRev(targetRev); err != nil {
			return nil, nil, err
		}
	}

	// Extract anchor text from content
	var anchorLines string
	data, err := repo.Content(rev, path)
	if (err != nil || len(data) == 0) && side == "L" && repo != nil && path != vcs.CommitMsgPath {
		compareTarget := targetRev
		if compareTarget == "" {
			compareTarget = c.Rev
		}
		if compareTarget != "" && compareTarget != rev {
			if files, fErr := repo.Files(rev, compareTarget); fErr == nil {
				normPath := strings.TrimPrefix(path, "/")
				for _, f := range files {
					if f != nil && f.Status == vcs.FileRenamed && f.OldPath != "" && strings.TrimPrefix(f.Path, "/") == normPath {
						data, err = repo.Content(rev, f.OldPath)
						break
					}
				}
			}
		}
	}
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

type patchHunk struct {
	deleted  []string
	inserted []string
}

func extractPatchHunks(parentData, fileData []byte) []patchHunk {
	pLines := diff.SplitLines(parentData)
	fLines := diff.SplitLines(fileData)
	edits := diff.Edits(pLines, fLines)
	var hunks []patchHunk
	for i := 0; i < len(edits); {
		if edits[i].Op == diff.OpEqual {
			i++
			continue
		}
		var h patchHunk
		for i < len(edits) && edits[i].Op != diff.OpEqual {
			if edits[i].Op == diff.OpDelete {
				h.deleted = append(h.deleted, pLines[edits[i].AIndex])
			} else {
				h.inserted = append(h.inserted, fLines[edits[i].BIndex])
			}
			i++
		}
		hunks = append(hunks, h)
	}
	return hunks
}

func sameOwnPatchHunks(dataPA, dataA, dataPB, dataB []byte) bool {
	hunksA := extractPatchHunks(dataPA, dataA)
	hunksB := extractPatchHunks(dataPB, dataB)
	if len(hunksA) != len(hunksB) {
		return false
	}
	for i := range hunksA {
		if !slices.Equal(hunksA[i].deleted, hunksB[i].deleted) ||
			!slices.Equal(hunksA[i].inserted, hunksB[i].inserted) {
			return false
		}
	}
	return true
}

const noOwnDiffCacheMax = 512

var (
	noOwnDiffCacheMu sync.Mutex
	noOwnDiffCache   = make(map[string]bool)
)

func noOwnDiffCacheKey(repo vcs.Repo, revA, parentA, revB, parentB string) string {
	if repo == nil || !vcs.IsImmutableHexRev(revA) || !vcs.IsImmutableHexRev(revB) {
		return ""
	}
	if parentA != "" && !vcs.IsImmutableHexRev(parentA) {
		return ""
	}
	if parentB != "" && !vcs.IsImmutableHexRev(parentB) {
		return ""
	}
	return repo.Root() + "\x00" + revA + "\x00" + parentA + "\x00" + revB + "\x00" + parentB
}

func revisionsHaveNoOwnDiff(repo vcs.Repo, revA, parentA, revB, parentB string) bool {
	if revA == "" || revB == "" {
		return false
	}
	if revA == revB {
		return true
	}
	if repo == nil {
		return false
	}
	if key := noOwnDiffCacheKey(repo, revA, parentA, revB, parentB); key != "" {
		noOwnDiffCacheMu.Lock()
		cached, ok := noOwnDiffCache[key]
		noOwnDiffCacheMu.Unlock()
		if ok {
			return cached
		}
	}
	commitB, err := repo.Commit(revB)
	if err != nil || commitB == nil {
		return false
	}
	cb := *commitB
	if parentB != "" {
		cb.Parent = parentB
	}
	return RevisionsHaveNoOwnDiff(repo, revA, parentA, &cb)
}

// RevisionsHaveNoOwnDiff reports whether commitB represents the exact same own commit
// changes (commit message, author, and per-parent patch hunks) as revA, differing only
// by upstream/parent rebase changes.
func RevisionsHaveNoOwnDiff(repo vcs.Repo, revA, parentA string, commitB *vcs.Change) bool {
	if revA == "" || commitB == nil || commitB.Rev == "" {
		return false
	}
	if revA == commitB.Rev {
		return true
	}
	if repo == nil {
		return false
	}
	cacheKey := noOwnDiffCacheKey(repo, revA, parentA, commitB.Rev, commitB.Parent)
	if cacheKey != "" {
		noOwnDiffCacheMu.Lock()
		cached, ok := noOwnDiffCache[cacheKey]
		noOwnDiffCacheMu.Unlock()
		if ok {
			return cached
		}
	}
	res := computeRevisionsHaveNoOwnDiff(repo, revA, parentA, commitB)
	if cacheKey != "" {
		noOwnDiffCacheMu.Lock()
		if len(noOwnDiffCache) >= noOwnDiffCacheMax {
			clear(noOwnDiffCache)
		}
		noOwnDiffCache[cacheKey] = res
		noOwnDiffCacheMu.Unlock()
	}
	return res
}

func computeRevisionsHaveNoOwnDiff(repo vcs.Repo, revA, parentA string, commitB *vcs.Change) bool {
	commitA, err := repo.Commit(revA)
	if err != nil || commitA == nil {
		return false
	}
	if parentA == "" && commitA.Parent != "" {
		parentA = commitA.Parent
	}
	revB := commitB.Rev
	parentB := commitB.Parent

	if strings.TrimSpace(commitA.Message) != strings.TrimSpace(commitB.Message) || commitA.Author != commitB.Author {
		return false
	}

	files, err := repo.Files(revA, revB)
	if err != nil {
		return false
	}
	var changedFiles []*vcs.File
	for _, f := range files {
		if f == nil || vcs.IsCommitMsgPath(f.Path) {
			continue
		}
		changedFiles = append(changedFiles, f)
	}
	if len(changedFiles) == 0 {
		return true
	}
	if parentA == "" || parentB == "" || parentA == parentB {
		return false
	}

	isOKOrMissing := func(err error) bool {
		return err == nil || errors.Is(err, os.ErrNotExist)
	}
	compareExistence := func(err1, err2 error, eq bool) (equal, exists1, exists2, ok bool) {
		if !isOKOrMissing(err1) || !isOKOrMissing(err2) {
			return false, false, false, false
		}
		if err1 != nil && err2 != nil {
			return true, false, false, true
		}
		if (err1 == nil) != (err2 == nil) {
			return false, err1 == nil, err2 == nil, true
		}
		return eq, true, true, true
	}

	oider, hasOIDer := repo.(vcs.BlobOIDer)
	blobsEqual := func(rev1, path1, rev2, path2 string) (equal, exists1, exists2, ok bool) {
		if hasOIDer {
			oid1, err1 := oider.BlobOID(rev1, path1)
			oid2, err2 := oider.BlobOID(rev2, path2)
			if eq, e1, e2, valid := compareExistence(err1, err2, oid1 == oid2); valid {
				return eq, e1, e2, true
			}
		}
		d1, err1 := repo.Content(rev1, path1)
		d2, err2 := repo.Content(rev2, path2)
		return compareExistence(err1, err2, bytes.Equal(d1, d2))
	}

	for _, f := range changedFiles {
		if f.Status == vcs.FileRenamed && f.OldPath != "" && f.OldPath != f.Path {
			eqAOld, _, _, ok1 := blobsEqual(revA, f.OldPath, parentA, f.OldPath)
			eqANew, _, _, ok2 := blobsEqual(revA, f.Path, parentA, f.Path)
			eqBOld, _, _, ok3 := blobsEqual(revB, f.OldPath, parentB, f.OldPath)
			eqBNew, _, _, ok4 := blobsEqual(revB, f.Path, parentB, f.Path)
			if ok1 && ok2 && ok3 && ok4 && eqAOld && eqANew && eqBOld && eqBNew {
				continue
			}
			return false
		}

		p := f.Path
		eqA, _, _, okA := blobsEqual(revA, p, parentA, p)
		eqB, _, _, okB := blobsEqual(revB, p, parentB, p)
		if !okA || !okB {
			return false
		}
		if eqA && eqB {
			continue
		}
		if eqA != eqB {
			return false
		}

		dataA, errA := repo.Content(revA, p)
		dataPA, errPA := repo.Content(parentA, p)
		dataB, errB := repo.Content(revB, p)
		dataPB, errPB := repo.Content(parentB, p)

		if !isOKOrMissing(errA) || !isOKOrMissing(errPA) || !isOKOrMissing(errB) || !isOKOrMissing(errPB) {
			return false
		}
		if (errA == nil) != (errB == nil) || (errPA == nil) != (errPB == nil) {
			return false
		}
		if errA != nil && errB != nil {
			continue
		}

		if (errA == nil && diff.IsBinary(dataA)) ||
			(errPA == nil && diff.IsBinary(dataPA)) ||
			(errB == nil && diff.IsBinary(dataB)) ||
			(errPB == nil && diff.IsBinary(dataPB)) {
			if !bytes.Equal(dataA, dataB) {
				return false
			}
			continue
		}

		if !sameOwnPatchHunks(dataPA, dataA, dataPB, dataB) {
			return false
		}

		interLinesA := diff.SplitLines(dataA)
		interLinesB := diff.SplitLines(dataB)
		rows := diff.BuildRows(interLinesA, interLinesB, diff.Edits(interLinesA, interLinesB))
		allEqual := true
		for _, r := range rows {
			if r.Type != diff.RowEqual {
				allEqual = false
				break
			}
		}
		if allEqual {
			continue
		}
		diff.MarkRebased(rows, dataA, dataB, dataPA, dataPB)
		if !diff.AllRebased(rows) {
			return false
		}
	}

	return true
}

func precomputeThreadUpdates(repo vcs.Repo, threads []*db.Thread, oldSnapByNum map[int]*db.Snapshot, pendingOnOld *db.Snapshot, newRev, newParent string, linesCache map[string][]string) (updates []db.ThreadAnchorUpdate, unmatchedRevs []string) {
	if len(threads) == 0 || (len(oldSnapByNum) == 0 && pendingOnOld == nil) {
		return nil, nil
	}
	if linesCache == nil {
		linesCache = make(map[string][]string)
	}
	getLines := func(rev, path string) []string {
		if repo == nil || rev == "" {
			return nil
		}
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

	for _, th := range threads {
		if th == nil {
			continue
		}
		var oldSnap *db.Snapshot
		if th.SnapshotNum > 0 {
			oldSnap = oldSnapByNum[th.SnapshotNum]
		} else if pendingOnOld != nil {
			if th.Rev == pendingOnOld.Rev || (th.Side == "L" && pendingOnOld.ParentRev != "" && th.Rev == pendingOnOld.ParentRev) {
				oldSnap = pendingOnOld
			}
		}
		if oldSnap == nil {
			continue
		}
		oldRev := oldSnap.Rev

		targetRev := newRev
		if th.Side == "L" && th.Rev != oldRev {
			targetRev = newParent
		}

		if targetRev == "" {
			if th.Rev != "" && th.Rev != newRev && th.Rev != newParent {
				unmatchedRevs = append(unmatchedRevs, th.Rev)
			}
			continue
		}
		if th.StartLine == 0 {
			if th.Rev != targetRev {
				updates = append(updates, db.ThreadAnchorUpdate{
					ID:        th.ID,
					Rev:       targetRev,
					StartLine: 0,
					EndLine:   0,
				})
			}
			continue
		}

		lines := getLines(targetRev, th.Path)
		if newStart, newEnd, matched := matchAnchorLinesRebase(lines, th.StartLine, th.AnchorLines); matched {
			if th.Rev != targetRev || th.StartLine != newStart || th.EndLine != newEnd {
				updates = append(updates, db.ThreadAnchorUpdate{
					ID:        th.ID,
					Rev:       targetRev,
					StartLine: newStart,
					EndLine:   newEnd,
				})
			}
			continue
		}
		if th.Rev != "" && th.Rev != newRev && th.Rev != newParent {
			unmatchedRevs = append(unmatchedRevs, th.Rev)
		}
	}
	return updates, unmatchedRevs
}

func hasAnySnapPin(pins []string, changeKey string) bool {
	prefix := "refs/reviewed/" + changeKey + "/snap-"
	for _, p := range pins {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func hasExactSnapPin(pins []string, changeKey string, snapNum int) bool {
	want := fmt.Sprintf("refs/reviewed/%s/snap-%d", changeKey, snapNum)
	for _, p := range pins {
		if p == want {
			return true
		}
	}
	return false
}

func pinUnmatchedRevs(repo vcs.Repo, changeKey string, unmatchedRevs []string, exclude map[string]bool) {
	if repo == nil || len(unmatchedRevs) == 0 {
		return
	}
	seen := make(map[string]bool, len(unmatchedRevs))
	for _, rev := range unmatchedRevs {
		if rev == "" || seen[rev] || exclude[rev] {
			continue
		}
		seen[rev] = true
		_ = repo.Pin(fmt.Sprintf("%s/rev-%s", changeKey, shortHash(rev)), rev)
	}
}

func carryForwardSnapshot(d *db.DB, repo vcs.Repo, repoID int64, c *vcs.Change, latest *db.Snapshot, shouldPin bool) (*db.Snapshot, bool, error) {
	threads, err := d.ThreadsForChange(repoID, c.Key)
	if err != nil {
		return nil, false, fmt.Errorf("loading threads for carry-forward: %w", err)
	}
	oldByNum := map[int]*db.Snapshot{latest.Num: latest}
	updates, unmatchedRevs := precomputeThreadUpdates(repo, threads, oldByNum, latest, c.Rev, c.Parent, nil)

	updatedSnap, updated, err := d.CarryForwardSnapshot(repoID, c.Key, latest.ID, latest.Num, latest.Rev, latest.ParentRev, c.Rev, c.Parent, updates)
	if err != nil {
		return nil, false, err
	}
	if updated && shouldPin && repo != nil {
		_ = repo.Pin(fmt.Sprintf("%s/snap-%d", c.Key, latest.Num), c.Rev)
		exclude := map[string]bool{c.Rev: true}
		if c.Parent != "" {
			exclude[c.Parent] = true
		}
		pinUnmatchedRevs(repo, c.Key, unmatchedRevs, exclude)
	}
	return updatedSnap, updated, nil
}

// SyncLiveRebasedSnapshot checks whether live change c is a pure rebase of its latest
// snapshot and carries that snapshot (with re-anchored threads and marks) forward in place.
func SyncLiveRebasedSnapshot(d *db.DB, repo vcs.Repo, repoID int64, c *vcs.Change, snaps []*db.Snapshot) ([]*db.Snapshot, bool) {
	if d == nil || repo == nil || c == nil || c.Working || c.Rev == "" || len(snaps) == 0 || SnapshotNumForRev(snaps, c.Rev) > 0 {
		return snaps, false
	}
	latest := snaps[len(snaps)-1]
	if !RevisionsHaveNoOwnDiff(repo, latest.Rev, latest.ParentRev, c) {
		return snaps, false
	}
	pins, err := repo.ListPins()
	shouldPin := err == nil && hasExactSnapPin(pins, c.Key, latest.Num)
	updatedSnap, updated, err := carryForwardSnapshot(d, repo, repoID, c, latest, shouldPin)
	if err != nil {
		return snaps, false
	}
	if reloaded, err := d.Snapshots(repoID, c.Key); err == nil && len(reloaded) > 0 {
		return reloaded, updated || SnapshotNumForRev(reloaded, c.Rev) > 0
	}
	if updated && updatedSnap != nil {
		out := make([]*db.Snapshot, len(snaps))
		copy(out, snaps)
		out[len(out)-1] = updatedSnap
		return out, true
	}
	return snaps, false
}

// TakeSnapshot records a snapshot (carrying forward an existing snapshot in place if
// c is a pure rebase with no own commit diff) and optionally pins the revision in VCS.
func TakeSnapshot(d *db.DB, repo vcs.Repo, repoID int64, c *vcs.Change, pin bool) (*db.Snapshot, bool, error) {
	if d == nil || c == nil {
		return nil, false, errors.New("nil db or change")
	}
	snaps, err := d.Snapshots(repoID, c.Key)
	if err != nil {
		return nil, false, err
	}
	if SnapshotNumForRev(snaps, c.Rev) > 0 {
		return d.TakeSnapshot(repoID, c)
	}
	if len(snaps) > 0 && repo != nil && !c.Working {
		latest := snaps[len(snaps)-1]
		if RevisionsHaveNoOwnDiff(repo, latest.Rev, latest.ParentRev, c) {
			updatedSnap, _, err := carryForwardSnapshot(d, repo, repoID, c, latest, pin)
			if err != nil {
				return nil, false, err
			}
			if updatedSnap != nil {
				return updatedSnap, false, nil
			}
			snaps, err = d.Snapshots(repoID, c.Key)
			if err != nil {
				return nil, false, err
			}
			if SnapshotNumForRev(snaps, c.Rev) > 0 {
				return d.TakeSnapshot(repoID, c)
			}
		}
	}

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

	if c != nil && repo != nil {
		if snaps, err := d.Snapshots(repoID, changeKey); err == nil && len(snaps) > 0 {
			if syncedSnaps, synced := SyncLiveRebasedSnapshot(d, repo, repoID, c, snaps); synced && len(syncedSnaps) > 0 {
				latest := syncedSnaps[len(syncedSnaps)-1]
				if res.Snapshot != nil && res.Snapshot.Num == latest.Num {
					res.Snapshot = latest
				}
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
	return runGC(d, repos, dryRun, keepMissing, vcs.Open)
}

func runGC(d *db.DB, repos []*db.RepoRecord, dryRun, keepMissing bool, openRepo func(string, *vcs.Cache) (vcs.Repo, error)) (*GCReport, error) {
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

			repo, err := openRepo(rec.RootPath, cache)
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

			// 4. Prune consecutive redundant/rebase-only snapshots on active changes
			sortedActiveKeys := make([]string, 0, len(activeKeys))
			for key := range activeKeys {
				sortedActiveKeys = append(sortedActiveKeys, key)
			}
			sort.Strings(sortedActiveKeys)

			validSnapNums := make(map[string]map[int]bool, len(sortedActiveKeys))
			snapLoadFailed := false
			var pins []string
			pinsLoaded := false

			for _, key := range sortedActiveKeys {
				snaps, err := d.Snapshots(rec.ID, key)
				if err != nil {
					snapLoadFailed = true
					continue
				}
				if len(snaps) == 0 {
					validSnapNums[key] = map[int]bool{}
					continue
				}
				if len(snaps) == 1 {
					validSnapNums[key] = map[int]bool{snaps[0].Num: true}
					continue
				}

				type snapGroup struct {
					snaps []*db.Snapshot
					final *db.Snapshot
				}
				groups := []snapGroup{{snaps: []*db.Snapshot{snaps[0]}, final: snaps[0]}}
				for i := 1; i < len(snaps); i++ {
					sn := snaps[i]
					cur := &groups[len(groups)-1]
					if revisionsHaveNoOwnDiff(repo, cur.final.Rev, cur.final.ParentRev, sn.Rev, sn.ParentRev) {
						cur.snaps = append(cur.snaps, sn)
						cur.final = sn
						continue
					}
					groups = append(groups, snapGroup{snaps: []*db.Snapshot{sn}, final: sn})
				}

				if len(groups) == len(snaps) {
					valid := make(map[int]bool, len(snaps))
					for _, sn := range snaps {
						valid[sn.Num] = true
					}
					validSnapNums[key] = valid
					continue
				}

				if dryRun {
					valid := make(map[int]bool, len(groups))
					for idx := range groups {
						valid[idx+1] = true
					}
					validSnapNums[key] = valid
					for _, g := range groups {
						for _, oldSnap := range g.snaps[:len(g.snaps)-1] {
							report.PrunedSnapshots = append(report.PrunedSnapshots, fmt.Sprintf("%s/%s/snap-%d", rec.Slug, key, oldSnap.Num))
						}
					}
					continue
				}

				threads, _ := d.ThreadsForChange(rec.ID, key)
				linesCache := make(map[string][]string)
				collapseGroups := make([]db.SnapshotCollapseGroup, len(groups))
				var allUnmatchedRevs []string
				for idx, g := range groups {
					newNum := idx + 1
					cg := db.SnapshotCollapseGroup{
						KeepID:       g.final.ID,
						NewNum:       newNum,
						NewRev:       g.final.Rev,
						NewParentRev: g.final.ParentRev,
						OldNums:      make([]int, len(g.snaps)),
					}
					oldByNum := make(map[int]*db.Snapshot, len(g.snaps)-1)
					for j, s := range g.snaps {
						cg.OldNums[j] = s.Num
						if j < len(g.snaps)-1 {
							cg.MergedSnapIDs = append(cg.MergedSnapIDs, s.ID)
							cg.MergedOldRevs = append(cg.MergedOldRevs, s.Rev)
							oldByNum[s.Num] = s
						}
					}
					if len(oldByNum) > 0 && len(threads) > 0 {
						updates, unmatched := precomputeThreadUpdates(repo, threads, oldByNum, nil, g.final.Rev, g.final.ParentRev, linesCache)
						cg.ThreadUpdates = updates
						allUnmatchedRevs = append(allUnmatchedRevs, unmatched...)
					}
					collapseGroups[idx] = cg
				}

				collapsed, err := d.CollapseSnapshots(rec.ID, key, len(snaps), collapseGroups)
				if err != nil || !collapsed {
					curSnaps, rErr := d.Snapshots(rec.ID, key)
					if rErr != nil {
						curSnaps = snaps
					}
					valid := make(map[int]bool, len(curSnaps))
					for _, sn := range curSnaps {
						valid[sn.Num] = true
					}
					validSnapNums[key] = valid
					continue
				}

				valid := make(map[int]bool, len(groups))
				for idx := range groups {
					valid[idx+1] = true
				}
				validSnapNums[key] = valid
				for _, g := range groups {
					for _, oldSnap := range g.snaps[:len(g.snaps)-1] {
						report.PrunedSnapshots = append(report.PrunedSnapshots, fmt.Sprintf("%s/%s/snap-%d", rec.Slug, key, oldSnap.Num))
					}
				}

				if !pinsLoaded {
					pins, _ = repo.ListPins()
					pinsLoaded = true
				}
				if hasAnySnapPin(pins, key) {
					survivingRevs := make(map[string]bool, len(collapseGroups)*2)
					for _, cg := range collapseGroups {
						if cg.NewRev != "" {
							survivingRevs[cg.NewRev] = true
							_ = repo.Pin(fmt.Sprintf("%s/snap-%d", key, cg.NewNum), cg.NewRev)
						}
						if cg.NewParentRev != "" {
							survivingRevs[cg.NewParentRev] = true
						}
					}
					pinUnmatchedRevs(repo, key, allUnmatchedRevs, survivingRevs)
				}
			}

			// 5. Prune VCS pins (for non-pending changes and invalid/pruned snapshot numbers)
			if !snapLoadFailed {
				if dryRun {
					stalePins, _ := vcs.StalePinsForSnapshots(repo, activeKeys, validSnapNums)
					report.PrunedPins = append(report.PrunedPins, stalePins...)
				} else {
					prunedPins, err := vcs.PrunePinsForSnapshots(repo, activeKeys, validSnapNums)
					if err == nil {
						report.PrunedPins = append(report.PrunedPins, prunedPins...)
					}
				}
			}
		}()
	}

	return report, nil
}
