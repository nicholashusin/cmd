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

func TestCLISnapshotTargetAndGCRedundantSnapshots(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "CLI Stack")
	runGit("config", "user.email", "clistack@example.com")

	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "base.txt")
	runGit("commit", "-m", "Base")

	const keyC1 = "I1111111111111111111111111111111111111111"
	const keyC2 = "I2222222222222222222222222222222222222222"

	if err := os.WriteFile(filepath.Join(repoDir, "c1.txt"), []byte("c1 v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "c1.txt")
	runGit("commit", "-m", "C1\n\nChange-Id: "+keyC1)
	revC1v1 := runGit("rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repoDir, "c2.txt"), []byte("c2 v1 line 1\nc2 v1 line 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "c2.txt")
	runGit("commit", "-m", "C2\n\nChange-Id: "+keyC2)
	revC2s1 := runGit("rev-parse", "HEAD")

	dbFile := filepath.Join(t.TempDir(), "cli_gc.db")
	env := &Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		WorkDir: repoDir,
	}

	// 1. Snapshot both C1 and C2 initially
	if err := env.Run([]string{"snapshot", "-db", dbFile, keyC1, keyC2}); err != nil {
		t.Fatalf("initial snapshot failed: %v", err)
	}

	// 2. Amend C1 and rebase C2 -> `review snapshot <keyC1>` records Snapshot 2 for C1 only;
	//    `review snapshot <keyC2>` carries forward Snapshot 1 without creating Snapshot 2.
	runGit("checkout", revC1v1)
	if err := os.WriteFile(filepath.Join(repoDir, "c1.txt"), []byte("c1 v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("commit", "-a", "--amend", "--no-edit")
	revC1v2 := runGit("rev-parse", "HEAD")
	runGit("cherry-pick", revC2s1)
	revC2s2 := runGit("rev-parse", "HEAD")

	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"snapshot", "-db", dbFile, keyC1}); err != nil {
		t.Fatalf("snapshot keyC1 failed: %v", err)
	}
	if out := env.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "Recorded Snapshot 2 for "+keyC1) {
		t.Errorf("expected Recorded Snapshot 2 for %s, got: %s", keyC1, out)
	}

	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"snapshots", "-db", dbFile, keyC2}); err != nil {
		t.Fatalf("snapshots keyC2 before snapshot command failed: %v", err)
	}
	if out := env.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "Snapshot 1: rev "+revC2s2) {
		t.Errorf("expected `review snapshots` to sync rebased Snapshot 1 to %s, got: %s", revC2s2, out)
	}

	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"snapshot", "-db", dbFile, keyC2}); err != nil {
		t.Fatalf("snapshot keyC2 failed: %v", err)
	}
	if out := env.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "Snapshot 1 is already up to date for "+keyC2) {
		t.Errorf("expected Snapshot 1 up to date for %s, got: %s", keyC2, out)
	}

	// 3. Seed pre-existing redundant snapshots 1..4 on C2 (1 & 2 equivalent; 3 & 4 equivalent) with comments & pins
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := d.EnsureRepo(repoDir)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	// Reset C2 snapshots to test 1..4 GC collapse
	repo, err := vcs.Open(repoDir, nil)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}

	// Add comment on C2 Snapshot 1 (revC2s2)
	if err := env.Run([]string{"add", "-from", "alice", "-db", dbFile, keyC2, "c2.txt:1", "comment on group 1"}); err != nil {
		d.Close()
		t.Fatalf("add comment 1: %v", err)
	}

	// Rebase C2 again (no own diff) and force-insert redundant Snapshot 2 via db.TakeSnapshot
	runGit("checkout", revC1v2)
	if err := os.WriteFile(filepath.Join(repoDir, "c1.txt"), []byte("c1 v3\n"), 0644); err != nil {
		d.Close()
		t.Fatal(err)
	}
	runGit("commit", "-a", "--amend", "--no-edit")
	revC1v3 := runGit("rev-parse", "HEAD")
	runGit("cherry-pick", revC2s2)
	repo2, _ := vcs.Open(repoDir, nil)
	c2Redundant2, _ := repo2.Commit("HEAD")
	if _, _, err := d.TakeSnapshot(rec.ID, c2Redundant2); err != nil {
		d.Close()
		t.Fatal(err)
	}
	_ = repo2.Pin(keyC2+"/snap-2", c2Redundant2.Rev)

	// Real edit on C2 -> Snapshot 3
	if err := os.WriteFile(filepath.Join(repoDir, "c2.txt"), []byte("c2 v2 line 1\nc2 v1 line 2\n"), 0644); err != nil {
		d.Close()
		t.Fatal(err)
	}
	runGit("commit", "-a", "--amend", "--no-edit")
	repo3, _ := vcs.Open(repoDir, nil)
	c2Edit3, _ := repo3.Commit("HEAD")
	if _, _, err := d.TakeSnapshot(rec.ID, c2Edit3); err != nil {
		d.Close()
		t.Fatal(err)
	}
	_ = repo3.Pin(keyC2+"/snap-3", c2Edit3.Rev)
	if err := env.Run([]string{"add", "-from", "bob", "-db", dbFile, keyC2, "c2.txt:1", "comment on group 2"}); err != nil {
		d.Close()
		t.Fatalf("add comment 2: %v", err)
	}

	// Rebase C2 v2 (no own diff vs Snapshot 3) -> redundant Snapshot 4
	runGit("checkout", revC1v3)
	if err := os.WriteFile(filepath.Join(repoDir, "c1.txt"), []byte("c1 v4\n"), 0644); err != nil {
		d.Close()
		t.Fatal(err)
	}
	runGit("commit", "-a", "--amend", "--no-edit")
	runGit("cherry-pick", c2Edit3.Rev)
	repo4, _ := vcs.Open(repoDir, nil)
	c2Redundant4, _ := repo4.Commit("HEAD")
	if _, _, err := d.TakeSnapshot(rec.ID, c2Redundant4); err != nil {
		d.Close()
		t.Fatal(err)
	}
	_ = repo4.Pin(keyC2+"/snap-4", c2Redundant4.Rev)
	// Keep C1's single latest rev aligned or snapshot C1 so C1 has no redundant snaps to keep assertions crisp
	d.Close()
	_ = repo

	// 4. `review gc -dry-run`
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"gc", "-dry-run", "-db", dbFile}); err != nil {
		t.Fatalf("gc -dry-run failed: %v", err)
	}
	dryOut := env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(dryOut, "[dry-run] Pruned redundant snapshot: "+rec.Slug+"/"+keyC2+"/snap-1") ||
		!strings.Contains(dryOut, "[dry-run] Pruned redundant snapshot: "+rec.Slug+"/"+keyC2+"/snap-3") ||
		!strings.Contains(dryOut, "[dry-run] Pruned VCS pin: refs/reviewed/"+keyC2+"/snap-3") ||
		!strings.Contains(dryOut, "[dry-run] Pruned VCS pin: refs/reviewed/"+keyC2+"/snap-4") {
		t.Errorf("unexpected gc -dry-run output:\n%s", dryOut)
	}

	// 5. Real `review gc`
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"gc", "-db", dbFile}); err != nil {
		t.Fatalf("gc failed: %v", err)
	}
	gcOut := env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(gcOut, "Pruned redundant snapshot: "+rec.Slug+"/"+keyC2+"/snap-1") ||
		!strings.Contains(gcOut, "Pruned redundant snapshot: "+rec.Slug+"/"+keyC2+"/snap-3") ||
		!strings.Contains(gcOut, "Pruned VCS pin: refs/reviewed/"+keyC2+"/snap-3") ||
		!strings.Contains(gcOut, "Pruned VCS pin: refs/reviewed/"+keyC2+"/snap-4") {
		t.Errorf("unexpected gc output:\n%s", gcOut)
	}

	// Verify `review snapshots` on C2 shows Snapshot 1 and Snapshot 2 only
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"snapshots", "-db", dbFile, keyC2}); err != nil {
		t.Fatalf("snapshots failed: %v", err)
	}
	snapsOut := env.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(snapsOut, "Snapshot 1: rev "+c2Redundant2.Rev) ||
		!strings.Contains(snapsOut, "Snapshot 2: rev "+c2Redundant4.Rev) ||
		strings.Contains(snapsOut, "Snapshot 3:") || strings.Contains(snapsOut, "Snapshot 4:") {
		t.Errorf("unexpected snapshots output after gc:\n%s", snapsOut)
	}

	// Verify `review comments -json` preserves both threads
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"comments", "-json", "-db", dbFile, keyC2}); err != nil {
		t.Fatalf("comments -json failed: %v", err)
	}
	var commentsOut []struct {
		ThreadID int64 `json:"thread_id"`
		Stale    bool  `json:"stale"`
	}
	if err := json.Unmarshal(env.Stdout.(*bytes.Buffer).Bytes(), &commentsOut); err != nil {
		t.Fatalf("decode comments json: %v", err)
	}
	if len(commentsOut) != 2 {
		t.Errorf("expected 2 preserved comments on C2 after gc, got %+v", commentsOut)
	}

	// Second `review gc` has nothing left to clean up
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"gc", "-db", dbFile}); err != nil {
		t.Fatalf("second gc failed: %v", err)
	}
	if out2 := env.Stdout.(*bytes.Buffer).String(); !strings.Contains(out2, "Nothing to clean up.") {
		t.Errorf("expected 'Nothing to clean up.' on second gc, got:\n%s", out2)
	}

	t.Run("CommentsJSONAfter40LineUpstreamRebaseWithoutPriorSync", func(t *testing.T) {
		dirR := t.TempDir()
		runR := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dirR
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
			}
			return strings.TrimSpace(string(out))
		}
		runR("init", "-b", "main")
		runR("config", "user.name", "CLI Rebase")
		runR("config", "user.email", "clirebase@example.com")

		baseLines := make([]string, 60)
		for i := range baseLines {
			baseLines[i] = "// line " + string(rune('a'+i%26))
		}
		writeLines := func(path string, lines []string) {
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		sharedPath := filepath.Join(dirR, "shared.go")
		writeLines(sharedPath, baseLines)
		runR("add", "shared.go")
		runR("commit", "-m", "Base")

		const k1 = "I9111111111111111111111111111111111111111"
		const k2 = "I9222222222222222222222222222222222222222"
		if err := os.WriteFile(filepath.Join(dirR, "c1.txt"), []byte("c1\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runR("add", "c1.txt")
		runR("commit", "-m", "C1\n\nChange-Id: "+k1)
		rev1 := runR("rev-parse", "HEAD")

		c2Lines := append([]string(nil), baseLines...)
		c2Lines[49] = "// unique c2 line 50"
		writeLines(sharedPath, c2Lines)
		runR("commit", "-a", "-m", "C2\n\nChange-Id: "+k2)

		dbR := filepath.Join(t.TempDir(), "cli_rebase_comments.db")
		envR := &Env{
			Stdin:   strings.NewReader(""),
			Stdout:  &bytes.Buffer{},
			Stderr:  &bytes.Buffer{},
			WorkDir: dirR,
		}
		if err := envR.Run([]string{"snapshot", "-db", dbR, k2}); err != nil {
			t.Fatalf("snapshot k2: %v", err)
		}
		if err := envR.Run([]string{"add", "-from", "alice", "-db", dbR, k2, "shared.go:50", "check shifted line"}); err != nil {
			t.Fatalf("add k2: %v", err)
		}

		// Amend C1 inserting 40 lines at top of shared.go and rebase C2 without any prior sync/view
		runR("checkout", rev1)
		var c1Lines []string
		for i := 0; i < 40; i++ {
			c1Lines = append(c1Lines, "// upstream insert")
		}
		c1Lines = append(c1Lines, baseLines...)
		writeLines(sharedPath, c1Lines)
		runR("commit", "-a", "--amend", "--no-edit")

		c2RebasedLines := append([]string(nil), c1Lines...)
		c2RebasedLines[40+49] = "// unique c2 line 50"
		writeLines(sharedPath, c2RebasedLines)
		runR("commit", "-a", "-m", "C2\n\nChange-Id: "+k2)

		envR.Stdout.(*bytes.Buffer).Reset()
		if err := envR.Run([]string{"comments", "-json", "-db", dbR, k2}); err != nil {
			t.Fatalf("comments -json: %v", err)
		}
		var outComments []struct {
			CurrentLine    int  `json:"current_line"`
			CurrentEndLine int  `json:"current_end_line"`
			Stale          bool `json:"stale"`
		}
		if err := json.Unmarshal(envR.Stdout.(*bytes.Buffer).Bytes(), &outComments); err != nil {
			t.Fatalf("unmarshal comments json: %v", err)
		}
		if len(outComments) != 1 || outComments[0].CurrentLine != 90 || outComments[0].CurrentEndLine != 90 || outComments[0].Stale {
			t.Fatalf("expected non-stale comment shifted to line 90, got %+v", outComments)
		}
	})
}

