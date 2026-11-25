package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func makeNumberedLines(n int) []string {
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		lines[i] = fmt.Sprintf("// line %02d", i+1)
	}
	return lines
}

func joinLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func setupGitStackRepo(t *testing.T) (dir string, d *db.DB, rec *db.RepoRecord, runGit func(args ...string) string) {
	t.Helper()
	dir = t.TempDir()
	runGit = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "Stack Tester")
	runGit("config", "user.email", "stack@example.com")

	baseLines := makeNumberedLines(60)
	if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(baseLines), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oldname.txt"), []byte("rename me\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "shared.go", "other.go", "oldname.txt")
	runGit("commit", "-m", "Base commit")

	dbPath := filepath.Join(t.TempDir(), "review.db")
	var err error
	d, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rec, err = d.EnsureRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, d, rec, runGit
}

func TestRevisionsHaveNoOwnDiffAndRebaseCarryForward(t *testing.T) {
	t.Run("DisjointFilesAndSameFileDisjointHunks", func(t *testing.T) {
		dir, d, rec, runGit := setupGitStackRepo(t)
		defer d.Close()

		const keyC1 = "I1111111111111111111111111111111111111111"
		const keyC2 = "I2222222222222222222222222222222222222222"

		// C1 modifies other.go
		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C1: update other\n\nChange-Id: "+keyC1)
		revC1v1 := runGit("rev-parse", "HEAD")

		// C2 modifies lines 50..52 of shared.go and adds c2only.go
		c2Lines := makeNumberedLines(60)
		c2Lines[49] = "// c2 edited 50"
		c2Lines[51] = "// c2 edited 52"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2Lines), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "c2only.go"), []byte("package main\nfunc c2Only() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "shared.go", "c2only.go")
		runGit("commit", "-m", "C2: edit bottom of shared\n\nBody line for C2.\n\nChange-Id: "+keyC2)

		repo1, err := vcs.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		c2v1, err := repo1.Commit("HEAD")
		if err != nil {
			t.Fatal(err)
		}

		snap1, created, err := TakeSnapshot(d, repo1, rec.ID, c2v1, true)
		if err != nil || !created || snap1.Num != 1 {
			t.Fatalf("TakeSnapshot C2 v1 failed: created=%v snap=%+v err=%v", created, snap1, err)
		}

		// Add comments on C2 Snapshot 1:
		// 1) shared.go:52 Side R (inside C2's hunk)
		thSharedR, _, err := AddComment(d, repo1, rec.ID, c2v1, c2v1.Rev, "shared.go", "R", 52, 52, "alice", "check shared line 52", false, false)
		if err != nil {
			t.Fatal(err)
		}
		// 2) shared.go:50 Side L (parent-anchored)
		thSharedL, _, err := AddComment(d, repo1, rec.ID, c2v1, c2v1.Parent, "shared.go", "L", 50, 50, "alice", "old parent line 50", false, false)
		if err != nil {
			t.Fatal(err)
		}
		// 3) /COMMIT_MSG body line ("Body line for C2.")
		msgBytes, err := repo1.Content(c2v1.Rev, vcs.CommitMsgPath)
		if err != nil {
			t.Fatal(err)
		}
		msgLines := diff.SplitLines(msgBytes)
		bodyLine := 0
		for i, l := range msgLines {
			if l == "Body line for C2." {
				bodyLine = i + 1
				break
			}
		}
		if bodyLine == 0 {
			t.Fatalf("could not find body line in /COMMIT_MSG:\n%s", string(msgBytes))
		}
		thMsgBody, _, err := AddComment(d, repo1, rec.ID, c2v1, c2v1.Rev, vcs.CommitMsgPath, "R", bodyLine, bodyLine, "alice", "msg body note", false, false)
		if err != nil {
			t.Fatal(err)
		}
		// 4) /COMMIT_MSG line 1 (Parent: <sha> ...) which will fail to match after parent SHA changes on rebase
		thMsgParentHdr, _, err := AddComment(d, repo1, rec.ID, c2v1, c2v1.Rev, vcs.CommitMsgPath, "R", 1, 1, "alice", "parent header note", false, false)
		if err != nil {
			t.Fatal(err)
		}

		// Mark c2only.go reviewed and C2 LGTM
		if err := d.SetFileReviewed(rec.ID, c2v1.Key, c2v1.Rev, "c2only.go", true); err != nil {
			t.Fatal(err)
		}
		tTrue := true
		if err := d.SetRevMark(rec.ID, c2v1.Key, c2v1.Rev, &tTrue, &tTrue); err != nil {
			t.Fatal(err)
		}

		// Rebase 1: Amend C1 touching only a disjoint file (other.go) and rebase C2 onto C1'
		runGit("checkout", revC1v1)
		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 3\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "-m", "C1: update other v2\n\nChange-Id: "+keyC1)
		revC1v2 := runGit("rev-parse", "HEAD")

		// Re-apply C2 onto C1' using cherry-pick
		runGit("cherry-pick", c2v1.Rev)
		repo2, err := vcs.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		c2v2, err := repo2.Commit("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if c2v2.Parent != revC1v2 {
			t.Fatalf("expected C2 v2 parent %s, got %s", revC1v2, c2v2.Parent)
		}
		if !RevisionsHaveNoOwnDiff(repo2, snap1.Rev, snap1.ParentRev, c2v2) {
			t.Fatalf("expected RevisionsHaveNoOwnDiff true after disjoint-file upstream amend")
		}

		// TakeSnapshot on C2 v2 carries forward Snapshot 1 without creating Snapshot 2
		snapCarry1, created2, err := TakeSnapshot(d, repo2, rec.ID, c2v2, true)
		if err != nil {
			t.Fatal(err)
		}
		if created2 || snapCarry1.Num != 1 || snapCarry1.Rev != c2v2.Rev {
			t.Fatalf("expected carry-forward created=false num=1 rev=%s, got created=%v snap=%+v", c2v2.Rev, created2, snapCarry1)
		}
		snapsAfter1, _ := d.Snapshots(rec.ID, c2v2.Key)
		if len(snapsAfter1) != 1 {
			t.Fatalf("expected SnapshotCount == 1, got %d", len(snapsAfter1))
		}
		if marks, _ := d.FileMarks(rec.ID, c2v2.Key, c2v2.Rev); !marks["c2only.go"] {
			t.Errorf("expected file_marks carried forward to %s, got %+v", c2v2.Rev, marks)
		}
		if revd, lgtm, _ := d.RevMark(rec.ID, c2v2.Key, c2v2.Rev); !revd || !lgtm {
			t.Errorf("expected rev_marks (reviewed=true, lgtm=true) carried forward, got (%v, %v)", revd, lgtm)
		}

		// Rebase 2: Amend C1 modifying lines 1..5 AND inserting 40 lines at top of shared.go!
		// C2 still edits lines 50..52 (now shifted to lines 90..92 -> +40 > ±30 radial window).
		runGit("checkout", revC1v2)
		c1Lines := make([]string, 0, 100)
		for i := 0; i < 40; i++ {
			c1Lines = append(c1Lines, fmt.Sprintf("// upstream insert %02d", i+1))
		}
		baseForC1 := makeNumberedLines(60)
		baseForC1[0] = "// c1 edited 01"
		baseForC1[4] = "// c1 edited 05"
		c1Lines = append(c1Lines, baseForC1...)
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c1Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "-m", "C1: update shared top\n\nChange-Id: "+keyC1)
		revC1v3 := runGit("rev-parse", "HEAD")

		// Re-apply C2's own bottom edits onto C1''
		c2v3Lines := append([]string(nil), c1Lines...)
		c2v3Lines[40+49] = "// c2 edited 50"
		c2v3Lines[40+51] = "// c2 edited 52"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2v3Lines), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "c2only.go"), []byte("package main\nfunc c2Only() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "shared.go", "c2only.go")
		runGit("commit", "-m", "C2: edit bottom of shared\n\nBody line for C2.\n\nChange-Id: "+keyC2)

		repo3, err := vcs.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		c2v3, err := repo3.Commit("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if c2v3.Parent != revC1v3 {
			t.Fatalf("expected C2 v3 parent %s, got %s", revC1v3, c2v3.Parent)
		}
		if !RevisionsHaveNoOwnDiff(repo3, snapCarry1.Rev, snapCarry1.ParentRev, c2v3) {
			t.Fatalf("expected RevisionsHaveNoOwnDiff true when C1 edited top of shared.go and C2 edited bottom")
		}

		// WITHOUT running TakeSnapshot on C2 v3, BuildChangeView must auto-sync Snapshot 1!
		view, err := BuildChangeView(d, repo3, rec, c2v3, "parent", "live")
		if err != nil {
			t.Fatalf("BuildChangeView failed: %v", err)
		}
		if view.SnapshotPending || view.TargetDesc != "Snapshot 1" {
			t.Fatalf("expected SnapshotPending=false TargetDesc='Snapshot 1', got pending=%v desc=%q", view.SnapshotPending, view.TargetDesc)
		}
		if len(view.Snapshots) != 1 || view.Snapshots[0].Rev != c2v3.Rev || view.Snapshots[0].ParentRev != c2v3.Parent {
			t.Fatalf("expected synced Snapshot 1 pointing to %s, got %+v", c2v3.Rev, view.Snapshots)
		}

		// Verify DB re-anchoring >30 lines + pin updates + unmatched parent header pin
		reloadedR, _ := d.ThreadByID(thSharedR.ID)
		if reloadedR.Rev != c2v3.Rev || reloadedR.StartLine != 92 || reloadedR.EndLine != 92 {
			t.Errorf("expected thSharedR shifted to line 92 on %s, got rev=%s lines=%d-%d", c2v3.Rev, reloadedR.Rev, reloadedR.StartLine, reloadedR.EndLine)
		}
		reloadedL, _ := d.ThreadByID(thSharedL.ID)
		if reloadedL.Rev != c2v3.Parent || reloadedL.StartLine != 90 || reloadedL.EndLine != 90 {
			t.Errorf("expected thSharedL shifted to line 90 on parent %s, got rev=%s lines=%d-%d", c2v3.Parent, reloadedL.Rev, reloadedL.StartLine, reloadedL.EndLine)
		}
		reloadedMsgBody, _ := d.ThreadByID(thMsgBody.ID)
		if reloadedMsgBody.Rev != c2v3.Rev {
			t.Errorf("expected thMsgBody re-anchored to %s, got %s", c2v3.Rev, reloadedMsgBody.Rev)
		}
		reloadedHdr, _ := d.ThreadByID(thMsgParentHdr.ID)
		if reloadedHdr.Rev != c2v1.Rev {
			t.Errorf("expected unmatched thMsgParentHdr to retain original rev %s, got %s", c2v1.Rev, reloadedHdr.Rev)
		}

		// Verify view.Threads has 4 visible threads, with shared/body non-stale and parent-header stale
		if len(view.Threads) != 4 {
			t.Fatalf("expected 4 visible threads in synced live view, got %d", len(view.Threads))
		}
		for _, vt := range view.Threads {
			if vt.ID == thMsgParentHdr.ID && !vt.Stale {
				t.Errorf("expected unmatched parent header thread #%d to be Stale=true", vt.ID)
			}
			if (vt.ID == thSharedR.ID || vt.ID == thSharedL.ID || vt.ID == thMsgBody.ID) && vt.Stale {
				t.Errorf("expected matched rebased thread #%d to remain non-stale", vt.ID)
			}
		}

		// Verify pins updated for snap-1 and unmatched rev-<short>
		pins, _ := repo3.ListPins()
		wantRevPin := fmt.Sprintf("refs/reviewed/%s/rev-%s", c2v1.Key, shortHash(c2v1.Rev))
		var hasSnapPin, hasRevPin bool
		for _, p := range pins {
			if p == fmt.Sprintf("refs/reviewed/%s/snap-1", c2v1.Key) {
				hasSnapPin = true
			}
			if p == wantRevPin {
				hasRevPin = true
			}
		}
		if !hasSnapPin || !hasRevPin {
			t.Errorf("expected snap-1 and %s pins, got %v", wantRevPin, pins)
		}
		if marks, _ := d.FileMarks(rec.ID, c2v3.Key, c2v3.Rev); !marks["c2only.go"] {
			t.Errorf("expected file_marks carried forward after live view sync")
		}
		if revd, lgtm, _ := d.RevMark(rec.ID, c2v3.Key, c2v3.Rev); !revd || !lgtm {
			t.Errorf("expected rev_marks carried forward after live view sync")
		}
	})

	t.Run("OwnEditsDetected", func(t *testing.T) {
		dir, d, rec, runGit := setupGitStackRepo(t)
		defer d.Close()

		const keyC1 = "Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		const keyC2 = "Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C1\n\nChange-Id: "+keyC1)
		revC1 := runGit("rev-parse", "HEAD")

		// C2 deletes line 10 and modifies line 20 in shared.go
		c2Base := makeNumberedLines(60)
		c2V1Lines := append([]string(nil), c2Base[:9]...)
		c2V1Lines = append(c2V1Lines, c2Base[10:]...) // deleted line 10
		c2V1Lines[18] = "// c2 modified 20"           // index 18 was line 20
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2V1Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C2 initial\n\nChange-Id: "+keyC2)

		repo1, _ := vcs.Open(dir, nil)
		c2v1, _ := repo1.Commit("HEAD")
		snap1, _, err := TakeSnapshot(d, repo1, rec.ID, c2v1, true)
		if err != nil {
			t.Fatal(err)
		}

		// Case 1: Amend C2 adding a line (same parent) -> false
		c2Add := append(append([]string(nil), c2V1Lines...), "// extra line")
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2Add), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "--no-edit")
		repoA, _ := vcs.Open(dir, nil)
		cA, _ := repoA.Commit("HEAD")
		if RevisionsHaveNoOwnDiff(repoA, snap1.Rev, snap1.ParentRev, cA) {
			t.Errorf("expected RevisionsHaveNoOwnDiff false when line added in C2")
		}

		// Case 2: Amend C2 changing commit message only -> false
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2V1Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "-m", "C2 changed subject\n\nChange-Id: "+keyC2)
		repoB, _ := vcs.Open(dir, nil)
		cB, _ := repoB.Commit("HEAD")
		if RevisionsHaveNoOwnDiff(repoB, snap1.Rev, snap1.ParentRev, cB) {
			t.Errorf("expected RevisionsHaveNoOwnDiff false when commit message changed")
		}

		// Case 3: Rebase onto amended C1 AND undo C2's deletion of line 10!
		runGit("checkout", revC1)
		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 99\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "--no-edit")

		c2UndoDel := makeNumberedLines(60) // line 10 restored!
		c2UndoDel[19] = "// c2 modified 20"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2UndoDel), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C2 initial\n\nChange-Id: "+keyC2)
		repoC, _ := vcs.Open(dir, nil)
		cC, _ := repoC.Commit("HEAD")
		if RevisionsHaveNoOwnDiff(repoC, snap1.Rev, snap1.ParentRev, cC) {
			t.Errorf("expected RevisionsHaveNoOwnDiff false when C2 undid a deletion during rebase")
		}

		// Case 4: Rebase onto amended C1 AND modify inserted line in C2 -> false
		c2Mod := makeNumberedLines(60)
		c2Mod = append(c2Mod[:9], c2Mod[10:]...)
		c2Mod[18] = "// c2 modified 20 differently"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2Mod), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "-m", "C2 initial\n\nChange-Id: "+keyC2)
		repoD, _ := vcs.Open(dir, nil)
		cD, _ := repoD.Commit("HEAD")
		if RevisionsHaveNoOwnDiff(repoD, snap1.Rev, snap1.ParentRev, cD) {
			t.Errorf("expected RevisionsHaveNoOwnDiff false when C2 modified inserted line")
		}

		// BuildChangeView shows SnapshotPending == true and TakeSnapshot records Snapshot 2
		viewD, err := BuildChangeView(d, repoD, rec, cD, "parent", "live")
		if err != nil {
			t.Fatal(err)
		}
		if !viewD.SnapshotPending || viewD.TargetDesc != "Live Snapshot" {
			t.Errorf("expected SnapshotPending=true after real edit, got pending=%v desc=%q", viewD.SnapshotPending, viewD.TargetDesc)
		}
		snap2, created2, err := TakeSnapshot(d, repoD, rec.ID, cD, true)
		if err != nil || !created2 || snap2.Num != 2 {
			t.Fatalf("expected TakeSnapshot to create Snapshot 2, got created=%v snap=%+v err=%v", created2, snap2, err)
		}
	})

	t.Run("UpstreamRenameOnly", func(t *testing.T) {
		dir, d, rec, runGit := setupGitStackRepo(t)
		defer d.Close()

		const keyC1 = "Icccccccccccccccccccccccccccccccccccccccc"
		const keyC2 = "Idddddddddddddddddddddddddddddddddddddddd"

		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C1\n\nChange-Id: "+keyC1)
		revC1 := runGit("rev-parse", "HEAD")

		if err := os.WriteFile(filepath.Join(dir, "c2.go"), []byte("package main\nvar c2 = 1\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "c2.go")
		runGit("commit", "-m", "C2\n\nChange-Id: "+keyC2)

		repo1, _ := vcs.Open(dir, nil)
		c2v1, _ := repo1.Commit("HEAD")
		snap1, _, err := TakeSnapshot(d, repo1, rec.ID, c2v1, true)
		if err != nil {
			t.Fatal(err)
		}

		// Amend C1 to rename oldname.txt -> newname.txt, then rebase C2
		runGit("checkout", revC1)
		runGit("mv", "oldname.txt", "newname.txt")
		runGit("commit", "--amend", "--no-edit")
		runGit("cherry-pick", c2v1.Rev)

		repo2, _ := vcs.Open(dir, nil)
		c2v2, _ := repo2.Commit("HEAD")
		if !RevisionsHaveNoOwnDiff(repo2, snap1.Rev, snap1.ParentRev, c2v2) {
			t.Errorf("expected RevisionsHaveNoOwnDiff true when upstream C1 renamed an untouched file")
		}
	})

	t.Run("JJStackAutoRebase", func(t *testing.T) {
		if _, err := exec.LookPath("jj"); err != nil {
			t.Skip("jj not installed")
		}
		dir := t.TempDir()
		runJJ := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("jj", append([]string{"--config", `ui.paginate="never"`}, args...)...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("jj %v failed: %s (%v)", args, string(out), err)
			}
			return strings.TrimSpace(string(out))
		}

		runJJ("git", "init", "--colocate")
		runJJ("config", "set", "--repo", "user.name", "JJ Stack")
		runJJ("config", "set", "--repo", "user.email", "jj@example.com")

		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(makeNumberedLines(60)), 0644); err != nil {
			t.Fatal(err)
		}
		runJJ("describe", "-m", "Base commit")
		runJJ("bookmark", "create", "main")

		// C1
		runJJ("new", "-m", "C1: top edit")
		c1Lines := makeNumberedLines(60)
		c1Lines[1] = "// c1 line 2"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c1Lines), 0644); err != nil {
			t.Fatal(err)
		}
		idC1 := runJJ("log", "--no-graph", "-r", "@", "-T", "change_id")

		// C2
		runJJ("new", "-m", "C2: bottom edit")
		c2Lines := append([]string(nil), c1Lines...)
		c2Lines[50] = "// c2 line 51"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2Lines), 0644); err != nil {
			t.Fatal(err)
		}
		idC2 := runJJ("log", "--no-graph", "-r", "@", "-T", "change_id")

		dbPath := filepath.Join(t.TempDir(), "jj.db")
		d, err := db.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		rec, err := d.EnsureRepo(dir)
		if err != nil {
			t.Fatal(err)
		}

		repo1, err := vcs.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		c1v1, err := repo1.Commit(idC1)
		if err != nil {
			t.Fatal(err)
		}
		c2v1, err := repo1.Commit(idC2)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := TakeSnapshot(d, repo1, rec.ID, c1v1, true); err != nil {
			t.Fatal(err)
		}
		snapC2, _, err := TakeSnapshot(d, repo1, rec.ID, c2v1, true)
		if err != nil {
			t.Fatal(err)
		}
		thC2, _, err := AddComment(d, repo1, rec.ID, c2v1, c2v1.Rev, "shared.go", "R", 51, 51, "alice", "jj stack comment", false, false)
		if err != nil {
			t.Fatal(err)
		}

		// Modify C1 via `jj new` + `jj squash`, auto-rebasing C2!
		runJJ("new", idC1)
		c1FixLines := append([]string{"// inserted line 1", "// inserted line 2"}, c1Lines...)
		c1FixLines[3] = "// c1 line 2 fixed"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c1FixLines), 0644); err != nil {
			t.Fatal(err)
		}
		runJJ("squash", "-u", "--into", idC1)

		repo2, err := vcs.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		c2v2, err := repo2.Commit(idC2)
		if err != nil {
			t.Fatal(err)
		}
		if c2v2.Rev == snapC2.Rev {
			t.Fatalf("expected jj squash into C1 to rebase C2 to a new commit ID")
		}

		// Opening C2 in BuildChangeView syncs Snapshot 1 cleanly and shifts comment by +2 lines!
		viewC2, err := BuildChangeView(d, repo2, rec, c2v2, "parent", "live")
		if err != nil {
			t.Fatal(err)
		}
		if viewC2.SnapshotPending || viewC2.TargetDesc != "Snapshot 1" {
			t.Fatalf("expected C2 synced to Snapshot 1 after jj auto-rebase, got pending=%v desc=%q", viewC2.SnapshotPending, viewC2.TargetDesc)
		}
		if len(viewC2.Threads) != 1 || viewC2.Threads[0].ID != thC2.ID || viewC2.Threads[0].Stale || viewC2.Threads[0].StartLine != 53 {
			t.Fatalf("expected thC2 visible non-stale at line 53, got %+v", viewC2.Threads)
		}
		if _, created, err := TakeSnapshot(d, repo2, rec.ID, c2v2, true); err != nil || created {
			t.Fatalf("expected TakeSnapshot on auto-rebased C2 created=false err=nil, got created=%v err=%v", created, err)
		}
	})

	t.Run("PublishChangeSyncsRebasedSnapshotAfterPriorDraftPromotion", func(t *testing.T) {
		dir, d, rec, runGit := setupGitStackRepo(t)
		defer d.Close()

		const keyC1 = "I7777777777777777777777777777777777777777"
		const keyC2 = "I8888888888888888888888888888888888888888"

		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C1\n\nChange-Id: "+keyC1)
		revC1v1 := runGit("rev-parse", "HEAD")

		c2Lines := makeNumberedLines(60)
		c2Lines[49] = "// c2 draft target 50"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C2\n\nChange-Id: "+keyC2)

		repo1, _ := vcs.Open(dir, nil)
		c2v1, _ := repo1.Commit("HEAD")
		// Leave an unsnapshotted (snapshot_num = 0) draft on rev1
		thDraft, _, err := AddComment(d, repo1, rec.ID, c2v1, c2v1.Rev, "shared.go", "R", 50, 50, "alice", "draft on rev1", true, false)
		if err != nil || thDraft.SnapshotNum != 0 {
			t.Fatalf("AddComment draft: th=%+v err=%v", thDraft, err)
		}

		// Purely rebase C2 (+40 lines upstream in C1) to rev2 before PublishChange
		runGit("checkout", revC1v1)
		var c1Lines []string
		for i := 0; i < 40; i++ {
			c1Lines = append(c1Lines, fmt.Sprintf("// upstream %02d", i+1))
		}
		c1Lines = append(c1Lines, makeNumberedLines(60)...)
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c1Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "--no-edit")

		c2v2Lines := append([]string(nil), c1Lines...)
		c2v2Lines[40+49] = "// c2 draft target 50"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2v2Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C2\n\nChange-Id: "+keyC2)

		repo2, _ := vcs.Open(dir, nil)
		c2v2, _ := repo2.Commit("HEAD")

		pubRes, err := PublishChange(d, repo2, rec.ID, keyC2, c2v2, true)
		if err != nil {
			t.Fatalf("PublishChange failed: %v", err)
		}
		if pubRes.Published != 1 || !pubRes.CreatedSnapshot || pubRes.Snapshot == nil || pubRes.Snapshot.Num != 1 || pubRes.Snapshot.Rev != c2v2.Rev {
			t.Fatalf("unexpected PublishResult: %+v (want snap1 rev=%s)", pubRes, c2v2.Rev)
		}
		snaps, _ := d.Snapshots(rec.ID, keyC2)
		if len(snaps) != 1 || snaps[0].Rev != c2v2.Rev {
			t.Fatalf("expected 1 snapshot synced to %s, got %+v", c2v2.Rev, snaps)
		}
		thAfter, _ := d.ThreadByID(thDraft.ID)
		if thAfter.SnapshotNum != 1 || thAfter.Rev != c2v2.Rev || thAfter.StartLine != 90 || thAfter.EndLine != 90 || thAfter.Messages[0].Draft {
			t.Errorf("expected published thread synced to snap=1 rev=%s lines=90-90, got %+v (msg=%+v)", c2v2.Rev, thAfter, thAfter.Messages[0])
		}
	})
}

