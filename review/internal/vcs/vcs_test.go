package vcs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildGraphBridging(t *testing.T) {
	c1 := &Change{Key: "k1", Rev: "c1", Subject: "Commit 1"}
	c2 := &Change{Key: "k2", Rev: "c2", Parent: "c1", Subject: "Commit 2 (empty)", Empty: true}
	c3 := &Change{Key: "k3", Rev: "c3", Parent: "c2", Subject: "Commit 3"}

	changes := []*Change{c3, c2, c1}
	rows := BuildGraph(changes, nil) // Default filters out Empty commits

	if len(rows) != 2 {
		t.Fatalf("expected 2 visible rows, got %d", len(rows))
	}
	if rows[0].Change.Rev != "c3" || rows[1].Change.Rev != "c1" {
		t.Errorf("unexpected rows order: %s, %s", rows[0].Change.Rev, rows[1].Change.Rev)
	}
}

func TestBuildGraphFilterEmptyAndWorking(t *testing.T) {
	c1 := &Change{Key: "k1", Rev: "c1", Subject: "Commit 1", Empty: false, Working: false}
	c2 := &Change{Key: "k2", Rev: "c2", Parent: "c1", Subject: "[WIP] empty commit", Empty: true, Working: false}
	c3 := &Change{Key: "k3", Rev: "c3", Parent: "c2", Subject: "Commit 3", Empty: false, Working: false}
	c4 := &Change{Key: "k4", Rev: "c4", Parent: "c3", Subject: "(no description set)", Empty: true, Working: true}

	changes := []*Change{c4, c3, c2, c1}
	rows := BuildGraph(changes, func(c *Change) bool {
		return !c.Working && !c.Empty
	})

	if len(rows) != 2 {
		t.Fatalf("expected 2 visible rows (c3 and c1), got %d", len(rows))
	}
	if rows[0].Change.Rev != "c3" || rows[1].Change.Rev != "c1" {
		t.Errorf("unexpected rows order: %s, %s", rows[0].Change.Rev, rows[1].Change.Rev)
	}
}