func TestCLIEdgeCases(t *testing.T) {
	env, dbFile, repoDir := setupTestCLIEnv(t)

	// 1. comments -json with zero comments emits "[]", never "null".
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"comments", "-json", "-db", dbFile}); err != nil {
		t.Fatalf("comments -json failed: %v", err)
	}
	if got := strings.TrimSpace(env.Stdout.(*bytes.Buffer).String()); got != "[]" {
		t.Errorf("empty comments -json = %q, want %q", got, "[]")
	}

	// 2. add rejects invalid -side and malformed line ranges.
	if err := env.Run([]string{"add", "-side", "X", "-db", dbFile, "HEAD", "hello.txt:1", "hi"}); err == nil {
		t.Errorf("expected error for invalid -side X")
	}
	if err := env.Run([]string{"add", "-db", dbFile, "HEAD", "hello.txt:10-5", "hi"}); err == nil {
		t.Errorf("expected error for reversed line range 10-5")
	}
	if err := env.Run([]string{"add", "-db", dbFile, "HEAD", "hello.txt:0", "hi"}); err == nil {
		t.Errorf("expected error for line 0")
	}

	// 3. unresolve error message says "unresolve" and validates all IDs before mutating.
	if err := env.Run([]string{"add", "-db", dbFile, "HEAD", "hello.txt:1", "comment 1"}); err != nil {
		t.Fatal(err)
	}
	if err := env.Run([]string{"unresolve", "-db", dbFile}); err == nil || !strings.Contains(err.Error(), "unresolve") {
		t.Errorf("expected unresolve usage error, got %v", err)
	}
	if err := env.Run([]string{"resolve", "-db", dbFile, "1", "notanint"}); err == nil {
		t.Fatalf("expected error for invalid second thread ID")
	}
	// Thread 1 must still be open since validation happened before mutation.
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	th, err := d.ThreadByID(1)
	d.Close()
	if err != nil || th == nil || th.Resolved {
		t.Errorf("thread #1 was mutated despite invalid second ID: %+v (err=%v)", th, err)
	}

	// 4. forget works from a subdirectory of the repo.
	subDir := filepath.Join(repoDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	subEnv := &Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		WorkDir: subDir,
	}
	if err := subEnv.Run([]string{"forget", "-db", dbFile}); err != nil {
		t.Fatalf("forget from subdir failed: %v", err)
	}
	if out := subEnv.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "Forgot repository") {
		t.Errorf("expected forget from subdir to find repo root, got: %s", out)
	}

	// 5. search does not create DB or register repo.
	unusedDB := filepath.Join(t.TempDir(), "should-not-exist.db")
	env.Stdout.(*bytes.Buffer).Reset()
	if err := env.Run([]string{"search", "-db", unusedDB, "line 2"}); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if _, err := os.Stat(unusedDB); !os.IsNotExist(err) {
		t.Errorf("review search unexpectedly created DB %s", unusedDB)
	}

	// 6. subcommand -h returns nil (exit 0), not flag.ErrHelp error.
	if err := env.Run([]string{"comments", "-h"}); err != nil {
		t.Errorf("comments -h returned error %v, want nil", err)
	}
}
