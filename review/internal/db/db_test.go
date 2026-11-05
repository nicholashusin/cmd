package db

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nicholas.sh/cmd/review/internal/vcs"
)

func TestPrefs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "review.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if got := d.Pref("theme", "auto"); got != "auto" {
		t.Errorf("expected fallback 'auto', got %q", got)
	}

	if err := d.SetPref("theme", "dark"); err != nil {
		t.Fatalf("SetPref failed: %v", err)
	}
	if got := d.Pref("theme", "auto"); got != "dark" {
		t.Errorf("expected 'dark', got %q", got)
	}

	if err := d.SetPref("listen_addr", "127.0.0.1:9090"); err != nil {
		t.Fatalf("SetPref listen_addr failed: %v", err)
	}
	if got := d.Pref("listen_addr", "localhost:2626"); got != "127.0.0.1:9090" {
		t.Errorf("expected '127.0.0.1:9090', got %q", got)
	}
}

func TestReposAndCascade(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "review.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo1, err := d.EnsureRepo("/path/to/project")
	if err != nil {
		t.Fatal(err)
	}
	if repo1.Slug != "project" {
		t.Errorf("expected slug 'project', got %q", repo1.Slug)
	}

	// Duplicate folder name under different path should get slug project.1
	repo2, err := d.EnsureRepo("/another/path/to/project")
	if err != nil {
		t.Fatal(err)
	}
	if repo2.Slug != "project.1" {
		t.Errorf("expected slug 'project.1', got %q", repo2.Slug)
	}

	repos, err := d.Repos()
	if err != nil || len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d (%v)", len(repos), err)
	}

	// Add thread to repo1
	th, _, err := d.AddThread(repo1.ID, "ch1", "rev1", 0, "main.go", "R", 10, 10, "code", "user", "comment", true)
	if err != nil {
		t.Fatal(err)
	}

	// Forget repo1
	forgotten, err := d.ForgetRepo("project")
	if err != nil || forgotten == nil {
		t.Fatalf("ForgetRepo failed: %v", err)
	}

	// Verify thread was cascaded
	thCheck, err := d.ThreadByID(th.ID)
	if err == nil && thCheck != nil {
		t.Errorf("expected thread to be deleted by cascade")
	}
}

