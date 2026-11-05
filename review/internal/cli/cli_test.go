package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"nicholas.sh/cmd/review/internal/db"
	"nicholas.sh/cmd/review/internal/engine"
	"nicholas.sh/cmd/review/internal/vcs"
)

func setupTestCLIEnv(t *testing.T) (*Env, string, string) {
	repoDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.name", "CLI Tester")
	run("config", "user.email", "clitester@example.com")

	filePath := filepath.Join(repoDir, "hello.txt")
	if err := os.WriteFile(filePath, []byte("line 1\nline 2\nline 3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello.txt")
	run("commit", "-m", "Initial commit\n\nChange-Id: I1234123412341234123412341234123412341234")

	dbFile := filepath.Join(t.TempDir(), "test_review.db")

	env := &Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		WorkDir: repoDir,
	}

	return env, dbFile, repoDir
}

func TestCLISnapshotAndComments(t *testing.T) {
	env, dbFile, repoDir := setupTestCLIEnv(t)

	// 1. Snapshot
	err := env.Run([]string{"snapshot", "-db", dbFile})
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	out := env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Recorded Snapshot 1") {
		t.Errorf("expected 'Recorded Snapshot 1', got: %s", out)
	}

	// 2. Snapshots list
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"snapshots", "-db", dbFile})
	if err != nil {
		t.Fatalf("snapshots failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Snapshot 1:") {
		t.Errorf("expected 'Snapshot 1:', got: %s", out)
	}

	// 3. Add comment (as draft)
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"add", "-draft", "-db", dbFile, "HEAD", "hello.txt:2", "Please check line 2"})
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Added comment thread #1") {
		t.Errorf("expected thread #1, got: %s", out)
	}

	// 4. Comments text (with -drafts)
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"comments", "-drafts", "-db", dbFile})
	if err != nil {
		t.Fatalf("comments failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "[thread #1] hello.txt:2") || !strings.Contains(out, "[msg #1]") {
		t.Errorf("expected thread #1 with msg #1, got: %s", out)
	}

	// 5. Comments -json (with -drafts)
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"comments", "-json", "-drafts", "-db", dbFile})
	if err != nil {
		t.Fatalf("comments -json failed: %v", err)
	}
	var threads []map[string]any
	if err := json.Unmarshal(env.Stdout.(*bytes.Buffer).Bytes(), &threads); err != nil {
		t.Fatalf("parsing comments json: %v", err)
	}
	if len(threads) != 1 || threads[0]["file"] != "hello.txt" {
		t.Errorf("unexpected json output: %+v", threads)
	}

	// 6. Edit message
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"edit", "-db", dbFile, "1", "Updated comment text"})
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Updated message #1: Updated comment text") {
		t.Errorf("unexpected edit output: %s", out)
	}

	// 7. Reply (as draft)
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"reply", "-draft", "-db", dbFile, "1", "Reply here"})
	if err != nil {
		t.Fatalf("reply failed: %v", err)
	}

	// 8. Resolve
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"resolve", "-db", dbFile, "1"})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Resolved thread #1") {
		t.Errorf("expected 'Resolved thread #1', got: %s", out)
	}

	// 9. Publish
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"publish", "-db", dbFile})
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Published 2 draft comments") {
		t.Errorf("expected 2 published drafts, got: %s", out)
	}

	// Publishing already-snapshotted drafts above should keep Snapshot 1 (not create Snapshot 2).
	dPub, err := db.Open(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	recPub, err := dPub.EnsureRepo(repoDir)
	if err != nil {
		dPub.Close()
		t.Fatal(err)
	}
	repoPub, err := vcs.Open(repoDir, nil)
	if err != nil {
		dPub.Close()
		t.Fatal(err)
	}
	cPub, err := resolveChange(repoPub, "HEAD")
	if err != nil {
		dPub.Close()
		t.Fatal(err)
	}
	snapsPub, err := dPub.Snapshots(recPub.ID, cPub.Key)
	if err != nil || len(snapsPub) != 1 {
		dPub.Close()
		t.Fatalf("expected 1 snapshot without redundant snapshot after publish, got %+v (err=%v)", snapsPub, err)
	}
	dPub.Close()

	// Amend commit and add a draft on pending live commit -> `review publish HEAD` auto-takes Snapshot 2.
	if err := os.WriteFile(filepath.Join(repoDir, "hello.txt"), []byte("line 1\nline 2\nline 3 v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmdAmend := exec.Command("git", "commit", "-a", "--amend", "--no-edit")
	cmdAmend.Dir = repoDir
	if outAmend, err := cmdAmend.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend failed: %s (%v)", string(outAmend), err)
	}
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"add", "-draft", "-db", dbFile, "HEAD", "hello.txt:3", "draft on v2"}); err != nil {
		t.Fatalf("add draft on v2 failed: %v", err)
	}
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"publish", "-db", dbFile, "HEAD"}); err != nil {
		t.Fatalf("publish HEAD failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Published 1 draft comments") {
		t.Errorf("expected 1 published draft, got: %s", out)
	}
	dPub2, err := db.Open(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer dPub2.Close()
	snapsPub2, err := dPub2.Snapshots(recPub.ID, cPub.Key)
	if err != nil || len(snapsPub2) != 2 {
		t.Fatalf("expected auto-snapshot 2 after publishing live draft, got %+v (err=%v)", snapsPub2, err)
	}
	threadsPub2, _ := dPub2.ThreadsForChange(recPub.ID, cPub.Key)
	if len(threadsPub2) < 2 || threadsPub2[len(threadsPub2)-1].SnapshotNum != 2 {
		t.Errorf("expected v2 thread assigned to snapshot 2, got %+v", threadsPub2)
	}

	// 10. Search
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"search", "-db", dbFile, "line 2"})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "hello.txt:") || !strings.Contains(out, "line 2") {
		t.Errorf("expected search match, got: %s", out)
	}

	// 11. GC
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"gc", "-dry-run", "-db", dbFile})
	if err != nil {
		t.Fatalf("gc failed: %v", err)
	}

	// 12. Forget
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"forget", "-db", dbFile})
	if err != nil {
		t.Fatalf("forget failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Forgot repository") {
		t.Errorf("expected forgot repository, got: %s", out)
	}
}