func TestRunGCPrunesRedundantSnapshotsAndPins(t *testing.T) {
	dir, d, rec, runGit := setupGitStackRepo(t)
	defer d.Close()

	const keyC1 = "I1111111111111111111111111111111111111111"
	const keyC2 = "I2222222222222222222222222222222222222222"

	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("commit", "-a", "-m", "C1 v1\n\nChange-Id: "+keyC1)
	revC1v1 := runGit("rev-parse", "HEAD")

	// Snap 1: C2 v1
	if err := os.WriteFile(filepath.Join(dir, "c2.go"), []byte("package main\nfunc f() int {\n\treturn 1\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "c2.go")
	runGit("commit", "-m", "C2 commit\n\nChange-Id: "+keyC2)

	repo1, _ := vcs.Open(dir, nil)
	c2s1, _ := repo1.Commit("HEAD")
	snap1, _, err := d.TakeSnapshot(rec.ID, c2s1)
	if err != nil {
		t.Fatal(err)
	}
	_ = repo1.Pin(keyC2+"/snap-1", c2s1.Rev)
	th1, _, err := AddComment(d, repo1, rec.ID, c2s1, c2s1.Rev, "c2.go", "R", 3, 3, "alice", "comment from snap 1", false, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.SetFileReviewed(rec.ID, keyC2, snap1.Rev, "c2.go", true)
	tTrue := true
	_ = d.SetRevMark(rec.ID, keyC2, snap1.Rev, &tTrue, &tTrue)

	// Snap 2: Pure rebase of C2 v1 onto C1 v2
	runGit("checkout", revC1v1)
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("commit", "-a", "--amend", "--no-edit")
	revC1v2 := runGit("rev-parse", "HEAD")
	runGit("cherry-pick", c2s1.Rev)

	repo2, _ := vcs.Open(dir, nil)
	c2s2, _ := repo2.Commit("HEAD")
	snap2, _, err := d.TakeSnapshot(rec.ID, c2s2)
	if err != nil {
		t.Fatal(err)
	}
	_ = repo2.Pin(keyC2+"/snap-2", c2s2.Rev)
	th2, _, err := AddComment(d, repo2, rec.ID, c2s2, c2s2.Rev, "c2.go", "R", 2, 2, "alice", "comment from snap 2", false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Snap 3: Real own edit on C2 v2
	if err := os.WriteFile(filepath.Join(dir, "c2.go"), []byte("package main\nfunc f() int {\n\treturn 2\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("commit", "-a", "--amend", "--no-edit")
	repo3, _ := vcs.Open(dir, nil)
	c2s3, _ := repo3.Commit("HEAD")
	snap3, _, err := d.TakeSnapshot(rec.ID, c2s3)
	if err != nil || snap3.Num != 3 {
		t.Fatalf("TakeSnapshot 3: %v (snap=%+v)", err, snap3)
	}
	_ = repo3.Pin(keyC2+"/snap-3", c2s3.Rev)
	th3, _, err := AddComment(d, repo3, rec.ID, c2s3, c2s3.Rev, "c2.go", "R", 3, 3, "bob", "comment from snap 3", false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Snap 4: Pure rebase of C2 v2 onto C1 v3
	runGit("checkout", revC1v2)
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 4\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("commit", "-a", "--amend", "--no-edit")
	runGit("cherry-pick", c2s3.Rev)

	repo4, _ := vcs.Open(dir, nil)
	c2s4, _ := repo4.Commit("HEAD")
	snap4, _, err := d.TakeSnapshot(rec.ID, c2s4)
	if err != nil {
		t.Fatal(err)
	}
	_ = repo4.Pin(keyC2+"/snap-4", c2s4.Rev)
	th4, _, err := AddComment(d, repo4, rec.ID, c2s4, c2s4.Rev, "c2.go", "R", 4, 4, "bob", "comment from snap 4", false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Dry-run GC reports snap-1 & snap-3 redundant and snap-3 & snap-4 pins stale, without mutating DB/pins
	reportDry, err := RunGC(d, []*db.RepoRecord{rec}, true, false)
	if err != nil {
		t.Fatalf("RunGC dry-run: %v", err)
	}
	wantPrunedSnaps := []string{
		fmt.Sprintf("%s/%s/snap-1", rec.Slug, keyC2),
		fmt.Sprintf("%s/%s/snap-3", rec.Slug, keyC2),
	}
	for _, want := range wantPrunedSnaps {
		found := false
		for _, got := range reportDry.PrunedSnapshots {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("dry-run missing pruned snapshot %q in %v", want, reportDry.PrunedSnapshots)
		}
	}
	for _, wantPin := range []string{"refs/reviewed/" + keyC2 + "/snap-3", "refs/reviewed/" + keyC2 + "/snap-4"} {
		found := false
		for _, got := range reportDry.PrunedPins {
			if got == wantPin {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("dry-run missing stale pin %q in %v", wantPin, reportDry.PrunedPins)
		}
	}
	if snapsDry, _ := d.Snapshots(rec.ID, keyC2); len(snapsDry) != 4 {
		t.Fatalf("dry-run must not modify snapshots, got %d", len(snapsDry))
	}

	// Real GC collapses 1..4 -> 1..2, updates threads & marks, updates pins 1..2, prunes pins 3..4
	report, err := RunGC(d, []*db.RepoRecord{rec}, false, false)
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if len(report.PrunedSnapshots) != 2 {
		t.Fatalf("expected 2 pruned snapshots, got %v", report.PrunedSnapshots)
	}

	snapsFinal, err := d.Snapshots(rec.ID, keyC2)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapsFinal) != 2 ||
		snapsFinal[0].Num != 1 || snapsFinal[0].Rev != snap2.Rev ||
		snapsFinal[1].Num != 2 || snapsFinal[1].Rev != snap4.Rev {
		t.Fatalf("unexpected collapsed snapshots: %+v (want #1=%s, #2=%s)", snapsFinal, snap2.Rev, snap4.Rev)
	}

	// Verify thread snapshot numbers and re-anchoring
	for _, tc := range []struct {
		id      int64
		wantNum int
		wantRev string
	}{
		{th1.ID, 1, snap2.Rev},
		{th2.ID, 1, snap2.Rev},
		{th3.ID, 2, snap4.Rev},
		{th4.ID, 2, snap4.Rev},
	} {
		th, err := d.ThreadByID(tc.id)
		if err != nil || th == nil {
			t.Fatalf("ThreadByID(%d): %v", tc.id, err)
		}
		if th.SnapshotNum != tc.wantNum || th.Rev != tc.wantRev {
			t.Errorf("thread #%d got (snap=%d, rev=%s), want (snap=%d, rev=%s)", tc.id, th.SnapshotNum, th.Rev, tc.wantNum, tc.wantRev)
		}
	}

	// Verify file_marks and rev_marks preserved onto snap2.Rev
	if marks, _ := d.FileMarks(rec.ID, keyC2, snap2.Rev); !marks["c2.go"] {
		t.Errorf("expected file_marks carried to surviving snap 1 rev %s, got %+v", snap2.Rev, marks)
	}
	if revd, lgtm, _ := d.RevMark(rec.ID, keyC2, snap2.Rev); !revd || !lgtm {
		t.Errorf("expected rev_marks carried to surviving snap 1 rev %s, got (%v, %v)", snap2.Rev, revd, lgtm)
	}

	// Verify VCS pins: snap-1 -> snap2.Rev, snap-2 -> snap4.Rev, snap-3/snap-4 removed
	pinsAfter, _ := repo4.ListPins()
	for _, p := range pinsAfter {
		if strings.HasSuffix(p, "/snap-3") || strings.HasSuffix(p, "/snap-4") {
			t.Errorf("expected stale pin %s deleted after GC", p)
		}
	}
	if got := runGit("rev-parse", "refs/reviewed/"+keyC2+"/snap-1"); got != snap2.Rev {
		t.Errorf("snap-1 pin = %s, want %s", got, snap2.Rev)
	}
	if got := runGit("rev-parse", "refs/reviewed/"+keyC2+"/snap-2"); got != snap4.Rev {
		t.Errorf("snap-2 pin = %s, want %s", got, snap4.Rev)
	}

	// Second RunGC should find nothing left to prune
	report2, err := RunGC(d, []*db.RepoRecord{rec}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report2.PrunedSnapshots) != 0 || len(report2.PrunedPins) != 0 || len(report2.PrunedChanges) != 0 {
		t.Errorf("expected idempotent second RunGC, got %+v", report2)
	}

	t.Run("ConcurrentSnapshotModificationSkipsCollapseAndPreservesPins", func(t *testing.T) {
		dirC, dC, recC, runGitC := setupGitStackRepo(t)
		defer dC.Close()

		const kC1 = "I5555555555555555555555555555555555555555"
		const kC2 = "I6666666666666666666666666666666666666666"

		if err := os.WriteFile(filepath.Join(dirC, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGitC("commit", "-a", "-m", "C1 v1\n\nChange-Id: "+kC1)
		revC1a := runGitC("rev-parse", "HEAD")

		if err := os.WriteFile(filepath.Join(dirC, "c2.go"), []byte("package main\nfunc g() int { return 1 }\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGitC("add", "c2.go")
		runGitC("commit", "-m", "C2 v1\n\nChange-Id: "+kC2)
		repoA, _ := vcs.Open(dirC, nil)
		c2a, _ := repoA.Commit("HEAD")
		s1, _, err := dC.TakeSnapshot(recC.ID, c2a)
		if err != nil {
			t.Fatal(err)
		}
		_ = repoA.Pin(kC2+"/snap-1", c2a.Rev)

		// Rebase C2 onto amended C1 to create a redundant Snapshot 2
		runGitC("checkout", revC1a)
		if err := os.WriteFile(filepath.Join(dirC, "other.go"), []byte("package main\nvar other = 3\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGitC("commit", "-a", "--amend", "--no-edit")
		revC1b := runGitC("rev-parse", "HEAD")
		runGitC("cherry-pick", c2a.Rev)
		repoB, _ := vcs.Open(dirC, nil)
		c2b, _ := repoB.Commit("HEAD")
		s2, _, err := dC.TakeSnapshot(recC.ID, c2b)
		if err != nil {
			t.Fatal(err)
		}
		_ = repoB.Pin(kC2+"/snap-2", c2b.Rev)

		// Rebase C2 again (no own diff) to get a third valid commit rev c2c
		runGitC("checkout", revC1b)
		if err := os.WriteFile(filepath.Join(dirC, "other.go"), []byte("package main\nvar other = 4\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGitC("commit", "-a", "--amend", "--no-edit")
		runGitC("cherry-pick", c2a.Rev)
		repoC, _ := vcs.Open(dirC, nil)
		c2c, _ := repoC.Commit("HEAD")

		// 1. Direct DB.CollapseSnapshots check: simulate concurrent CarryForwardSnapshot on s2 (KeepID)
		//    keeping COUNT(*) == 2 while changing s2.Rev to c2c.Rev -> CollapseSnapshots must return false.
		if _, updated, err := dC.CarryForwardSnapshot(recC.ID, kC2, s2.ID, s2.Num, s2.Rev, s2.ParentRev, c2c.Rev, c2c.Parent, nil); err != nil || !updated {
			t.Fatalf("CarryForwardSnapshot: updated=%v err=%v", updated, err)
		}
		collapsedDirect, err := dC.CollapseSnapshots(recC.ID, kC2, 2, []db.SnapshotCollapseGroup{{
			KeepID:        s2.ID,
			NewNum:        1,
			NewRev:        s2.Rev, // stale rev before concurrent carry-forward
			NewParentRev:  s2.ParentRev,
			MergedSnapIDs: []int64{s1.ID},
			MergedOldRevs: []string{s1.Rev},
			OldNums:       []int{1, 2},
		}})
		if err != nil {
			t.Fatalf("CollapseSnapshots unexpected error: %v", err)
		}
		if collapsedDirect {
			t.Fatalf("expected CollapseSnapshots to return false when KeepID rev changed concurrently")
		}
		snapsVerify, _ := dC.Snapshots(recC.ID, kC2)
		if len(snapsVerify) != 2 || snapsVerify[1].Rev != c2c.Rev {
			t.Fatalf("expected DB snapshots preserved at count=2 rev=%s, got %+v", c2c.Rev, snapsVerify)
		}

		// 2. Full runGC check: trigger concurrent TakeSnapshot (creating Snapshot 3) during Files() inspection
		//    so CollapseSnapshots aborts, report.PrunedSnapshots stays empty, and snap-2/snap-3 pins are NOT pruned.
		var injected atomic.Bool
		var hookOpen func(string, *vcs.Cache) (vcs.Repo, error)
		hookOpen = func(root string, cache *vcs.Cache) (vcs.Repo, error) {
			r, err := vcs.Open(root, cache)
			if err != nil {
				return nil, err
			}
			return &gcConcurrentRepo{
				Repo: r,
				onFiles: func() {
					if injected.CompareAndSwap(false, true) {
						// Concurrently add Snapshot 3 (distinct edit commit) and pin it
						s3, created3, err := dC.TakeSnapshot(recC.ID, &vcs.Change{Key: kC2, Rev: c2a.Rev + "-concurrent", Parent: c2a.Parent})
						if err != nil || !created3 || s3.Num != 3 {
							t.Errorf("concurrent TakeSnapshot failed: created=%v snap=%+v err=%v", created3, s3, err)
						}
						_ = r.Pin(kC2+"/snap-3", c2c.Rev)
					}
				},
			}, nil
		}

		// Ensure noOwnDiffCache doesn't bypass Files() inside this test repo
		noOwnDiffCacheMu.Lock()
		clear(noOwnDiffCache)
		noOwnDiffCacheMu.Unlock()

		rep, err := runGC(dC, []*db.RepoRecord{recC}, false, false, hookOpen)
		if err != nil {
			t.Fatalf("runGC failed: %v", err)
		}
		if !injected.Load() {
			t.Fatalf("expected concurrent hook in Files() to fire during runGC")
		}
		if len(rep.PrunedSnapshots) != 0 {
			t.Errorf("expected empty PrunedSnapshots when CollapseSnapshots aborts concurrently, got %v", rep.PrunedSnapshots)
		}
		for _, p := range rep.PrunedPins {
			if strings.Contains(p, kC2+"/snap-") {
				t.Errorf("expected snap-* pin %q NOT pruned when CollapseSnapshots aborts", p)
			}
		}
		pinsLeft, _ := repoC.ListPins()
		for _, wantPin := range []string{
			"refs/reviewed/" + kC2 + "/snap-1",
			"refs/reviewed/" + kC2 + "/snap-2",
			"refs/reviewed/" + kC2 + "/snap-3",
		} {
			found := false
			for _, p := range pinsLeft {
				if p == wantPin {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected pin %q preserved after aborted CollapseSnapshots, got %v", wantPin, pinsLeft)
			}
		}
	})
}

type gcConcurrentRepo struct {
	vcs.Repo
	onFiles func()
}

func (r *gcConcurrentRepo) Files(baseRev, targetRev string) ([]*vcs.File, error) {
	if r.onFiles != nil {
		r.onFiles()
	}
	return r.Repo.Files(baseRev, targetRev)
}

type countingFilesRepo struct {
	vcs.Repo
	filesCalls  atomic.Int64
	commitCalls atomic.Int64
}

func (r *countingFilesRepo) Files(baseRev, targetRev string) ([]*vcs.File, error) {
	r.filesCalls.Add(1)
	return r.Repo.Files(baseRev, targetRev)
}

func (r *countingFilesRepo) Commit(rev string) (*vcs.Change, error) {
	r.commitCalls.Add(1)
	return r.Repo.Commit(rev)
}

func TestRebaseAuditPass1Fixes(t *testing.T) {
	t.Run("NoOwnDiffCacheAvoidsRepeatedFilesOnImmutableSHAs", func(t *testing.T) {
		dir, d, rec, runGit := setupGitStackRepo(t)
		defer d.Close()

		const keyC1 = "I1111111111111111111111111111111111111111"
		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C1 v1\n\nChange-Id: "+keyC1)

		repo1, err := vcs.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		c1v1, err := repo1.Commit("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		snap1, _, err := TakeSnapshot(d, repo1, rec.ID, c1v1, true)
		if err != nil {
			t.Fatal(err)
		}

		// Modify C1 so RevisionsHaveNoOwnDiff returns false (same parent, different file content)
		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 3\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "--no-edit")

		repo2, err := vcs.Open(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		c1v2, err := repo2.Commit("HEAD")
		if err != nil {
			t.Fatal(err)
		}
		wrapped := &countingFilesRepo{Repo: repo2}
		snaps := []*db.Snapshot{snap1}

		// First SyncLiveRebasedSnapshot invokes repo.Files once and returns false
		_, synced1 := SyncLiveRebasedSnapshot(d, wrapped, rec.ID, c1v2, snaps)
		if synced1 {
			t.Fatalf("expected synced1=false after real edit")
		}
		if got := wrapped.filesCalls.Load(); got != 1 {
			t.Fatalf("expected 1 Files call on cache miss, got %d", got)
		}
		commitsAfterFirst := wrapped.commitCalls.Load()

		// Subsequent SyncLiveRebasedSnapshot and direct RevisionsHaveNoOwnDiff calls hit noOwnDiffCache
		for i := 0; i < 4; i++ {
			_, synced := SyncLiveRebasedSnapshot(d, wrapped, rec.ID, c1v2, snaps)
			if synced {
				t.Fatalf("expected synced=false on repeated call %d", i)
			}
			if RevisionsHaveNoOwnDiff(wrapped, snap1.Rev, snap1.ParentRev, c1v2) {
				t.Fatalf("expected RevisionsHaveNoOwnDiff false on cache hit %d", i)
			}
		}
		if got := wrapped.filesCalls.Load(); got != 1 {
			t.Errorf("expected zero extra Files calls on cache hit, got total %d", got)
		}
		if got := wrapped.commitCalls.Load(); got != commitsAfterFirst {
			t.Errorf("expected zero extra Commit calls on cache hit, got %d (was %d)", got, commitsAfterFirst)
		}

		// Non-hex symbolic rev names bypass the immutable cache
		c1v2Sym := *c1v2
		c1v2Sym.Rev = "HEAD"
		if RevisionsHaveNoOwnDiff(wrapped, snap1.Rev, snap1.ParentRev, &c1v2Sym) {
			t.Errorf("expected false comparing edited HEAD against snap1")
		}
		if got := wrapped.filesCalls.Load(); got != 2 {
			t.Errorf("expected non-hex rev to bypass cache (Files calls = %d, want 2)", got)
		}
	})

	t.Run("PromotedPendingThreadsReanchoredOnCarryForward", func(t *testing.T) {
		dir, d, rec, runGit := setupGitStackRepo(t)
		defer d.Close()

		const keyC1 = "Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		const keyC2 = "Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C1\n\nChange-Id: "+keyC1)
		revC1v1 := runGit("rev-parse", "HEAD")

		c2Lines := makeNumberedLines(60)
		c2Lines[49] = "// c2 line 50"
		c2Lines[51] = "// c2 line 52"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C2\n\nChange-Id: "+keyC2)

		repo1, _ := vcs.Open(dir, nil)
		c2v1, _ := repo1.Commit("HEAD")
		snap1, _, err := TakeSnapshot(d, repo1, rec.ID, c2v1, true)
		if err != nil {
			t.Fatal(err)
		}

		// Create snapshot_num = 0 threads directly on oldRev / oldParent (simulating pending threads before carry-forward)
		thPendingR, _, err := d.AddThread(rec.ID, keyC2, c2v1.Rev, 0, "shared.go", "R", 52, 52, "// c2 line 52", "alice", "pending R on oldRev", false)
		if err != nil {
			t.Fatal(err)
		}
		thPendingL, _, err := d.AddThread(rec.ID, keyC2, c2v1.Parent, 0, "shared.go", "L", 50, 50, "// line 50", "alice", "pending L on oldParent", false)
		if err != nil {
			t.Fatal(err)
		}
		// Also a pending thread on Parent: header line 1 of /COMMIT_MSG that won't match after parent SHA changes
		msgBytes, _ := repo1.Content(c2v1.Rev, vcs.CommitMsgPath)
		msgFirstLine := diff.SplitLines(msgBytes)[0]
		thPendingUnmatched, _, err := d.AddThread(rec.ID, keyC2, c2v1.Rev, 0, vcs.CommitMsgPath, "R", 1, 1, msgFirstLine, "alice", "pending unmatched parent header", false)
		if err != nil {
			t.Fatal(err)
		}

		// Amend C1 inserting 40 lines at top of shared.go and rebase C2
		runGit("checkout", revC1v1)
		var c1Lines []string
		for i := 0; i < 40; i++ {
			c1Lines = append(c1Lines, fmt.Sprintf("// top insert %02d", i+1))
		}
		c1Lines = append(c1Lines, makeNumberedLines(60)...)
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c1Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "--no-edit")

		c2v2Lines := append([]string(nil), c1Lines...)
		c2v2Lines[40+49] = "// c2 line 50"
		c2v2Lines[40+51] = "// c2 line 52"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2v2Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C2\n\nChange-Id: "+keyC2)

		repo2, _ := vcs.Open(dir, nil)
		c2v2, _ := repo2.Commit("HEAD")
		snaps, synced := SyncLiveRebasedSnapshot(d, repo2, rec.ID, c2v2, []*db.Snapshot{snap1})
		if !synced || len(snaps) != 1 || snaps[0].Rev != c2v2.Rev {
			t.Fatalf("expected SyncLiveRebasedSnapshot carry-forward to %s, got synced=%v snaps=%+v", c2v2.Rev, synced, snaps)
		}

		// Verify promoted threads were re-anchored to newRev/newParent and shifted by +40 lines
		gotR, _ := d.ThreadByID(thPendingR.ID)
		if gotR.SnapshotNum != 1 || gotR.Rev != c2v2.Rev || gotR.StartLine != 92 || gotR.EndLine != 92 {
			t.Errorf("thPendingR = %+v, want snap=1 rev=%s lines=92-92", gotR, c2v2.Rev)
		}
		gotL, _ := d.ThreadByID(thPendingL.ID)
		if gotL.SnapshotNum != 1 || gotL.Rev != c2v2.Parent || gotL.StartLine != 90 || gotL.EndLine != 90 {
			t.Errorf("thPendingL = %+v, want snap=1 rev=%s lines=90-90", gotL, c2v2.Parent)
		}
		gotUnmatched, _ := d.ThreadByID(thPendingUnmatched.ID)
		if gotUnmatched.SnapshotNum != 1 || gotUnmatched.Rev != c2v1.Rev {
			t.Errorf("thPendingUnmatched = %+v, want snap=1 rev=%s", gotUnmatched, c2v1.Rev)
		}
		pins, _ := repo2.ListPins()
		wantRevPin := fmt.Sprintf("refs/reviewed/%s/rev-%s", keyC2, shortHash(c2v1.Rev))
		foundRevPin := false
		for _, p := range pins {
			if p == wantRevPin {
				foundRevPin = true
				break
			}
		}
		if !foundRevPin {
			t.Errorf("expected unmatched promoted thread to pin %s, got %v", wantRevPin, pins)
		}
	})

	t.Run("AddCommentRangeWithTargetSyncsRebaseImmediatelyWithoutRevPin", func(t *testing.T) {
		dir, d, rec, runGit := setupGitStackRepo(t)
		defer d.Close()

		const keyC1 = "Icccccccccccccccccccccccccccccccccccccccc"
		const keyC2 = "Idddddddddddddddddddddddddddddddddddddddd"

		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C1\n\nChange-Id: "+keyC1)
		revC1v1 := runGit("rev-parse", "HEAD")

		c2Lines := makeNumberedLines(60)
		c2Lines[49] = "// c2 edit 50"
		if err := os.WriteFile(filepath.Join(dir, "shared.go"), joinLines(c2Lines), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "-m", "C2\n\nChange-Id: "+keyC2)

		repo1, _ := vcs.Open(dir, nil)
		c2v1, _ := repo1.Commit("HEAD")
		if _, _, err := TakeSnapshot(d, repo1, rec.ID, c2v1, true); err != nil {
			t.Fatal(err)
		}

		// Amend C1 on disjoint other.go and rebase C2 without viewing or snapshotting C2 yet
		runGit("checkout", revC1v1)
		if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package main\nvar other = 3\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("commit", "-a", "--amend", "--no-edit")
		runGit("cherry-pick", c2v1.Rev)

		repo2, _ := vcs.Open(dir, nil)
		c2v2, _ := repo2.Commit("HEAD")

		th, _, err := AddCommentRangeWithTarget(d, repo2, rec.ID, c2v2, c2v2.Rev, c2v2.Rev, "shared.go", "R", 50, 50, 0, 0, "alice", "immediate snap 1", false, false)
		if err != nil {
			t.Fatal(err)
		}
		if th.SnapshotNum != 1 {
			t.Fatalf("expected SnapshotNum=1 immediately on unsynced purely rebased commit, got %d", th.SnapshotNum)
		}
		snaps, _ := d.Snapshots(rec.ID, keyC2)
		if len(snaps) != 1 || snaps[0].Rev != c2v2.Rev {
			t.Fatalf("expected Snapshot 1 synced to %s, got %+v", c2v2.Rev, snaps)
		}
		pins, _ := repo2.ListPins()
		for _, p := range pins {
			if strings.HasPrefix(p, "refs/reviewed/"+keyC2+"/rev-") {
				t.Errorf("unexpected stray rev-* pin %q after AddCommentRangeWithTarget on rebased commit", p)
			}
		}
		if got := runGit("rev-parse", "refs/reviewed/"+keyC2+"/snap-1"); got != c2v2.Rev {
			t.Errorf("snap-1 pin = %s, want %s", got, c2v2.Rev)
		}
	})
}

func TestParseSearchQuery(t *testing.T) {
	q := ParseSearchQuery(`f:^internal/ -f:_test\.go$ s:Broker method:Broadcast p:ui "exact phrase" -ignored /foo\d+/ author:alice is:unresolved case:yes`)
	if !q.CaseSensitive {
		t.Errorf("expected CaseSensitive=true when case:yes appears at end")
	}
	if len(q.FileFilters) != 2 || q.FileFilters[0].Negated || !q.FileFilters[1].Negated {
		t.Errorf("unexpected FileFilters: %+v", q.FileFilters)
	}
	if len(q.SymbolFilters) != 2 || q.SymbolFilters[0].Kind != "" || q.SymbolFilters[1].Kind != "method" {
		t.Errorf("unexpected SymbolFilters: %+v", q.SymbolFilters)
	}
	if len(q.PkgFilters) != 1 || q.PkgFilters[0].Raw != "ui" {
		t.Errorf("unexpected PkgFilters: %+v", q.PkgFilters)
	}
	if len(q.AuthorFilters) != 1 || q.AuthorFilters[0].Raw != "alice" {
		t.Errorf("unexpected AuthorFilters: %+v", q.AuthorFilters)
	}
	if !q.WantUnresolved {
		t.Errorf("expected WantUnresolved=true")
	}
	if len(q.Terms) != 3 {
		t.Fatalf("expected 3 content terms, got %d (%+v)", len(q.Terms), q.Terms)
	}
	if q.Terms[0].Raw != "exact phrase" || !q.Terms[0].Literal || q.Terms[0].MatchString("EXACT PHRASE") {
		t.Errorf("expected case-sensitive literal phrase term, got %+v", q.Terms[0])
	}
	if !q.Terms[1].Negated || q.Terms[1].Raw != "ignored" {
		t.Errorf("expected negated term, got %+v", q.Terms[1])
	}
	if !q.Terms[2].MatchString("foo42") || q.Terms[2].MatchString("FOO42") {
		t.Errorf("expected case-sensitive regex term, got %+v", q.Terms[2])
	}

	// Unclosed regex gracefully falls back to literal without panicking.
	qBad := ParseSearchQuery("foo(")
	if len(qBad.Terms) != 1 || !qBad.Terms[0].MatchString("call foo(bar)") {
		t.Errorf("expected literal fallback for invalid regex, got %+v", qBad.Terms)
	}
}

func TestCodeSearch(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}
	run("init", "-b", "main")
	run("config", "user.name", "Search Tester")
	run("config", "user.email", "search@example.com")

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/myrepo\n\ngo 1.24\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "auth"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "auth", "token.go"), []byte("package auth\n\ntype TokenStore struct{}\n\nfunc (t *TokenStore) VerifyToken(tok string) bool {\n\treturn tok == \"secretNeedle\"\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "auth", "token_test.go"), []byte("package auth\n\nfunc TestVerifyToken() {\n\t_ = \"secretNeedle\"\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src", "net", "http"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "net", "http", "server.go"), []byte("package http\n\ntype Handler interface {\n\tServeHTTP(w int)\n}\n\ntype Server struct {\n\tReadTimeout int\n}\n\nfunc (s *Server) ServeHTTP(w int) {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base commit")
	run("checkout", "-b", "feature")

	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package main\n\nfunc HandleLogin() string {\n\t_ = \"secretNeedle\"\n\t_ = \"secretNeedle\"\n\t_ = \"secretNeedle\"\n\t_ = \"secretNeedle\"\n\t_ = \"secretNeedle\"\n\treturn \"secretNeedle\"\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "handler.go")
	run("commit", "-m", "Add login handler\n\nChange-Id: I1111111111111111111111111111111111111111")

	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rec, err := d.EnsureRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := vcs.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.Changes()
	if err != nil || len(changes) == 0 {
		t.Fatalf("expected changes, got %v", err)
	}
	c := changes[0]

	th1, _, err := AddComment(d, repo, rec.ID, c, c.Rev, "handler.go", "R", 3, 3, "alice", "Check secretNeedle validation", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddMessage(th1.ID, "carol", "Follow-up reply on same thread", false); err != nil {
		t.Fatal(err)
	}
	th2, _, err := AddComment(d, repo, rec.ID, c, c.Rev, "handler.go", "R", 4, 4, "bob", "Looks resolved already", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetResolved(th2.ID, true); err != nil {
		t.Fatal(err)
	}

	runSearch := func(q string) *CodeSearchResult {
		t.Helper()
		res, err := CodeSearch(context.Background(), CodeSearchOptions{
			DB:           d,
			Repo:         repo,
			RepoID:       rec.ID,
			Change:       c,
			BaseRev:      c.Parent,
			TargetRev:    c.Rev,
			IsLiveTarget: true,
			Query:        q,
		})
		if err != nil {
			t.Fatalf("CodeSearch(%q): %v", q, err)
		}
		return res
	}

	// 1. Quick Open via f: returns matching repo files (including unchanged files).
	resFiles := runSearch("f:token")
	if len(resFiles.Files) != 2 {
		t.Fatalf("f:token expected 2 files, got %+v", resFiles.Files)
	}

	// 2. Symbol search via method: + p: finds TokenStore.VerifyToken in unchanged pkg/auth.
	resSyms := runSearch("method:Verify p:auth")
	if len(resSyms.Symbols) != 1 || resSyms.Symbols[0].Name != "TokenStore.VerifyToken" || resSyms.Symbols[0].Kind != "method" {
		t.Fatalf("method:Verify p:auth expected TokenStore.VerifyToken, got %+v", resSyms.Symbols)
	}

	// 3. Repo-wide code search with -f:_test ranks changed file first and excludes tests.
	resCode := runSearch("secretNeedle -f:_test")
	if len(resCode.Code) != 2 {
		t.Fatalf("expected 2 non-test code files, got %+v", resCode.Code)
	}
	if resCode.Code[0].Path != "handler.go" || !resCode.Code[0].InChange {
		t.Errorf("expected changed handler.go first, got %+v", resCode.Code[0])
	}
	if resCode.Code[1].Path != "pkg/auth/token.go" || resCode.Code[1].InChange {
		t.Errorf("expected unchanged pkg/auth/token.go second, got %+v", resCode.Code[1])
	}
	if len(resCode.Comments) != 1 || resCode.Comments[0].Author != "alice" {
		t.Errorf("expected default search to also include matching comment by alice, got %+v", resCode.Comments)
	}

	// 4. Scoped comment search with author: & is:unresolved, plus deduplicated is:unresolved listing.
	resComments := runSearch("in:comments is:unresolved author:alice")
	if len(resComments.Comments) != 1 || resComments.Comments[0].Author != "alice" || resComments.Comments[0].Resolved {
		t.Fatalf("expected 1 unresolved comment by alice, got %+v", resComments.Comments)
	}
	if len(resComments.Code) != 0 || len(resComments.Files) != 0 {
		t.Errorf("expected in:comments not to return code/files, got code=%d files=%d", len(resComments.Code), len(resComments.Files))
	}
	resUnres := runSearch("is:unresolved")
	if len(resUnres.Comments) != 1 || resUnres.Comments[0].ThreadID != th1.ID {
		t.Fatalf("expected multi-message unresolved thread deduplicated to 1 hit, got %+v", resUnres.Comments)
	}

	// 5. in:change restricts search to modified files only.
	resInChange := runSearch("in:change secretNeedle")
	if len(resInChange.Code) != 1 || resInChange.Code[0].Path != "handler.go" {
		t.Errorf("expected in:change to return only handler.go, got %+v", resInChange.Code)
	}

	// 6. Code Search style qualified Go symbol queries (pkg.Sym, src-trimmed import path, fields, interface methods, module path).
	resQualBare := runSearch(`"net/http.Server"`)
	if len(resQualBare.Symbols) == 0 || resQualBare.Symbols[0].Name != "Server" || resQualBare.Symbols[0].Kind != "struct" || resQualBare.Symbols[0].Path != "src/net/http/server.go" {
		t.Fatalf("expected \"net/http.Server\" to rank struct Server #1, got %+v", resQualBare.Symbols)
	}
	resQualUnquoted := runSearch(`net/http.Server`)
	if len(resQualUnquoted.Symbols) == 0 || resQualUnquoted.Symbols[0].Name != "Server" || resQualUnquoted.Symbols[0].Path != "src/net/http/server.go" {
		t.Fatalf("expected unquoted net/http.Server to rank struct Server #1, got %+v", resQualUnquoted.Symbols)
	}
	resQualSym := runSearch(`s:^net/http\.Server$`)
	if len(resQualSym.Symbols) != 1 || resQualSym.Symbols[0].Name != "Server" {
		t.Fatalf("expected s:^net/http\\.Server$ to match only Server, got %+v", resQualSym.Symbols)
	}
	resQualMethod := runSearch(`s:net/http.Server.ServeHTTP`)
	if len(resQualMethod.Symbols) != 1 || resQualMethod.Symbols[0].Name != "Server.ServeHTTP" {
		t.Fatalf("expected s:net/http.Server.ServeHTTP to match Server.ServeHTTP, got %+v", resQualMethod.Symbols)
	}
	resQualField := runSearch(`field:net/http.Server.ReadTimeout`)
	if len(resQualField.Symbols) != 1 || resQualField.Symbols[0].Name != "Server.ReadTimeout" || resQualField.Symbols[0].Kind != "field" {
		t.Fatalf("expected field:net/http.Server.ReadTimeout to match ReadTimeout field, got %+v", resQualField.Symbols)
	}
	resIfaceMethod := runSearch(`method:http.Handler.ServeHTTP`)
	if len(resIfaceMethod.Symbols) != 1 || resIfaceMethod.Symbols[0].Name != "Handler.ServeHTTP" {
		t.Fatalf("expected method:http.Handler.ServeHTTP to match Handler.ServeHTTP, got %+v", resIfaceMethod.Symbols)
	}
	resModQual := runSearch(`s:example.com/myrepo/pkg/auth.TokenStore.VerifyToken`)
	if len(resModQual.Symbols) != 1 || resModQual.Symbols[0].Name != "TokenStore.VerifyToken" {
		t.Fatalf("expected module-qualified symbol query to match TokenStore.VerifyToken, got %+v", resModQual.Symbols)
	}
	resNegFirst := runSearch(`-s:Handler s:Server`)
	if len(resNegFirst.Symbols) == 0 || resNegFirst.Symbols[0].Name != "Server" {
		t.Fatalf("expected -s:Handler s:Server to rank exact Server #1, got %+v", resNegFirst.Symbols)
	}
	resModPkg := runSearch(`p:example.com/myrepo/pkg/auth secretNeedle`)
	if len(resModPkg.Code) == 0 || resModPkg.Code[0].Path != "pkg/auth/token.go" {
		t.Fatalf("expected module-qualified p: filter to work in code search, got %+v", resModPkg.Code)
	}
	resModPkgFiles := runSearch(`p:example.com/myrepo/pkg/auth`)
	if len(resModPkgFiles.Files) != 2 {
		t.Fatalf("expected module-qualified p: filter to find 2 files in pkg/auth, got %+v", resModPkgFiles.Files)
	}
}