func TestGitRepoOperations(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.name", "Test User")
	run("config", "user.email", "test@example.com")

	// Commit 1
	testFile := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello.txt")
	run("commit", "-m", "Initial commit\n\nChange-Id: I1111111111111111111111111111111111111111")

	// Commit 2
	if err := os.WriteFile(testFile, []byte("hello world\nsecond line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "hello.txt")
	run("commit", "-m", "Second commit\n\nChange-Id: I2222222222222222222222222222222222222222")

	cache := NewCache()
	defer cache.Evict(dir)

	repo, err := Open(dir, cache)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if repo.Kind() != "git" {
		t.Fatalf("expected kind 'git', got %s", repo.Kind())
	}

	changes, err := repo.Changes()
	if err != nil {
		t.Fatalf("Changes failed: %v", err)
	}
	if len(changes) == 0 {
		t.Fatalf("expected changes, got 0")
	}

	cLatest := changes[0]
	if cLatest.ParentSubject != "Initial commit" {
		t.Errorf("expected ParentSubject %q, got %q", "Initial commit", cLatest.ParentSubject)
	}
	if cLatest.Committer != "Test User <test@example.com>" {
		t.Errorf("expected Committer %q, got %q", "Test User <test@example.com>", cLatest.Committer)
	}
	if cLatest.CommitDate.IsZero() {
		t.Errorf("expected non-zero CommitDate")
	}

	commitMsgBytes, err := repo.Content(cLatest.Rev, CommitMsgPath)
	if err != nil {
		t.Fatalf("Content(CommitMsgPath) failed: %v", err)
	}
	commitMsgNoSlash, err := repo.Content(cLatest.Rev, "COMMIT_MSG")
	if err != nil || string(commitMsgNoSlash) != string(commitMsgBytes) {
		t.Fatalf("Content(\"COMMIT_MSG\") mismatch: err=%v, got=%q want=%q", err, string(commitMsgNoSlash), string(commitMsgBytes))
	}
	if oider, ok := repo.(BlobOIDer); ok {
		if _, err := oider.BlobOID(cLatest.Rev, "COMMIT_MSG"); err == nil {
			t.Errorf("BlobOID(COMMIT_MSG) expected unsupported error, got nil")
		}
	}
	commitMsg := string(commitMsgBytes)
	wantParentPrefix := "Parent:     " + cLatest.Parent[:8] + " (Initial commit)\n"
	if !strings.Contains(commitMsg, wantParentPrefix) {
		t.Errorf("expected /COMMIT_MSG to contain %q, got:\n%s", wantParentPrefix, commitMsg)
	}
	if !strings.Contains(commitMsg, "Author:     Test User <test@example.com>\n") ||
		!strings.Contains(commitMsg, "AuthorDate: ") ||
		!strings.Contains(commitMsg, "Commit:     Test User <test@example.com>\n") ||
		!strings.Contains(commitMsg, "CommitDate: ") {
		t.Errorf("expected metadata headers in /COMMIT_MSG, got:\n%s", commitMsg)
	}
	if strings.Contains(commitMsg, "Change-Id:  ") {
		t.Errorf("did not expect duplicate Change-Id header when Change-Id trailer is in message, got:\n%s", commitMsg)
	}

	files, err := repo.Files(cLatest.Parent, cLatest.Rev)
	if err != nil {
		t.Fatalf("Files failed: %v", err)
	}
	// files should have /COMMIT_MSG and hello.txt
	foundHello := false
	for _, f := range files {
		if f.Path == "hello.txt" {
			foundHello = true
		}
	}
	if !foundHello {
		t.Errorf("expected hello.txt in changed files")
	}

	// Content check
	content, err := repo.Content(cLatest.Rev, "hello.txt")
	if err != nil {
		t.Fatalf("Content failed: %v", err)
	}
	if !strings.Contains(string(content), "second line") {
		t.Errorf("unexpected content: %s", string(content))
	}

	// Pin check
	pinErr := repo.Pin("k2/rev-1234", cLatest.Rev)
	if pinErr != nil {
		t.Fatalf("Pin failed: %v", pinErr)
	}
	pins, err := repo.ListPins()
	if err != nil {
		t.Fatalf("ListPins failed: %v", err)
	}
	if len(pins) != 1 || !strings.Contains(pins[0], "k2/rev-1234") {
		t.Errorf("unexpected pins: %v", pins)
	}

	// PrunePins check: if k2 is NOT pending, it should be deleted
	deleted, err := PrunePins(repo, map[string]bool{})
	if err != nil {
		t.Fatalf("PrunePins failed: %v", err)
	}
	if len(deleted) != 1 {
		t.Errorf("expected 1 deleted pin, got %d", len(deleted))
	}

	// Test rename with space and path traversal security
	run("mv", "hello.txt", "my file.txt")
	testSpaceFile := filepath.Join(dir, "my file.txt")
	if err := os.WriteFile(testSpaceFile, []byte("hello world\nsecond line\nthird line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "my file.txt")
	run("commit", "-m", "Rename to space file")

	changesAfterRename, err := repo.Changes()
	if err != nil || len(changesAfterRename) == 0 {
		t.Fatalf("Changes after rename failed: %v", err)
	}
	cRenamed := changesAfterRename[0]
	renameFiles, err := repo.Files(cRenamed.Parent, cRenamed.Rev)
	if err != nil {
		t.Fatalf("Files after rename failed: %v", err)
	}

	foundRename := false
	for _, f := range renameFiles {
		if f.Path == "my file.txt" {
			foundRename = true
			if f.StatusCode() != "R" {
				t.Errorf("expected status 'R', got '%s'", f.StatusCode())
			}
			if f.OldPath != "hello.txt" {
				t.Errorf("expected OldPath 'hello.txt', got '%s'", f.OldPath)
			}
			if f.AddedLines != 1 {
				t.Errorf("expected 1 AddedLine, got %d", f.AddedLines)
			}
		}
	}
	if !foundRename {
		t.Errorf("expected 'my file.txt' in renamed files: %+v", renameFiles)
	}

	// Path traversal check on Content("working", ...)
	_, err = repo.Content("working", "../outside.txt")
	if err == nil || !strings.Contains(err.Error(), "escapes repository root") {
		t.Errorf("expected path traversal error for ../outside.txt, got %v", err)
	}
}

func TestJJWorkspaces(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}

	primaryDir := t.TempDir()
	runJJ := func(dir string, args ...string) string {
		cmd := exec.Command("jj", append([]string{"--config", "ui.paginate=\"never\""}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("jj %v in %s failed: %s (%v)", args, dir, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runJJ(primaryDir, "git", "init", "--colocate")
	runJJ(primaryDir, "config", "set", "--repo", "user.name", "JJ Tester")
	runJJ(primaryDir, "config", "set", "--repo", "user.email", "jj@example.com")

	// Create a commit in primary
	if err := os.WriteFile(filepath.Join(primaryDir, "primary.txt"), []byte("from primary\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runJJ(primaryDir, "describe", "-m", "Primary commit")

	cache := NewCache()
	defer cache.Evict(primaryDir)

	repoPrimary, err := Open(primaryDir, cache)
	if err != nil {
		t.Fatalf("Open primary failed: %v", err)
	}
	if repoPrimary.Kind() != "jj" {
		t.Fatalf("expected kind 'jj', got %s", repoPrimary.Kind())
	}

	changes, err := repoPrimary.Changes()
	if err != nil {
		t.Fatalf("Changes failed: %v", err)
	}
	if len(changes) == 0 {
		t.Fatalf("expected at least 1 change")
	}
	if changes[0].Subject != "Primary commit" {
		t.Errorf("expected Subject 'Primary commit', got %q", changes[0].Subject)
	}

	// Create secondary workspace
	secondaryDir := t.TempDir()
	runJJ(primaryDir, "workspace", "add", secondaryDir)

	repoSecondary, err := Open(secondaryDir, cache)
	if err != nil {
		t.Fatalf("Open secondary workspace failed: %v", err)
	}
	if repoSecondary.Kind() != "jj" {
		t.Fatalf("expected kind 'jj' for secondary workspace, got %s", repoSecondary.Kind())
	}

	t.Run("JJCommitMetadataAndParentSubject", func(t *testing.T) {
		rootChild := changes[0]
		if rootChild.Committer != "JJ Tester <jj@example.com>" {
			t.Errorf("expected Committer %q, got %q", "JJ Tester <jj@example.com>", rootChild.Committer)
		}
		if rootChild.CommitDate.IsZero() {
			t.Errorf("expected non-zero CommitDate")
		}
		rootMsgBytes, err := repoPrimary.Content(rootChild.Rev, CommitMsgPath)
		if err != nil {
			t.Fatalf("Content(CommitMsgPath) failed: %v", err)
		}
		rootMsg := string(rootMsgBytes)
		if strings.Contains(rootMsg, "Parent:") {
			t.Errorf("expected no Parent header on root child, got:\n%s", rootMsg)
		}
		if !strings.Contains(rootMsg, "Change-Id:  "+rootChild.Key+"\n") {
			t.Errorf("expected Change-Id header for jj change %s, got:\n%s", rootChild.Key, rootMsg)
		}

		runJJ(primaryDir, "new", "-m", "Child jj commit")
		repoChild, err := Open(primaryDir, nil)
		if err != nil {
			t.Fatalf("Open primary after new failed: %v", err)
		}
		childChanges, err := repoChild.Changes()
		if err != nil || len(childChanges) == 0 {
			t.Fatalf("Changes after new failed: %v", err)
		}
		child := childChanges[0]
		if child.ParentSubject != "Primary commit" {
			t.Errorf("expected child ParentSubject %q, got %q", "Primary commit", child.ParentSubject)
		}
		childMsgBytes, err := repoChild.Content(child.Rev, CommitMsgPath)
		if err != nil {
			t.Fatalf("Content(child, CommitMsgPath) failed: %v", err)
		}
		childMsg := string(childMsgBytes)
		wantParent := "Parent:     " + child.Parent[:8] + " (Primary commit)\n"
		if !strings.Contains(childMsg, wantParent) {
			t.Errorf("expected child /COMMIT_MSG to contain %q, got:\n%s", wantParent, childMsg)
		}
		if !strings.Contains(childMsg, "Commit:     JJ Tester <jj@example.com>\n") ||
			!strings.Contains(childMsg, "Change-Id:  "+child.Key+"\n") {
			t.Errorf("expected Commit and Change-Id headers in child /COMMIT_MSG, got:\n%s", childMsg)
		}
	})
}

func TestJJColocatedRelativeGitTarget(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}

	dir := t.TempDir()
	cmd := exec.Command("jj", "--config", "ui.paginate=\"never\"", "git", "init", "--colocate")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jj init failed: %s (%v)", string(out), err)
	}

	repo, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	jr, ok := repo.(*JJRepo)
	if !ok {
		t.Fatalf("expected *JJRepo, got %T", repo)
	}
	if !filepath.IsAbs(jr.gitDir) {
		t.Errorf("expected absolute gitDir, got: %s", jr.gitDir)
	}
	if _, err := os.Stat(jr.gitDir); err != nil {
		t.Errorf("gitDir does not exist on disk: %s (%v)", jr.gitDir, err)
	}
}

func TestJJLiveSnapshotWorkspaceEditPromptness(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}

	dir := t.TempDir()
	runJJ := func(args ...string) string {
		cmd := exec.Command("jj", append([]string{"--config", "ui.paginate=\"never\""}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("jj %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runJJ("git", "init", "--colocate")
	runJJ("config", "set", "--repo", "user.name", "JJ Tester")
	runJJ("config", "set", "--repo", "user.email", "jj@example.com")

	filePath := filepath.Join(dir, "code.txt")
	if err := os.WriteFile(filePath, []byte("initial content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runJJ("describe", "-m", "Initial commit")

	cache := NewCache()
	defer cache.Evict(dir)

	repo, err := Open(dir, cache)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	changes, err := repo.Changes()
	if err != nil {
		t.Fatalf("Changes failed: %v", err)
	}
	if len(changes) == 0 {
		t.Fatalf("expected at least 1 change")
	}
	firstRev := changes[0].Rev

	sig1 := cache.WatchSignature([]string{dir})

	// Modify file directly on disk WITHOUT running any jj commands manually
	if err := os.WriteFile(filePath, []byte("updated content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sig2 := cache.WatchSignature([]string{dir})
	_ = sig1
	_ = sig2

	// Wait 300ms (> 250ms jjCacheTTL)
	time.Sleep(300 * time.Millisecond)

	// Open a new Repo handle (simulating a new request arriving at the server)
	repo2, err := Open(dir, cache)
	if err != nil {
		t.Fatalf("Open second handle failed: %v", err)
	}

	changes2, err := repo2.Changes()
	if err != nil {
		t.Fatalf("Changes second handle failed: %v", err)
	}
	if len(changes2) == 0 {
		t.Fatalf("expected at least 1 change")
	}
	secondRev := changes2[0].Rev

	if secondRev == firstRev {
		t.Errorf("expected working copy change rev to update after disk edit and 300ms, got same rev %s", firstRev)
	}

	content, err := repo2.Content(secondRev, "code.txt")
	if err != nil {
		t.Fatalf("Content failed: %v", err)
	}
	if string(content) != "updated content\n" {
		t.Errorf("expected updated content, got %q", string(content))
	}
}

func TestGitDiffFilesSingleSubprocess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Diff Tester")
	runGit("config", "user.email", "difftester@example.com")

	if err := os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("line 1\nline 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f2.txt"), []byte("alpha\nbeta\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "del.txt"), []byte("will delete\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial commit")
	rev1 := runGit("rev-parse", "HEAD")

	// 2nd commit: modify f1, rename f2->f3, delete del.txt, add bin.dat
	if err := os.WriteFile(filepath.Join(dir, "f1.txt"), []byte("line 1\nline 2 modified\nline 3 added\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("mv", "f2.txt", "f3.txt")
	_ = os.Remove(filepath.Join(dir, "del.txt"))
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0, 1, 2, 3}, 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "second commit")
	rev2 := runGit("rev-parse", "HEAD")

	files, err := gitDiffFiles(filepath.Join(dir, ".git"), dir, rev1, rev2)
	if err != nil {
		t.Fatalf("gitDiffFiles failed: %v", err)
	}

	byPath := make(map[string]*File)
	for _, f := range files {
		byPath[f.Path] = f
	}

	if byPath[CommitMsgPath] == nil {
		t.Errorf("missing CommitMsgPath in files")
	}

	f1 := byPath["f1.txt"]
	if f1 == nil || f1.Status != FileModified || f1.AddedLines != 2 || f1.DeletedLines != 1 {
		t.Errorf("f1.txt mismatch: %+v", f1)
	}

	f3 := byPath["f3.txt"]
	if f3 == nil || f3.Status != FileRenamed || f3.OldPath != "f2.txt" {
		t.Errorf("f3.txt rename mismatch: %+v", f3)
	}

	del := byPath["del.txt"]
	if del == nil || del.Status != FileDeleted || del.DeletedLines != 1 {
		t.Errorf("del.txt mismatch: %+v", del)
	}

	bin := byPath["bin.dat"]
	if bin == nil || bin.Status != FileAdded || !bin.Binary {
		t.Errorf("bin.dat mismatch: %+v", bin)
	}
}

func TestFormatCommitMsg(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	authorDate := time.Date(2026, 3, 1, 10, 20, 30, 0, loc)
	commitDate := time.Date(2026, 3, 1, 11, 22, 33, 0, loc)

	t.Run("nil", func(t *testing.T) {
		if got := FormatCommitMsg(nil); got != "" {
			t.Errorf("FormatCommitMsg(nil) = %q, want empty", got)
		}
	})

	t.Run("full jj commit without Change-Id trailer", func(t *testing.T) {
		c := &Change{
			Key:           "kqqqzzzzxxxx",
			Rev:           "1234567890abcdef1234567890abcdef12345678",
			Parent:        "abcdef1234567890abcdef1234567890abcdef12",
			ParentSubject: "parent subject",
			Author:        "Alice <alice@example.com>",
			Date:          authorDate,
			Committer:     "Bob <bob@example.com>",
			CommitDate:    commitDate,
			Message:       "net/http: do stuff\n\nBody text.",
		}
		want := "" +
			"Parent:     abcdef12 (parent subject)\n" +
			"Author:     Alice <alice@example.com>\n" +
			"AuthorDate: 2026-03-01 10:20:30 -0500\n" +
			"Commit:     Bob <bob@example.com>\n" +
			"CommitDate: 2026-03-01 11:22:33 -0500\n" +
			"Change-Id:  kqqqzzzzxxxx\n" +
			"\n" +
			"net/http: do stuff\n\nBody text.\n"
		if got := FormatCommitMsg(c); got != want {
			t.Errorf("FormatCommitMsg mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("deduplicates existing Change-Id trailer", func(t *testing.T) {
		c := &Change{
			Key:        "I1234567890abcdef",
			Rev:        "deadbeef",
			Parent:     "abcdef1234567890",
			Author:     "Alice <alice@example.com>",
			Date:       authorDate,
			Committer:  "Alice <alice@example.com>",
			CommitDate: commitDate,
			Message:    "net/http: fix bug\n\nChange-Id: I1234567890abcdef\n",
		}
		got := FormatCommitMsg(c)
		if strings.Count(strings.ToLower(got), "change-id:") != 1 {
			t.Errorf("expected exactly 1 Change-Id line, got:\n%s", got)
		}
		if !strings.Contains(got, "Parent:     abcdef12\n") {
			t.Errorf("expected shortened parent without subject, got:\n%s", got)
		}
	})

	t.Run("root parent non-hex parent and non-change keys", func(t *testing.T) {
		cRoot := &Change{
			Key:     "working",
			Rev:     "working",
			Parent:  "0000000000000000000000000000000000000000",
			Message: "Working changes",
		}
		gotRoot := FormatCommitMsg(cRoot)
		if gotRoot != "Working changes\n" {
			t.Errorf("got %q, want %q", gotRoot, "Working changes\n")
		}

		cSHA := &Change{
			Key:     "deadbeef1234",
			Rev:     "deadbeef1234",
			Message: "",
		}
		if got := FormatCommitMsg(cSHA); got != "" {
			t.Errorf("empty commit with Key==Rev: got %q, want empty", got)
		}

		cHeadersOnly := &Change{
			Author:  "Alice <alice@example.com>",
			Message: "",
		}
		if got, want := FormatCommitMsg(cHeadersOnly), "Author:     Alice <alice@example.com>\n"; got != want {
			t.Errorf("headers without body: got %q, want %q", got, want)
		}
	})

	t.Run("cleanIdentity", func(t *testing.T) {
		cases := []struct {
			in, want string
		}{
			{"", ""},
			{"<>", ""},
			{" <> ", ""},
			{"Alice <>", "Alice"},
			{"Alice <alice@example.com>", "Alice <alice@example.com>"},
		}
		for _, tc := range cases {
			if got := cleanIdentity(tc.in); got != tc.want {
				t.Errorf("cleanIdentity(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})
}

func TestGitRepoChangesMemoization(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "Memo Tester")
	runGit("config", "user.email", "memo@example.com")
	runGit("commit", "--allow-empty", "-m", "first commit")

	repo, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ch1, err := repo.Changes()
	if err != nil {
		t.Fatalf("Changes failed: %v", err)
	}
	ch2, err := repo.Changes()
	if err != nil {
		t.Fatalf("Changes second call failed: %v", err)
	}
	if len(ch1) == 0 || len(ch2) == 0 {
		t.Fatalf("expected at least 1 change")
	}
	if &ch1[0] != &ch2[0] {
		t.Errorf("expected cached changes slice pointer to be reused")
	}
}

func TestRepoListDir(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s", args, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	runGit("config", "user.name", "ListDir Tester")
	runGit("config", "user.email", "listdir@example.com")

	if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte("package pkg\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "b.go"), []byte("package pkg\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "sub", "c.go"), []byte("package sub\n"), 0644); err != nil {
		t.Fatal(err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "first commit")
	c1Rev := runGit("rev-parse", "HEAD")

	repo, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// 1. Working copy root
	rootFiles, err := repo.ListDir("working", "")
	if err != nil {
		t.Fatalf("ListDir working root failed: %v", err)
	}
	if len(rootFiles) != 1 || rootFiles[0] != "root.go" {
		t.Errorf("expected [root.go], got %v", rootFiles)
	}

	// 2. Working copy pkg
	pkgFiles, err := repo.ListDir("working", "pkg")
	if err != nil {
		t.Fatalf("ListDir working pkg failed: %v", err)
	}
	slices.Sort(pkgFiles)
	if len(pkgFiles) != 2 || pkgFiles[0] != "a.go" || pkgFiles[1] != "b.go" {
		t.Errorf("expected [a.go, b.go], got %v", pkgFiles)
	}

	// 3. Historical commit pkg
	c1PkgFiles, err := repo.ListDir(c1Rev, "pkg")
	if err != nil {
		t.Fatalf("ListDir c1 pkg failed: %v", err)
	}
	slices.Sort(c1PkgFiles)
	if len(c1PkgFiles) != 2 || c1PkgFiles[0] != "a.go" || c1PkgFiles[1] != "b.go" {
		t.Errorf("expected [a.go, b.go], got %v", c1PkgFiles)
	}
}

func TestListDirAndContentNonexistent(t *testing.T) {
	t.Run("Git", func(t *testing.T) {
		dir := t.TempDir()
		runGit := func(args ...string) string {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=Tester",
				"GIT_AUTHOR_EMAIL=tester@example.com",
				"GIT_COMMITTER_NAME=Tester",
				"GIT_COMMITTER_EMAIL=tester@example.com",
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
			}
			return strings.TrimSpace(string(out))
		}

		runGit("init", "-b", "main")
		runGit("config", "user.name", "Tester")
		runGit("config", "user.email", "tester@example.com")

		if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("package main\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "root.go")
		runGit("commit", "-m", "init")
		headRev := runGit("rev-parse", "HEAD")

		repo, err := Open(dir, nil)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		for _, rev := range []string{"working", headRev} {
			// ListDir on nonexistent directory must return non-nil empty slice and nil error.
			files, err := repo.ListDir(rev, "vendor/nonexistent")
			if err != nil {
				t.Errorf("[%s] ListDir(vendor/nonexistent) err = %v, want nil", rev, err)
			}
			if files == nil {
				t.Errorf("[%s] ListDir(vendor/nonexistent) returned nil slice, want non-nil empty slice", rev)
			}
			if len(files) != 0 {
				t.Errorf("[%s] ListDir(vendor/nonexistent) len = %d, want 0", rev, len(files))
			}

			// Content on nonexistent file must return os.ErrNotExist.
			data, err := repo.Content(rev, "nonexistent/go.mod")
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("[%s] Content(nonexistent/go.mod) err = %v, want os.ErrNotExist (data=%q)", rev, err, string(data))
			}
		}
	})

	t.Run("JJ", func(t *testing.T) {
		if _, err := exec.LookPath("jj"); err != nil {
			t.Skip("jj not installed")
		}

		dir := t.TempDir()
		runJJ := func(args ...string) string {
			cmd := exec.Command("jj", append([]string{"--config", "ui.paginate=\"never\""}, args...)...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"JJ_USER=Tester",
				"JJ_EMAIL=tester@example.com",
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("jj %v failed: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}

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
		runJJ("git", "init", "--colocate")
		runJJ("config", "set", "--repo", "user.name", "Tester")
		runJJ("config", "set", "--repo", "user.email", "tester@example.com")

		if err := os.WriteFile(filepath.Join(dir, "root.go"), []byte("package main\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runJJ("describe", "-m", "init")
		commitID := runJJ("log", "--no-graph", "-r", "@", "-T", "commit_id")

		repo, err := Open(dir, nil)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		for _, rev := range []string{"working", commitID} {
			// ListDir on nonexistent directory must return non-nil empty slice and nil error.
			files, err := repo.ListDir(rev, "vendor/nonexistent")
			if err != nil {
				t.Errorf("[%s] ListDir(vendor/nonexistent) err = %v, want nil", rev, err)
			}
			if files == nil {
				t.Errorf("[%s] ListDir(vendor/nonexistent) returned nil slice, want non-nil empty slice", rev)
			}
			if len(files) != 0 {
				t.Errorf("[%s] ListDir(vendor/nonexistent) len = %d, want 0", rev, len(files))
			}

			// Content on nonexistent file must return os.ErrNotExist.
			data, err := repo.Content(rev, "nonexistent/go.mod")
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("[%s] Content(nonexistent/go.mod) err = %v, want os.ErrNotExist (data=%q)", rev, err, string(data))
			}
		}
	})
}

func TestListGoFiles(t *testing.T) {
	t.Run("GitRepo", func(t *testing.T) {
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

		filesToCreate := map[string]string{
			"main.go":            "package main\n",
			"pkg/foo.go":         "package pkg\n",
			"pkg/bar/bar.go":     "package bar\n",
			"vendor/vendored.go": "package vendor\n",
			"testdata/test.go":   "package test\n",
			".hidden/hidden.go":  "package hidden\n",
			"_ignore/ignore.go":  "package ignore\n",
			"pkg/.secret.go":     "package pkg\n",
			"pkg/_sub.go":        "package pkg\n",
			"readme.md":          "# Readme\n",
		}

		for path, content := range filesToCreate {
			full := filepath.Join(dir, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}

		runGit("add", "-f", ".")
		runGit("commit", "-m", "init commit")
		commitSHA := runGit("rev-parse", "HEAD")

		repo, err := Open(dir, nil)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		want := []string{"main.go", "pkg/bar/bar.go", "pkg/foo.go"}

		for _, rev := range []string{"working", commitSHA} {
			got, err := repo.ListGoFiles(rev)
			if err != nil {
				t.Fatalf("[%s] ListGoFiles failed: %v", rev, err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("[%s] ListGoFiles = %v, want %v", rev, got, want)
			}
		}
	})

	t.Run("JJRepo", func(t *testing.T) {
		if _, err := exec.LookPath("jj"); err != nil {
			t.Skip("jj not installed")
		}

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
		runJJ := func(args ...string) string {
			cmd := exec.Command("jj", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("jj %v failed: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}

		runGit("init", "-b", "main")
		runJJ("git", "init", "--colocate")
		runJJ("config", "set", "--repo", "user.name", "Tester")
		runJJ("config", "set", "--repo", "user.email", "tester@example.com")

		filesToCreate := map[string]string{
			"main.go":            "package main\n",
			"pkg/foo.go":         "package pkg\n",
			"pkg/bar/bar.go":     "package bar\n",
			"vendor/vendored.go": "package vendor\n",
			"testdata/test.go":   "package test\n",
			".hidden/hidden.go":  "package hidden\n",
			"_ignore/ignore.go":  "package ignore\n",
			"pkg/.secret.go":     "package pkg\n",
			"pkg/_sub.go":        "package pkg\n",
			"readme.md":          "# Readme\n",
		}

		for path, content := range filesToCreate {
			full := filepath.Join(dir, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}

		runJJ("describe", "-m", "init commit")
		commitID := runJJ("log", "--no-graph", "-r", "@", "-T", "commit_id")

		repo, err := Open(dir, nil)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		want := []string{"main.go", "pkg/bar/bar.go", "pkg/foo.go"}

		for _, rev := range []string{"working", commitID} {
			got, err := repo.ListGoFiles(rev)
			if err != nil {
				t.Fatalf("[%s] ListGoFiles failed: %v", rev, err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("[%s] ListGoFiles = %v, want %v", rev, got, want)
			}
		}
	})
}

func TestIsImmutableHexRev(t *testing.T) {
	tests := []struct {
		rev  string
		want bool
	}{
		{"", false},
		{"abc", false},
		{strings.Repeat("a", 39), false},
		{strings.Repeat("a", 40), true},
		{strings.Repeat("A", 40), true},
		{strings.Repeat("0", 40), true},
		{"0123456789abcdef0123456789abcdef01234567", true},
		{strings.Repeat("g", 40), false},
		{strings.Repeat("a", 41), false},
		{strings.Repeat("f", 63), false},
		{strings.Repeat("f", 64), true},
		{strings.Repeat("F", 64), true},
		{strings.Repeat("0", 64), true},
		{strings.Repeat("a", 65), false},
		{strings.Repeat("a", 39) + "z", false},
		{"head", false},
		{"master", false},
	}

	for _, tt := range tests {
		got := IsImmutableHexRev(tt.rev)
		if got != tt.want {
			t.Errorf("IsImmutableHexRev(%q) = %v, want %v", tt.rev, got, tt.want)
		}
	}
}

func TestResolveChange(t *testing.T) {
	changes := []*Change{
		nil,
		{Key: "qpvzllno", Rev: "abcdef1234567890abcdef1234567890abcdef12"},
		{Key: "kxuropmn", Rev: "9876543210fedcba9876543210fedcba98765432"},
		{Key: "abc", Rev: "1111222233334444555566667777888899990000"},
		{Key: "amb1", Rev: "3333000000000000000000000000000000000000"},
		{Key: "amb2", Rev: "4444000000000000000000000000000000000000"},
	}

	tests := []struct {
		name    string
		query   string
		wantKey string
	}{
		{"empty query", "", ""},
		{"exact key", "qpvzllno", "qpvzllno"},
		{"exact rev", "abcdef1234567890abcdef1234567890abcdef12", "qpvzllno"},
		{"key prefix >= 4", "qpvz", "qpvzllno"},
		{"rev prefix >= 4", "98765", "kxuropmn"},
		{"unique 3-char key prefix", "qpv", "qpvzllno"},
		{"unique 3-char rev prefix", "987", "kxuropmn"},
		{"key prefix < 3 not exact", "qp", ""},
		{"rev prefix < 3", "98", ""},
		{"ambiguous 3-char prefix", "amb", ""},
		{"exact short key", "abc", "abc"},
		{"no match", "zzzzzzzz", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveChange(changes, tt.query)
			if tt.wantKey == "" {
				if got != nil {
					t.Fatalf("ResolveChange(%q) = %+v, want nil", tt.query, got)
				}
			} else {
				if got == nil || got.Key != tt.wantKey {
					t.Fatalf("ResolveChange(%q) = %+v, want Key %q", tt.query, got, tt.wantKey)
				}
			}
		})
	}

	if got := ResolveChange(nil, "abc"); got != nil {
		t.Errorf("ResolveChange(nil, ...) = %v, want nil", got)
	}
}

func TestBlobOID(t *testing.T) {
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

	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("nested content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "first commit")
	c1 := runGit("rev-parse", "HEAD")

	repo, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	oider, ok := repo.(BlobOIDer)
	if !ok {
		t.Fatalf("repo does not implement BlobOIDer")
	}

	oid1, err := oider.BlobOID(c1, "file.txt")
	if err != nil {
		t.Fatalf("BlobOID(c1, file.txt) err = %v", err)
	}
	if len(oid1) != 40 && len(oid1) != 64 {
		t.Errorf("BlobOID returned unexpected hash length: %q", oid1)
	}

	oidNested, err := oider.BlobOID(c1, "sub/nested.txt")
	if err != nil {
		t.Fatalf("BlobOID(c1, sub/nested.txt) err = %v", err)
	}
	if len(oidNested) != 40 && len(oidNested) != 64 {
		t.Errorf("BlobOID returned unexpected hash length: %q", oidNested)
	}

	// Missing file in existing tree -> os.ErrNotExist
	_, err = oider.BlobOID(c1, "nonexistent.txt")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("BlobOID(c1, nonexistent.txt) err = %v, want os.ErrNotExist", err)
	}

	// Missing file in nonexistent directory -> os.ErrNotExist
	_, err = oider.BlobOID(c1, "missingdir/file.txt")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("BlobOID(c1, missingdir/file.txt) err = %v, want os.ErrNotExist", err)
	}

	// "working" -> unsupported error
	_, err = oider.BlobOID("working", "file.txt")
	if err == nil {
		t.Errorf("BlobOID(working, file.txt) expected error, got nil")
	}

	// COMMIT_MSG -> unsupported error
	_, err = oider.BlobOID(c1, CommitMsgPath)
	if err == nil {
		t.Errorf("BlobOID(c1, CommitMsgPath) expected error, got nil")
	}

	// Modify file in second commit
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello modified world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file.txt")
	runGit("commit", "-m", "second commit")
	c2 := runGit("rev-parse", "HEAD")

	oid2, err := oider.BlobOID(c2, "file.txt")
	if err != nil {
		t.Fatalf("BlobOID(c2, file.txt) err = %v", err)
	}
	if oid2 == oid1 {
		t.Errorf("expected oid2 != oid1 after modification, got %q", oid2)
	}

	// sub/nested.txt unchanged between c1 and c2 -> identical OID
	oidNested2, err := oider.BlobOID(c2, "sub/nested.txt")
	if err != nil {
		t.Fatalf("BlobOID(c2, sub/nested.txt) err = %v", err)
	}
	if oidNested2 != oidNested {
		t.Errorf("expected identical nested OID across unchanged commits: %s vs %s", oidNested2, oidNested)
	}
}

func TestGoBlobRepoGitAndJJ(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=BlobTester",
			"GIT_AUTHOR_EMAIL=blobtester@example.com",
			"GIT_COMMITTER_NAME=BlobTester",
			"GIT_COMMITTER_EMAIL=blobtester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "BlobTester")
	runGit("config", "user.email", "blobtester@example.com")

	writeFile := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Commit 1: files across multiple dirs, plus vendor/testdata/hidden
	writeFile("root.go", "package root\n")
	writeFile("pkg/a/a.go", "package a\nfunc A() {}\n")
	writeFile("pkg/b/b.go", "package b\nfunc B() {}\n")
	writeFile("pkg/b/sub/nested.go", "package sub\nfunc Nested() {}\n")
	writeFile("vendor/lib.go", "package vendor\n")
	writeFile("testdata/dummy.go", "package testdata\n")
	writeFile("pkg/a/.hidden.go", "package a\n")
	writeFile("pkg/a/_ignore.go", "package a\n")
	writeFile("pkg/a/readme.txt", "not go\n")

	runGit("add", ".")
	runGit("commit", "-m", "commit 1")
	c1 := runGit("rev-parse", "HEAD")

	repo, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	blobRepo, ok := repo.(GoBlobRepo)
	if !ok {
		t.Fatalf("expected repo to implement GoBlobRepo, got %T", repo)
	}

	blobs1, err := blobRepo.ListGoFileBlobs(c1)
	if err != nil {
		t.Fatalf("ListGoFileBlobs(c1) failed: %v", err)
	}

	wantPaths := []string{
		"pkg/a/a.go",
		"pkg/b/b.go",
		"pkg/b/sub/nested.go",
		"root.go",
	}

	var gotPaths []string
	var oids []string
	for _, b := range blobs1 {
		gotPaths = append(gotPaths, b.Path)
		oids = append(oids, b.OID)
		if len(b.OID) != 40 && len(b.OID) != 64 {
			t.Errorf("invalid blob OID length for %s: %s", b.Path, b.OID)
		}
	}

	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("ListGoFileBlobs(c1) paths = %v, want %v", gotPaths, wantPaths)
	}

	// Verify ListGoFiles matches ListGoFileBlobs exactly
	goFiles1, err := repo.ListGoFiles(c1)
	if err != nil {
		t.Fatalf("ListGoFiles(c1) failed: %v", err)
	}
	if !slices.Equal(goFiles1, wantPaths) {
		t.Fatalf("ListGoFiles(c1) = %v, want %v", goFiles1, wantPaths)
	}

	// Test StreamBlobs with valid OIDs and one missing/dummy OID
	testOIDs := append([]string{}, oids...)
	testOIDs = append(testOIDs, "0123456789abcdef0123456789abcdef01234567") // missing dummy OID

	streamedContents := make(map[string]string)
	err = blobRepo.StreamBlobs(testOIDs, func(idx int, oid string, data []byte) {
		streamedContents[oid] = string(data)
	})
	if err != nil {
		t.Fatalf("StreamBlobs failed: %v", err)
	}

	for _, b := range blobs1 {
		data, ok := streamedContents[b.OID]
		if !ok || len(data) == 0 {
			t.Errorf("missing streamed content for %s (%s)", b.Path, b.OID)
		}
	}

	// Commit 2: modify pkg/a/a.go, add pkg/c/c.go
	writeFile("pkg/a/a.go", "package a\nfunc A2() {}\n")
	writeFile("pkg/c/c.go", "package c\nfunc C() {}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "commit 2")
	c2 := runGit("rev-parse", "HEAD")

	blobs2, err := blobRepo.ListGoFileBlobs(c2)
	if err != nil {
		t.Fatalf("ListGoFileBlobs(c2) failed: %v", err)
	}

	wantPaths2 := []string{
		"pkg/a/a.go",
		"pkg/b/b.go",
		"pkg/b/sub/nested.go",
		"pkg/c/c.go",
		"root.go",
	}

	var gotPaths2 []string
	for _, b := range blobs2 {
		gotPaths2 = append(gotPaths2, b.Path)
	}
	if !slices.Equal(gotPaths2, wantPaths2) {
		t.Fatalf("ListGoFileBlobs(c2) paths = %v, want %v", gotPaths2, wantPaths2)
	}

	// Test JJ colocated repository if jj is installed
	if _, err := exec.LookPath("jj"); err == nil {
		jjDir := t.TempDir()
		cmd := exec.Command("jj", "--config", "ui.paginate=\"never\"", "git", "init", "--colocate")
		cmd.Dir = jjDir
		if _, err := cmd.CombinedOutput(); err == nil {
			fullA := filepath.Join(jjDir, "a.go")
			_ = os.WriteFile(fullA, []byte("package a\n"), 0644)
			commitCmd := exec.Command("jj", "--config", "ui.paginate=\"never\"", "commit", "-m", "first jj commit")
			commitCmd.Dir = jjDir
			_ = commitCmd.Run()

			jjRepo, err := Open(jjDir, nil)
			if err == nil {
				if jjBlobRepo, ok := jjRepo.(GoBlobRepo); ok {
					changes, err := jjRepo.Changes()
					if err == nil && len(changes) > 0 {
						jjBlobs, err := jjBlobRepo.ListGoFileBlobs(changes[0].Rev)
						if err == nil && len(jjBlobs) > 0 {
							if jjBlobs[0].Path != "a.go" {
								t.Errorf("JJ ListGoFileBlobs path = %q, want a.go", jjBlobs[0].Path)
							}
						}
					}
				}
			}
		}
	}
}

func TestDirGoBlobRepoGitAndJJ(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=DirBlobTester",
			"GIT_AUTHOR_EMAIL=dirblobtester@example.com",
			"GIT_COMMITTER_NAME=DirBlobTester",
			"GIT_COMMITTER_EMAIL=dirblobtester@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init")
	runGit("config", "user.name", "DirBlobTester")
	runGit("config", "user.email", "dirblobtester@example.com")

	writeFile := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile("pkg/a/a.go", "package a\nfunc A() int { return 1 }\n")
	writeFile("pkg/a/b.go", "package a\nfunc B() int { return 2 }\n")
	writeFile("pkg/a/a_test.go", "package a\nfunc TestA() {}\n")
	writeFile("pkg/a/.hidden.go", "package a\n")
	writeFile("pkg/a/_ignore.go", "package a\n")
	writeFile("pkg/a/readme.txt", "not go code\n")

	runGit("add", ".")
	runGit("commit", "-m", "commit 1")
	c1 := runGit("rev-parse", "HEAD")

	repo, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	dirRepo, ok := repo.(DirGoBlobRepo)
	if !ok {
		t.Fatalf("expected repo to implement DirGoBlobRepo, got %T", repo)
	}

	files, err := dirRepo.ReadDirGoFiles(c1, "pkg/a")
	if err != nil {
		t.Fatalf("ReadDirGoFiles failed: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("ReadDirGoFiles returned %d files, want 2", len(files))
	}

	if files[0].Name != "a.go" || files[1].Name != "b.go" {
		t.Fatalf("ReadDirGoFiles returned names [%s, %s], want [a.go, b.go]", files[0].Name, files[1].Name)
	}
	if files[0].OID == "" || files[1].OID == "" {
		t.Fatalf("ReadDirGoFiles returned empty OID: %v, %v", files[0].OID, files[1].OID)
	}
	if string(files[0].Data) != "package a\nfunc A() int { return 1 }\n" {
		t.Fatalf("ReadDirGoFiles data mismatch on a.go: %q", string(files[0].Data))
	}
	if string(files[1].Data) != "package a\nfunc B() int { return 2 }\n" {
		t.Fatalf("ReadDirGoFiles data mismatch on b.go: %q", string(files[1].Data))
	}

	// Test JJ colocated repository if jj is installed
	if _, err := exec.LookPath("jj"); err == nil {
		jjDir := t.TempDir()
		cmd := exec.Command("jj", "--config", "ui.paginate=\"never\"", "git", "init", "--colocate")
		cmd.Dir = jjDir
		if _, err := cmd.CombinedOutput(); err == nil {
			pkgDir := filepath.Join(jjDir, "pkg", "sub")
			_ = os.MkdirAll(pkgDir, 0755)
			_ = os.WriteFile(filepath.Join(pkgDir, "sub.go"), []byte("package sub\nfunc Sub() {}\n"), 0644)
			_ = os.WriteFile(filepath.Join(pkgDir, "sub_test.go"), []byte("package sub\n"), 0644)
			commitCmd := exec.Command("jj", "--config", "ui.paginate=\"never\"", "commit", "-m", "first jj commit")
			commitCmd.Dir = jjDir
			_ = commitCmd.Run()

			jjRepo, err := Open(jjDir, nil)
			if err == nil {
				if jjDirRepo, ok := jjRepo.(DirGoBlobRepo); ok {
					changes, err := jjRepo.Changes()
					if err == nil && len(changes) > 0 {
						jjFiles, err := jjDirRepo.ReadDirGoFiles(changes[0].Rev, "pkg/sub")
						if err == nil {
							if len(jjFiles) != 1 || jjFiles[0].Name != "sub.go" {
								t.Errorf("JJ ReadDirGoFiles files = %v, want [sub.go]", jjFiles)
							}
						}
					}
				}
			}
		}
	}
}

func FuzzParseGitTreeAndSpecs(f *testing.F) {
	validTree1 := append([]byte("100644 main.go\x00"), make([]byte, 20)...)
	validTree2 := append([]byte("040000 pkg\x00"), make([]byte, 20)...)
	validTree2 = append(validTree2, append([]byte("100644 lib.go\x00"), make([]byte, 20)...)...)
	validTree32 := append([]byte("100644 main.go\x00"), make([]byte, 32)...)

	seeds := []struct {
		tree    []byte
		hashLen uint8
		spec    string
	}{
		{validTree1, 0, "2026-03-01T12:00:00Z"},
		{validTree2, 0, "2026-03-01T12:00:00-05:00"},
		{validTree32, 1, "not a timestamp"},
		{[]byte(""), 0, ""},
		{[]byte("100644"), 0, "100644"},
		{[]byte("100644 \x00"), 0, "100644 \x00"},
		{append([]byte("160000 sub\x00"), make([]byte, 20)...), 0, "2026-03-01T12:00:00.123456789Z"},
		{[]byte("corrupt data without space"), 0, "invalid spec"},
		{[]byte("0"), 0, "0"},
	}

	for _, s := range seeds {
		f.Add(s.tree, s.hashLen, s.spec)
	}

	f.Fuzz(func(t *testing.T, treeBuf []byte, hashLenIdx uint8, specStr string) {
		if len(treeBuf) > 8192 || len(specStr) > 4096 {
			return
		}

		hashLen := 20
		if hashLenIdx%2 == 1 {
			hashLen = 32
		}

		// 1. Fuzz binary tree entry parser
		raw := treeBuf
		count := 0
		for len(raw) > 0 {
			mode, name, sha, rest, ok := nextGitTreeEntry(raw, hashLen)
			if !ok {
				break
			}
			if len(rest) >= len(raw) {
				t.Fatalf("nextGitTreeEntry made no forward progress: len(rest)=%d >= len(raw)=%d", len(rest), len(raw))
			}
			_ = mode
			_ = name
			if len(sha) != hashLen*2 {
				t.Fatalf("expected hex sha len %d, got %d", hashLen*2, len(sha))
			}
			raw = rest
			count++
			if count > 10000 {
				t.Fatalf("too many entries parsed without terminating")
			}
		}

		// 2. Fuzz parseSubtree
		hexSHA := "0123456789abcdef0123456789abcdef01234567"
		if hashLen == 32 {
			hexSHA = strings.Repeat("0123456789abcdef", 4)
		}
		_ = parseSubtree(treeBuf, hexSHA)

		// 3. Fuzz parseJJTimestamp
		_ = parseJJTimestamp(specStr)
	})
}