func TestCLICommentsOrderingAndDraftHiding(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.name", "Stack Tester")
	run("config", "user.email", "stack@example.com")

	// Commit 1: ancestor
	if err := os.WriteFile(filepath.Join(repoDir, "file1.txt"), []byte("line 1\nline 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "file1.txt")
	run("commit", "-m", "Commit 1\n\nChange-Id: I1111111111111111111111111111111111111111")

	// Commit 2: child
	if err := os.WriteFile(filepath.Join(repoDir, "file2.txt"), []byte("item A\nitem B\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "file2.txt")
	run("commit", "-m", "Commit 2\n\nChange-Id: I2222222222222222222222222222222222222222")

	dbFile := filepath.Join(t.TempDir(), "test_order.db")
	env := &Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		WorkDir: repoDir,
	}

	// 1. Add published comment on Commit 2 (child)
	err := env.Run([]string{"add", "-from", "alice", "-db", dbFile, "HEAD", "file2.txt:1", "child comment"})
	if err != nil {
		t.Fatalf("add child comment failed: %v", err)
	}

	// 2. Add published comment on Commit 1 (ancestor)
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"add", "-from", "bob", "-db", dbFile, "HEAD~1", "file1.txt:1", "ancestor comment"})
	if err != nil {
		t.Fatalf("add ancestor comment failed: %v", err)
	}

	// 3. Add draft comment on Commit 2
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"add", "-draft", "-from", "charlie", "-db", dbFile, "HEAD", "file2.txt:2", "draft comment"})
	if err != nil {
		t.Fatalf("add draft comment failed: %v", err)
	}

	// 4. Default comments (without -drafts): must hide draft and order ancestor first!
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"comments", "-json", "-db", dbFile})
	if err != nil {
		t.Fatalf("comments -json failed: %v", err)
	}

	var publishedThreads []map[string]any
	if err := json.Unmarshal(env.Stdout.(*bytes.Buffer).Bytes(), &publishedThreads); err != nil {
		t.Fatalf("parsing comments json: %v", err)
	}

	if len(publishedThreads) != 2 {
		t.Fatalf("expected exactly 2 published threads, got %d: %+v", len(publishedThreads), publishedThreads)
	}

	// Ancestor (file1.txt) must come before Child (file2.txt)
	if publishedThreads[0]["file"] != "file1.txt" {
		t.Errorf("expected first thread to be ancestor file1.txt, got %v", publishedThreads[0]["file"])
	}
	if publishedThreads[1]["file"] != "file2.txt" {
		t.Errorf("expected second thread to be child file2.txt, got %v", publishedThreads[1]["file"])
	}

	// 5. With -drafts: all 3 comments must appear
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"comments", "-json", "-drafts", "-db", dbFile})
	if err != nil {
		t.Fatalf("comments -json -drafts failed: %v", err)
	}
	var allThreads []map[string]any
	if err := json.Unmarshal(env.Stdout.(*bytes.Buffer).Bytes(), &allThreads); err != nil {
		t.Fatalf("parsing all comments json: %v", err)
	}
	if len(allThreads) != 3 {
		t.Fatalf("expected 3 threads with drafts, got %d", len(allThreads))
	}

	// 6. Context lines in human-readable output
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"comments", "-c", "1", "-db", dbFile})
	if err != nil {
		t.Fatalf("comments -c 1 failed: %v", err)
	}
	out := env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, ">    1 | line 1") {
		t.Errorf("expected context line with marker, got:\n%s", out)
	}

	// 7. Test reply -resolve
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"reply", "-resolve", "-db", dbFile, "1", "fixed and done"})
	if err != nil {
		t.Fatalf("reply -resolve failed: %v", err)
	}
	out = env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "resolved") {
		t.Errorf("expected resolve message, got: %s", out)
	}

	// Thread 1 is now resolved; by default comments hides resolved threads
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"comments", "-json", "-db", dbFile})
	if err != nil {
		t.Fatalf("comments after resolve failed: %v", err)
	}
	var unresolved []map[string]any
	if err := json.Unmarshal(env.Stdout.(*bytes.Buffer).Bytes(), &unresolved); err != nil {
		t.Fatalf("parsing unresolved comments: %v", err)
	}
	if len(unresolved) != 1 || unresolved[0]["file"] != "file1.txt" {
		t.Errorf("expected only file1.txt unresolved, got: %+v", unresolved)
	}
}