func TestSnapshotsAndUpgrade(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "review.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	r, err := d.EnsureRepo("/path/to/repo")
	if err != nil {
		t.Fatal(err)
	}

	c1 := &vcs.Change{Key: "k1", Rev: "rev1", Parent: "p1"}
	snap1, created, err := d.TakeSnapshot(r.ID, c1)
	if err != nil || !created || snap1.Num != 1 {
		t.Fatalf("first snapshot failed: created=%v, num=%d, err=%v", created, snap1.Num, err)
	}

	// Taking snapshot again for same rev should return existing
	snap1Again, created2, err := d.TakeSnapshot(r.ID, c1)
	if err != nil || created2 || snap1Again.Num != 1 {
		t.Fatalf("redundant snapshot failed: created=%v, num=%d, err=%v", created2, snap1Again.Num, err)
	}

	// Create comment on live revision (snapshot_num = 0) on side R and side L
	c2 := &vcs.Change{Key: "k1", Rev: "rev2", Parent: "p1"}
	th, _, err := d.AddThread(r.ID, c2.Key, c2.Rev, 0, "foo.go", "R", 5, 5, "fn()", "dev", "look here", true)
	if err != nil {
		t.Fatal(err)
	}
	if th.SnapshotNum != 0 {
		t.Errorf("expected snapshot_num = 0, got %d", th.SnapshotNum)
	}

	thL, _, err := d.AddThread(r.ID, c2.Key, c2.Parent, 0, "foo.go", "L", 2, 2, "old()", "dev", "left comment", true)
	if err != nil {
		t.Fatal(err)
	}
	if thL.SnapshotNum != 0 {
		t.Errorf("expected left thread snapshot_num = 0, got %d", thL.SnapshotNum)
	}

	// Now snapshot rev2 -> should be num 2 and upgrade both threads!
	snap2, created3, err := d.TakeSnapshot(r.ID, c2)
	if err != nil || !created3 || snap2.Num != 2 {
		t.Fatalf("second snapshot failed: created=%v, num=%d, err=%v", created3, snap2.Num, err)
	}

	thUpdated, err := d.ThreadByID(th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thUpdated.SnapshotNum != 2 {
		t.Errorf("expected upgraded snapshot_num = 2, got %d", thUpdated.SnapshotNum)
	}

	thLUpdated, err := d.ThreadByID(thL.ID)
	if err != nil {
		t.Fatal(err)
	}
	if thLUpdated.SnapshotNum != 2 {
		t.Errorf("expected upgraded left thread snapshot_num = 2, got %d", thLUpdated.SnapshotNum)
	}

	// Re-taking snapshot on an older rev (rev1) should NOT create a redundant snapshot
	// and must still promote any snapshot_num = 0 threads on rev1 to snapshot 1.
	thRev1Zero, _, err := d.AddThread(r.ID, c1.Key, c1.Rev, 0, "foo.go", "R", 9, 9, "x()", "dev", "back on rev1", true)
	if err != nil {
		t.Fatal(err)
	}
	snap1Matched, created4, err := d.TakeSnapshot(r.ID, c1)
	if err != nil || created4 || snap1Matched.Num != 1 {
		t.Fatalf("expected existing snapshot 1 on rev1 without duplicate, got created=%v snap=%+v err=%v", created4, snap1Matched, err)
	}
	thRev1Updated, err := d.ThreadByID(thRev1Zero.ID)
	if err != nil || thRev1Updated.SnapshotNum != 1 {
		t.Fatalf("expected thRev1Zero promoted to snapshot 1, got %+v (err=%v)", thRev1Updated, err)
	}

	// ChangeKeysWithDrafts and PromoteOrphanedZeroThreads
	if _, _, err := d.AddThread(r.ID, "k2", "r2v1", 0, "bar.go", "R", 1, 1, "a", "dev", "published zero", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AddThread(r.ID, "k3", "r3v1", 0, "baz.go", "R", 1, 1, "b", "dev", "draft zero", true); err != nil {
		t.Fatal(err)
	}
	keysWithDrafts, err := d.ChangeKeysWithDrafts(r.ID)
	if err != nil {
		t.Fatalf("ChangeKeysWithDrafts: %v", err)
	}
	if len(keysWithDrafts) != 2 || keysWithDrafts[0] != "k1" || keysWithDrafts[1] != "k3" {
		t.Errorf("ChangeKeysWithDrafts = %v, want [k1 k3]", keysWithDrafts)
	}
	if err := d.PromoteOrphanedZeroThreads(r.ID, "k2", 1); err != nil {
		t.Fatal(err)
	}
	if err := d.PromoteOrphanedZeroThreads(r.ID, "k3", 2); err != nil {
		t.Fatal(err)
	}
	k2Threads, _ := d.ThreadsForChange(r.ID, "k2")
	k3Threads, _ := d.ThreadsForChange(r.ID, "k3")
	if len(k2Threads) != 1 || k2Threads[0].SnapshotNum != 0 {
		t.Errorf("expected published thread on k2 to remain snapshot_num=0, got %+v", k2Threads)
	}
	if len(k3Threads) != 1 || k3Threads[0].SnapshotNum != 2 {
		t.Errorf("expected draft thread on k3 to promote to snapshot_num=2, got %+v", k3Threads)
	}
}

func TestThreadsMessagesAndReviews(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "review.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	r, err := d.EnsureRepo("/path/to/repo")
	if err != nil {
		t.Fatal(err)
	}

	// Add thread and message
	th, msg, err := d.AddThread(r.ID, "keyA", "revA", 1, "test.go", "R", 1, 3, "line1\nline2\nline3", "alice", "initial comment", true)
	if err != nil {
		t.Fatal(err)
	}
	if !msg.Draft {
		t.Errorf("expected message to be draft")
	}

	// Edit message
	updatedMsg, _, err := d.EditMessage(msg.ID, "edited comment")
	if err != nil {
		t.Fatalf("EditMessage failed: %v", err)
	}
	if updatedMsg.Body != "edited comment" {
		t.Errorf("expected edited body, got %q", updatedMsg.Body)
	}

	// Reply
	reply, err := d.AddMessage(th.ID, "bob", "reply comment", true)
	if err != nil {
		t.Fatalf("AddMessage failed: %v", err)
	}

	// Edit form-drafts on keyA, keyB, and another repo r2; plus a reply form-draft on keyA.
	r2, err := d.EnsureRepo("/other/repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveFormDraft(r.ID, "keyA", "editA", "", "", 0, 0, 0, 0, true, "", fmt.Sprintf("%d", msg.ID), "unsaved edit A"); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveFormDraft(r.ID, "keyA", "replyA", "", "", 0, 0, 0, 0, true, fmt.Sprintf("%d", th.ID), "", "unsaved reply A"); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveFormDraft(r.ID, "keyB", "editB", "", "", 0, 0, 0, 0, true, "", "999", "unsaved edit B"); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveFormDraft(r2.ID, "keyC", "editC", "", "", 0, 0, 0, 0, true, "", "1000", "unsaved edit C"); err != nil {
		t.Fatal(err)
	}

	// Publish keyA -> deletes keyA edit form-draft but keeps keyA reply draft, keyB edit draft, and r2 edit draft.
	pubCount, err := d.PublishChange(r.ID, "keyA")
	if err != nil || pubCount != 2 {
		t.Fatalf("PublishChange failed: count=%d, err=%v", pubCount, err)
	}
	draftsA, err := d.FormDraftsForChange(r.ID, "keyA")
	if err != nil || len(draftsA) != 1 || draftsA[0].DraftKey != "replyA" {
		t.Fatalf("expected only replyA to remain on keyA after PublishChange, got %+v (err=%v)", draftsA, err)
	}
	if draftsB, err := d.FormDraftsForChange(r.ID, "keyB"); err != nil || len(draftsB) != 1 {
		t.Fatalf("expected keyB edit draft to survive keyA PublishChange, got %+v (err=%v)", draftsB, err)
	}

	// PublishRepo r -> cleans keyB edit draft too, leaving r2 untouched.
	if _, err := d.PublishRepo(r.ID); err != nil {
		t.Fatalf("PublishRepo failed: %v", err)
	}
	if draftsB, err := d.FormDraftsForChange(r.ID, "keyB"); err != nil || len(draftsB) != 0 {
		t.Fatalf("expected keyB edit draft deleted after PublishRepo, got %+v (err=%v)", draftsB, err)
	}
	if draftsC, err := d.FormDraftsForChange(r2.ID, "keyC"); err != nil || len(draftsC) != 1 {
		t.Fatalf("expected other repo edit draft untouched by PublishRepo, got %+v (err=%v)", draftsC, err)
	}

	// Delete draft reply should fail now that it's published
	if err := d.DeleteDraftMessage(reply.ID); err == nil {
		t.Errorf("expected error deleting published message, got nil")
	}

	// File review marks
	err = d.SetFileReviewed(r.ID, "keyA", "revA", "test.go", true)
	if err != nil {
		t.Fatal(err)
	}
	marks, err := d.FileMarks(r.ID, "keyA", "revA")
	if err != nil || !marks["test.go"] {
		t.Errorf("expected test.go to be reviewed: %v", marks)
	}

	// Revision mark
	tTrue := true
	err = d.SetRevMark(r.ID, "keyA", "revA", &tTrue, &tTrue)
	if err != nil {
		t.Fatal(err)
	}
	revd, lgtm, err := d.RevMark(r.ID, "keyA", "revA")
	if err != nil || !revd || !lgtm {
		t.Errorf("expected reviewed and lgtm, got (%v, %v, %v)", revd, lgtm, err)
	}

	// Summaries
	sums, err := d.RepoSummaries(r.ID)
	if err != nil {
		t.Fatalf("RepoSummaries failed: %v", err)
	}
	sumA := sums["keyA"]
	if sumA == nil || !sumA.Reviewed || !sumA.LGTM || sumA.OpenThreads != 1 {
		t.Errorf("unexpected summary: %+v", sumA)
	}

	// Prune
	pruned, err := d.PruneChanges(r.ID, map[string]bool{})
	if err != nil || len(pruned) != 1 || pruned[0] != "keyA" {
		t.Errorf("PruneChanges failed: pruned=%v, err=%v", pruned, err)
	}
}

func TestThreadRange(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "review.db")

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer d.Close()

	r, err := d.EnsureRepo("/path/to/repo")
	if err != nil {
		t.Fatalf("EnsureRepo failed: %v", err)
	}

	thRange, _, err := d.AddThreadRange(r.ID, "ck1", "rev1", 0, "file.go", "R", 5, 8, 3, 12, "range-anchor", "alice", "range comment", false)
	if err != nil {
		t.Fatalf("AddThreadRange failed: %v", err)
	}
	if thRange.StartCol != 3 || thRange.EndCol != 12 {
		t.Errorf("expected start_col=3 end_col=12, got (%d, %d)", thRange.StartCol, thRange.EndCol)
	}

	thByID, err := d.ThreadByID(thRange.ID)
	if err != nil {
		t.Fatalf("ThreadByID(%d) failed: %v", thRange.ID, err)
	}
	if thByID.StartCol != 3 || thByID.EndCol != 12 {
		t.Errorf("ThreadByID returned wrong cols: got (%d, %d)", thByID.StartCol, thByID.EndCol)
	}

	threads, err := d.ThreadsForChange(r.ID, "ck1")
	if err != nil {
		t.Fatalf("ThreadsForChange failed: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != thRange.ID || threads[0].StartCol != 3 || threads[0].EndCol != 12 {
		t.Fatalf("unexpected ThreadsForChange result: %+v", threads)
	}
}

func TestStateDigestMemoization(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rec, err := d.EnsureRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	sig0 := d.StateDigest()
	if sig0 == "" {
		t.Fatalf("expected non-empty sig0")
	}

	// Repeated calls must return identical digest (cached)
	sig0Repeat := d.StateDigest()
	if sig0Repeat != sig0 {
		t.Errorf("expected cached sig %s, got %s", sig0, sig0Repeat)
	}

	// SetPref must not change StateDigest
	if err := d.SetPref("theme", "dark"); err != nil {
		t.Fatal(err)
	}
	sigAfterPref := d.StateDigest()
	if sigAfterPref != sig0 {
		t.Errorf("expected StateDigest unchanged after SetPref, got %s vs %s", sigAfterPref, sig0)
	}

	// Add thread -> digest must change
	_, msg, err := d.AddThread(rec.ID, "c1", "r1", 0, "file.go", "R", 1, 1, "anchor", "author", "comment", true)
	if err != nil {
		t.Fatal(err)
	}
	sig1 := d.StateDigest()
	if sig1 == sig0 {
		t.Errorf("expected StateDigest to change after AddThread, remained %s", sig1)
	}

	// Delete draft message -> digest must return to sig0 (undo)
	if err := d.DeleteDraftMessage(msg.ID); err != nil {
		t.Fatal(err)
	}
	sig2 := d.StateDigest()
	if sig2 != sig0 {
		t.Errorf("expected StateDigest to restore to %s after deleting draft, got %s", sig0, sig2)
	}

	// Second *DB instance writing to the same database file (multi-process simulation)
	d2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	_, _, err = d2.AddThread(rec.ID, "c1", "r1", 0, "file.go", "R", 2, 2, "anchor2", "author2", "comment2", false)
	if err != nil {
		t.Fatal(err)
	}

	// First instance must detect the external write (via PRAGMA data_version) and return a new digest
	sig3 := d.StateDigest()
	if sig3 == sig2 {
		t.Errorf("expected instance d to detect external write from d2 and change digest, remained %s", sig3)
	}

	// Calling SetPref right after an uncached write on d must not mask the write
	sigA := d.StateDigest()
	_, _, err = d.AddThread(rec.ID, "c1", "r1", 0, "file.go", "R", 3, 3, "anchor3", "author3", "comment3", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPref("theme", "light"); err != nil {
		t.Fatal(err)
	}
	sigB := d.StateDigest()
	if sigB == sigA {
		t.Errorf("expected sigB != sigA after AddThread followed by SetPref, remained %s", sigB)
	}
}

func TestRepoSummariesMulti(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	r1, _ := d.EnsureRepo("/path/to/r1")
	r2, _ := d.EnsureRepo("/path/to/r2")

	_, _, _ = d.TakeSnapshot(r1.ID, &vcs.Change{Key: "c1", Rev: "rev1", Parent: "parent1"})
	_, _, _ = d.TakeSnapshot(r2.ID, &vcs.Change{Key: "c2", Rev: "rev2", Parent: "parent2"})

	tTrue := true
	_ = d.SetRevMark(r1.ID, "c1", "rev1", &tTrue, &tTrue)

	th1, _, _ := d.AddThread(r1.ID, "c1", "rev1", 0, "file1.go", "R", 1, 1, "a", "u", "msg1", false)
	_, _ = d.AddMessage(th1.ID, "u", "draft reply", true)

	multi, err := d.RepoSummariesMulti([]int64{r1.ID, r2.ID})
	if err != nil {
		t.Fatalf("RepoSummariesMulti failed: %v", err)
	}

	s1 := multi[r1.ID]["c1"]
	if s1 == nil {
		t.Fatalf("missing summary for r1/c1")
	}
	if !s1.LGTM || !s1.Reviewed || s1.SnapshotCount != 1 || s1.OpenThreads != 1 || s1.DraftCount != 1 {
		t.Errorf("unexpected summary for r1/c1: %+v", s1)
	}

	s2 := multi[r2.ID]["c2"]
	if s2 == nil {
		t.Fatalf("missing summary for r2/c2")
	}
	if s2.SnapshotCount != 1 || s2.OpenThreads != 0 {
		t.Errorf("unexpected summary for r2/c2: %+v", s2)
	}

	// Empty repo list
	empty, err := d.RepoSummariesMulti(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("RepoSummariesMulti(nil) = %v, %v, want empty map, nil", empty, err)
	}
}

func TestCommentCountsForChange(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	r, _ := d.EnsureRepo("/path/to/repo")

	// Thread 1 on fileA.go (unresolved, 1 published msg, 1 draft msg)
	th1, _, _ := d.AddThread(r.ID, "c1", "rev1", 0, "fileA.go", "R", 10, 10, "anchor", "author", "comment", false)
	_, _ = d.AddMessage(th1.ID, "author", "draft reply", true)

	// Thread 2 on /fileA.go (resolved, 1 published msg)
	th2, _, _ := d.AddThread(r.ID, "c1", "rev1", 0, "/fileA.go", "R", 20, 20, "anchor", "author", "resolved comment", false)
	_ = d.SetResolved(th2.ID, true)

	// Thread 3 on fileB.go (unresolved, 1 draft msg)
	_, _, _ = d.AddThread(r.ID, "c1", "rev1", 0, "fileB.go", "R", 5, 5, "anchor", "author", "draft only", true)

	// Overall counts for change
	unres, res, draft, err := d.CommentCountsForChange(r.ID, "c1", "")
	if err != nil {
		t.Fatalf("CommentCountsForChange(change) err: %v", err)
	}
	if unres != 2 || res != 1 || draft != 2 {
		t.Errorf("CommentCountsForChange(change) = (unres=%d, res=%d, draft=%d), want (2, 1, 2)", unres, res, draft)
	}

	// Filtered counts for fileA.go
	fUnres, fRes, fDraft, err := d.CommentCountsForChange(r.ID, "c1", "fileA.go")
	if err != nil {
		t.Fatalf("CommentCountsForChange(fileA.go) err: %v", err)
	}
	if fUnres != 1 || fRes != 1 || fDraft != 1 {
		t.Errorf("CommentCountsForChange(fileA.go) = (unres=%d, res=%d, draft=%d), want (1, 1, 1)", fUnres, fRes, fDraft)
	}

	// Filtered counts for fileB.go
	bUnres, bRes, bDraft, err := d.CommentCountsForChange(r.ID, "c1", "/fileB.go")
	if err != nil {
		t.Fatalf("CommentCountsForChange(fileB.go) err: %v", err)
	}
	if bUnres != 1 || bRes != 0 || bDraft != 1 {
		t.Errorf("CommentCountsForChange(fileB.go) = (unres=%d, res=%d, draft=%d), want (1, 0, 1)", bUnres, bRes, bDraft)
	}
}

func TestSQLitePragmas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "review.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var syncMode int
	if err := d.db.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil {
		t.Fatalf("query synchronous: %v", err)
	}
	if syncMode != 1 { // 1 == NORMAL
		t.Errorf("PRAGMA synchronous = %d, want 1 (NORMAL)", syncMode)
	}

	var tempStore int
	if err := d.db.QueryRow("PRAGMA temp_store").Scan(&tempStore); err != nil {
		t.Fatalf("query temp_store: %v", err)
	}
	if tempStore != 2 { // 2 == MEMORY
		t.Errorf("PRAGMA temp_store = %d, want 2 (MEMORY)", tempStore)
	}
}

func TestThreadsForChanges(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	r, err := d.EnsureRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Empty query
	resEmpty, err := d.ThreadsForChanges(r.ID, nil)
	if err != nil || len(resEmpty) != 0 {
		t.Fatalf("ThreadsForChanges(nil) = %v, err = %v", resEmpty, err)
	}

	// Add threads on c1 and c2
	th1, m1, err := d.AddThread(r.ID, "c1", "rev1", 0, "file1.go", "R", 10, 10, "anchor1", "alice", "msg 1", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.AddMessage(th1.ID, "bob", "msg 1 reply", false)
	if err != nil {
		t.Fatal(err)
	}

	th2, _, err := d.AddThread(r.ID, "c1", "rev1", 0, "file2.go", "R", 5, 5, "anchor2", "carol", "msg 2", true)
	if err != nil {
		t.Fatal(err)
	}

	th3, _, err := d.AddThread(r.ID, "c2", "rev2", 0, "file3.go", "R", 1, 1, "anchor3", "dave", "msg 3", false)
	if err != nil {
		t.Fatal(err)
	}

	res, err := d.ThreadsForChanges(r.ID, []string{"c1", "c2", "c3"})
	if err != nil {
		t.Fatalf("ThreadsForChanges err: %v", err)
	}

	if len(res["c1"]) != 2 {
		t.Errorf("expected 2 threads for c1, got %d", len(res["c1"]))
	} else {
		// Verify start_line ordering: file2.go (line 5) before file1.go (line 10)
		if res["c1"][0].ID != th2.ID || res["c1"][1].ID != th1.ID {
			t.Errorf("expected start_line ascending ordering for c1 threads")
		}
		// Verify messages on th1
		var th1Res *Thread
		for _, th := range res["c1"] {
			if th.ID == th1.ID {
				th1Res = th
				break
			}
		}
		if th1Res == nil || len(th1Res.Messages) != 2 {
			t.Errorf("expected 2 messages on th1, got %v", th1Res)
		} else if th1Res.Messages[0].ID != m1.ID {
			t.Errorf("expected first message ID %d, got %d", m1.ID, th1Res.Messages[0].ID)
		}
	}

	if len(res["c2"]) != 1 || res["c2"][0].ID != th3.ID {
		t.Errorf("expected 1 thread for c2 matching th3, got %v", res["c2"])
	}

	if len(res["c3"]) != 0 {
		t.Errorf("expected 0 threads for c3, got %d", len(res["c3"]))
	}
}

func TestFormDrafts(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer d.Close()

	repo, err := d.EnsureRepo("/path/to/repo")
	if err != nil {
		t.Fatalf("AddRepo failed: %v", err)
	}

	sig0 := d.StateDigest()

	// 1. Save new draft
	err = d.SaveFormDraft(repo.ID, "c1", "d1", "foo.go", "R", 10, 15, 2, 8, false, "", "", "first draft")
	if err != nil {
		t.Fatalf("SaveFormDraft failed: %v", err)
	}

	// StateDigest must remain unchanged
	sig1 := d.StateDigest()
	if sig1 != sig0 {
		t.Errorf("expected StateDigest unchanged after SaveFormDraft, got %s vs %s", sig1, sig0)
	}

	// 2. Fetch drafts
	drafts, err := d.FormDraftsForChange(repo.ID, "c1")
	if err != nil {
		t.Fatalf("FormDraftsForChange failed: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("expected 1 draft, got %d", len(drafts))
	}
	if drafts[0].DraftKey != "d1" || drafts[0].Content != "first draft" || drafts[0].TargetPath != "foo.go" || drafts[0].Line != 10 || drafts[0].EndLine != 15 || drafts[0].StartCol != 2 || drafts[0].EndCol != 8 || drafts[0].Unresolved != false {
		t.Errorf("draft fields mismatch: %+v", drafts[0])
	}

	// 3. Update draft
	err = d.SaveFormDraft(repo.ID, "c1", "d1", "foo.go", "R", 10, 15, 2, 8, true, "", "", "updated draft")
	if err != nil {
		t.Fatalf("SaveFormDraft update failed: %v", err)
	}
	drafts, err = d.FormDraftsForChange(repo.ID, "c1")
	if err != nil || len(drafts) != 1 || drafts[0].Content != "updated draft" || !drafts[0].Unresolved {
		t.Fatalf("expected updated content, got %v, err=%v", drafts, err)
	}

	// 4. Save thread reply draft and message edit draft
	if err := d.SaveFormDraft(repo.ID, "c1", "reply:t1", "foo.go", "R", 10, 10, 0, 0, true, "t1", "", "reply text"); err != nil {
		t.Fatalf("SaveFormDraft reply failed: %v", err)
	}
	if err := d.SaveFormDraft(repo.ID, "c1", "edit:m1", "foo.go", "R", 10, 10, 0, 0, true, "t1", "m1", "edit text"); err != nil {
		t.Fatalf("SaveFormDraft edit failed: %v", err)
	}

	drafts, err = d.FormDraftsForChange(repo.ID, "c1")
	if err != nil || len(drafts) != 3 {
		t.Fatalf("expected 3 drafts, got %d, err=%v", len(drafts), err)
	}

	// 5. DeleteFormDraftForMessage
	if err := d.DeleteFormDraftForMessage(repo.ID, "c1", "m1"); err != nil {
		t.Fatalf("DeleteFormDraftForMessage failed: %v", err)
	}
	drafts, err = d.FormDraftsForChange(repo.ID, "c1")
	if err != nil || len(drafts) != 2 {
		t.Fatalf("expected 2 drafts after message draft delete, got %d", len(drafts))
	}

	// 6. DeleteFormDraftsForThread
	if err := d.DeleteFormDraftsForThread(repo.ID, "c1", "t1"); err != nil {
		t.Fatalf("DeleteFormDraftsForThread failed: %v", err)
	}
	drafts, err = d.FormDraftsForChange(repo.ID, "c1")
	if err != nil || len(drafts) != 1 || drafts[0].DraftKey != "d1" {
		t.Fatalf("expected only d1 remaining, got %v", drafts)
	}

	// 7. Save empty/whitespace content deletes the draft
	if err := d.SaveFormDraft(repo.ID, "c1", "d1", "foo.go", "R", 10, 10, 0, 0, true, "", "", "   "); err != nil {
		t.Fatalf("SaveFormDraft whitespace delete failed: %v", err)
	}
	drafts, err = d.FormDraftsForChange(repo.ID, "c1")
	if err != nil || len(drafts) != 0 {
		t.Fatalf("expected 0 drafts after empty save, got %d", len(drafts))
	}

	// 8. StaleChangeKeys and PruneChanges with form_drafts
	if err := d.SaveFormDraft(repo.ID, "stale-change", "d-stale", "stale.go", "R", 1, 1, 0, 0, true, "", "", "stale content"); err != nil {
		t.Fatalf("SaveFormDraft stale failed: %v", err)
	}
	staleKeys, err := d.StaleChangeKeys(repo.ID, map[string]bool{"active-change": true})
	if err != nil {
		t.Fatalf("StaleChangeKeys failed: %v", err)
	}
	foundStale := false
	for _, k := range staleKeys {
		if k == "stale-change" {
			foundStale = true
			break
		}
	}
	if !foundStale {
		t.Errorf("expected stale-change in StaleChangeKeys, got %v", staleKeys)
	}

	pruned, err := d.PruneChanges(repo.ID, map[string]bool{"active-change": true})
	if err != nil {
		t.Fatalf("PruneChanges failed: %v", err)
	}
	if len(pruned) == 0 {
		t.Errorf("expected pruned changes, got 0")
	}
	drafts, err = d.FormDraftsForChange(repo.ID, "stale-change")
	if err != nil || len(drafts) != 0 {
		t.Errorf("expected 0 drafts after PruneChanges, got %d", len(drafts))
	}
}

func TestConcurrentDBWriteTransactions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "review.db")

	d1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open d1 failed: %v", err)
	}
	defer d1.Close()

	d2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open d2 failed: %v", err)
	}
	defer d2.Close()

	repo, err := d1.EnsureRepo("/path/to/concurrent-repo")
	if err != nil {
		t.Fatalf("EnsureRepo failed: %v", err)
	}

	const iters = 25
	errCh := make(chan error, 4)
	var wg sync.WaitGroup

	// Worker 1 (on d1): TakeSnapshot and AddThreadRange
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			c := &vcs.Change{
				Key:    fmt.Sprintf("change-%d", i%5),
				Rev:    fmt.Sprintf("rev-%d-%d", i%5, i),
				Parent: fmt.Sprintf("prev-%d", i),
			}
			if _, _, err := d1.TakeSnapshot(repo.ID, c); err != nil {
				errCh <- fmt.Errorf("d1 TakeSnapshot %d failed: %w", i, err)
				return
			}
			if _, _, err := d1.AddThreadRange(repo.ID, c.Key, c.Rev, 1, "main.go", "R", 10, 12, 0, 0, "anchor", "author", "comment", false); err != nil {
				errCh <- fmt.Errorf("d1 AddThreadRange %d failed: %w", i, err)
				return
			}
		}
	}()

	// Worker 2 (on d2): SaveFormDraft and DeleteFormDraft
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			changeKey := fmt.Sprintf("change-%d", i%5)
			draftKey := fmt.Sprintf("draft-%d", i)
			if err := d2.SaveFormDraft(repo.ID, changeKey, draftKey, "foo.go", "R", i+1, i+1, 0, 0, true, "", "", fmt.Sprintf("draft body %d", i)); err != nil {
				errCh <- fmt.Errorf("d2 SaveFormDraft %d failed: %w", i, err)
				return
			}
			if i%2 == 0 {
				if err := d2.DeleteFormDraft(repo.ID, changeKey, draftKey); err != nil {
					errCh <- fmt.Errorf("d2 DeleteFormDraft %d failed: %w", i, err)
					return
				}
			}
		}
	}()

	// Worker 3 (on d2): SetFileMark and StateDigest
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			changeKey := fmt.Sprintf("change-%d", i%5)
			rev := fmt.Sprintf("rev-%d-%d", i%5, i)
			if err := d2.SetFileReviewed(repo.ID, changeKey, rev, "main.go", i%2 == 0); err != nil {
				errCh <- fmt.Errorf("d2 SetFileReviewed %d failed: %w", i, err)
				return
			}
			_ = d2.StateDigest()
		}
	}()

	// Worker 4 (on d1): PruneChanges
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters/2; i++ {
			time.Sleep(2 * time.Millisecond)
			_, _ = d1.PruneChanges(repo.ID, map[string]bool{"change-0": true, "change-1": true})
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func FuzzDBDraftsAndDigest(f *testing.F) {
	f.Add("change-1", "draft-1", "path/to/file.go", "R", 10, "content here", "author", "body text")
	f.Add("", "", "", "", 0, "", "", "")
	f.Add("c\x00key", "d'quote", "file with space.go", "L", -5, "draft\nmultiline\r\n", "user@example.com", "body \x00 null")

	f.Fuzz(func(t *testing.T, changeKey, draftKey, filePath, side string, line int, content, author, body string) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "fuzz.db")
		d, err := Open(dbPath)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer d.Close()

		repo, err := d.EnsureRepo("/path/to/fuzz-repo")
		if err != nil {
			t.Fatalf("EnsureRepo failed: %v", err)
		}

		// Save draft
		_ = d.SaveFormDraft(repo.ID, changeKey, draftKey, filePath, side, line, line, 0, 0, true, "", "", content)
		_, _ = d.FormDraftsForChange(repo.ID, changeKey)

		// Add thread
		_, _, _ = d.AddThreadRange(repo.ID, changeKey, "rev-1", 1, filePath, side, line, line, 0, 0, "anchor", author, body, false)

		// Digest
		dig := d.StateDigest()
		if len(dig) != 64 {
			t.Fatalf("expected 64-char sha256 digest, got len %d: %q", len(dig), dig)
		}

		// Prune
		_, _ = d.PruneChanges(repo.ID, map[string]bool{changeKey: true})
	})
}
