package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/diff"
	"nicholas.sh/cmd/review/internal/vcs"
)

func setupTestGitRepo(t *testing.T) (string, *db.DB, *db.RepoRecord, vcs.Repo) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.name", "Engine Tester")
	run("config", "user.email", "tester@example.com")

	// Base commit
	fileA := filepath.Join(dir, "fileA.go")
	if err := os.WriteFile(fileA, []byte("package main\n\nfunc main() {\n\t// initial\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "fileA.go")
	run("commit", "-m", "Initial commit")

	// Commit 1
	if err := os.WriteFile(fileA, []byte("package main\n\n// Added header\nfunc main() {\n\t// initial\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("commit", "-a", "-m", "Add header\n\nChange-Id: I1234567890abcdef1234567890abcdef12345678")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := d.EnsureRepo(dir)
	if err != nil {
		t.Fatal(err)
	}

	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	return dir, d, rec, repo
}

func TestBuildChangeViewAndReanchor(t *testing.T) {
	dir, d, rec, repo := setupTestGitRepo(t)
	defer d.Close()

	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes found: %v", err)
	}
	c := changes[0]

	// Add a comment on "func main()" (currently at line 4)
	th, _, err := AddComment(d, repo, rec.ID, c, c.Rev, "fileA.go", "R", 4, 4, "alice", "needs doc comment", true, false)
	if err != nil {
		t.Fatalf("AddComment failed: %v", err)
	}

	// Add a comment on side L (parent revision)
	thL, _, err := AddComment(d, repo, rec.ID, c, c.Parent, "fileA.go", "L", 1, 1, "bob", "left comment", true, false)
	if err != nil {
		t.Fatalf("AddComment side L failed: %v", err)
	}
	_ = thL

	// Verify pin was created for unsnapshotted live commit
	pins, err := repo.ListPins()
	if err != nil || len(pins) == 0 {
		t.Errorf("expected VCS pin to be created, got pins: %v", pins)
	}

	// Amend commit adding 3 lines at the top so "func main()" shifts down
	fileA := filepath.Join(dir, "fileA.go")
	newContent := "package main\n\n// Line 1\n// Line 2\n// Line 3\n// Added header\nfunc main() {\n\t// initial\n}\n"
	if err := os.WriteFile(fileA, []byte(newContent), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	// Refresh repo and changes
	repoFresh, _ := vcs.Open(dir, nil)
	changesFresh, _ := repoFresh.Changes()
	cFresh := changesFresh[0]

	// Build view: reanchor should detect shifted position
	view, err := BuildChangeView(d, repoFresh, rec, cFresh, "parent", "")
	if err != nil {
		t.Fatalf("BuildChangeView failed: %v", err)
	}

	// Check reanchored threads
	if len(view.Threads) != 2 {
		t.Fatalf("expected 2 threads, got %d", len(view.Threads))
	}
	var reanchoredR, reanchoredL *db.Thread
	for _, tItem := range view.Threads {
		if tItem.ID == th.ID {
			reanchoredR = tItem
		} else if tItem.ID == thL.ID {
			reanchoredL = tItem
		}
	}
	if reanchoredR == nil {
		t.Fatalf("side R thread %d not found in view.Threads", th.ID)
	}
	if reanchoredR.Stale {
		t.Errorf("side R thread was unexpectedly marked stale")
	}
	if reanchoredR.StartLine != 7 {
		t.Errorf("expected side R StartLine shifted to 7, got %d", reanchoredR.StartLine)
	}

	if reanchoredL == nil {
		t.Fatalf("side L thread %d not found in view.Threads", thL.ID)
	}
	if reanchoredL.Stale {
		t.Errorf("side L thread was unexpectedly marked stale")
	}
	if reanchoredL.StartLine != 1 {
		t.Errorf("expected side L StartLine 1, got %d", reanchoredL.StartLine)
	}
}

func TestSearchChange(t *testing.T) {
	_, d, _, repo := setupTestGitRepo(t)
	defer d.Close()

	changes, _ := repo.Changes()
	c := changes[0]

	// Search for "Added header"
	res, err := SearchChange(repo, c, c.Rev, c.Parent, "Added header", false)
	if err != nil {
		t.Fatalf("SearchChange failed: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected search match for 'Added header'")
	}
	if res[0].Path != "fileA.go" || len(res[0].Matches) == 0 {
		t.Errorf("unexpected match result: %+v", res)
	}

	// Case-insensitive search
	resCI, err := SearchChange(repo, c, c.Rev, c.Parent, "added header", true)
	if err != nil || len(resCI) == 0 {
		t.Fatalf("expected case-insensitive match")
	}

	// Search in /COMMIT_MSG
	resMsg, err := SearchChange(repo, c, c.Rev, c.Parent, "Change-Id", false)
	if err != nil || len(resMsg) == 0 {
		t.Fatalf("expected match in /COMMIT_MSG")
	}
	foundCommitMsg := false
	for _, r := range resMsg {
		if r.Path == vcs.CommitMsgPath {
			foundCommitMsg = true
		}
	}
	if !foundCommitMsg {
		t.Errorf("expected match specifically in /COMMIT_MSG")
	}
}

func TestRunGC(t *testing.T) {
	dir, d, rec, _ := setupTestGitRepo(t)
	defer d.Close()

	// Run GC in dry-run mode
	reportDry, err := RunGC(d, []*db.RepoRecord{rec}, true, false)
	if err != nil {
		t.Fatalf("RunGC dry run failed: %v", err)
	}
	if len(reportDry.ForgottenRepos) != 0 {
		t.Errorf("expected 0 forgotten repos in dry run")
	}

	// Test missing repo cleanup
	missingRec := &db.RepoRecord{
		ID:       999,
		Slug:     "nonexistent",
		RootPath: filepath.Join(dir, "does-not-exist"),
	}
	report, err := RunGC(d, []*db.RepoRecord{missingRec}, false, false)
	if err != nil {
		t.Fatalf("RunGC failed: %v", err)
	}
	if len(report.ForgottenRepos) != 1 || report.ForgottenRepos[0] != "nonexistent" {
		t.Errorf("expected 'nonexistent' to be forgotten, got: %v", report.ForgottenRepos)
	}
}

type testDiffRepo struct {
	vcs.Repo
	content map[string][]byte
}

func (m *testDiffRepo) Content(rev, path string) ([]byte, error) {
	if data, ok := m.content[rev+":"+path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("file not found: %s@%s", path, rev)
}

func TestReanchorThreadsForChange(t *testing.T) {
	mock := &testDiffRepo{
		content: map[string][]byte{
			"base:main.go":   []byte("left line 1\nleft line 2\nleft line 3\n"),
			"target:main.go": []byte("preamble 1\npreamble 2\nright line 1\nright line 2\n"),
		},
	}

	threads := []*db.Thread{
		{
			ID:          1,
			Path:        "main.go",
			StartLine:   0,
			EndLine:     0,
			AnchorLines: "",
			Side:        "R",
			Rev:         "target",
		},
		{
			ID:          2,
			Path:        "main.go",
			StartLine:   2,
			EndLine:     2,
			AnchorLines: "left line 2",
			Side:        "L",
			Rev:         "base",
		},
		{
			ID:          3,
			Path:        "main.go",
			StartLine:   1,
			EndLine:     1,
			AnchorLines: "right line 1",
			Side:        "R",
			Rev:         "target",
		},
		{
			ID:          4,
			Path:        "main.go",
			StartLine:   10,
			EndLine:     10,
			AnchorLines: "this line was deleted",
			Side:        "R",
			Rev:         "target",
		},
	}

	views := ReanchorThreadsForChange(mock, nil, nil, "target", "base", threads, nil)
	if len(views) != 4 {
		t.Fatalf("expected 4 reanchored thread views, got %d", len(views))
	}
	if views[0].Thread.ID != 1 || views[0].Stale {
		t.Errorf("expected thread 1 to be non-stale file thread, got: %+v", views[0])
	}
	if views[1].Thread.ID != 2 || views[1].Stale || views[1].CurrentStart != 2 || views[1].CurrentEnd != 2 {
		t.Errorf("expected thread 2 non-stale at left line 2, got: %+v", views[1])
	}
	if views[2].Thread.ID != 3 || views[2].Stale || views[2].CurrentStart != 3 || views[2].CurrentEnd != 3 {
		t.Errorf("expected thread 3 shifted to right line 3, got: %+v", views[2])
	}
	if views[3].Thread.ID != 4 || !views[3].Stale {
		t.Errorf("expected thread 4 to be stale, got: %+v", views[3])
	}

	fileThreads, leftThreads, rightThreads := ReanchorThreadsWithLines(
		diff.SplitLines(mock.content["base:main.go"]),
		diff.SplitLines(mock.content["target:main.go"]),
		threads,
	)
	if len(fileThreads) != 2 || len(leftThreads[2]) != 1 || len(rightThreads[3]) != 1 || rightThreads[3][0].CurrentStart != 3 {
		t.Errorf("unexpected ReanchorThreadsWithLines output: file=%d left=%v right=%v", len(fileThreads), leftThreads, rightThreads)
	}

	// Verify GetThreadContextCached formatting
	ctxSnippet := GetThreadContextCached(mock, &ThreadView{
		Thread: &db.Thread{
			Rev:       "target",
			Path:      "main.go",
			StartLine: 3,
			EndLine:   3,
		},
	}, 1, nil)
	if !strings.Contains(ctxSnippet, ">    3 | right line 1") {
		t.Errorf("unexpected context snippet: %q", ctxSnippet)
	}
}

func TestRunGCDryRunStaleChanges(t *testing.T) {
	dir, d, rec, repo := setupTestGitRepo(t)
	defer d.Close()

	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes found: %v", err)
	}
	c := changes[0]

	// Add thread and snapshot for current change
	_, _, err = AddComment(d, repo, rec.ID, c, c.Rev, "fileA.go", "R", 1, 1, "alice", "note", true, false)
	if err != nil {
		t.Fatalf("AddComment failed: %v", err)
	}
	_, _, err = TakeSnapshot(d, repo, rec.ID, c, false)
	if err != nil {
		t.Fatalf("TakeSnapshot failed: %v", err)
	}
	if err := d.SaveFormDraft(rec.ID, c.Key, "draft:fileA", "fileA.go", "R", 1, 1, 0, 0, true, "", "", "stale unsaved draft"); err != nil {
		t.Fatalf("SaveFormDraft failed: %v", err)
	}

	// Now amend commit with a new Change-Id so its change key changes
	cmd := exec.Command("git", "commit", "--amend", "-m", "Rewritten commit\n\nChange-Id: I9999999999999999999999999999999999999999")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("amend failed: %s (%v)", string(out), err)
	}

	// Dry run GC
	reportDry, err := RunGC(d, []*db.RepoRecord{rec}, true, false)
	if err != nil {
		t.Fatalf("RunGC dry run failed: %v", err)
	}

	expectedStale := filepath.Join(rec.Slug, c.Key)
	foundStale := false
	for _, p := range reportDry.PrunedChanges {
		if p == expectedStale {
			foundStale = true
			break
		}
	}
	if !foundStale {
		t.Errorf("expected dry-run GC to report stale change %q, got: %v", expectedStale, reportDry.PrunedChanges)
	}

	// Verify data was NOT pruned during dry run
	threads, _ := d.ThreadsForChange(rec.ID, c.Key)
	if len(threads) == 0 {
		t.Errorf("threads should not be pruned during dry-run GC")
	}
	drafts, _ := d.FormDraftsForChange(rec.ID, c.Key)
	if len(drafts) == 0 {
		t.Errorf("drafts should not be pruned during dry-run GC")
	}

	// Run actual GC
	report, err := RunGC(d, []*db.RepoRecord{rec}, false, false)
	if err != nil {
		t.Fatalf("RunGC failed: %v", err)
	}
	foundPruned := false
	for _, p := range report.PrunedChanges {
		if p == expectedStale {
			foundPruned = true
			break
		}
	}
	if !foundPruned {
		t.Errorf("expected GC to prune %q, got: %v", expectedStale, report.PrunedChanges)
	}

	// Verify data is now pruned
	threads, _ = d.ThreadsForChange(rec.ID, c.Key)
	if len(threads) != 0 {
		t.Errorf("expected 0 threads after real GC, got %d", len(threads))
	}
	drafts, _ = d.FormDraftsForChange(rec.ID, c.Key)
	if len(drafts) != 0 {
		t.Errorf("expected 0 drafts after real GC, got %d", len(drafts))
	}
}

func TestBuildChangeViewDefaultSelection(t *testing.T) {
	dir, d, rec, repo := setupTestGitRepo(t)
	defer d.Close()

	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes: %v", err)
	}
	c := changes[0]

	// 1. Initial view: no snapshots yet
	v1, err := BuildChangeView(d, repo, rec, c, "", "")
	if err != nil {
		t.Fatalf("BuildChangeView: %v", err)
	}
	if v1.BaseRev != c.Parent || v1.BaseSnapNum != 0 || v1.BaseDesc != "Parent" {
		t.Errorf("expected default base parent, got BaseRev=%s BaseSnapNum=%d BaseDesc=%s", v1.BaseRev, v1.BaseSnapNum, v1.BaseDesc)
	}
	if v1.TargetRev != c.Rev || v1.TargetSnapNum != 0 || !v1.IsLiveTarget || !v1.SnapshotPending {
		t.Errorf("expected default target live snapshot pending, got TargetRev=%s TargetSnapNum=%d IsLiveTarget=%v SnapshotPending=%v",
			v1.TargetRev, v1.TargetSnapNum, v1.IsLiveTarget, v1.SnapshotPending)
	}

	// 2. Take Snapshot 1 (so latestSnap.Rev == c.Rev)
	snap1, created, err := d.TakeSnapshot(rec.ID, c)
	if err != nil || !created || snap1.Num != 1 {
		t.Fatalf("TakeSnapshot failed: created=%v snap1=%+v (err=%v)", created, snap1, err)
	}

	// Mark a file reviewed
	_ = d.SetFileReviewed(rec.ID, c.Key, c.Rev, "fileA.go", true)

	// Check view with default parameters ("" and "")
	v2, err := BuildChangeView(d, repo, rec, c, "", "")
	if err != nil {
		t.Fatalf("BuildChangeView: %v", err)
	}
	if v2.BaseRev != c.Parent || v2.BaseSnapNum != 0 {
		t.Errorf("expected BaseRev == c.Parent and BaseSnapNum == 0 even after snapshot & reviewed, got BaseRev=%s BaseSnapNum=%d", v2.BaseRev, v2.BaseSnapNum)
	}
	if v2.TargetRev != c.Rev || v2.TargetSnapNum != 0 || !v2.IsLiveTarget {
		t.Errorf("expected TargetRev == c.Rev, TargetSnapNum == 0, IsLiveTarget == true, got TargetRev=%s TargetSnapNum=%d IsLiveTarget=%v",
			v2.TargetRev, v2.TargetSnapNum, v2.IsLiveTarget)
	}
	if v2.SnapshotPending {
		t.Errorf("expected SnapshotPending == false since snap1.Rev == c.Rev, got true")
	}
	if v2.TargetDesc != "Snapshot 1" {
		t.Errorf("expected TargetDesc == 'Snapshot 1', got %q", v2.TargetDesc)
	}

	// Check with explicit ("parent", "live")
	v2Explicit, err := BuildChangeView(d, repo, rec, c, "parent", "live")
	if err != nil {
		t.Fatalf("BuildChangeView parent/live: %v", err)
	}
	if v2Explicit.BaseSnapNum != 0 || v2Explicit.TargetSnapNum != 0 || !v2Explicit.IsLiveTarget {
		t.Errorf("expected parent/live to select BaseSnapNum=0, TargetSnapNum=0, IsLiveTarget=true")
	}

	// Check with explicit ("snap:1", "snap:1")
	v2Snap, err := BuildChangeView(d, repo, rec, c, "snap:1", "snap:1")
	if err != nil {
		t.Fatalf("BuildChangeView snap:1/snap:1: %v", err)
	}
	if v2Snap.BaseSnapNum != 1 || v2Snap.BaseRev != snap1.Rev {
		t.Errorf("expected BaseSnapNum=1 BaseRev=%s, got BaseSnapNum=%d BaseRev=%s", snap1.Rev, v2Snap.BaseSnapNum, v2Snap.BaseRev)
	}
	if v2Snap.TargetSnapNum != 1 || v2Snap.TargetRev != snap1.Rev || v2Snap.IsLiveTarget {
		t.Errorf("expected TargetSnapNum=1 TargetRev=%s IsLiveTarget=false, got TargetSnapNum=%d TargetRev=%s IsLiveTarget=%v",
			snap1.Rev, v2Snap.TargetSnapNum, v2Snap.TargetRev, v2Snap.IsLiveTarget)
	}

	// 3. Amend commit to a new c.Rev
	fileA := filepath.Join(dir, "fileA.go")
	_ = os.WriteFile(fileA, []byte("package main\n\n// amended\nfunc main() {}\n"), 0644)
	cmd := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	repoFresh, _ := vcs.Open(dir, nil)
	changesFresh, _ := repoFresh.Changes()
	cFresh := changesFresh[0]

	v3, err := BuildChangeView(d, repoFresh, rec, cFresh, "", "")
	if err != nil {
		t.Fatalf("BuildChangeView after amend: %v", err)
	}
	if v3.BaseRev != cFresh.Parent || v3.BaseSnapNum != 0 {
		t.Errorf("expected BaseRev == cFresh.Parent and BaseSnapNum == 0 after amend, got BaseRev=%s BaseSnapNum=%d", v3.BaseRev, v3.BaseSnapNum)
	}
	if v3.TargetRev != cFresh.Rev || v3.TargetSnapNum != 0 || !v3.IsLiveTarget {
		t.Errorf("expected TargetRev == cFresh.Rev, TargetSnapNum == 0, IsLiveTarget == true, got TargetRev=%s TargetSnapNum=%d IsLiveTarget=%v",
			v3.TargetRev, v3.TargetSnapNum, v3.IsLiveTarget)
	}
	if !v3.SnapshotPending {
		t.Errorf("expected SnapshotPending == true after amend, got false")
	}
	if v3.TargetDesc != "Live Snapshot" {
		t.Errorf("expected TargetDesc == 'Live Snapshot', got %q", v3.TargetDesc)
	}

	t.Run("SnapshotScopedInlineVisibilityAndInterdiff", func(t *testing.T) {
		dirS, dS, recS, repoS := setupTestGitRepo(t)
		defer dS.Close()

		chS, err := repoS.Changes()
		if err != nil || len(chS) == 0 {
			t.Fatalf("no changes: %v", err)
		}
		cS1 := chS[0]
		snapS1, created, err := TakeSnapshot(dS, repoS, recS.ID, cS1, true)
		if err != nil || !created || snapS1.Num != 1 {
			t.Fatalf("TakeSnapshot 1: created=%v snap=%+v err=%v", created, snapS1, err)
		}
		thSnap1R, _, err := AddComment(dS, repoS, recS.ID, cS1, snapS1.Rev, "fileA.go", "R", 4, 4, "alice", "snap1 comment", false, false)
		if err != nil || thSnap1R.SnapshotNum != 1 {
			t.Fatalf("AddComment snap1 R: th=%+v err=%v", thSnap1R, err)
		}

		// Live currently == Snapshot 1: comment must remain inline on default live view.
		viewSnap1Live, err := BuildChangeView(dS, repoS, recS, cS1, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(viewSnap1Live.Threads) != 1 || viewSnap1Live.Threads[0].ID != thSnap1R.ID {
			t.Fatalf("expected Snapshot 1 comment inline on live when live==snap1, got %+v", viewSnap1Live.Threads)
		}

		// Amend commit -> cS2 is now pending (no Snapshot 2 yet).
		if err := os.WriteFile(filepath.Join(dirS, "fileA.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"v2\")\n}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		cmdAmend := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
		cmdAmend.Dir = dirS
		if out, err := cmdAmend.CombinedOutput(); err != nil {
			t.Fatalf("git commit --amend: %s (%v)", string(out), err)
		}
		repoS2, err := vcs.Open(dirS, nil)
		if err != nil {
			t.Fatal(err)
		}
		chS2, _ := repoS2.Changes()
		cS2 := chS2[0]

		// 1. Default Parent -> Live (v2): Snapshot 1 comment is NOT shown inline or in per-file counts,
		//    while ChangeCounts still retains it across all snapshots.
		viewParentLive, err := BuildChangeView(dS, repoS2, recS, cS2, "parent", "live")
		if err != nil {
			t.Fatal(err)
		}
		if len(viewParentLive.Threads) != 0 {
			t.Errorf("expected 0 inline threads on parent..live when Snapshot 1 != live, got %+v", viewParentLive.Threads)
		}
		if viewParentLive.ChangeCounts.Unresolved != 1 {
			t.Errorf("expected ChangeCounts.Unresolved = 1 across snapshots, got %+v", viewParentLive.ChangeCounts)
		}
		for _, vf := range viewParentLive.Files {
			if vf.Path == "fileA.go" && (len(vf.Threads) != 0 || vf.CommentCounts.Unresolved != 0) {
				t.Errorf("expected 0 per-file threads/counts on fileA.go for parent..live, got threads=%+v counts=%+v", vf.Threads, vf.CommentCounts)
			}
		}

		// 2. Parent -> Snapshot 1: Snapshot 1 comment IS shown inline on Right.
		viewParentSnap1, err := BuildChangeView(dS, repoS2, recS, cS2, "parent", "snap:1")
		if err != nil {
			t.Fatal(err)
		}
		if len(viewParentSnap1.Threads) != 1 || viewParentSnap1.Threads[0].ID != thSnap1R.ID {
			t.Errorf("expected Snapshot 1 thread in parent..snap:1, got %+v", viewParentSnap1.Threads)
		}
		leftBytesP1, _ := repoS2.Content(viewParentSnap1.BaseRev, "fileA.go")
		rightBytesP1, _ := repoS2.Content(viewParentSnap1.TargetRev, "fileA.go")
		_, lMapP1, rMapP1, p1Counts := ReanchorViewThreadsWithLines(diff.SplitLines(leftBytesP1), diff.SplitLines(rightBytesP1), viewParentSnap1.BaseRev, viewParentSnap1.TargetRev, viewParentSnap1.Snapshots, viewParentSnap1.IsLiveTarget, false, []*db.Thread{thSnap1R})
		if len(lMapP1) != 0 || len(rMapP1[4]) != 1 || rMapP1[4][0].DisplaySide() != "R" || p1Counts.Unresolved != 1 {
			t.Errorf("expected thSnap1R on right pane of parent..snap:1 with count=1, got left=%v right=%v counts=%+v", lMapP1, rMapP1, p1Counts)
		}

		// 3. Interdiff Base: Snapshot 1 -> Target: Live (A1): Snapshot 1 R-side comment displays on Left pane!
		viewInterdiff, err := BuildChangeView(dS, repoS2, recS, cS2, "snap:1", "live")
		if err != nil {
			t.Fatal(err)
		}
		if len(viewInterdiff.Threads) != 1 || viewInterdiff.Threads[0].ID != thSnap1R.ID {
			t.Fatalf("expected Snapshot 1 thread in snap:1..live, got %+v", viewInterdiff.Threads)
		}
		leftBytes, _ := repoS2.Content(viewInterdiff.BaseRev, "fileA.go")
		rightBytes, _ := repoS2.Content(viewInterdiff.TargetRev, "fileA.go")
		_, lMapInt, rMapInt, intCounts := ReanchorViewThreadsWithLines(diff.SplitLines(leftBytes), diff.SplitLines(rightBytes), viewInterdiff.BaseRev, viewInterdiff.TargetRev, viewInterdiff.Snapshots, viewInterdiff.IsLiveTarget, true, []*db.Thread{thSnap1R})
		if len(rMapInt) != 0 || len(lMapInt[4]) != 1 || lMapInt[4][0].DisplaySide() != "L" || intCounts.Unresolved != 1 {
			t.Errorf("expected thSnap1R on left pane (EffectiveSide=L) in snap:1..live with count=1, got left=%v right=%v counts=%+v", lMapInt, rMapInt, intCounts)
		}

		// 4. Adding a Side=L comment against Parent while viewing Live (cS2.Rev) must NOT tag Snapshot 1!
		thLeftDraft, _, err := AddCommentRangeWithTarget(dS, repoS2, recS.ID, cS2, cS2.Parent, cS2.Rev, "fileA.go", "L", 1, 1, 0, 0, "alice", "left draft on live", true, false)
		if err != nil {
			t.Fatal(err)
		}
		if thLeftDraft.SnapshotNum != 0 {
			t.Errorf("expected Side=L draft on Parent while viewing Live to get snapshot_num=0, got %d", thLeftDraft.SnapshotNum)
		}

		// 5. PublishChange auto-snapshots unsnapshotted round (cS2 -> Snapshot 2) and publishes draft.
		pubRes, err := PublishChange(dS, repoS2, recS.ID, cS2.Key, cS2, true)
		if err != nil {
			t.Fatalf("PublishChange: %v", err)
		}
		if pubRes.Published != 1 || !pubRes.CreatedSnapshot || pubRes.Snapshot == nil || pubRes.Snapshot.Num != 2 {
			t.Fatalf("expected Published=1 CreatedSnapshot=true Snapshot 2, got %+v", pubRes)
		}
		thLeftReloaded, _ := dS.ThreadByID(thLeftDraft.ID)
		if thLeftReloaded.SnapshotNum != 2 || thLeftReloaded.Messages[0].Draft {
			t.Errorf("expected thLeftDraft promoted to Snapshot 2 and published, got %+v", thLeftReloaded)
		}

		// 6. Adding a draft reply on Snapshot 1 after amending to rev3 must NOT take a snapshot when publishing!
		if err := os.WriteFile(filepath.Join(dirS, "fileA.go"), []byte("package main\n\n// v3 un-snapshotted\nfunc main() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		cmdAmend3 := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
		cmdAmend3.Dir = dirS
		if out, err := cmdAmend3.CombinedOutput(); err != nil {
			t.Fatalf("git commit --amend v3: %s (%v)", string(out), err)
		}
		repoS3, _ := vcs.Open(dirS, nil)
		chS3, _ := repoS3.Changes()
		cS3 := chS3[0]
		if _, err := dS.AddMessage(thSnap1R.ID, "bob", "reply on snap 1", true); err != nil {
			t.Fatal(err)
		}
		pubReplyRes, err := PublishChange(dS, repoS3, recS.ID, cS3.Key, cS3, true)
		if err != nil {
			t.Fatalf("PublishChange reply only: %v", err)
		}
		if pubReplyRes.Published != 1 || pubReplyRes.CreatedSnapshot || pubReplyRes.Snapshot != nil {
			t.Errorf("expected reply on existing snapshot to publish without creating snapshot, got %+v", pubReplyRes)
		}

		// 7. Draft on v3 (snapshot_num=0), amended to v4 with no new drafts -> PublishRepo snapshots v3, NOT v4!
		thV3, _, err := AddComment(dS, repoS3, recS.ID, cS3, cS3.Rev, "fileA.go", "R", 3, 3, "alice", "v3 draft", true, false)
		if err != nil || thV3.SnapshotNum != 0 {
			t.Fatalf("AddComment v3: th=%+v err=%v", thV3, err)
		}
		if err := os.WriteFile(filepath.Join(dirS, "fileA.go"), []byte("package main\n\n// v4 no drafts\nfunc main() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		cmdAmend4 := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
		cmdAmend4.Dir = dirS
		if out, err := cmdAmend4.CombinedOutput(); err != nil {
			t.Fatalf("git commit --amend v4: %s (%v)", string(out), err)
		}
		repoS4, _ := vcs.Open(dirS, nil)
		pubRepoRes, err := PublishRepo(dS, repoS4, recS.ID, true)
		if err != nil {
			t.Fatalf("PublishRepo: %v", err)
		}
		if pubRepoRes.Published != 1 || !pubRepoRes.CreatedSnapshot || pubRepoRes.Snapshot == nil || pubRepoRes.Snapshot.Num != 3 || pubRepoRes.Snapshot.Rev != cS3.Rev {
			t.Fatalf("expected PublishRepo to snapshot v3 as Snapshot 3 without snapshotting v4, got %+v (want rev=%s)", pubRepoRes, cS3.Rev)
		}
		snapsAll, _ := dS.Snapshots(recS.ID, cS3.Key)
		if len(snapsAll) != 3 {
			t.Errorf("expected 3 total snapshots, got %d", len(snapsAll))
		}
	})

	t.Run("ViewScopeAndReanchorEdgeCases", func(t *testing.T) {
		dirE, dE, recE, repoE := setupTestGitRepo(t)
		defer dE.Close()

		changes1, _ := repoE.Changes()
		cE1 := changes1[0]
		snap1, _, err := TakeSnapshot(dE, repoE, recE.ID, cE1, true)
		if err != nil {
			t.Fatalf("TakeSnapshot 1: %v", err)
		}

		// Amend to cE2 -> rev2 un-snapshotted
		if err := os.WriteFile(filepath.Join(dirE, "fileA.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"v2\")\n}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		cmdAmend := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
		cmdAmend.Dir = dirE
		if out, err := cmdAmend.CombinedOutput(); err != nil {
			t.Fatalf("git commit --amend: %s (%v)", string(out), err)
		}
		repoE2, _ := vcs.Open(dirE, nil)
		changes2, _ := repoE2.Changes()
		cE2 := changes2[0]
		snapsE, _ := dE.Snapshots(recE.ID, cE2.Key)

		// 1. ThreadSide when base=snap:1:
		//    - orphan-promoted Side=R thread (Rev != snap1.Rev) shows on Left ("L", true)
		//    - parent-anchored Side=L thread (Rev == snap1.ParentRev) stays hidden ("", false)
		scopeBaseSnap1 := NewViewScope(snapsE, snap1.Rev, cE2.Rev, true)
		orphanR := &db.Thread{ID: 101, SnapshotNum: 1, Side: "R", Rev: "unreachable-rev"}
		if side, ok := scopeBaseSnap1.ThreadSide(orphanR); !ok || side != "L" {
			t.Errorf("expected orphan-promoted R-side thread on Left pane of base=snap:1, got (%q, %v)", side, ok)
		}
		parentL := &db.Thread{ID: 102, SnapshotNum: 1, Side: "L", Rev: snap1.ParentRev}
		if side, ok := scopeBaseSnap1.ThreadSide(parentL); ok {
			t.Errorf("expected parent-anchored L-side thread hidden when base=snap:1, got (%q, %v)", side, ok)
		}

		// 2. ThreadSide when snapshot_num == 0 and raw custom base != snapshot:
		//    - Side=R thread anchored on baseRev shows on Left ("L", true)
		//    - Side=L draft on c.Parent stays hidden when target != live
		scopeRawBase := NewViewScope(snapsE, cE1.Rev, cE2.Rev, false)
		zeroOnRawBaseR := &db.Thread{ID: 103, SnapshotNum: 0, Side: "R", Rev: cE1.Rev}
		if side, ok := scopeRawBase.ThreadSide(zeroOnRawBaseR); !ok || side != "L" {
			t.Errorf("expected snapshot_num=0 R-side thread on custom baseRev -> (\"L\", true), got (%q, %v)", side, ok)
		}
		scopeRawBaseParent := NewViewScope(snapsE, cE2.Parent, cE1.Rev, false)
		zeroOnParentL := &db.Thread{ID: 104, SnapshotNum: 0, Side: "L", Rev: cE2.Parent}
		if side, ok := scopeRawBaseParent.ThreadSide(zeroOnParentL); ok {
			t.Errorf("expected snapshot_num=0 L-side draft on parent hidden when target!=live, got (%q, %v)", side, ok)
		}

		// 3. ReanchorThreadsForView symmetric L -> R flip vs ReanchorThreadsForChange (CLI nil snaps)
		thInterCommitMsg, _, err := AddCommentRangeWithTarget(dE, repoE2, recE.ID, cE2, snap1.Rev, cE2.Rev, vcs.CommitMsgPath, "L", 1, 1, 0, 0, "alice", "left commit msg on snap1", false, false)
		if err != nil {
			t.Fatalf("AddCommentRangeWithTarget snap1 COMMIT_MSG L: %v", err)
		}
		tvView := ReanchorThreadsForView(repoE2, cE2, snapsE, snap1.Rev, cE2.Parent, false, []*db.Thread{thInterCommitMsg}, nil)
		if len(tvView) != 1 || tvView[0].DisplaySide() != "R" || tvView[0].Stale {
			t.Errorf("expected ReanchorThreadsForView(parent..snap:1) to flip L->R and anchor non-stale, got %+v", tvView)
		}
		// CLI (snaps == nil) still uses ReanchorThreadsForChange without flipping L->R against live parent.
		tvCLI := ReanchorThreadsForChange(repoE2, cE2, nil, snap1.Rev, cE2.Parent, []*db.Thread{thInterCommitMsg}, nil)
		if len(tvCLI) != 1 || tvCLI[0].DisplaySide() != "L" || !tvCLI[0].Stale {
			t.Errorf("expected ReanchorThreadsForChange(snaps=nil) to preserve Side=L Stale=true, got %+v", tvCLI)
		}

		// 4. AddCommentRangeWithTarget coerces /COMMIT_MSG Side=L on snap1.ParentRev -> Side=R on snap1.Rev
		//    (even after rebase where snap1.ParentRev != c.Parent), while leaving snap1.Rev -> c.Rev as Side=L.
		rebasedChange := *cE2
		rebasedChange.Parent = "0000000000000000000000000000000000000001"
		thRebasedCommitMsg, _, err := AddCommentRangeWithTarget(dE, repoE2, recE.ID, &rebasedChange, snap1.ParentRev, snap1.Rev, vcs.CommitMsgPath, "L", 1, 1, 0, 0, "bob", "coerced on snap1", true, false)
		if err != nil {
			t.Fatalf("AddCommentRangeWithTarget rebased parent: %v", err)
		}
		if thRebasedCommitMsg.Side != "R" || thRebasedCommitMsg.Rev != snap1.Rev {
			t.Errorf("expected coerced {Side:R, Rev:%s}, got {Side:%s, Rev:%s}", snap1.Rev, thRebasedCommitMsg.Side, thRebasedCommitMsg.Rev)
		}

		// 5. AddCommentRangeWithTarget returns an error on nil *vcs.Change without panicking.
		if _, _, err := AddCommentRangeWithTarget(dE, repoE2, recE.ID, nil, cE2.Rev, cE2.Rev, "fileA.go", "R", 1, 1, 0, 0, "alice", "nil change", true, false); err == nil {
			t.Errorf("expected AddCommentRangeWithTarget(nil change) to return an error")
		}

		// 6. ReanchorThreads and ReanchorViewThreadsWithLines clear Stale=false on clean matches
		//    even if the input *db.Thread had Stale=true from an earlier view.
		staleInput := &db.Thread{
			ID:          201,
			SnapshotNum: 1,
			Side:        "R",
			Rev:         snap1.Rev,
			Path:        "fileA.go",
			StartLine:   1,
			EndLine:     1,
			AnchorLines: "package main",
			Stale:       true,
		}
		staleFileInput := &db.Thread{
			ID:          202,
			SnapshotNum: 1,
			Side:        "R",
			Rev:         snap1.Rev,
			Path:        "fileA.go",
			StartLine:   0,
			Stale:       true,
		}
		tvReset := ReanchorThreads(repoE2, snap1.Rev, snap1.ParentRev, []*db.Thread{staleInput, staleFileInput})
		if len(tvReset) != 2 || tvReset[0].Stale || tvReset[1].Stale {
			t.Errorf("expected ReanchorThreads to clear Stale=false on matched/file-level threads, got %+v", tvReset)
		}
		fileData, _ := repoE2.Content(snap1.Rev, "fileA.go")
		fileLines := diff.SplitLines(fileData)
		fTh, _, rTh, _ := ReanchorViewThreadsWithLines(nil, fileLines, snap1.ParentRev, snap1.Rev, snapsE, false, false, []*db.Thread{staleInput, staleFileInput})
		if len(fTh) != 1 || fTh[0].Stale || len(rTh[1]) != 1 || rTh[1][0].Stale {
			t.Errorf("expected ReanchorViewThreadsWithLines to clear Stale=false, got file=%+v right=%+v", fTh, rTh)
		}
	})

	t.Run("PublishChangeDifferingCommitKeyDoesNotMutateCache", func(t *testing.T) {
		dirP, dP, recP, repoP := setupTestGitRepo(t)
		defer dP.Close()

		changes1, _ := repoP.Changes()
		c1 := changes1[0]

		// Amend commit so c1.Rev is a prior un-snapshotted revision.
		if err := os.WriteFile(filepath.Join(dirP, "fileA.go"), []byte("package main\n\nfunc main() { println(\"v2\") }\n"), 0644); err != nil {
			t.Fatal(err)
		}
		cmdAmend := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
		cmdAmend.Dir = dirP
		if out, err := cmdAmend.CombinedOutput(); err != nil {
			t.Fatalf("git commit --amend: %s (%v)", string(out), err)
		}
		repoP2, _ := vcs.Open(dirP, nil)
		changes2, _ := repoP2.Changes()
		c2 := changes2[0]

		// Wrap repoP2 returning a cached *vcs.Change with a different non-empty Key (e.g. git fallback SHA before Change-Id added).
		cachedPrior := *c1
		cachedPrior.Key = "different-legacy-key"
		wrapped := &commitOverrideRepo{
			Repo: repoP2,
			overrides: map[string]*vcs.Change{
				c1.Rev: &cachedPrior,
			},
		}

		// Create draft under c2.Key anchored on c1.Rev.
		thPrior, _, err := AddComment(dP, repoP2, recP.ID, c2, c1.Rev, "fileA.go", "R", 3, 3, "alice", "draft on v1", true, false)
		if err != nil || thPrior.SnapshotNum != 0 {
			t.Fatalf("AddComment v1: th=%+v err=%v", thPrior, err)
		}

		res, err := PublishChange(dP, wrapped, recP.ID, c2.Key, c2, true)
		if err != nil {
			t.Fatalf("PublishChange: %v", err)
		}
		if !res.CreatedSnapshot || res.Snapshot == nil || res.Snapshot.Rev != c1.Rev || res.Snapshot.ChangeKey != c2.Key {
			t.Fatalf("expected Snapshot created under changeKey=%s rev=%s, got %+v", c2.Key, c1.Rev, res.Snapshot)
		}
		if cachedPrior.Key != "different-legacy-key" {
			t.Errorf("PublishChange mutated cached vcs.Change.Key to %q", cachedPrior.Key)
		}
		snaps, _ := dP.Snapshots(recP.ID, c2.Key)
		if len(snaps) != 1 || snaps[0].Rev != c1.Rev {
			t.Errorf("expected 1 snapshot on %s for %s, got %+v", c2.Key, c1.Rev, snaps)
		}
	})
}

type commitOverrideRepo struct {
	vcs.Repo
	overrides map[string]*vcs.Change
}

func (r *commitOverrideRepo) Commit(rev string) (*vcs.Change, error) {
	if ch, ok := r.overrides[rev]; ok {
		return ch, nil
	}
	return r.Repo.Commit(rev)
}

func TestComputeCommentCounts(t *testing.T) {
	threads := []*db.Thread{
		{
			ID:       1,
			Resolved: false,
			Messages: []*db.Message{
				{ID: 1, Draft: false},
				{ID: 2, Draft: true},
			},
		},
		{
			ID:       2,
			Resolved: true,
			Messages: []*db.Message{
				{ID: 3, Draft: false},
			},
		},
		{
			ID:       3,
			Resolved: false,
			Messages: []*db.Message{
				{ID: 4, Draft: true},
				{ID: 5, Draft: true},
			},
		},
	}
	counts := ComputeCommentCounts(threads)
	if counts.Unresolved != 2 {
		t.Errorf("expected 2 unresolved, got %d", counts.Unresolved)
	}
	if counts.Resolved != 1 {
		t.Errorf("expected 1 resolved, got %d", counts.Resolved)
	}
	if counts.Draft != 3 {
		t.Errorf("expected 3 draft messages, got %d", counts.Draft)
	}
}

func TestBuildRelationChainExcludesEmptyCommits(t *testing.T) {
	// Chain: c3 -> c2 (empty) -> c1 (base)
	c1 := &vcs.Change{
		Key:     "c1",
		Rev:     "rev1",
		Subject: "commit 1",
		Empty:   false,
		Working: false,
	}
	c2 := &vcs.Change{
		Key:     "c2",
		Rev:     "rev2",
		Parent:  "rev1",
		Subject: "WIP: empty commit",
		Empty:   true,
		Working: false,
	}
	c3 := &vcs.Change{
		Key:     "c3",
		Rev:     "rev3",
		Parent:  "rev2",
		Subject: "commit 3",
		Empty:   false,
		Working: false,
	}

	changes := []*vcs.Change{c3, c2, c1}

	// When viewing c3, empty commit c2 must be excluded from relation chain,
	// and c3 should connect to c1!
	chain := BuildRelationChain(changes, "c3", nil)
	foundC2 := false
	for _, row := range chain {
		if row.Change.Key == "c2" {
			foundC2 = true
		}
	}
	if foundC2 {
		t.Errorf("empty commit c2 should be excluded from relation chain when viewing c3")
	}
	if len(chain) != 2 {
		t.Errorf("expected 2 rows in chain (c3 and c1), got %d", len(chain))
	}

	// When explicitly viewing c2 itself, it should be visible
	chain2 := BuildRelationChain(changes, "c2", nil)
	foundC2WhenCurrent := false
	for _, row := range chain2 {
		if row.Change.Key == "c2" {
			foundC2WhenCurrent = true
		}
	}
	if !foundC2WhenCurrent {
		t.Errorf("empty commit c2 should be visible when it is currentKey")
	}
}

func TestBuildChangeViewCarryForwardReviewed(t *testing.T) {
	dir, d, rec, _ := setupTestGitRepo(t)
	defer d.Close()

	fileB := filepath.Join(dir, "fileB.go")
	if err := os.WriteFile(fileB, []byte("package main\n\nfunc helper() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "fileB.go")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "commit", "--amend", "--no-edit")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	repoFresh, _ := vcs.Open(dir, nil)
	changes, err := repoFresh.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes: %v", err)
	}
	c := changes[0]

	// Take Snapshot 1
	snap1, _, err := d.TakeSnapshot(rec.ID, c)
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	// Mark fileA.go and /COMMIT_MSG as reviewed in Snapshot 1
	if err := d.SetFileReviewed(rec.ID, c.Key, snap1.Rev, "fileA.go", true); err != nil {
		t.Fatalf("SetFileReviewed: %v", err)
	}
	if err := d.SetFileReviewed(rec.ID, c.Key, snap1.Rev, "/COMMIT_MSG", true); err != nil {
		t.Fatalf("SetFileReviewed commit msg: %v", err)
	}

	// Amend commit: modify fileB.go, leave fileA.go and commit message unchanged
	if err := os.WriteFile(fileB, []byte("package main\n\nfunc helper() { /* modified */ }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "commit", "-a", "--amend", "--no-edit")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	repoLive, _ := vcs.Open(dir, nil)
	changesLive, _ := repoLive.Changes()
	cLive := changesLive[0]

	// BuildChangeView against parent on live snapshot
	view, err := BuildChangeView(d, repoLive, rec, cLive, "parent", "live")
	if err != nil {
		t.Fatalf("BuildChangeView: %v", err)
	}

	if len(view.Files) == 0 || view.Files[0].Path != vcs.CommitMsgPath {
		t.Fatalf("expected %s to be the first file in view.Files, got %+v", vcs.CommitMsgPath, view.Files)
	}
	if !view.Files[0].Reviewed {
		t.Errorf("expected view.Files[0] (%s) Reviewed to carry forward as true", vcs.CommitMsgPath)
	}

	viewBetweenSnaps, err := BuildChangeView(d, repoLive, rec, cLive, snap1.Rev, "live")
	if err != nil {
		t.Fatalf("BuildChangeView between snaps: %v", err)
	}
	for _, f := range viewBetweenSnaps.Files {
		if f.Path == vcs.CommitMsgPath {
			t.Errorf("did not expect %s when only code changed between snapshots", vcs.CommitMsgPath)
		}
	}

	var fileAReviewed, fileBReviewed bool
	for _, f := range view.Files {
		if f.Path == "fileA.go" {
			fileAReviewed = f.Reviewed
		}
		if f.Path == "fileB.go" {
			fileBReviewed = f.Reviewed
		}
	}

	if !fileAReviewed {
		t.Errorf("expected unchanged fileA.go Reviewed to carry forward as true")
	}
	if fileBReviewed {
		t.Errorf("expected modified fileB.go Reviewed to be false")
	}

	// Now explicitly unmark fileA.go on live snapshot
	if err := d.SetFileReviewed(rec.ID, cLive.Key, cLive.Rev, "fileA.go", false); err != nil {
		t.Fatalf("SetFileReviewed: %v", err)
	}

	viewUnmarked, err := BuildChangeView(d, repoLive, rec, cLive, "parent", "live")
	if err != nil {
		t.Fatalf("BuildChangeView: %v", err)
	}
	for _, f := range viewUnmarked.Files {
		if f.Path == "fileA.go" && f.Reviewed {
			t.Errorf("expected explicitly unmarked fileA.go to have Reviewed == false")
		}
	}

	// Test BuildChangeViewForFile fast path
	viewFileA, err := BuildChangeViewForFile(d, repoLive, rec, cLive, "parent", "live", "fileA.go")
	if err != nil {
		t.Fatalf("BuildChangeViewForFile: %v", err)
	}
	if len(viewFileA.Files) != 1 || viewFileA.Files[0].Path != "fileA.go" {
		t.Errorf("expected exactly 1 file (fileA.go), got %+v", viewFileA.Files)
	}
	if len(viewFileA.RelationChain) != 0 {
		t.Errorf("expected relation chain to be empty in BuildChangeViewForFile")
	}
}

func TestBlobOIDCarryForwardWithDeletedFile(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	// Commit 0 (parent)
	if err := os.WriteFile(filepath.Join(dir, "del.go"), []byte("package main\n// delete me\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package main\n// keep me\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	// Commit 1: delete del.go, modify keep.go
	_ = os.Remove(filepath.Join(dir, "del.go"))
	runGit("rm", "del.go")
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package main\n// keep me v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "keep.go")
	runGit("commit", "-m", "delete del.go and update keep\n\nChange-Id: Iabcdef1234567890abcdef1234567890abcdef12")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, _ := d.EnsureRepo(dir)
	repo, _ := vcs.Open(dir, nil)
	changes, _ := repo.Changes()
	c := changes[0]

	// Take snapshot 1
	snap1, _, err := d.TakeSnapshot(rec.ID, c)
	if err != nil {
		t.Fatal(err)
	}

	// Mark del.go as reviewed in snap 1
	if err := d.SetFileReviewed(rec.ID, c.Key, snap1.Rev, "del.go", true); err != nil {
		t.Fatal(err)
	}

	// Commit 2: amend commit to change keep.go again, leaving del.go deleted
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package main\n// keep me v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "keep.go")
	runGit("commit", "--amend", "-m", "delete del.go and update keep amended\n\nChange-Id: Iabcdef1234567890abcdef1234567890abcdef12")

	repoLive, _ := vcs.Open(dir, nil)
	changesLive, _ := repoLive.Changes()
	cLive := changesLive[0]

	view, err := BuildChangeView(d, repoLive, rec, cLive, "parent", "live")
	if err != nil {
		t.Fatalf("BuildChangeView: %v", err)
	}

	var delReviewed bool
	for _, f := range view.Files {
		if f.Path == "del.go" {
			delReviewed = f.Reviewed
		}
	}
	if !delReviewed {
		t.Errorf("expected deleted file del.go Reviewed to carry forward as true via BlobOID")
	}
}

type contentCountingRepo struct {
	vcs.Repo
	contentCount int
}

func (r *contentCountingRepo) Content(rev, path string) ([]byte, error) {
	r.contentCount++
	return r.Repo.Content(rev, path)
}

func TestGetThreadContextCached(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-b", "main")
	runGit("config", "user.name", "Tester")
	runGit("config", "user.email", "tester@example.com")

	content := "package main\n\nfunc A() {\n\t// line 4\n\t// line 5\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.go")
	runGit("commit", "-m", "first commit")
	rev := runGit("rev-parse", "HEAD")

	baseRepo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	countingRepo := &contentCountingRepo{Repo: baseRepo}
	linesCache := make(map[string][]string)

	tv1 := &ThreadView{
		Thread: &db.Thread{
			Rev:       rev,
			Path:      "main.go",
			StartLine: 3,
			EndLine:   4,
		},
	}
	tv2 := &ThreadView{
		Thread: &db.Thread{
			Rev:       rev,
			Path:      "main.go",
			StartLine: 4,
			EndLine:   5,
		},
	}

	ctx1 := GetThreadContextCached(countingRepo, tv1, 1, linesCache)
	if !strings.Contains(ctx1, "func A()") {
		t.Fatalf("unexpected ctx1: %q", ctx1)
	}
	if countingRepo.contentCount != 1 {
		t.Fatalf("expected contentCount == 1 after first call, got %d", countingRepo.contentCount)
	}

	ctx2 := GetThreadContextCached(countingRepo, tv2, 1, linesCache)
	if !strings.Contains(ctx2, "line 4") {
		t.Fatalf("unexpected ctx2: %q", ctx2)
	}
	if countingRepo.contentCount != 1 {
		t.Fatalf("expected contentCount to remain 1 after second call with cache, got %d", countingRepo.contentCount)
	}

	// Missing file is also cached as negative entry
	tvMissing := &ThreadView{
		Thread: &db.Thread{
			Rev:       rev,
			Path:      "nonexistent.go",
			StartLine: 1,
			EndLine:   1,
		},
	}
	_ = GetThreadContextCached(countingRepo, tvMissing, 1, linesCache)
	countAfterMiss := countingRepo.contentCount
	if countAfterMiss != 2 {
		t.Fatalf("expected contentCount == 2 after missing file lookup, got %d", countAfterMiss)
	}
	_ = GetThreadContextCached(countingRepo, tvMissing, 1, linesCache)
	if countingRepo.contentCount != countAfterMiss {
		t.Fatalf("expected negative cache hit for missing file, Content was called again: %d", countingRepo.contentCount)
	}
}

func TestMatchAnchorLinesNearest(t *testing.T) {
	// Construct lines where "}" appears at lines 10, 42, and 75.
	// Original comment was at line 40, and 2 lines were inserted above it, moving it to line 42.
	// Radial search must match line 42 (delta = +2), not line 10 (delta = -30).
	var lines []string
	for i := 1; i <= 80; i++ {
		switch i {
		case 10, 42, 75:
			lines = append(lines, "}")
		case 20, 41:
			lines = append(lines, "\treturn nil")
		default:
			lines = append(lines, fmt.Sprintf("\tx := %d", i))
		}
	}

	// 1. Shift forward (lines inserted above)
	newStart, newEnd, matched := matchAnchorLines(lines, 40, "}")
	if !matched {
		t.Fatalf("expected matched true for shifted '}'")
	}
	if newStart != 42 || newEnd != 42 {
		t.Errorf("expected match at nearest shifted line 42, got [%d, %d]", newStart, newEnd)
	}

	// 2. Shift backward (lines deleted above)
	// Original comment was at line 44, line deleted above moved it to line 42.
	newStart, newEnd, matched = matchAnchorLines(lines, 44, "}")
	if !matched {
		t.Fatalf("expected matched true for backward-shifted '}'")
	}
	if newStart != 42 || newEnd != 42 {
		t.Errorf("expected match at nearest shifted line 42, got [%d, %d]", newStart, newEnd)
	}

	// 3. Exact match
	newStart, newEnd, matched = matchAnchorLines(lines, 42, "}")
	if !matched || newStart != 42 || newEnd != 42 {
		t.Errorf("expected exact match at line 42, got [%d, %d] (matched=%v)", newStart, newEnd, matched)
	}

	// 4. Multi-line anchor matching shifted block
	multiAnchor := "\tx := 39\n\tx := 40"
	newStart, newEnd, matched = matchAnchorLines(lines, 37, multiAnchor)
	if !matched || newStart != 39 || newEnd != 40 {
		t.Errorf("expected multi-line anchor to match [39, 40], got [%d, %d] (matched=%v)", newStart, newEnd, matched)
	}
}

type searchTestRepo struct {
	vcs.Repo
	files   []*vcs.File
	content map[string][]byte
}

func (m *searchTestRepo) Files(from, to string) ([]*vcs.File, error) {
	return m.files, nil
}

func (m *searchTestRepo) Content(rev, path string) ([]byte, error) {
	if data, ok := m.content[rev+":"+path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("file not found: %s@%s", path, rev)
}

func TestSearchChangeCommitMsg(t *testing.T) {
	c := &vcs.Change{
		Key:     "c1",
		Rev:     "rev1",
		Parent:  "parent1",
		Subject: "Fix parser bug",
		Message: "Fix parser bug\n\nThis commit resolves issue #42 in the lexer.",
	}
	mockRepo := &searchTestRepo{
		files: []*vcs.File{
			{Path: "lexer.go", Status: vcs.FileModified},
		},
		content: map[string][]byte{
			"rev1:lexer.go": []byte("package lexer\n\nfunc Scan() {}\n"),
		},
	}

	// 1. Search matching commit message body
	results, err := SearchChange(mockRepo, c, "rev1", "parent1", "resolves issue #42", true)
	if err != nil {
		t.Fatalf("SearchChange failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected at least 1 result for commit message query")
	}
	if results[0].Path != vcs.CommitMsgPath {
		t.Errorf("expected first result to be %s, got %s", vcs.CommitMsgPath, results[0].Path)
	}
	// FormatCommitMsg prepends headers + blank line:
	// 1: Parent:     parent1
	// 2: Change-Id:  c1
	// 3:
	// 4: Fix parser bug
	// 5:
	// 6: This commit resolves issue #42 in the lexer.
	if len(results[0].Matches) != 1 || results[0].Matches[0].Line != 6 {
		t.Errorf("expected match on line 6, got: %+v", results[0].Matches)
	}

	// 2. Search matching formatted Change-Id header
	idResults, err := SearchChange(mockRepo, c, "rev1", "parent1", "Change-Id", true)
	if err != nil {
		t.Fatalf("SearchChange Change-Id failed: %v", err)
	}
	if len(idResults) != 1 || idResults[0].Path != vcs.CommitMsgPath || len(idResults[0].Matches) != 1 || idResults[0].Matches[0].Line != 2 {
		t.Errorf("expected Change-Id match on line 2 of %s, got: %+v", vcs.CommitMsgPath, idResults)
	}

	// 3. Search matching both commit message and file
	results, err = SearchChange(mockRepo, c, "rev1", "parent1", "lexer", true)
	if err != nil {
		t.Fatalf("SearchChange failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (commit msg and lexer.go), got %d: %+v", len(results), results)
	}
	if results[0].Path != vcs.CommitMsgPath || results[1].Path != "lexer.go" {
		t.Errorf("unexpected result paths: %v, %v", results[0].Path, results[1].Path)
	}
}

func TestCommitMsgLeftSideReanchorAndAddComment(t *testing.T) {
	dir, d, rec, repo := setupTestGitRepo(t)
	defer d.Close()

	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("no changes found: %v", err)
	}
	c1 := changes[0]

	snap1, _, err := d.TakeSnapshot(rec.ID, c1)
	if err != nil {
		t.Fatalf("TakeSnapshot 1: %v", err)
	}

	// Comment on line 1 (Author: ...) on Side L anchored to Snapshot 1.
	// Parent commit has the exact same Author header on line 1, so reanchoring
	// against base=parent must NOT falsely match c.Parent.
	thLeft, _, err := AddComment(d, repo, rec.ID, c1, snap1.Rev, vcs.CommitMsgPath, "L", 1, 1, "alice", "check author", false, false)
	if err != nil {
		t.Fatalf("AddComment Side L on snap1: %v", err)
	}
	if thLeft.Side != "L" || thLeft.Rev != snap1.Rev || thLeft.AnchorLines == "" {
		t.Fatalf("unexpected thLeft: %+v", thLeft)
	}

	cmd := exec.Command("git", "commit", "--amend", "-m", "Amended header\n\nChange-Id: I1234567890abcdef1234567890abcdef12345678")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend failed: %s (%v)", string(out), err)
	}

	repo2, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes2, err := repo2.Changes()
	if err != nil || len(changes2) == 0 {
		t.Fatalf("no changes after amend: %v", err)
	}
	c2 := changes2[0]
	snap2, _, err := d.TakeSnapshot(rec.ID, c2)
	if err != nil {
		t.Fatalf("TakeSnapshot 2: %v", err)
	}
	snaps, _ := d.Snapshots(rec.ID, c2.Key)

	if CommitMsgHasBase(repo2, c2, snaps, "", c2.Rev) {
		t.Errorf("CommitMsgHasBase with empty leftRev = true, want false")
	}
	if CommitMsgHasBase(repo2, c2, snaps, "00000000", c2.Rev) {
		t.Errorf("CommitMsgHasBase with all-zeros leftRev = true, want false")
	}
	if CommitMsgHasBase(repo2, c2, snaps, c2.Parent, c2.Rev) {
		t.Errorf("CommitMsgHasBase with c2.Parent = true, want false")
	}
	if !CommitMsgHasBase(repo2, c2, snaps, snap1.Rev, snap2.Rev) {
		t.Errorf("CommitMsgHasBase with snap1.Rev = false, want true")
	}

	// Comparing Snapshot 1 vs Snapshot 2: Side L anchors cleanly on snap1.Rev.
	viewSnaps, err := BuildChangeView(d, repo2, rec, c2, "snap:1", "snap:2")
	if err != nil {
		t.Fatalf("BuildChangeView snap:1..snap:2: %v", err)
	}
	if len(viewSnaps.Files) == 0 || viewSnaps.Files[0].Path != vcs.CommitMsgPath || len(viewSnaps.Files[0].Threads) != 1 {
		t.Fatalf("expected /COMMIT_MSG thread in snap:1..snap:2, got %+v", viewSnaps.Files)
	}
	if viewSnaps.Files[0].Threads[0].Stale {
		t.Errorf("expected /COMMIT_MSG Side L thread not stale when comparing snap:1 vs snap:2")
	}

	// Comparing Parent vs Live (when c2 is Snapshot 2): Snapshot 1 Side L thread is NOT shown inline,
	// though ChangeCounts retains it across all snapshots, and parent..snap:1 shows it inline.
	viewParent, err := BuildChangeView(d, repo2, rec, c2, "parent", "live")
	if err != nil {
		t.Fatalf("BuildChangeView parent..live: %v", err)
	}
	if len(viewParent.Files) == 0 || viewParent.Files[0].Path != vcs.CommitMsgPath || len(viewParent.Files[0].Threads) != 0 {
		t.Fatalf("expected Snapshot 1 /COMMIT_MSG thread hidden inline in parent..live, got %+v", viewParent.Files)
	}
	if viewParent.ChangeCounts.Unresolved != 1 {
		t.Errorf("expected ChangeCounts.Unresolved == 1 across snapshots, got %+v", viewParent.ChangeCounts)
	}

	viewParentSnap1, err := BuildChangeView(d, repo2, rec, c2, "parent", "snap:1")
	if err != nil {
		t.Fatalf("BuildChangeView parent..snap:1: %v", err)
	}
	if len(viewParentSnap1.Files) == 0 || viewParentSnap1.Files[0].Path != vcs.CommitMsgPath || len(viewParentSnap1.Files[0].Threads) != 1 {
		t.Fatalf("expected /COMMIT_MSG thread in parent..snap:1, got %+v", viewParentSnap1.Files)
	}
	if viewParentSnap1.Files[0].Threads[0].Stale {
		t.Errorf("expected /COMMIT_MSG thread not stale when comparing parent vs snap:1")
	}

	tvsParent := ReanchorThreads(repo2, c2.Rev, c2.Parent, []*db.Thread{thLeft})
	if len(tvsParent) != 1 || !tvsParent[0].Stale {
		t.Errorf("expected ReanchorThreads(parent) to mark Side L /COMMIT_MSG stale, got %+v", tvsParent)
	}

	// Adding a Side L comment with empty rev or c2.Parent must canonicalize to Side R on c2.Rev.
	thEmptyRev, _, err := AddComment(d, repo2, rec.ID, c2, "", "COMMIT_MSG", "L", 1, 1, "bob", "no parent side", false, false)
	if err != nil {
		t.Fatalf("AddComment empty rev Side L: %v", err)
	}
	if thEmptyRev.Side != "R" || thEmptyRev.Rev != c2.Rev || thEmptyRev.Path != vcs.CommitMsgPath {
		t.Errorf("expected normalized {Side:R, Rev:%s, Path:%s}, got {Side:%s, Rev:%s, Path:%s}",
			c2.Rev, vcs.CommitMsgPath, thEmptyRev.Side, thEmptyRev.Rev, thEmptyRev.Path)
	}

	thParentRev, _, err := AddComment(d, repo2, rec.ID, c2, c2.Parent, vcs.CommitMsgPath, "L", 1, 1, "bob", "parent rev side", false, false)
	if err != nil {
		t.Fatalf("AddComment c.Parent rev Side L: %v", err)
	}
	if thParentRev.Side != "R" || thParentRev.Rev != c2.Rev || thParentRev.Path != vcs.CommitMsgPath {
		t.Errorf("expected parent-rev Side L normalized to {Side:R, Rev:%s, Path:%s}, got {Side:%s, Rev:%s, Path:%s}",
			c2.Rev, vcs.CommitMsgPath, thParentRev.Side, thParentRev.Rev, thParentRev.Path)
	}
}

func FuzzMatchAnchorLines(f *testing.F) {
	f.Add("line 1\nline 2\nline 3\nline 4\n", 2, "line 2")
	f.Add("a\nb\nc\nd\ne\nf\ng\n", 4, "d\ne")
	f.Add("func main() {\n\treturn\n}\n", 1, "func main() {")
	f.Add("", 0, "")
	f.Add("single line", -1, "single line")
	f.Add("repeating\nrepeating\nrepeating\n", 2, "repeating")

	f.Fuzz(func(t *testing.T, doc string, startLine int, anchor string) {
		lines := diff.SplitLines([]byte(doc))
		newStart, newEnd, matched := matchAnchorLines(lines, startLine, anchor)
		if !matched {
			return
		}
		if len(lines) == 0 || anchor == "" {
			t.Fatalf("matched true for empty lines or anchor")
		}
		if newStart < 1 || newEnd > len(lines) || newStart > newEnd {
			t.Fatalf("invalid matched range: [%d, %d] in len(lines)=%d", newStart, newEnd, len(lines))
		}
		gotAnchor := strings.Join(lines[newStart-1:newEnd], "\n")
		if gotAnchor != anchor {
			t.Fatalf("matched range text mismatch:\ngot:  %q\nwant: %q", gotAnchor, anchor)
		}
	})
}