func TestCLIReplyValidation(t *testing.T) {
	env, dbFile, _ := setupTestCLIEnv(t)

	// Create a comment thread #1
	err := env.Run([]string{"add", "-db", dbFile, "HEAD", "hello.txt:1", "Initial comment"})
	if err != nil {
		t.Fatalf("add comment failed: %v", err)
	}

	// 1. Reply with no text and no -resolve should fail
	err = env.Run([]string{"reply", "-db", dbFile, "1"})
	if err == nil || !strings.Contains(err.Error(), "reply text required") {
		t.Fatalf("expected 'reply text required' error, got: %v", err)
	}

	// 2. Reply with -resolve and no text should succeed
	env.Stdout.(*bytes.Buffer).Reset()
	err = env.Run([]string{"reply", "-resolve", "-db", dbFile, "1"})
	if err != nil {
		t.Fatalf("reply -resolve without text failed: %v", err)
	}
	out := env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "Marked thread #1 as resolved") {
		t.Errorf("expected resolution output, got: %s", out)
	}

	// 3. Reply to nonexistent thread should fail
	err = env.Run([]string{"reply", "-resolve", "-db", dbFile, "99999"})
	if err == nil {
		t.Fatalf("expected error resolving nonexistent thread, got nil")
	}

	err = env.Run([]string{"reply", "-db", dbFile, "99999", "some reply"})
	if err == nil {
		t.Fatalf("expected error replying to nonexistent thread, got nil")
	}
}

func TestCLICommitMsgAddSideLAndCommentsStale(t *testing.T) {
	repoDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.name", "CLI Tester")
	run("config", "user.email", "clitester@example.com")

	if err := os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("parent\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "Parent commit\n\nChange-Id: Iaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("child\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "b.txt")
	run("commit", "-m", "Child commit\n\nChange-Id: Ibbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	dbFile := filepath.Join(t.TempDir(), "test_commit_msg.db")
	env := &Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		WorkDir: repoDir,
	}

	// Snapshot 1 of child commit.
	if err := env.Run([]string{"snapshot", "-db", dbFile, "HEAD"}); err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}

	// 1. `review add -side L HEAD COMMIT_MSG:2` must normalize to Side=R on HEAD.
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"add", "-side", "L", "-db", dbFile, "HEAD", "COMMIT_MSG:2", "note on author"}); err != nil {
		t.Fatalf("add -side L COMMIT_MSG failed: %v", err)
	}
	if out := env.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, vcs.CommitMsgPath+":2") {
		t.Errorf("expected output to report %s:2, got: %s", vcs.CommitMsgPath, out)
	}

	// 2. `review add -s 1 -side L HEAD COMMIT_MSG:2` must also target snap.Rev (not snap.ParentRev).
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"add", "-s", "1", "-side", "L", "-db", dbFile, "HEAD", "COMMIT_MSG:2", "note on snap author"}); err != nil {
		t.Fatalf("add -s 1 -side L COMMIT_MSG failed: %v", err)
	}

	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"comments", "-json", "-db", dbFile}); err != nil {
		t.Fatalf("comments -json failed: %v", err)
	}
	var listed []struct {
		File  string `json:"file"`
		Side  string `json:"side"`
		Stale bool   `json:"stale"`
	}
	if err := json.Unmarshal(env.Stdout.(*bytes.Buffer).Bytes(), &listed); err != nil {
		t.Fatalf("decode comments -json: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 threads, got %d: %+v", len(listed), listed)
	}
	for i, th := range listed {
		if th.File != vcs.CommitMsgPath || th.Side != "R" || th.Stale {
			t.Errorf("thread[%d] = %+v, want file=%q side=R stale=false", i, th, vcs.CommitMsgPath)
		}
	}

	// 3. A historical Side=L thread on /COMMIT_MSG created against snap 1 must report Stale=true in `review comments` (which diffs against c.Parent).
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := vcs.Open(repoDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := resolveChange(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.AddComment(d, repo, rec.ID, c, c.Rev, vcs.CommitMsgPath, "L", 2, 2, "reviewer", "old snapshot left side", false, false); err != nil {
		t.Fatalf("AddComment Side=L failed: %v", err)
	}

	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"comments", "-json", "-db", dbFile}); err != nil {
		t.Fatalf("comments -json failed: %v", err)
	}
	if err := json.Unmarshal(env.Stdout.(*bytes.Buffer).Bytes(), &listed); err != nil {
		t.Fatalf("decode comments -json: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("expected 3 threads, got %d", len(listed))
	}
	var staleLeftFound bool
	for _, th := range listed {
		if th.Side == "L" && th.File == vcs.CommitMsgPath {
			staleLeftFound = true
			if !th.Stale {
				t.Errorf("expected historical Side=L /COMMIT_MSG thread to be Stale=true against c.Parent")
			}
		}
	}
	if !staleLeftFound {
		t.Errorf("expected to find Side=L /COMMIT_MSG thread in %+v", listed)
	}
}

func TestCLIUnknownSubcommandAndFlags(t *testing.T) {
	env, dbFile, _ := setupTestCLIEnv(t)

	// 1. Unknown subcommand must return unknown subcommand error.
	if err := env.Run([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("review bogus error = %v, want error containing %q", err, "unknown subcommand")
	}

	// 2. Explicit unknown change argument to snapshot must return resolving change error.
	if err := env.Run([]string{"snapshot", "-db", dbFile, "nonexistent-rev"}); err == nil || !strings.Contains(err.Error(), "resolving change nonexistent-rev") {
		t.Fatalf("review snapshot nonexistent-rev error = %v, want error containing %q", err, "resolving change nonexistent-rev")
	}

	// 3. Add usage includes [-s snap] and help output matches refined CLI.
	if err := env.Run([]string{"add", "-db", dbFile, "HEAD"}); err == nil || !strings.Contains(err.Error(), "[-s snap]") {
		t.Fatalf("review add usage error = %v, want error mentioning [-s snap]", err)
	}

	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"help"}); err != nil {
		t.Fatalf("review help failed: %v", err)
	}
	helpOut := env.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{
		"local code review tool for Go code in Jujutsu and Git",
		"review restart [-a addr] [-n] [-db f]",
		"List review comments (-json, -all, -drafts, -s, -c)",
		"Add a comment (-from, -side, -s, -draft)",
	} {
		if !strings.Contains(helpOut, want) {
			t.Errorf("help output missing %q:\n%s", want, helpOut)
		}
	}
}
